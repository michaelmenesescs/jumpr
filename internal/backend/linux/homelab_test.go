//go:build homelab

// Homelab suite: the Linux probe batches and parsers against a real Ubuntu
// 24.04 node. Every command here is read-only and comes from the production
// probe definitions (probes.go), so the text the parsers see is the text the
// product will send. Nothing in this file compiles into `go test ./...`.

package linux

import (
	"context"
	"net"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"jumpr/internal/errs"
	"jumpr/internal/probe"
	"jumpr/internal/resource"
	"jumpr/internal/transport"
	sshx "jumpr/internal/transport/ssh"
)

const (
	// homelabDialTimeout is the budget for deciding whether the node is
	// reachable at all. A laptop away from the tailnet must skip, not fail.
	homelabDialTimeout = 3 * time.Second
	// homelabCallTimeout bounds one remote call in these tests.
	homelabCallTimeout = 10 * time.Second
	// homelabSetupTimeout bounds a whole test, dials and cleanup included.
	homelabSetupTimeout = 4 * homelabCallTimeout
	// homelabNonce is fixed so a failing run can be replayed by hand: the
	// markers it prints are stable.
	homelabNonce = "0badc0de"
)

// homelabHost returns the ssh_config alias under test. Every test in the
// homelab suite starts here, so an unset variable skips instead of failing.
func homelabHost(t *testing.T) string {
	t.Helper()
	host := os.Getenv("JUMPR_HOMELAB_UBUNTU")
	if host == "" {
		t.Skip("JUMPR_HOMELAB_UBUNTU not set")
	}
	return host
}

// homelabSpec resolves host from the real ~/.ssh/config and skips when its TCP
// port does not answer within homelabDialTimeout.
func homelabSpec(t *testing.T, host string) sshx.HostSpec {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home directory: %v", err)
	}
	r, err := sshx.NewResolver([]string{home + "/.ssh/config"}, home, os.Getenv("USER"))
	if err != nil {
		t.Fatalf("read ~/.ssh/config: %v", err)
	}
	spec, err := r.Resolve(host)
	if err != nil {
		t.Fatalf("resolve %s: %v", host, err)
	}
	addr := net.JoinHostPort(spec.HostName, strconv.Itoa(spec.Port))
	conn, derr := net.DialTimeout("tcp", addr, homelabDialTimeout)
	if derr != nil {
		t.Skipf("%s (%s) unreachable within %v: %v", host, addr, homelabDialTimeout, derr)
	}
	_ = conn.Close()
	return spec
}

// homelabDialOpts is what the client uses for a real node: the user's agent
// and the known_hosts from ssh_config, with no prompter (a test cannot answer
// one).
func homelabDialOpts() sshx.DialOptions {
	return sshx.DialOptions{
		Auth:           sshx.AuthOptions{AgentSocket: func() string { return os.Getenv("SSH_AUTH_SOCK") }},
		ConnectTimeout: homelabCallTimeout,
	}
}

// homelabTransport returns a pooled SSH executor for host and its HostID.
func homelabTransport(t *testing.T, host string) (*sshx.Transport, resource.HostID) {
	t.Helper()
	homelabSpec(t, host)
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home directory: %v", err)
	}
	r, err := sshx.NewResolver([]string{home + "/.ssh/config"}, home, os.Getenv("USER"))
	if err != nil {
		t.Fatalf("read ~/.ssh/config: %v", err)
	}
	pool := sshx.NewPool(sshx.PoolOptions{Resolver: r, Dial: homelabDialOpts()})
	t.Cleanup(func() { _ = pool.Close() })
	return sshx.New(pool, sshx.Options{ExecTimeout: homelabCallTimeout}), resource.HostID(host)
}

// homelabCtx is the context for one homelab test.
func homelabCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), homelabSetupTimeout)
	t.Cleanup(cancel)
	return ctx
}

// homelabRun executes one production probe batch on the node and splits the
// reply, failing the test on a transport error.
func homelabRun(t *testing.T, ctx context.Context, tr *sshx.Transport, host resource.HostID, kind resource.KindName) (probe.Batch, map[string]probe.SectionResult) {
	t.Helper()
	batch, err := newBatch(homelabNonce, kind)
	if err != nil {
		t.Fatalf("%s: build probe batch: %v", kind, err)
	}
	res, err := tr.Exec(ctx, host, batch.Command(homelabCallTimeout))
	homelabSkipIfSilent(t, string(kind)+" probe", err)
	if err != nil {
		t.Fatalf("%s: run probe batch: %v", kind, err)
	}
	return batch, batch.Split(res.Stdout, res.Stderr)
}

// homelabSkipIfSilent turns "the node never answered" into a skip: a laptop
// away from the tailnet, or a node that is down, must not fail CI. Everything
// after the first answer is a real failure.
func homelabSkipIfSilent(t *testing.T, what string, err error) {
	t.Helper()
	if err == nil {
		return
	}
	switch errs.KindOf(err) {
	case errs.Timeout, errs.Unreachable:
		t.Skipf("%s: node did not answer: %v", what, err)
	}
}

// sectionCheck fails when one section of the batch did not deliver a result.
func sectionCheck(t *testing.T, host resource.HostID, batch probe.Batch, sec map[string]probe.SectionResult) {
	t.Helper()
	ids := make([]string, 0, len(batch.Sections))
	for _, s := range batch.Sections {
		ids = append(ids, s.ID)
	}
	for _, id := range ids {
		r, ok := sec[id]
		if !ok || r.Missing {
			t.Fatalf("section %q returned no result on %s", id, host)
		}
		if e := sectionErr(host, id, r, toleratesNonZero(id)); e != nil {
			t.Fatalf("section %q on %s: %v (stderr %q)", id, host, e, strings.TrimSpace(r.Stderr))
		}
	}
}

// TestHomelabProbeOverview runs the real overview batch (038) and parses it
// through the host row builder (040).
func TestHomelabProbeOverview(t *testing.T) {
	host := homelabHost(t)
	tr, id := homelabTransport(t, host)
	ctx := homelabCtx(t)

	batch, sec := homelabRun(t, ctx, tr, id, KindHost)
	sectionCheck(t, id, batch, sec)

	b := New(Options{Exec: tr, Timeout: homelabCallTimeout})
	bad := map[string]error{}
	row := b.hostRow(id, sec, bad, map[string]error{})

	if len(bad) > 0 {
		ids := make([]string, 0, len(bad))
		for k := range bad {
			ids = append(ids, k+": "+string(errs.KindOf(bad[k])))
		}
		sort.Strings(ids)
		t.Fatalf("host row degraded on %s: %s", host, strings.Join(ids, "; "))
	}
	if d, ok := row.Labels["_degraded"]; ok {
		t.Fatalf("host row carries _degraded=%q", d)
	}

	facts, ok := HostFactsOf(row)
	if !ok {
		t.Fatalf("host row has no typed facts: Raw is %T", row.Raw)
	}
	if !facts.HaveLoad {
		t.Fatalf("load average did not parse on %s", host)
	}
	for name, v := range map[string]float64{"1m": facts.Load.L1, "5m": facts.Load.L5, "15m": facts.Load.L15} {
		if v != v || v < 0 || v > 1e6 {
			t.Fatalf("load %s = %v, want a finite non-negative number", name, v)
		}
	}
	if !facts.HaveMem {
		t.Fatalf("meminfo did not parse on %s", host)
	}
	if facts.Mem.TotalKB <= 0 {
		t.Fatalf("memory total = %d kB, want > 0", facts.Mem.TotalKB)
	}
	if facts.NProc <= 0 {
		t.Fatalf("nproc = %d, want > 0", facts.NProc)
	}
	info, ok := row.Raw.(*hostInfo)
	if !ok || info.OS == nil {
		t.Fatalf("os-release did not parse on %s", host)
	}
	if info.OS.ID != "ubuntu" {
		t.Fatalf("os-release ID = %q, want ubuntu (%q)", info.OS.ID, info.OS.PrettyName)
	}
	if !strings.Contains(strings.ToLower(info.OS.PrettyName), "ubuntu") {
		t.Fatalf("os-release PRETTY_NAME = %q, want it to name Ubuntu", info.OS.PrettyName)
	}

	// Every field the probe returned must carry a value: no empty cell and no
	// "—" placeholder for a section that answered.
	for _, field := range []string{"host", "load", "mem", "os", "kernel", "uptime", "state"} {
		v := row.Str(field)
		if v == "" || v == "—" {
			t.Fatalf("field %q is %q although the probe returned it", field, v)
		}
	}

	t.Logf("%s: %s %s kernel %s load %s/%s/%s mem %.1f%% of %d kB pids %d state %s",
		host, info.OS.ID, info.OS.VersionID, info.Kernel, row.Str("load"), fmtFloat(facts.Load.L5), fmtFloat(facts.Load.L15),
		facts.Mem.UsedPercent(), facts.Mem.TotalKB, facts.NProc, info.State)
}

// fmtFloat is a short log-only float format.
func fmtFloat(v float64) string { return strconv.FormatFloat(v, 'f', 2, 64) }

// TestHomelabUnits runs the unit listing through the parser (041).
func TestHomelabUnits(t *testing.T) {
	host := homelabHost(t)
	tr, id := homelabTransport(t, host)
	ctx := homelabCtx(t)

	batch, sec := homelabRun(t, ctx, tr, id, KindUnit)
	sectionCheck(t, id, batch, sec)
	raw := sec[secUnits].Stdout

	units, err := ParseListUnits(raw)
	if err != nil {
		t.Fatalf("ParseListUnits on %s: %v", host, err)
	}
	if len(units) < 5 {
		t.Fatalf("%s reported %d units, want at least 5", host, len(units))
	}
	active := 0
	for _, u := range units {
		if u.Name == "" {
			t.Fatalf("empty unit name in %d units", len(units))
		}
		if u.Load == "" || u.Active == "" || u.Sub == "" {
			t.Fatalf("unit %q parsed incomplete: %+v", u.Name, u)
		}
		if u.Active == "active" {
			active++
		}
	}
	if active == 0 {
		t.Fatalf("no active unit among %d units on %s", len(units), host)
	}

	// Round-trip: the names the parser found are the names the probe printed,
	// in the same order.
	var printed []string
	for _, line := range strings.Split(raw, "\n") {
		f := strings.Fields(line)
		if len(f) > 0 && (f[0] == "●" || f[0] == "*") {
			f = f[1:]
		}
		if len(f) < 4 {
			continue
		}
		printed = append(printed, f[0])
	}
	if len(printed) != len(units) {
		t.Fatalf("probe printed %d unit lines, parser returned %d", len(printed), len(units))
	}
	for i, name := range printed {
		if units[i].Name != name {
			t.Fatalf("unit %d: parsed %q, probe printed %q", i, units[i].Name, name)
		}
	}

	// The same listing through the Resource path: one row per unit, named and
	// typed, nothing degraded.
	b := New(Options{Exec: tr, Timeout: homelabCallTimeout})
	rows, err := b.unitRows(id, resource.Query{}, sec, map[string]error{}, map[string]error{})
	if err != nil {
		t.Fatalf("unitRows on %s: %v", host, err)
	}
	if len(rows) != len(units) {
		t.Fatalf("unitRows returned %d rows for %d units", len(rows), len(units))
	}
	names := make([]string, 0, 3)
	for _, r := range rows {
		if r.Ref.Name == "" {
			t.Fatalf("unit row without a name: %+v", r.Ref)
		}
		if len(names) < 3 {
			names = append(names, r.Ref.Name)
		}
	}

	t.Logf("%s: %d units, %d active, first %v", host, len(units), active, names)
}

// TestHomelabJournal runs the journal listing (50 JSON lines) through the
// parser (042), including a unit filter that matches nothing.
func TestHomelabJournal(t *testing.T) {
	host := homelabHost(t)
	tr, id := homelabTransport(t, host)
	ctx := homelabCtx(t)

	cmd := JournalCommand(JournalOptions{Limit: 50})
	res, err := tr.Exec(ctx, id, transport.Command{Script: cmd, Timeout: homelabCallTimeout})
	homelabSkipIfSilent(t, "journalctl", err)
	if err != nil {
		t.Fatalf("journalctl on %s: %v", host, err)
	}
	if res.RC != 0 {
		t.Fatalf("journalctl on %s: rc %d, stderr %q", host, res.RC, strings.TrimSpace(string(res.Stderr)))
	}
	entries, skipped := ParseJournal(res.Stdout)
	if len(entries) == 0 {
		t.Fatalf("journalctl returned %d bytes and no parsable entry on %s", len(res.Stdout), host)
	}
	if skipped > 0 {
		t.Logf("journal: %d of %d lines did not parse", skipped, len(entries)+skipped)
	}
	for i := 1; i < len(entries); i++ {
		if entries[i].Time.Before(entries[i-1].Time) {
			t.Fatalf("journal timestamps not monotonic at %d: %s then %s",
				i, entries[i-1].Time.Format(time.RFC3339Nano), entries[i].Time.Format(time.RFC3339Nano))
		}
	}
	if entries[len(entries)-1].Message == "" && entries[len(entries)-1].Unit == "" {
		t.Fatalf("last journal entry is empty: %+v", entries[len(entries)-1])
	}

	// A unit filter that matches nothing must yield no entries, not a panic.
	mute := JournalCommand(JournalOptions{Unit: "jumpr-homelab-no-such-unit.service", Limit: 50})
	res, err = tr.Exec(ctx, id, transport.Command{Script: mute, Timeout: homelabCallTimeout})
	if err != nil {
		t.Fatalf("journalctl -u (no match) on %s: %v", host, err)
	}
	none, _ := ParseJournal(res.Stdout)
	if len(none) != 0 {
		t.Fatalf("journalctl -u jumpr-homelab-no-such-unit.service returned %d entries", len(none))
	}

	t.Logf("%s: %d journal entries (rc %d on the empty filter, %d bytes)", host, len(entries), res.RC, len(res.Stdout))
}

// TestHomelabProbeIsReadOnly asserts that no probe batch the Linux backend can
// send contains a command that changes the node.
func TestHomelabProbeIsReadOnly(t *testing.T) {
	host := homelabHost(t)

	forbidden := []*regexp.Regexp{
		regexp.MustCompile(`\bsystemctl\s+(start|stop|restart|enable|disable)\b`),
		regexp.MustCompile(`\bkill\b`),
		regexp.MustCompile(`\brm\b`),
	}
	kinds := []resource.KindName{KindHost, KindUnit, KindProcess, KindSocket, KindMount, KindDisk,
		KindInterface, KindRoute, KindNeigh, KindCgroup, KindPkg}

	var scripts []string
	for _, kind := range kinds {
		batch, err := newBatch(homelabNonce, kind)
		if err != nil {
			t.Fatalf("%s: build probe batch: %v", kind, err)
		}
		for _, s := range batch.Sections {
			scripts = append(scripts, string(kind)+"/"+s.ID+" "+s.Cmd)
		}
	}
	scripts = append(scripts, "journal "+JournalCommand(JournalOptions{Limit: 50, Sudo: true}))

	for _, script := range scripts {
		for _, re := range forbidden {
			if m := re.FindString(script); m != "" {
				t.Fatalf("probe command writes to the node: %q matches %q", script, m)
			}
		}
	}
	t.Logf("%s: %d probe commands checked, none changes the node", host, len(scripts))
}

// TestHomelabTiming measures the numbers SPEC §15 quotes: dial, first byte and
// the full overview probe, all against a warm connection.
func TestHomelabTiming(t *testing.T) {
	host := homelabHost(t)
	spec := homelabSpec(t, host)
	ctx := homelabCtx(t)

	// Cold dial: a fresh TCP connection and SSH handshake.
	dialStart := time.Now()
	client, err := sshx.DialDirect(ctx, spec, homelabDialOpts())
	if err != nil {
		t.Skipf("dial %s: %v", host, err)
	}
	dial := time.Since(dialStart)
	_ = client.Close()

	tr, id := homelabTransport(t, host)

	// First byte: how long a stream takes to deliver anything at all.
	first := time.Duration(0)
	st, err := tr.Start(ctx, id, transport.Command{Script: "echo tick", Timeout: homelabCallTimeout})
	homelabSkipIfSilent(t, "start stream", err)
	if err != nil {
		t.Fatalf("start stream on %s: %v", host, err)
	}
	start := time.Now()
	select {
	case line, ok := <-st.Lines():
		first = time.Since(start)
		if !ok || strings.TrimSpace(string(line)) != "tick" {
			t.Fatalf("first line = %q (open %v), want tick", line, ok)
		}
	case <-ctx.Done():
		_ = st.Close()
		t.Fatalf("no first byte from %s within %v", host, homelabSetupTimeout)
	}
	_, _ = st.Wait()
	_ = st.Close()

	// Full probe: the whole overview batch, warm connection.
	batch, err := newBatch(homelabNonce, KindHost)
	if err != nil {
		t.Fatalf("build probe batch: %v", err)
	}
	start = time.Now()
	res, err := tr.Exec(ctx, id, batch.Command(homelabCallTimeout))
	probeTook := time.Since(start)
	homelabSkipIfSilent(t, "overview probe", err)
	if err != nil {
		t.Fatalf("overview probe on %s: %v", host, err)
	}

	t.Logf("%s timing: dial %v, first byte %v, full overview probe %v (%d bytes, %d sections)",
		host, dial.Round(time.Millisecond), first.Round(time.Millisecond),
		probeTook.Round(time.Millisecond), len(res.Stdout), len(batch.Sections))
	t.Logf("SPEC §15 target: single-host overview < 1.5s via a warm bastion")
}
