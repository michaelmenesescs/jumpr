package transporttest

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"jumpr/internal/errs"
	"jumpr/internal/resource"
	"jumpr/internal/transport"
)

var bg = context.Background()

func TestFakeRuleOrder(t *testing.T) {
	f := New().On("uptime", Reply{Stdout: "first"}).On("up", Reply{Stdout: "second"})
	r, err := f.Exec(bg, "h1", transport.Command{Script: "uptime -p"})
	if err != nil || string(r.Stdout) != "first" || r.Via != "fake" || r.Host != "h1" {
		t.Fatalf("got %+v %v", r, err)
	}
	if f.Name() != "fake" {
		t.Fatal("name")
	}
}

func TestFakeHostRuleWins(t *testing.T) {
	f := New().On("x", Reply{Stdout: "any"}).OnHost("h2", "x", Reply{Stdout: "h2", RC: 3})
	r, _ := f.Exec(bg, "h2", transport.Command{Script: "x"})
	if string(r.Stdout) != "h2" || r.RC != 3 {
		t.Fatalf("%+v", r)
	}
	r, _ = f.Exec(bg, "h1", transport.Command{Script: "x"})
	if string(r.Stdout) != "any" {
		t.Fatalf("%+v", r)
	}
}

func TestFakeOnFunc(t *testing.T) {
	f := New().OnFunc(func(h resource.HostID, c transport.Command) (Reply, bool) {
		return Reply{Stdout: string(h)}, c.Script == "who"
	})
	r, _ := f.Exec(bg, "hx", transport.Command{Script: "who"})
	if string(r.Stdout) != "hx" {
		t.Fatal(string(r.Stdout))
	}
	if r, _ = f.Exec(bg, "hx", transport.Command{Script: "other"}); r.RC != 127 {
		t.Fatal("want 127")
	}
}

func TestFakeNoRule127(t *testing.T) {
	r, err := New().Exec(bg, "h", transport.Command{Script: "frobnicate --now"})
	if err != nil || r.RC != 127 || string(r.Stderr) != "sh: frobnicate: command not found" {
		t.Fatalf("%+v %v", r, err)
	}
}

func TestFakeTimeout(t *testing.T) {
	f := New().On("slow", Reply{Delay: time.Second})
	_, err := f.Exec(bg, "h", transport.Command{Script: "slow", Timeout: 20 * time.Millisecond})
	if errs.KindOf(err) != errs.Timeout {
		t.Fatalf("%v", err)
	}
}

func TestFakeCancel(t *testing.T) {
	f := New().On("slow", Reply{Delay: time.Second})
	ctx, cancel := context.WithCancel(bg)
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	_, err := f.Exec(ctx, "h", transport.Command{Script: "slow"})
	if errs.KindOf(err) != errs.Canceled {
		t.Fatalf("%v", err)
	}
}

func TestFakeErrReply(t *testing.T) {
	e := errs.New(errs.Unreachable, "x", "down")
	_, err := New().On("a", Reply{Err: e}).Exec(bg, "h", transport.Command{Script: "a"})
	if !errors.Is(err, e) {
		t.Fatal(err)
	}
}

func TestFakeTruncates(t *testing.T) {
	f := New().On("big", Reply{Stdout: strings.Repeat("a", 100)})
	r, _ := f.Exec(bg, "h", transport.Command{Script: "big", MaxOutput: 10})
	if len(r.Stdout) != 10 || !r.Truncated {
		t.Fatalf("%d %v", len(r.Stdout), r.Truncated)
	}
	r, _ = f.Exec(bg, "h", transport.Command{Script: "big"})
	if len(r.Stdout) != 100 || r.Truncated {
		t.Fatal("unexpected truncation")
	}
}

func TestFakeStreamLinesAndClose(t *testing.T) {
	f := New().On("tail", Reply{Lines: []string{"a", "b", "c"}, Delay: time.Millisecond})
	s, err := f.Start(bg, "h", transport.Command{Script: "tail -f"})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for l := range s.Lines() {
		got = append(got, string(l))
	}
	if strings.Join(got, ",") != "a,b,c" {
		t.Fatal(got)
	}
	if _, err := s.Wait(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	f2 := New().On("tail", Reply{Lines: []string{"a", "b", "c", "d"}, Delay: 50 * time.Millisecond})
	s2, _ := f2.Start(bg, "h", transport.Command{Script: "tail"})
	<-s2.Lines()
	s2.Close()
	n := 0
	for range s2.Lines() {
		n++
	}
	if n > 1 {
		t.Fatalf("stream kept going: %d", n)
	}
}

func TestFakeShellEcho(t *testing.T) {
	f := New()
	sh, err := f.OpenShell(bg, "h", transport.PTYRequest{Cols: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	sh.Write([]byte("ls\n"))
	buf := make([]byte, 16)
	n, _ := sh.Read(buf)
	if string(buf[:n]) != "ls\n" {
		t.Fatal(string(buf[:n]))
	}
	f.ShellOutput("h", []byte("hello"))
	n, _ = sh.Read(buf)
	if string(buf[:n]) != "hello" {
		t.Fatal(string(buf[:n]))
	}
	sh.Resize(100, 30)
	if r := f.Resizes("h"); len(r) != 1 || r[0] != [2]int{100, 30} {
		t.Fatal(r)
	}
	done := make(chan error)
	go func() { done <- sh.Wait() }()
	sh.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := sh.Read(buf); err != io.EOF {
		t.Fatal(err)
	}
}

func TestFakeInFlightMax(t *testing.T) {
	f := New().On("w", Reply{Delay: 50 * time.Millisecond})
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f.Exec(bg, "h", transport.Command{Script: "w"})
		}()
	}
	wg.Wait()
	if m := f.InFlightMax(); m != 5 {
		t.Fatalf("max %d", m)
	}
	if len(f.Calls()) != 5 {
		t.Fatal("calls")
	}
}
