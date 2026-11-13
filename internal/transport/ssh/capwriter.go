package ssh

import "sync"

// capWriter keeps the first limit bytes written to it and discards the rest.
//
// Write always reports every byte as written and never returns an error: a
// full buffer must not kill the remote command (the output cap is jumper's
// limit, the command keeps running until it is done or the timeout fires).
type capWriter struct {
	mu        sync.Mutex
	buf       []byte
	limit     int
	truncated bool
}

// newCapWriter returns a writer that keeps the first limit bytes.
func newCapWriter(limit int) *capWriter {
	if limit < 0 {
		limit = 0
	}
	return &capWriter{limit: limit}
}

// Write keeps what still fits and accepts the rest silently.
func (w *capWriter) Write(p []byte) (int, error) {
	n := len(p)
	if n == 0 {
		return 0, nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	room := w.limit - len(w.buf)
	if room <= 0 {
		w.truncated = true
		return n, nil
	}
	if n > room {
		w.buf = append(w.buf, p[:room]...)
		w.truncated = true
		return n, nil
	}
	w.buf = append(w.buf, p...)
	return n, nil
}

// Bytes returns a copy of the kept bytes. It is safe to call while a remote
// command is still writing (a timed-out run may still be draining).
func (w *capWriter) Bytes() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]byte, len(w.buf))
	copy(out, w.buf)
	return out
}

// Len returns how many bytes are buffered.
func (w *capWriter) Len() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.buf)
}

// Truncated reports whether any output was dropped.
func (w *capWriter) Truncated() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.truncated
}
