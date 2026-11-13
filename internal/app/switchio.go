package app

import (
	"io"
	"log/slog"
	"sync"

	"jumper/internal/safe"
)

// SwitchWriter is the output pipe of a session whose terminal can come and go.
// Bubble Tea writes every frame here; Write forwards to the writer of the
// client that is attached right now, or discards the frame when nobody is
// attached (BUILD-PLAN §6: rendering happens in the server, detached output is
// thrown away).
//
// A writer that fails is detached: its error goes to onError once and is never
// returned to Bubble Tea, because a renderer that sees a write error stops for
// good and the session would be mute after the terminal was unplugged.
type SwitchWriter struct {
	mu      sync.Mutex
	w       io.Writer
	onError func(error)
}

// NewSwitchWriter returns a writer with no destination; onError may be nil.
func NewSwitchWriter(onError func(error)) *SwitchWriter {
	return &SwitchWriter{onError: onError}
}

// Write sends p to the current writer. With none attached the bytes are
// discarded and the write reports success.
func (s *SwitchWriter) Write(p []byte) (int, error) {
	if s == nil {
		return len(p), nil
	}
	s.mu.Lock()
	w := s.w
	if w == nil {
		s.mu.Unlock()
		return len(p), nil
	}
	n, err := w.Write(p)
	if err == nil {
		s.mu.Unlock()
		return n, nil
	}
	s.w = nil
	report := s.onError
	s.mu.Unlock()
	if report != nil {
		report(err)
	}
	// The frame is lost, but Bubble Tea must not learn about it.
	return len(p), nil
}

// Set makes w the destination of every later write.
func (s *SwitchWriter) Set(w io.Writer) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.w = w
	s.mu.Unlock()
}

// Clear detaches the destination: later writes are discarded.
func (s *SwitchWriter) Clear() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.w = nil
	s.mu.Unlock()
}

// SwitchReader is the input pipe of a session whose terminal can come and go.
// Read blocks until a client is attached and has bytes to give.
//
// The one rule that matters (task 078 pitfall): Read never returns the error of
// its source, including io.EOF. Bubble Tea's input loop ends on the first error
// it sees, so a return here would make the session deaf for the rest of its
// life. A failed source is cleared, reported through onError and Read keeps
// blocking until a new source is set. Only Close makes Read return io.EOF.
type SwitchReader struct {
	mu      sync.Mutex
	src     io.Reader
	pump    chan struct{} // closed to stop the goroutine reading src
	wake    chan struct{} // closed and replaced on every state change
	closed  bool
	onError func(error)
	// log is where a panic in the source's reader goroutine is reported. It is
	// nil for a reader built without one: the panic is still recovered.
	log *slog.Logger

	data chan []byte // chunks produced by the current source's goroutine
	pend []byte      // bytes of the last chunk Read did not hand out yet
}

// NewSwitchReader returns a reader with no source; onError may be nil.
func NewSwitchReader(onError func(error)) *SwitchReader {
	return &SwitchReader{
		wake:    make(chan struct{}),
		data:    make(chan []byte),
		onError: onError,
	}
}

// SetLog gives the reader the session's logger, so a panic while it is pumping
// a client's input is recovered and logged instead of ending the process.
func (s *SwitchReader) SetLog(log *slog.Logger) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.log = log
	s.mu.Unlock()
}

// Read blocks until a source is set and returns at most one chunk from it.
func (s *SwitchReader) Read(p []byte) (int, error) {
	if s == nil {
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}
	for {
		if len(s.pend) > 0 {
			n := copy(p, s.pend)
			s.pend = s.pend[n:]
			if len(s.pend) == 0 {
				s.pend = nil
			}
			return n, nil
		}
		s.mu.Lock()
		closed, wake := s.closed, s.wake
		s.mu.Unlock()
		if closed {
			return 0, io.EOF
		}
		select {
		case chunk := <-s.data:
			s.pend = chunk
		case <-wake:
			// A source was set, cleared or closed: look again.
		}
	}
}

// Set makes r the source of every later Read. Replacing a source stops its
// reader goroutine; a source that was blocked inside Read finishes on its own.
func (s *SwitchReader) Set(r io.Reader) {
	if s == nil {
		return
	}
	if r == nil {
		s.Clear()
		return
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	old := s.pump
	s.src = r
	s.pump = make(chan struct{})
	stop := s.pump
	close(s.wake)
	s.wake = make(chan struct{})
	log := s.log
	s.mu.Unlock()
	if old != nil {
		close(old)
	}
	safe.Go(log, "switch reader", func() { s.pumpChunks(r, stop) })
}

// Clear detaches the source: Read keeps blocking until Set is called again.
func (s *SwitchReader) Clear() {
	if s == nil {
		return
	}
	s.mu.Lock()
	stop := s.pump
	s.pump = nil
	s.src = nil
	close(s.wake)
	s.wake = make(chan struct{})
	s.mu.Unlock()
	if stop != nil {
		close(stop)
	}
}

// Close ends the reader: the next Read (and any blocked one) returns io.EOF.
func (s *SwitchReader) Close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	stop := s.pump
	s.pump = nil
	s.src = nil
	s.closed = true
	close(s.wake)
	s.wake = make(chan struct{})
	s.mu.Unlock()
	if stop != nil {
		close(stop)
	}
}

// pumpChunks copies the source into Read one Read at a time, so a client that
// writes one byte per Write still reaches Bubble Tea as one byte per message.
func (s *SwitchReader) pumpChunks(r io.Reader, stop chan struct{}) {
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			chunk := append([]byte(nil), buf[:n]...)
			select {
			case s.data <- chunk:
			case <-stop:
				return
			}
		}
		if err != nil {
			s.fail(r, err)
			return
		}
		select {
		case <-stop:
			return
		default:
		}
	}
}

// fail clears the source that returned err, if it is still the current one, and
// reports the error. Read keeps blocking: a detach is not an end of input.
func (s *SwitchReader) fail(r io.Reader, err error) {
	s.mu.Lock()
	if s.src != r {
		// Already replaced: the new source's reader is not the failing one.
		s.mu.Unlock()
		return
	}
	s.src = nil
	s.pump = nil
	close(s.wake)
	s.wake = make(chan struct{})
	report := s.onError
	s.mu.Unlock()
	if report != nil {
		report(err)
	}
}
