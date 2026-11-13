package app

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// countWriter records what reaches it and can be made to fail.
type countWriter struct {
	mu   sync.Mutex
	buf  bytes.Buffer
	fail error
}

func (w *countWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.fail != nil {
		return 0, w.fail
	}
	return w.buf.Write(p)
}

func (w *countWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// TestSwitchWriterDiscardsWhenCleared is requirement 3: with no client the
// frames go nowhere and the write still reports success, which is what keeps
// Bubble Tea rendering while detached.
func TestSwitchWriterDiscardsWhenCleared(t *testing.T) {
	sw := NewSwitchWriter(nil)
	if n, err := sw.Write([]byte("frame")); n != 5 || err != nil {
		t.Fatalf("Write with no writer = %d, %v; want 5, nil", n, err)
	}
	sink := &countWriter{}
	sw.Set(sink)
	if _, err := sw.Write([]byte("hello")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	sw.Clear()
	if _, err := sw.Write([]byte("lost")); err != nil {
		t.Fatalf("Write after Clear: %v", err)
	}
	if got := sink.String(); got != "hello" {
		t.Fatalf("sink = %q, want %q", got, "hello")
	}
}

// TestSwitchWriterFailingWriterDroppedOnce is requirement 3's other half: the
// error is reported once, the writer is detached, and the error never reaches
// Bubble Tea.
func TestSwitchWriterFailingWriterDroppedOnce(t *testing.T) {
	var mu sync.Mutex
	var reports []error
	sw := NewSwitchWriter(func(err error) {
		mu.Lock()
		reports = append(reports, err)
		mu.Unlock()
	})
	boom := errors.New("broken pipe")
	failing := &countWriter{fail: boom}
	sw.Set(failing)

	for i := 0; i < 3; i++ {
		n, err := sw.Write([]byte("frame"))
		if err != nil {
			t.Fatalf("Write %d returned %v, want nil", i, err)
		}
		if n != 5 {
			t.Fatalf("Write %d = %d bytes, want 5", i, n)
		}
	}
	mu.Lock()
	got := len(reports)
	var first error
	if got > 0 {
		first = reports[0]
	}
	mu.Unlock()
	if got != 1 {
		t.Fatalf("%d reports for a failing writer, want 1", got)
	}
	if !errors.Is(first, boom) {
		t.Fatalf("reported %v, want %v", first, boom)
	}
}

// TestSwitchReaderBlocksUntilSource is requirement 3: Read waits for a client.
func TestSwitchReaderBlocksUntilSource(t *testing.T) {
	sr := NewSwitchReader(nil)
	defer sr.Close()

	type result struct {
		n   int
		err error
	}
	got := make(chan result, 1)
	go func() {
		p := make([]byte, 8)
		n, err := sr.Read(p)
		got <- result{n: n, err: err}
	}()

	select {
	case r := <-got:
		t.Fatalf("Read returned %+v with no source", r)
	case <-time.After(50 * time.Millisecond):
	}

	pr, pw := io.Pipe()
	defer pw.Close()
	sr.Set(pr)
	if _, err := pw.Write([]byte("x")); err != nil {
		t.Fatalf("pipe write: %v", err)
	}
	select {
	case r := <-got:
		if r.err != nil || r.n != 1 {
			t.Fatalf("Read = %d, %v; want 1, nil", r.n, r.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Read never returned the byte that was written")
	}
}

// TestSwitchReaderEOFDoesNotSurface is the single most important detail of M5:
// an io.EOF from the source must not reach Bubble Tea, or the input loop ends
// for good and the session is deaf after the first detach.
func TestSwitchReaderEOFDoesNotSurface(t *testing.T) {
	var mu sync.Mutex
	var reports []error
	sr := NewSwitchReader(func(err error) {
		mu.Lock()
		reports = append(reports, err)
		mu.Unlock()
	})
	defer sr.Close()

	type result struct {
		n    int
		data string
		err  error
	}
	got := make(chan result, 1)
	read := func() {
		p := make([]byte, 16)
		n, err := sr.Read(p)
		got <- result{n: n, data: string(p[:n]), err: err}
	}

	// A source that ends at once: Read must survive it and keep blocking.
	sr.Set(strings.NewReader(""))
	go read()
	select {
	case r := <-got:
		t.Fatalf("Read returned %+v after the source's EOF", r)
	case <-time.After(100 * time.Millisecond):
	}
	mu.Lock()
	reported := len(reports)
	mu.Unlock()
	if reported != 1 {
		t.Fatalf("%d errors reported for the ending source, want 1", reported)
	}

	// A new source is picked up by the same blocked Read.
	sr.Set(strings.NewReader("abc"))
	select {
	case r := <-got:
		if r.err != nil || r.data != "abc" {
			t.Fatalf("Read = %q, %v; want \"abc\", nil", r.data, r.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Read never used the source set after the EOF")
	}
}

// TestSwitchReaderCloseReturnsEOF is the only way Read may report io.EOF.
func TestSwitchReaderCloseReturnsEOF(t *testing.T) {
	sr := NewSwitchReader(nil)
	done := make(chan error, 1)
	go func() {
		p := make([]byte, 4)
		_, err := sr.Read(p)
		done <- err
	}()
	time.Sleep(20 * time.Millisecond)
	sr.Close()
	select {
	case err := <-done:
		if !errors.Is(err, io.EOF) {
			t.Fatalf("Read after Close = %v, want io.EOF", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not end the blocked Read")
	}
}
