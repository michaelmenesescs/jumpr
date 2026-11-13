//go:build darwin

package server

import (
	"net"
	"os"

	"golang.org/x/sys/unix"
)

// peerAllowed reports whether the peer of an accepted unix socket connection
// runs as the same uid as the server (SPEC §21.4). The credential comes from
// LOCAL_PEERCRED, which is asked for before a single frame is read.
func peerAllowed(c *net.UnixConn) bool {
	if c == nil {
		return false
	}
	raw, err := c.SyscallConn()
	if err != nil {
		return false
	}
	var (
		cred    *unix.Xucred
		credErr error
	)
	if err := raw.Control(func(fd uintptr) {
		cred, credErr = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
	}); err != nil {
		return false
	}
	if credErr != nil || cred == nil {
		return false
	}
	return int(cred.Uid) == os.Getuid()
}
