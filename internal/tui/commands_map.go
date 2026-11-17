package tui

import (
	"context"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"jumpr/internal/resource"
	"jumpr/internal/safe"
	"jumpr/internal/svcmap"
	"jumpr/internal/transport"
)

// `:map` is the service map: one text view that says what talks to what.
//
//	:map          the cluster and the hosts
//	:map k8s      the Kubernetes cluster only
//	:map hosts    the SSH hosts only
//
// The cluster half comes from the Kubernetes backend (observed edges from
// Hubble when the cluster runs Cilium, declared edges on every cluster). The
// host half is built here from each host's sockets: a listening socket names
// the systemd unit that owns it, and an established connection to another
// host's listener is an edge. Everything is read-only.
const (
	mapUsage = "usage: :map [k8s|hosts]"
	// mapTimeout bounds the whole collection.
	mapTimeout = 40 * time.Second
	// mapHostTimeout bounds one host's socket listing.
	mapHostTimeout = 10 * time.Second
	// mapWorkers is how many hosts are asked at once.
	mapWorkers = 16
)

// K8sServiceMapper is what `:map` needs from the Kubernetes backend.
type K8sServiceMapper interface {
	ServiceMap(ctx context.Context) (*svcmap.Graph, error)
}

// socketScript lists a host's addresses, its listening TCP sockets and its
// established TCP connections, each socket with the process and the systemd
// unit that owns it when sudo -n allows ss to say.
const socketScript = `S=""; if sudo -n true 2>/dev/null; then S="sudo -n"; fi; ` +
	`ip -o -4 addr show scope global 2>/dev/null | awk '{split($4,a,"/"); print "addr", a[1]}'; ` +
	`own() { pid=$(printf '%s' "$1" | sed -n 's/.*pid=\([0-9]*\).*/\1/p'); name=$(printf '%s' "$1" | sed -n 's/.*(("\([^"]*\)".*/\1/p'); unit=""; ` +
	`if [ -n "$pid" ]; then unit=$(ps -o unit= -p "$pid" 2>/dev/null | tr -d ' '); fi; echo "${name:--} ${unit:--}"; }; ` +
	`$S ss -H -ltnp 2>/dev/null | while read -r st rq sq loc peer users; do echo "listen $loc $(own "$users")"; done; ` +
	`$S ss -H -tnp state established 2>/dev/null | while read -r rq sq loc peer users; do echo "conn $loc $peer $(own "$users")"; done`

// RegisterMapCommand registers `:map` on c.
func RegisterMapCommand(c *Commands) error {
	if c == nil {
		return nil
	}
	return c.Register(Command{
		Name:  "map",
		Usage: ":map [k8s|hosts]",
		Help:  "service map: what talks to what, observed (Hubble, sockets) and declared",
		Run:   runMap,
	})
}

// hostSockets is one host's parsed socket listing.
type hostSockets struct {
	host   resource.HostID
	addrs  []string
	listen map[string]svcmap.Vertex // port -> owner
	conns  []hostConn
	err    error
}

type hostConn struct {
	localPort string
	peerAddr  string
	peerPort  string
	owner     svcmap.Vertex
}

// splitHostPort splits "10.0.0.1:443", "[::ffff:10.0.0.1]:443" and "*:22".
func splitHostPort(s string) (addr, port string) {
	i := strings.LastIndex(s, ":")
	if i < 0 {
		return s, ""
	}
	addr, port = s[:i], s[i+1:]
	addr = strings.Trim(addr, "[]")
	addr = strings.TrimPrefix(addr, "::ffff:")
	if j := strings.IndexByte(addr, '%'); j >= 0 {
		addr = addr[:j] // interface scope, e.g. 127.0.0.53%lo
	}
	return addr, port
}

// socketOwner names who owns a socket on host: the systemd unit when ss could
// name a process inside a service unit, the process when only that is known,
// and the host itself otherwise.
func socketOwner(host resource.HostID, name, unit string) svcmap.Vertex {
	switch {
	case strings.HasSuffix(unit, ".service"):
		return svcmap.Vertex{Kind: svcmap.Unit, Scope: string(host), Name: unit}
	case name != "" && name != "-":
		return svcmap.Vertex{Kind: svcmap.Process, Scope: string(host), Name: name}
	default:
		return svcmap.Vertex{Kind: svcmap.Host, Name: string(host)}
	}
}

// parseSockets reads socketScript's output. Lines it does not know are skipped.
func parseSockets(host resource.HostID, out string) hostSockets {
	hs := hostSockets{host: host, listen: map[string]svcmap.Vertex{}}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		switch {
		case len(f) == 2 && f[0] == "addr":
			hs.addrs = append(hs.addrs, f[1])
		case len(f) >= 4 && f[0] == "listen":
			if _, port := splitHostPort(f[1]); port != "" {
				owner := socketOwner(host, f[2], f[3])
				if old, ok := hs.listen[port]; !ok || old.Kind == svcmap.Host {
					hs.listen[port] = owner
				}
			}
		case len(f) >= 5 && f[0] == "conn":
			_, lp := splitHostPort(f[1])
			pa, pp := splitHostPort(f[2])
			hs.conns = append(hs.conns, hostConn{localPort: lp, peerAddr: pa, peerPort: pp, owner: socketOwner(host, f[3], f[4])})
		}
	}
	return hs
}

// buildHostMap turns every host's sockets into a graph. An established
// connection whose local port is not one of the host's own listeners was
// opened by this host, so it is an edge from its owner to whoever listens on
// the peer's port. Connections to this host's own listeners are the same
// edges seen from the other side, and are left to that side.
func buildHostMap(all []hostSockets) *svcmap.Graph {
	g := svcmap.New()
	byAddr := map[string]*hostSockets{}
	for i := range all {
		for _, a := range all[i].addrs {
			byAddr[a] = &all[i]
		}
	}
	unknownInbound := 0
	failed := 0
	for i := range all {
		hs := &all[i]
		if hs.err != nil {
			failed++
			continue
		}
		g.Add(svcmap.Vertex{Kind: svcmap.Host, Name: string(hs.host)})
		for _, c := range hs.conns {
			if _, own := hs.listen[c.localPort]; own {
				if _, known := byAddr[c.peerAddr]; !known && !strings.HasPrefix(c.peerAddr, "127.") && c.peerAddr != "::1" {
					unknownInbound++
				}
				continue
			}
			var to svcmap.Vertex
			switch peer, known := byAddr[c.peerAddr]; {
			case strings.HasPrefix(c.peerAddr, "127.") || c.peerAddr == "::1":
				l, ok := hs.listen[c.peerPort]
				if !ok {
					continue
				}
				to = l
			case known:
				if l, ok := peer.listen[c.peerPort]; ok {
					to = l
				} else {
					to = svcmap.Vertex{Kind: svcmap.Host, Name: string(peer.host)}
				}
			default:
				to = svcmap.Vertex{Kind: svcmap.External, Name: c.peerAddr}
			}
			g.Connect(svcmap.Edge{From: g.Add(c.owner), To: g.Add(to), Port: c.peerPort + "/TCP", Source: svcmap.Socket, Forwarded: 1})
		}
	}
	g.Notes = append(g.Notes, "host edges are TCP connections established right now; the count is connections, and short-lived ones are missed")
	if unknownInbound > 0 {
		g.Notes = append(g.Notes, plainCount(unknownInbound, "inbound connection")+" from addresses outside the inventory are not drawn")
	}
	if failed > 0 {
		g.Notes = append(g.Notes, plainCount(failed, "host")+" could not be asked for sockets")
	}
	return g
}

func plainCount(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return strconv.Itoa(n) + " " + word + "s"
}

// collectHostMap asks every host for its sockets, a bounded number at a time.
func collectHostMap(ctx context.Context, log *slog.Logger, exec transport.Executor, hosts []resource.HostID) *svcmap.Graph {
	all := make([]hostSockets, len(hosts))
	for i, h := range hosts {
		// A worker that dies leaves its host marked as not asked.
		all[i] = hostSockets{host: h, err: context.Canceled}
	}
	sem := make(chan struct{}, mapWorkers)
	var wg sync.WaitGroup
	for i, h := range hosts {
		wg.Add(1)
		safe.Go(log, "tui.map.sockets", func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			res, err := exec.Exec(ctx, h, transport.Command{Script: socketScript, Timeout: mapHostTimeout})
			if err != nil {
				all[i] = hostSockets{host: h, err: err}
				return
			}
			all[i] = parseSockets(h, string(res.Stdout))
		})
	}
	wg.Wait()
	sort.Slice(all, func(i, j int) bool { return all[i].host < all[j].host })
	return buildHostMap(all)
}

func runMap(env *Env, args string) tea.Cmd {
	scope := strings.TrimSpace(args)
	switch scope {
	case "", "k8s", "hosts":
	default:
		return Flash(FlashError, "%s", mapUsage)
	}
	if env == nil {
		return Flash(FlashError, "no session")
	}
	var mapper K8sServiceMapper
	if env.Registry != nil && scope != "hosts" {
		if b, ok := env.Registry.Get(k8sBackendName); ok {
			mapper, _ = b.(K8sServiceMapper)
		}
	}
	var hosts []resource.HostID
	if env.Exec != nil && scope != "k8s" {
		hosts = probeHosts(env)
	}
	if mapper == nil && len(hosts) == 0 {
		return Flash(FlashError, "nothing to map: no Kubernetes backend and no hosts")
	}
	exec := env.Exec
	log := env.Log
	ascii := asciiOnly(env)
	work := func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), mapTimeout)
		defer cancel()
		var lines []string
		if mapper != nil {
			lines = append(lines, "KUBERNETES", "")
			g, err := mapper.ServiceMap(ctx)
			if err != nil {
				lines = append(lines, "could not read the cluster: "+err.Error(), "")
			} else {
				lines = append(lines, g.Lines(ascii)...)
				lines = append(lines, "")
			}
		}
		if len(hosts) > 0 {
			lines = append(lines, "HOSTS", "")
			lines = append(lines, collectHostMap(ctx, log, exec, hosts).Lines(ascii)...)
		}
		return PushViewMsg{View: newTextPage(env, "service map", lines)}
	}
	return tea.Batch(Flash(FlashInfo, "building the service map"), work)
}
