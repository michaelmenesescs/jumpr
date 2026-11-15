package linux

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"jumpr/internal/resource"
	"jumpr/internal/transport/transporttest"
)

// journalLine renders one journalctl -o json line.
func journalLine(usec int64, pri int, source, msg string) string {
	return fmt.Sprintf(`{"__REALTIME_TIMESTAMP":"%d","PRIORITY":"%d","SYSLOG_IDENTIFIER":%q,"MESSAGE":%q,"__CURSOR":"c%d"}`,
		usec, pri, source, msg, usec)
}

func TestJournalListWindow(t *testing.T) {
	ex := transporttest.New()
	b := New(Options{
		Exec:      ex,
		Inventory: inv("web-01"),
		Timeout:   time.Second,
		Nonce:     func() string { return "deadbeef" },
		Now:       func() time.Time { return time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC) },
	})

	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)
	older := now.Add(-30 * time.Minute).UnixMicro()
	newer := now.Add(-5 * time.Minute).UnixMicro()
	ex.OnHost("web-01", "journalctl", transporttest.Reply{Stdout: strings.Join([]string{
		journalLine(older, 3, "nginx", "upstream timed out"),
		journalLine(newer, 6, "nginx", "started"),
	}, "\n")})

	window, err := resource.ParseTimeWindow("-1h", now)
	if err != nil {
		t.Fatalf("ParseTimeWindow: %v", err)
	}
	q := resource.Query{
		Kind:   KindJournal,
		Hosts:  []resource.HostID{"web-01"},
		Args:   map[string]string{"unit": "nginx.service", "priority": "4"},
		Window: window,
	}
	ch, err := b.List(context.Background(), q)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	results := drainResults(t, ch)
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	if results[0].Err != nil {
		t.Fatalf("result error: %v", results[0].Err)
	}
	items := results[0].Items
	if len(items) != 2 {
		t.Fatalf("got %d rows, want 2", len(items))
	}

	calls := ex.Calls()
	if len(calls) != 1 {
		t.Fatalf("got %d remote calls, want 1", len(calls))
	}
	cmd := calls[0].Command
	if !cmd.Sudo {
		t.Error("journal command must run under sudo")
	}
	for _, want := range []string{
		"journalctl", "-o json",
		"--since @" + fmt.Sprint(window.From.Unix()),
		"--until @" + fmt.Sprint(window.To.Unix()),
		"-u nginx.service", "-p 4", "-n 500",
	} {
		if !strings.Contains(cmd.Script, want) {
			t.Errorf("command %q missing %q", cmd.Script, want)
		}
	}

	// Oldest first from journalctl; the backend must not re-sort.
	if got := items[0].Fields["message"]; got != "upstream timed out" {
		t.Errorf("first message = %v, want the older entry", got)
	}
	if got := items[1].Fields["message"]; got != "started" {
		t.Errorf("second message = %v, want the newer entry", got)
	}
	if items[0].Status != resource.StatusCrit {
		t.Errorf("pri 3 status = %v, want crit", items[0].Status)
	}
	if items[1].Status != resource.StatusOK {
		t.Errorf("pri 6 status = %v, want ok", items[1].Status)
	}
	if got := items[0].Fields["source"]; got != "nginx" {
		t.Errorf("source = %v, want nginx", got)
	}
	if got := items[0].Fields["pri"]; got != "err" {
		t.Errorf("pri = %v, want err", got)
	}
	if got := items[0].Ref.Kind; got != KindJournal {
		t.Errorf("ref kind = %v, want journal", got)
	}
	if got := items[0].Ref.Host; got != "web-01" {
		t.Errorf("ref host = %v, want web-01", got)
	}
	if _, ok := items[0].Fields["time"].(time.Time); !ok {
		t.Errorf("time field = %T, want time.Time", items[0].Fields["time"])
	}
}

func TestJournalListKernel(t *testing.T) {
	ex := transporttest.New()
	b := New(Options{
		Exec:      ex,
		Inventory: inv("web-01"),
		Nonce:     func() string { return "deadbeef" },
	})
	ex.OnHost("web-01", "journalctl", transporttest.Reply{Stdout: journalLine(1, 6, "kernel", "boot")})

	ch, err := b.List(context.Background(), resource.Query{
		Kind:  KindJournal,
		Hosts: []resource.HostID{"web-01"},
		Args:  map[string]string{"kernel": "1"},
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	drainResults(t, ch)
	calls := ex.Calls()
	if len(calls) != 1 || !strings.Contains(calls[0].Command.Script, "-k") {
		t.Fatalf("kernel=1 must add -k: %+v", calls)
	}
}

func TestJournalListFailure(t *testing.T) {
	ex := transporttest.New()
	b := New(Options{
		Exec:      ex,
		Inventory: inv("web-01"),
		Nonce:     func() string { return "deadbeef" },
	})
	ex.OnHost("web-01", "journalctl", transporttest.Reply{
		RC:     1,
		Stderr: "sudo: a password is required",
	})

	ch, err := b.List(context.Background(), resource.Query{Kind: KindJournal, Hosts: []resource.HostID{"web-01"}})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	results := drainResults(t, ch)
	if len(results) != 1 || results[0].Err == nil {
		t.Fatalf("want one failed result, got %+v", results)
	}
	if !strings.Contains(results[0].Err.Error(), "sudo") {
		t.Errorf("error = %v, want classified sudo", results[0].Err)
	}
}

func TestFollowDeliversAndStops(t *testing.T) {
	ex := transporttest.New()
	ex.On("journalctl", transporttest.Reply{
		Delay: 20 * time.Millisecond,
		Lines: []string{
			journalLine(1000, 6, "nginx", "one"),
			"not a journal line",
			journalLine(2000, 4, "nginx", "two"),
			journalLine(3000, 3, "nginx", "three"),
			journalLine(4000, 6, "nginx", "four"),
		},
	})
	b := New(Options{
		Exec:      ex,
		Stream:    ex,
		Inventory: inv("web-01"),
		Now:       func() time.Time { return time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC) },
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := b.FollowJournal(ctx, "web-01", JournalOptions{Unit: "nginx.service", Sudo: true})
	if err != nil {
		t.Fatalf("FollowJournal: %v", err)
	}

	var got []Entry
	for len(got) < 2 {
		select {
		case e, ok := <-ch:
			if !ok {
				t.Fatalf("channel closed after %d entries, want 2", len(got))
			}
			got = append(got, e)
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out after %d entries", len(got))
		}
	}
	if got[0].Message != "one" || got[1].Message != "two" {
		t.Fatalf("entries = %+v, want one, two (unparseable line skipped)", got)
	}

	cancel()
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("channel still delivering after cancel")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("channel did not close after cancel")
	}

	calls := ex.Calls()
	if len(calls) != 1 {
		t.Fatalf("got %d stream calls, want 1", len(calls))
	}
	script := calls[0].Command.Script
	for _, want := range []string{"journalctl", "-f", "-u nginx.service"} {
		if !strings.Contains(script, want) {
			t.Errorf("follow command %q missing %q", script, want)
		}
	}
}

func TestFollowJournalNeedsStreamer(t *testing.T) {
	b := New(Options{Exec: transporttest.New(), Inventory: inv("web-01")})
	if _, err := b.FollowJournal(context.Background(), "web-01", JournalOptions{}); err == nil {
		t.Fatal("want an error without a streamer")
	}
}

func TestJournalLinks(t *testing.T) {
	b := New(Options{Exec: transporttest.New(), Inventory: inv("web-01")})

	host := resource.Resource{Ref: resource.Ref{Kind: KindHost, Host: "web-01", Name: "web-01"}}
	links := b.Links(host)
	var found bool
	for _, l := range links {
		if l.Label == "journal" {
			found = true
			if l.Key != "j" || l.To.Kind != KindJournal || len(l.To.Hosts) != 1 || l.To.Hosts[0] != "web-01" {
				t.Errorf("host journal link = %+v", l)
			}
		}
	}
	if !found {
		t.Fatal("host has no journal link")
	}

	unit := resource.Resource{Ref: resource.Ref{Kind: KindUnit, Host: "web-01", Name: "nginx.service"}}
	links = b.Links(unit)
	var journal resource.Link
	for _, l := range links {
		if l.Label == "journal" {
			journal = l
		}
	}
	if journal.Label != "journal" {
		t.Fatalf("unit links = %+v, want a journal link", links)
	}
	got := journal
	if got.Label != "journal" || got.Key != "j" {
		t.Errorf("unit journal link = %+v", got)
	}
	if got.To.Kind != KindJournal || got.To.Args["unit"] != "nginx.service" || got.To.Hosts[0] != "web-01" {
		t.Errorf("unit journal query = %+v", got.To)
	}
}
