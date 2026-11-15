package linux

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"jumpr/internal/probe"
	"jumpr/internal/resource"
	"jumpr/internal/transport"
	"jumpr/internal/transport/transporttest"
)

// readLinuxTestdata reads one file under testdata/linux.
func readLinuxTestdata(t *testing.T, parts ...string) string {
	t.Helper()
	p := filepath.Join(append([]string{"..", "..", "..", "testdata", "linux"}, parts...)...)
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return string(b)
}

// timerStamp renders a parsed time the way systemctl prints it, so the tests do
// not depend on the zone offset time.Parse gives an unknown abbreviation.
func timerStamp(t time.Time) string { return t.Format("Mon 2006-01-02 15:04:05 MST") }

func TestParseDependenciesRocky9(t *testing.T) {
	fwd := ParseDependencies(readLinuxTestdata(t, "deps", "rocky9", "list-dependencies.txt"))
	if len(fwd) == 0 {
		t.Fatal("no nodes parsed")
	}
	if got, want := fwd[0], (DepNode{Unit: "multi-user.target", Depth: 0}); got != want {
		t.Errorf("root = %+v, want %+v", got, want)
	}
	want := map[string]int{
		"multi-user.target":                  0,
		"basic.target":                       1,
		"sysinit.target":                     2,
		"systemd-journald.socket":            3,
		"systemd-journald.service":           3,
		"systemd-journald-dev-log.socket":    3,
		"paths.target":                       2,
		"slices.target":                      2,
		"sockets.target":                     2,
		"timers.target":                      2,
		"network-online.target":              1,
		"NetworkManager-wait-online.service": 2,
		"nginx.service":                      1,
		"sshd.service":                       1,
	}
	for _, n := range fwd {
		depth, ok := want[n.Unit]
		if !ok {
			t.Errorf("unexpected node %q", n.Unit)
			continue
		}
		if n.Depth != depth {
			t.Errorf("%s depth = %d, want %d", n.Unit, n.Depth, depth)
		}
		delete(want, n.Unit)
	}
	for unit := range want {
		t.Errorf("missing node %q", unit)
	}

	rev := ParseDependencies(readLinuxTestdata(t, "deps", "rocky9", "list-dependencies-reverse.txt"))
	if len(rev) != 2 {
		t.Fatalf("reverse nodes = %d, want 2: %+v", len(rev), rev)
	}
	if rev[0].Unit != "multi-user.target" || rev[0].Depth != 0 || rev[1].Unit != "graphical.target" || rev[1].Depth != 1 {
		t.Errorf("reverse = %+v", rev)
	}
}

func TestParseDependenciesGlyphsAndEmpty(t *testing.T) {
	// The unplain tree layout carries state and tree glyphs; both are dropped
	// from the unit name. Depth still comes from indentation only.
	out := "nginx.service\n" +
		"● ├─system.slice\n" +
		"● └─nginx.service\n" +
		"○ └─basic.target\n" +
		"× └─broken.service\n" +
		"\n" +
		"   \n"
	got := ParseDependencies(out)
	want := []string{"nginx.service", "system.slice", "nginx.service", "basic.target", "broken.service"}
	if len(got) != len(want) {
		t.Fatalf("nodes = %+v, want %v", got, want)
	}
	for i, unit := range want {
		if got[i].Unit != unit {
			t.Errorf("node %d = %+v, want unit %q", i, got[i], unit)
		}
	}
	if got[1].Depth != 0 {
		t.Errorf("a glyph line without indentation claimed a depth: %+v", got[1])
	}

	if nodes := ParseDependencies(""); len(nodes) != 0 {
		t.Errorf("empty output gave %+v", nodes)
	}
	if nodes := ParseDependencies("\n \n\t\n"); len(nodes) != 0 {
		t.Errorf("blank output gave %+v", nodes)
	}
}

func TestParseTimersRocky9(t *testing.T) {
	timers := ParseTimers(readLinuxTestdata(t, "timers", "rocky9", "list-timers.txt"))
	if len(timers) != 3 {
		t.Fatalf("timers = %d, want 3: %+v", len(timers), timers)
	}

	first := timers[0]
	if first.Name != "logrotate.timer" || first.Activates != "logrotate.service" {
		t.Errorf("names = %q/%q", first.Name, first.Activates)
	}
	if got := timerStamp(first.Next); got != "Thu 2026-11-12 14:00:00 EST" {
		t.Errorf("next = %q", got)
	}
	if first.Left != 21*time.Minute {
		t.Errorf("left = %v, want 21m", first.Left)
	}
	if got := timerStamp(first.Last); got != "Thu 2026-11-12 13:00:00 EST" {
		t.Errorf("last = %q", got)
	}
	if first.Passed != 38*time.Minute {
		t.Errorf("passed = %v, want 38m", first.Passed)
	}
	if first.Never {
		t.Error("first timer marked Never")
	}

	// A timer that never ran prints dashes for last and passed.
	never := timers[1]
	if never.Name != "dnf-makecache.timer" || never.Activates != "dnf-makecache.service" {
		t.Errorf("never names = %q/%q", never.Name, never.Activates)
	}
	if never.Left != 11*time.Hour {
		t.Errorf("never left = %v, want 11h", never.Left)
	}
	if !never.Last.IsZero() || never.Passed != 0 {
		t.Errorf("never last/passed = %v/%v, want zero", never.Last, never.Passed)
	}
	if never.Never {
		t.Error("a timer with a next elapse must not be Never")
	}

	// A multi-token timespan is one duration.
	if got := timers[2].Left; got != 90*time.Minute {
		t.Errorf("third left = %v, want 1h30m", got)
	}
}

func TestParseTimersUbuntu(t *testing.T) {
	timers := ParseTimers(readLinuxTestdata(t, "timers", "ubuntu24.04", "list-timers.txt"))
	if len(timers) != 4 {
		t.Fatalf("timers = %d, want 4: %+v", len(timers), timers)
	}
	if got := timers[0].Left; got != 21*time.Minute {
		t.Errorf("first left = %v, want 21m", got)
	}

	// The n/a row: no next elapse, but the last run is still known.
	na := timers[1]
	if na.Name != "systemd-tmpfiles-clean.timer" || na.Activates != "systemd-tmpfiles-clean.service" {
		t.Errorf("n/a names = %q/%q", na.Name, na.Activates)
	}
	if !na.Never || !na.Next.IsZero() || na.Left != 0 {
		t.Errorf("n/a next = %v left = %v never = %v, want zero/zero/true", na.Next, na.Left, na.Never)
	}
	if got := timerStamp(na.Last); got != "Thu 2026-11-12 00:00:00 UTC" {
		t.Errorf("n/a last = %q", got)
	}
	if na.Passed != 14*time.Hour {
		t.Errorf("n/a passed = %v, want 14h", na.Passed)
	}

	if got := timers[2].Left; got != 6*24*time.Hour {
		t.Errorf("days left = %v, want 144h", got)
	}
	if got := timers[3].Left; got != 90*time.Minute {
		t.Errorf("second left = %v, want 1h30m", got)
	}
}

func TestParseTimersEmptyAndJunk(t *testing.T) {
	if timers := ParseTimers(""); len(timers) != 0 {
		t.Errorf("empty output gave %+v", timers)
	}
	// A lone word is not a row: the timer and activates names come last.
	if timers := ParseTimers("Thu 2026-11-12 14:00:00 UTC\n"); len(timers) != 0 {
		t.Errorf("half a row gave %+v", timers)
	}
	// A layout without the "left" middle column still yields the last run.
	out := "n/a n/a Thu 2026-11-12 13:00:00 UTC 38min ago t.timer t.service\n"
	timers := ParseTimers(out)
	if len(timers) != 1 {
		t.Fatalf("timers = %+v", timers)
	}
	if !timers[0].Never || !timers[0].Next.IsZero() {
		t.Errorf("no-left row next = %v never = %v", timers[0].Next, timers[0].Never)
	}
	if timerStamp(timers[0].Last) != "Thu 2026-11-12 13:00:00 UTC" || timers[0].Passed != 38*time.Minute {
		t.Errorf("no-left row last/passed = %v/%v", timers[0].Last, timers[0].Passed)
	}
}

func TestParseTimerShowTimes(t *testing.T) {
	out := "NextElapseUSecRealtime=Thu 2026-11-12 14:00:00 UTC\n" +
		"LastTriggerUSec=Thu 2026-11-12 13:00:00 UTC\n"
	next, last := ParseTimerShowTimes(out)
	if timerStamp(next) != "Thu 2026-11-12 14:00:00 UTC" {
		t.Errorf("next = %q", timerStamp(next))
	}
	if timerStamp(last) != "Thu 2026-11-12 13:00:00 UTC" {
		t.Errorf("last = %q", timerStamp(last))
	}

	// --timestamp=usec prints microseconds since the epoch.
	usec := time.Date(2026, 11, 12, 14, 0, 0, 0, time.UTC).UnixMicro()
	next, last = ParseTimerShowTimes("NextElapseUSecRealtime=" + strconv.FormatInt(usec, 10) + "\nLastTriggerUSec=n/a\n")
	if !next.UTC().Equal(time.Date(2026, 11, 12, 14, 0, 0, 0, time.UTC)) {
		t.Errorf("usec next = %v", next)
	}
	if !last.IsZero() {
		t.Errorf("n/a last = %v, want zero", last)
	}
}

// timersReply builds the marked-up reply of a batch whose sections are known by
// ID, the way a node prints them.
func timersReply(nonce string, bodies map[string]string) transporttest.Reply {
	var out, errb strings.Builder
	for id, body := range bodies {
		mark(&out, nonce, id, 0, body)
		mark(&errb, nonce, id, 0, "")
	}
	return transporttest.Reply{Stdout: out.String(), Stderr: errb.String()}
}

func TestTimersListRows(t *testing.T) {
	ex := transporttest.New()
	b := newTestBackend(t, ex, inv("web-01"))
	ex.OnFunc(func(_ resource.HostID, cmd transport.Command) (transporttest.Reply, bool) {
		if !strings.Contains(cmd.Script, "list-timers") {
			return transporttest.Reply{}, false
		}
		return timersReply("deadbeef", map[string]string{
			secTimers: readLinuxTestdata(t, "timers", "rocky9", "list-timers.txt"),
		}), true
	})

	ch, err := b.List(context.Background(), resource.Query{Kind: KindTimer, Hosts: []resource.HostID{"web-01"}})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	results := drainResults(t, ch)
	if len(results) != 1 || results[0].Err != nil {
		t.Fatalf("results = %+v", results)
	}
	items := results[0].Items
	if len(items) != 3 {
		t.Fatalf("items = %d, want 3", len(items))
	}
	row := items[0]
	if row.Ref.Kind != KindTimer || row.Ref.Name != "logrotate.timer" || row.Ref.Host != "web-01" {
		t.Errorf("bad ref %+v", row.Ref)
	}
	if got := row.Str("timer"); got != "logrotate.timer" {
		t.Errorf("timer field = %q", got)
	}
	if got := row.Str("activates"); got != "logrotate.service" {
		t.Errorf("activates field = %q", got)
	}
	next, ok := row.Time("next")
	if !ok || timerStamp(next) != "Thu 2026-11-12 14:00:00 EST" {
		t.Errorf("next field = %v (%v)", next, ok)
	}
	if left, ok := row.Fields["left"].(time.Duration); !ok || left != 21*time.Minute {
		t.Errorf("left field = %v", row.Fields["left"])
	}

	// Enter opens the timer's own unit detail, j the journal of what it runs.
	links := b.Links(row)
	if len(links) != 2 {
		t.Fatalf("links = %+v", links)
	}
	if links[0].Key != "enter" || links[0].To.Kind != KindUnitDetail || links[0].To.Args["unit"] != "logrotate.timer" {
		t.Errorf("detail link = %+v", links[0])
	}
	if links[1].Key != "j" || links[1].To.Kind != KindJournal || links[1].To.Args["unit"] != "logrotate.service" {
		t.Errorf("journal link = %+v", links[1])
	}
}

func TestTimersEnrichOnlyWhenTheTextParseFails(t *testing.T) {
	ex := transporttest.New()
	b := newTestBackend(t, ex, inv("web-01"))
	// NEITHER row carries a next elapse in the text: the first because the
	// layout gave no timestamp at all, the second because it is "n/a".
	listing := "garbage 21min left Thu 2026-11-12 13:00:00 UTC 38min ago a.timer a.service\n" +
		"n/a n/a Thu 2026-11-12 12:00:00 UTC 2h ago b.timer b.service\n"
	shown := map[string]string{
		"show0": "NextElapseUSecRealtime=Thu 2026-11-12 15:00:00 UTC\nLastTriggerUSec=Thu 2026-11-12 13:00:00 UTC\n",
	}
	ex.OnFunc(func(_ resource.HostID, cmd transport.Command) (transporttest.Reply, bool) {
		switch {
		case strings.Contains(cmd.Script, "list-timers"):
			return timersReply("deadbeef", map[string]string{secTimers: listing}), true
		case strings.Contains(cmd.Script, "systemctl show"):
			return timersReply("deadbeef", shown), true
		}
		return transporttest.Reply{}, false
	})

	timers, err := b.timers(context.Background(), "web-01")
	if err != nil {
		t.Fatalf("timers: %v", err)
	}
	if len(timers) != 2 {
		t.Fatalf("timers = %+v", timers)
	}
	if got := timerStamp(timers[0].Next); got != "Thu 2026-11-12 15:00:00 UTC" {
		t.Errorf("enriched next = %q", got)
	}
	if timers[0].Never {
		t.Error("enriched timer still marked Never")
	}
	if !timers[1].Next.IsZero() || !timers[1].Never {
		t.Errorf("n/a timer was enriched: next = %v never = %v", timers[1].Next, timers[1].Never)
	}

	// One listing plus exactly one enrichment round trip for the one timer.
	calls := ex.Calls()
	shows := 0
	for _, c := range calls {
		if strings.Contains(c.Command.Script, "systemctl show") {
			shows++
		}
	}
	if shows != 1 {
		t.Errorf("systemctl show ran %d times, want 1", shows)
	}
}

func TestDependenciesRoundTrip(t *testing.T) {
	ex := transporttest.New()
	b := newTestBackend(t, ex, inv("web-01"))
	ex.OnFunc(func(_ resource.HostID, cmd transport.Command) (transporttest.Reply, bool) {
		if !strings.Contains(cmd.Script, "list-dependencies") {
			return transporttest.Reply{}, false
		}
		return timersReply("deadbeef", map[string]string{
			secDeps:    readLinuxTestdata(t, "deps", "rocky9", "list-dependencies.txt"),
			secDepsRev: readLinuxTestdata(t, "deps", "rocky9", "list-dependencies-reverse.txt"),
		}), true
	})

	deps, err := b.UnitDependencies(context.Background(), "web-01", "multi-user.target")
	if err != nil {
		t.Fatalf("UnitDependencies: %v", err)
	}
	if len(deps.Forward) != 14 {
		t.Errorf("forward nodes = %d, want 14", len(deps.Forward))
	}
	if len(deps.Reverse) != 2 {
		t.Errorf("reverse nodes = %d, want 2", len(deps.Reverse))
	}
	if deps.Forward[0].Unit != "multi-user.target" || deps.Forward[0].Depth != 0 {
		t.Errorf("forward root = %+v", deps.Forward[0])
	}

	// The reverse run is a second section of the same round trip.
	if _, err := b.UnitDependencies(context.Background(), "", "nginx.service"); err == nil {
		t.Error("UnitDependencies accepted an empty host")
	}
	if _, err := b.UnitDependencies(context.Background(), "web-01", "bad name"); err == nil {
		t.Error("UnitDependencies accepted an invalid unit name")
	}
}

// TestTimersProbeBatchIsValid checks that the timer section survives probe's
// validation, since the listing goes through a batch.
func TestTimersProbeBatchIsValid(t *testing.T) {
	batch, err := probe.NewBatch("deadbeef", timerSections()...)
	if err != nil {
		t.Fatalf("timerSections: %v", err)
	}
	if len(batch.Sections) != 1 || !strings.Contains(batch.Sections[0].Cmd, "list-timers --all") {
		t.Errorf("sections = %+v", batch.Sections)
	}
}
