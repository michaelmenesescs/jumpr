package linux

import (
	"regexp"
	"strconv"
	"strings"
)

// SockProc is one process that holds a socket, from the `users:((...))` column.
type SockProc struct {
	Name string
	PID  int
	FD   int
}

// Socket is one row of `ss -H -tuanp`.
type Socket struct {
	Proto     string
	State     string
	RecvQ     int
	SendQ     int
	LocalAddr string
	LocalPort string
	PeerAddr  string
	PeerPort  string
	Procs     []SockProc
}

// ssProcRE matches one entry of the `users:(("name",pid=1,fd=2),...)` column.
var ssProcRE = regexp.MustCompile(`\("([^"]*)",pid=(\d+),fd=(\d+)\)`)

// ssSplitAddr splits an address at the last colon, so IPv6 forms like
// `[::]:22` and scoped forms like `127.0.0.53%lo:53` keep their address intact.
func ssSplitAddr(s string) (addr, port string) {
	i := strings.LastIndexByte(s, ':')
	if i < 0 {
		return s, ""
	}
	return s[:i], s[i+1:]
}

// ParseSS parses `ss -H -tuanp` output. The process column is optional; each
// `("name",pid=N,fd=N)` entry becomes one SockProc. Lines that do not carry at
// least the six fixed fields with numeric queues are skipped and counted.
func ParseSS(out string) ([]Socket, int) {
	var socks []Socket
	skipped := 0
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 6 {
			skipped++
			continue
		}
		recvq, err := strconv.Atoi(fields[2])
		if err != nil {
			skipped++
			continue
		}
		sendq, err := strconv.Atoi(fields[3])
		if err != nil {
			skipped++
			continue
		}
		localAddr, localPort := ssSplitAddr(fields[4])
		peerAddr, peerPort := ssSplitAddr(fields[5])
		s := Socket{
			Proto:     fields[0],
			State:     fields[1],
			RecvQ:     recvq,
			SendQ:     sendq,
			LocalAddr: localAddr,
			LocalPort: localPort,
			PeerAddr:  peerAddr,
			PeerPort:  peerPort,
		}
		for _, m := range ssProcRE.FindAllStringSubmatch(line, -1) {
			pid, err := strconv.Atoi(m[2])
			if err != nil {
				continue
			}
			fd, err := strconv.Atoi(m[3])
			if err != nil {
				continue
			}
			s.Procs = append(s.Procs, SockProc{Name: m[1], PID: pid, FD: fd})
		}
		socks = append(socks, s)
	}
	return socks, skipped
}

// Listening reports whether the socket accepts inbound traffic: `LISTEN` for
// TCP, or `UNCONN` on a UDP socket.
func (s Socket) Listening() bool {
	switch s.State {
	case "LISTEN":
		return true
	case "UNCONN":
		return strings.HasPrefix(strings.ToLower(s.Proto), "udp")
	}
	return false
}

// ListenersOnPort returns the listening sockets bound to port, in input order.
func ListenersOnPort(socks []Socket, port string) []Socket {
	var out []Socket
	for _, s := range socks {
		if s.Listening() && s.LocalPort == port {
			out = append(out, s)
		}
	}
	return out
}
