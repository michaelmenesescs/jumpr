package server

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"math/rand"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"jumper/internal/errs"
)

func TestFrameRoundTripAllTypes(t *testing.T) {
	types := []byte{FrameHello, FrameWelcome, FrameStdin, FrameStdout, FrameResize, FrameDetach, FrameExit, FrameResult}
	for _, typ := range types {
		payload := []byte("payload for " + string(rune(typ)))
		var buf bytes.Buffer
		if err := WriteFrame(&buf, typ, payload); err != nil {
			t.Fatalf("type %#x: WriteFrame: %v", typ, err)
		}
		gotType, gotPayload, err := ReadFrame(&buf)
		if err != nil {
			t.Fatalf("type %#x: ReadFrame: %v", typ, err)
		}
		if gotType != typ {
			t.Errorf("type %#x: got type %#x", typ, gotType)
		}
		if !bytes.Equal(gotPayload, payload) {
			t.Errorf("type %#x: payload %q, want %q", typ, gotPayload, payload)
		}
	}
}

func TestFrameEmptyPayload(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteFrame(&buf, FrameDetach, nil); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	if buf.Len() != 5 {
		t.Fatalf("frame length %d, want 5", buf.Len())
	}
	typ, payload, err := ReadFrame(&buf)
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if typ != FrameDetach || len(payload) != 0 {
		t.Fatalf("got type %#x payload %q", typ, payload)
	}
}

func TestFrameHeaderLayout(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteFrame(&buf, FrameStdout, []byte("abc")); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	b := buf.Bytes()
	if b[0] != FrameStdout {
		t.Errorf("type byte %#x", b[0])
	}
	if got := binary.BigEndian.Uint32(b[1:5]); got != 3 {
		t.Errorf("length %d, want 3 (big endian)", got)
	}
	if string(b[5:]) != "abc" {
		t.Errorf("payload %q", b[5:])
	}
}

func TestFrameSingleWrite(t *testing.T) {
	cw := &countingWriter{}
	if err := WriteFrame(cw, FrameStdout, bytes.Repeat([]byte("x"), 1000)); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	if cw.calls != 1 {
		t.Fatalf("Write called %d times, want 1", cw.calls)
	}
}

func TestFrameOversizeWrite(t *testing.T) {
	var buf bytes.Buffer
	err := WriteFrame(&buf, FrameStdout, make([]byte, MaxPayload+1))
	if !errs.Is(err, errs.Protocol) {
		t.Fatalf("err = %v, want protocol", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("wrote %d bytes for a rejected frame", buf.Len())
	}
}

func TestFrameOversizeRead(t *testing.T) {
	var hdr [5]byte
	hdr[0] = FrameStdout
	binary.BigEndian.PutUint32(hdr[1:], MaxPayload+1)
	_, _, err := ReadFrame(bytes.NewReader(hdr[:]))
	if !errs.Is(err, errs.Protocol) {
		t.Fatalf("err = %v, want protocol", err)
	}
}

func TestFrameTruncatedHeader(t *testing.T) {
	for _, n := range []int{1, 3, 4} {
		var buf bytes.Buffer
		if err := WriteFrame(&buf, FrameStdout, []byte("abc")); err != nil {
			t.Fatalf("WriteFrame: %v", err)
		}
		_, _, err := ReadFrame(bytes.NewReader(buf.Bytes()[:n]))
		if !errs.Is(err, errs.Protocol) {
			t.Fatalf("%d header bytes: err = %v, want protocol", n, err)
		}
	}
}

func TestFrameTruncatedPayload(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteFrame(&buf, FrameStdout, []byte("abcdef")); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	_, _, err := ReadFrame(bytes.NewReader(buf.Bytes()[:8]))
	if !errs.Is(err, errs.Protocol) {
		t.Fatalf("err = %v, want protocol", err)
	}
}

func TestFrameCleanEOF(t *testing.T) {
	_, _, err := ReadFrame(bytes.NewReader(nil))
	if !errors.Is(err, io.EOF) {
		t.Fatalf("err = %v, want io.EOF", err)
	}
	// EOF after a complete frame is still a clean EOF on the next read.
	var buf bytes.Buffer
	if err := WriteFrame(&buf, FrameDetach, nil); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	if _, _, err := ReadFrame(&buf); err != nil {
		t.Fatalf("first frame: %v", err)
	}
	if _, _, err := ReadFrame(&buf); !errors.Is(err, io.EOF) {
		t.Fatalf("second frame: err = %v, want io.EOF", err)
	}
}

func TestFrameReaderError(t *testing.T) {
	want := errors.New("boom")
	_, _, err := ReadFrame(failingReader{err: want})
	if !errs.Is(err, errs.Protocol) {
		t.Fatalf("err = %v, want protocol", err)
	}
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want it to wrap %v", err, want)
	}
}

func TestFrameShortWrite(t *testing.T) {
	err := WriteFrame(shortWriter{}, FrameStdout, []byte("abc"))
	if !errs.Is(err, errs.Protocol) {
		t.Fatalf("err = %v, want protocol", err)
	}
}

func TestJSONRoundTrip(t *testing.T) {
	now := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	cases := []struct {
		typ byte
		v   any
		out any
	}{
		{
			FrameHello,
			Hello{
				Protocol: ProtocolVersion,
				Version:  "1.2.3",
				Command:  "attach",
				Session:  "main",
				Detached: true,
				Cols:     132,
				Rows:     40,
				Env:      map[string]string{"TERM": "xterm-256color"},
			},
			&Hello{},
		},
		{
			FrameWelcome,
			Welcome{Protocol: ProtocolVersion, Version: "1.2.3", Session: "main", Created: true},
			&Welcome{},
		},
		{FrameResize, Resize{Cols: 132, Rows: 40}, &Resize{}},
		{FrameExit, Exit{Reason: "detached", Message: "bye"}, &Exit{}},
		{
			FrameResult,
			Result{
				OK:       true,
				Error:    "",
				Sessions: []SessionInfo{{Name: "main", Windows: 3, Attached: true, Created: now, Activity: now}},
			},
			&Result{},
		},
	}
	for _, c := range cases {
		var buf bytes.Buffer
		if err := WriteJSON(&buf, c.typ, c.v); err != nil {
			t.Fatalf("type %#x: WriteJSON: %v", c.typ, err)
		}
		typ, payload, err := ReadFrame(&buf)
		if err != nil {
			t.Fatalf("type %#x: ReadFrame: %v", c.typ, err)
		}
		if typ != c.typ {
			t.Fatalf("type %#x: frame type %#x", c.typ, typ)
		}
		if err := json.Unmarshal(payload, c.out); err != nil {
			t.Fatalf("type %#x: decode: %v", c.typ, err)
		}
		if !reflect.DeepEqual(c.v, reflect.ValueOf(c.out).Elem().Interface()) {
			t.Errorf("type %#x: round trip mismatch: %#v vs %#v", c.typ, c.v, c.out)
		}
	}
}

func TestJSONTags(t *testing.T) {
	for _, c := range []struct {
		name string
		v    any
		want string
	}{
		{"hello", Hello{Protocol: 1, Command: "ls", Env: map[string]string{"TERM": "x"}},
			`{"protocol":1,"version":"","command":"ls","session":"","detached":false,"cols":0,"rows":0,"env":{"TERM":"x"}}`},
		{"welcome", Welcome{Protocol: 1, Session: "main"}, `{"protocol":1,"version":"","session":"main","created":false}`},
		{"resize", Resize{Cols: 132, Rows: 40}, `{"cols":132,"rows":40}`},
		{"exit", Exit{Reason: "ended", Message: "m"}, `{"reason":"ended","message":"m"}`},
		{"result", Result{OK: true}, `{"ok":true,"error":"","sessions":null}`},
	} {
		var buf bytes.Buffer
		if err := WriteJSON(&buf, FrameDetach, c.v); err != nil {
			t.Fatalf("%s: WriteJSON: %v", c.name, err)
		}
		_, payload, err := ReadFrame(&buf)
		if err != nil {
			t.Fatalf("%s: ReadFrame: %v", c.name, err)
		}
		if string(payload) != c.want {
			t.Errorf("%s: JSON %s, want %s", c.name, payload, c.want)
		}
	}
}

func TestJSONUnknownFieldsIgnored(t *testing.T) {
	var h Hello
	if err := json.Unmarshal([]byte(`{"protocol":1,"session":"main","future":{"x":1}}`), &h); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if h.Protocol != 1 || h.Session != "main" {
		t.Fatalf("got %#v", h)
	}
}

func TestChunkWriterSplits100KiB(t *testing.T) {
	var buf bytes.Buffer
	var mu sync.Mutex
	w := ChunkWriter(&buf, FrameStdout, &mu)
	payload := make([]byte, 100<<10)
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	n, err := w.Write(payload)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != len(payload) {
		t.Fatalf("Write returned %d, want %d", n, len(payload))
	}

	r := bytes.NewReader(buf.Bytes())
	var got []byte
	frames := 0
	for {
		typ, p, err := ReadFrame(r)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("frame %d: %v", frames, err)
		}
		if typ != FrameStdout {
			t.Fatalf("frame %d: type %#x", frames, typ)
		}
		if len(p) > MaxChunk {
			t.Fatalf("frame %d: payload %d bytes, limit %d", frames, len(p), MaxChunk)
		}
		got = append(got, p...)
		frames++
	}
	if frames != 4 {
		t.Fatalf("got %d frames, want 4", frames)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("reassembled payload differs (%d vs %d bytes)", len(got), len(payload))
	}
}

func TestChunkWriterExactAndEmpty(t *testing.T) {
	var buf bytes.Buffer
	w := ChunkWriter(&buf, FrameStdin, nil)
	if n, err := w.Write(nil); n != 0 || err != nil {
		t.Fatalf("empty write: %d, %v", n, err)
	}
	if buf.Len() != 0 {
		t.Fatalf("empty write produced %d bytes", buf.Len())
	}
	if _, err := w.Write(make([]byte, MaxChunk)); err != nil {
		t.Fatalf("exact write: %v", err)
	}
	if _, err := w.Write([]byte{1}); err != nil {
		t.Fatalf("second write: %v", err)
	}
	if _, _, err := ReadFrame(&buf); err != nil {
		t.Fatalf("first frame: %v", err)
	}
	typ, p, err := ReadFrame(&buf)
	if err != nil {
		t.Fatalf("second frame: %v", err)
	}
	if typ != FrameStdin || len(p) != 1 {
		t.Fatalf("second frame: type %#x len %d", typ, len(p))
	}
}

func TestSessionName(t *testing.T) {
	for _, c := range []struct {
		name string
		ok   bool
	}{
		{"main", true},
		{"db-incident", true},
		{"a", true},
		{"A9", true},
		{"a.b_c-d", true},
		{strings.Repeat("a", 32), true},
		{"", false},
		{strings.Repeat("a", 33), false},
		{"-main", false},
		{".main", false},
		{"_main", false},
		{"main/x", false},
		{"main x", false},
		{"main:x", false},
		{"héllo", false},
	} {
		if got := ValidSessionName(c.name); got != c.ok {
			t.Errorf("ValidSessionName(%q) = %v, want %v", c.name, got, c.ok)
		}
	}
}

// TestFrameRandomBytes feeds deterministic pseudo-random bytes to ReadFrame and
// checks it never panics and always terminates with a classified error.
func TestFrameRandomBytes(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 2000; i++ {
		b := make([]byte, rng.Intn(64))
		rng.Read(b)
		typ, payload, err := ReadFrame(bytes.NewReader(b))
		if err == nil {
			if len(payload) > MaxPayload {
				t.Fatalf("case %d: payload %d bytes over limit", i, len(payload))
			}
			_ = typ
			continue
		}
		if !errors.Is(err, io.EOF) && !errs.Is(err, errs.Protocol) {
			t.Fatalf("case %d: unclassified error %v", i, err)
		}
	}
}

func FuzzReadFrame(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{FrameDetach, 0, 0, 0, 0})
	f.Add([]byte{FrameStdout, 0, 0, 0, 3, 'a', 'b', 'c'})
	f.Add(bytes.Repeat([]byte{0xff}, 16))
	f.Fuzz(func(t *testing.T, b []byte) {
		_, payload, err := ReadFrame(bytes.NewReader(b))
		if err == nil && len(payload) > MaxPayload {
			t.Fatalf("payload %d bytes over limit", len(payload))
		}
	})
}

type countingWriter struct {
	calls int
	buf   bytes.Buffer
}

func (c *countingWriter) Write(p []byte) (int, error) {
	c.calls++
	return c.buf.Write(p)
}

type failingReader struct{ err error }

func (f failingReader) Read([]byte) (int, error) { return 0, f.err }

type shortWriter struct{}

func (shortWriter) Write(p []byte) (int, error) { return len(p) - 1, nil }
