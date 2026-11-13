//go:build !darwin

package server

import "net"

// peerAllowed always accepts: the peer uid check uses LOCAL_PEERCRED, which
// only exists on darwin. macOS is the only shipping client platform, so the
// other builds exist for tests and CI on Linux.
func peerAllowed(*net.UnixConn) bool { return true }
