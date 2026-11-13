package ssh

import (
	"context"
	"io"
	"jumper/internal/testload"
	"strings"
	"sync"
	"testing"
	"time"

	"jumper/internal/errs"
	"jumper/internal/transport"
)

func TestStreamLines(t *testing.T) {
	f := newXFFixture(t, xfOptions{handler: func(_ string, _ io.Reader, stdout, stderr io.Writer) int {
		for _, line := range []string{"one", "two", "three"} {
			_, _ = io.WriteString(stdout, line+"\n")
			time.Sleep(5 * time.Millisecond)
		}
		_, _ = io.WriteString(stderr, "bye\n")
		return 0
	}})

	st, err := f.tr.Start(context.Background(), f.host, transport.Command{Script: "journalctl -f"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	var got []string
	for line := range st.Lines() {
		got = append(got, string(line))
	}
	res, err := st.Wait()
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if want := "one|two|three"; strings.Join(got, "|") != want {
		t.Fatalf("lines = %q, want %q", got, want)
	}
	if res.RC != 0 {
		t.Fatalf("RC = %d, want 0", res.RC)
	}
	if len(res.Stdout) != 0 {
		t.Fatalf("Stdout = %q, want empty (lines carry stdout)", res.Stdout)
	}
	if string(res.Stderr) != "bye\n" {
		t.Fatalf("Stderr = %q, want the capped tail", res.Stderr)
	}
	if res.Host != f.host || res.Via != "ssh" {
		t.Fatalf("result = %+v, want host and via filled", res)
	}
	if res.Duration <= 0 {
		t.Fatalf("Duration = %v, want > 0", res.Duration)
	}
	// The lease came back when the command ended.
	if st := f.pool.Stats(); st.Leases != 0 {
		t.Fatalf("stats = %+v, want no lease left behind", st)
	}
}

func TestStreamCloseStopsRemote(t *testing.T) {
	stopped := make(chan struct{})
	var once sync.Once
	f := newXFFixture(t, xfOptions{handler: func(_ string, _ io.Reader, stdout, _ io.Writer) int {
		_, _ = io.WriteString(stdout, "first\n")
		for {
			time.Sleep(10 * time.Millisecond)
			if _, err := io.WriteString(stdout, "tick\n"); err != nil {
				once.Do(func() { close(stopped) })
				return 0
			}
		}
	}})

	st, err := f.tr.Start(context.Background(), f.host, transport.Command{Script: "journalctl -f"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	select {
	case line := <-st.Lines():
		if string(line) != "first" {
			t.Fatalf("first line = %q, want %q", line, "first")
		}
	case <-time.After(testload.Scale(5 * time.Second)):
		t.Fatal("no line arrived from the remote command")
	}

	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	type result struct {
		res transport.ExecResult
		err error
	}
	waited := make(chan result, 1)
	go func() {
		res, err := st.Wait()
		waited <- result{res, err}
	}()
	select {
	case got := <-waited:
		if !errs.Is(got.err, errs.Canceled) {
			t.Fatalf("Wait err = %v (kind %q), want kind %q", got.err, errs.KindOf(got.err), errs.Canceled)
		}
		if got.res.Host != f.host {
			t.Fatalf("result = %+v, want the host filled", got.res)
		}
	case <-time.After(testload.Scale(5 * time.Second)):
		t.Fatal("Wait did not return after Close")
	}

	// Lines is closed, and the lease is back.
	closedLines := make(chan struct{})
	go func() {
		for range st.Lines() {
		}
		close(closedLines)
	}()
	select {
	case <-closedLines:
	case <-time.After(testload.Scale(5 * time.Second)):
		t.Fatal("Lines was not closed after Close")
	}
	if s := f.pool.Stats(); s.Leases != 0 {
		t.Fatalf("stats = %+v, want the lease released by Close", s)
	}

	// The remote side noticed: its next write failed.
	select {
	case <-stopped:
	case <-time.After(testload.Scale(5 * time.Second)):
		t.Fatal("the remote command kept running after Close")
	}
}

func TestStreamLongLineSplit(t *testing.T) {
	payload := strings.Repeat("x", 100<<10)
	f := newXFFixture(t, xfOptions{handler: func(_ string, _ io.Reader, stdout, _ io.Writer) int {
		_, _ = io.WriteString(stdout, payload+"\n")
		return 0
	}})

	st, err := f.tr.Start(context.Background(), f.host, transport.Command{Script: "journalctl -o json"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	var pieces []string
	for line := range st.Lines() {
		pieces = append(pieces, string(line))
	}
	res, err := st.Wait()
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if res.RC != 0 {
		t.Fatalf("RC = %d, want 0", res.RC)
	}
	if len(pieces) < 2 {
		t.Fatalf("a %d byte line came back in %d pieces, want it split", len(payload), len(pieces))
	}
	total := 0
	for i, p := range pieces {
		if len(p) > streamLineMax {
			t.Fatalf("piece %d is %d bytes, want at most %d", i, len(p), streamLineMax)
		}
		if strings.Trim(p, "x") != "" {
			t.Fatalf("piece %d holds something other than the payload", i)
		}
		total += len(p)
	}
	if total != len(payload) {
		t.Fatalf("pieces total %d bytes, want %d", total, len(payload))
	}
	if strings.Join(pieces, "") != payload {
		t.Fatal("the pieces do not reassemble to the line")
	}
}
