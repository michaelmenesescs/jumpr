package app

import "fmt"

// ReadOnlyConflict is the message for a client that asked for --readonly while
// a server that was not started read-only is already running: the flag is a
// property of the server, so attaching would silently give a writable session.
// It is "" when there is no conflict (no live server, or it is read-only).
func ReadOnlyConflict(socketPath, statePath string, wantReadOnly bool) string {
	if !wantReadOnly || dialServer(socketPath) != nil {
		return ""
	}
	if st, err := ReadServerState(statePath); err == nil && st.ReadOnly {
		return ""
	}
	return fmt.Sprintf("a jumper server is already running and was not started with --readonly, "+
		"so it would allow writes. Run `jumper kill-server` and start again with --readonly "+
		"(socket %s), or use `jumper --standalone --readonly`.", socketPath)
}
