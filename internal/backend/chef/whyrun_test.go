package chef

import (
	"context"
	"strings"
	"testing"

	"jumpr/internal/action"
	"jumpr/internal/errs"
	"jumpr/internal/resource"
	"jumpr/internal/transport"
	"jumpr/internal/transport/transporttest"
)

// whyRunBody is one host's why-run output. The timestamp and the pid differ per
// host and must not split the group.
func whyRunBody(host resource.HostID) string {
	pid := "4242"
	if host == "web-02" {
		pid = "5150"
	}
	return `[2026-11-13T10:00:00+00:00] INFO: *** Chef Infra Client 19.2.12 *** (pid ` + pid + `)
[2026-11-13T10:00:00+00:00] INFO: Run List is [role[lab]]
[2026-11-13T10:00:31+00:00] INFO: Converging
Recipe: lab::default
  * file[/etc/motd.chef] action create (up to date)
Chef Infra Client finished, 0/2 resources would have been updated
`
}

// whyRunFake answers the why-run command and nothing else.
func whyRunFake(t *testing.T) *transporttest.Fake {
	t.Helper()
	return transporttest.New().OnFunc(func(host resource.HostID, cmd transport.Command) (transporttest.Reply, bool) {
		if !strings.Contains(cmd.Script, "--why-run") {
			return transporttest.Reply{}, false
		}
		return transporttest.Reply{Stdout: whyRunBody(host)}, true
	})
}

// TestWhyRunGroupsNormalizedOutput checks requirement 2: the fan-out runs
// `sudo -n chef-client --why-run …` on every host, the output is normalized and
// hosts that converged identically end up in one group.
func TestWhyRunGroupsNormalizedOutput(t *testing.T) {
	fake := whyRunFake(t)
	c := NewClient(ClientOptions{Exec: fake})
	hosts := []resource.HostID{"web-01", "web-02", "web-03"}
	rep, err := c.WhyRun(context.Background(), hosts)
	if err != nil {
		t.Fatalf("WhyRun: %v", err)
	}
	if rep.Total != 3 || len(rep.Rows) != 3 {
		t.Fatalf("total = %d rows = %d, want 3 and 3", rep.Total, len(rep.Rows))
	}
	if len(rep.Summary.Groups) != 1 || len(rep.Summary.Groups[0].Hosts) != 3 {
		t.Fatalf("groups = %+v, want one group of three hosts", rep.Summary.Groups)
	}
	if got := rep.Headline(); got != "3 identical" {
		t.Errorf("Headline() = %q", got)
	}
	for _, row := range rep.Rows {
		if !row.HasCounts || row.Updated != 0 || row.Total != 2 {
			t.Errorf("%s summary = %+v, want 0/2 resources", row.Host, row)
		}
		if row.Failed {
			t.Errorf("%s reported failed", row.Host)
		}
	}
	if got := rep.WouldChange(); got != 0 {
		t.Errorf("WouldChange() = %d, want 0", got)
	}

	calls := fake.Calls()
	if len(calls) != 3 {
		t.Fatalf("calls = %d, want one per host", len(calls))
	}
	want := "chef-client --why-run --no-color -l warn --run-lock-timeout 30"
	for _, call := range calls {
		if call.Command.Script != want {
			t.Errorf("script = %q, want %q", call.Command.Script, want)
		}
		if !call.Command.Sudo {
			t.Error("sudo = false, want the dry run under sudo -n")
		}
	}
}

// TestWhyRunDifferingHostIsItsOwnGroup checks the grouping separates a host
// whose why-run would change things.
func TestWhyRunDifferingHostIsItsOwnGroup(t *testing.T) {
	fake := transporttest.New().OnFunc(func(host resource.HostID, cmd transport.Command) (transporttest.Reply, bool) {
		if host == "web-03" {
			return transporttest.Reply{Stdout: "Chef Infra Client finished, 2/45 resources would have been updated\n"}, true
		}
		return transporttest.Reply{Stdout: "Chef Infra Client finished, 0/45 resources would have been updated\n"}, true
	})
	c := NewClient(ClientOptions{Exec: fake})
	rep, err := c.WhyRun(context.Background(), []resource.HostID{"web-01", "web-02", "web-03"})
	if err != nil {
		t.Fatalf("WhyRun: %v", err)
	}
	if len(rep.Summary.Groups) != 2 {
		t.Fatalf("groups = %+v, want 2", rep.Summary.Groups)
	}
	if got := rep.Headline(); got != "2 identical · 1 differ" {
		t.Errorf("Headline() = %q", got)
	}
	if got := rep.WouldChange(); got != 1 {
		t.Errorf("WouldChange() = %d, want 1", got)
	}
	if got := rep.Rows[2].Updated; got != 2 {
		t.Errorf("web-03 updated = %d, want 2", got)
	}
	if !strings.Contains(rep.Text(), "2/45 resources would have been updated") {
		t.Errorf("Text() = %q, want the parsed summary line of the differing group", rep.Text())
	}
}

// TestWhyRunUsesCincClient checks `chef.client: cinc-client` selects the Cinc
// binary, which is what the lab nodes run.
func TestWhyRunUsesCincClient(t *testing.T) {
	fake := whyRunFake(t)
	c := NewClient(ClientOptions{Exec: fake, Client: cincClient})
	if c.Name() != cincClient {
		t.Fatalf("Name() = %q", c.Name())
	}
	script, display := c.script(true)
	if script != "cinc-client --why-run --no-color -l warn --run-lock-timeout 30" {
		t.Errorf("script = %q", script)
	}
	if display != "sudo -n "+script {
		t.Errorf("display = %q", display)
	}
	if _, err := c.WhyRun(context.Background(), []resource.HostID{"web-01"}); err != nil {
		t.Fatalf("WhyRun: %v", err)
	}
	if got := fake.Calls()[0].Command.Script; !strings.HasPrefix(got, cincClient+" ") {
		t.Errorf("script = %q, want the cinc-client line", got)
	}
}

// TestWhyRunRunLockIsAClearFailure checks the pitfall: another run's lock is
// reported as one, not as a wall of client output.
func TestWhyRunRunLockIsAClearFailure(t *testing.T) {
	lock := "ERROR: Chef Infra Client cannot concurrently run more than one operation at a time.\n" +
		"Chef Infra Client cannot acquire the run lock on /var/chef/local-mode-cache/lockfile\n"
	fake := transporttest.New().On("chef-client", transporttest.Reply{RC: 1, Stdout: lock})
	c := NewClient(ClientOptions{Exec: fake})
	res := c.Step(context.Background(), action.Target{Host: "web-01"}, false)
	if res.Err == nil {
		t.Fatalf("output = %q, want the lock classified as a failure", res.Output)
	}
	if errs.KindOf(res.Err) != errs.Remote {
		t.Errorf("kind = %q, want remote", errs.KindOf(res.Err))
	}
	if !strings.Contains(res.Err.Error(), "run lock") {
		t.Errorf("error = %q, want it to name the run lock", res.Err)
	}
	if res.RC != 1 {
		t.Errorf("RC = %d, want 1", res.RC)
	}
}

// TestWhyRunFailsOnNonZeroRC checks any other non-zero exit is a failure with
// the first line of output as its message.
func TestWhyRunFailsOnNonZeroRC(t *testing.T) {
	fake := transporttest.New().On("chef-client", transporttest.Reply{
		RC: 1, Stdout: "[2026-11-13T10:00:00+00:00] ERROR: Failed to converge\n",
	})
	c := NewClient(ClientOptions{Exec: fake})
	res := c.Step(context.Background(), action.Target{Host: "web-01"}, true)
	if res.Err == nil {
		t.Fatal("want an error for rc 1")
	}
	if !strings.Contains(res.Err.Error(), "Failed to converge") {
		t.Errorf("error = %q", res.Err)
	}
	if strings.Contains(res.Output, "2026-11-13T10:00:00") {
		t.Errorf("output %q still carries a timestamp", res.Output)
	}
}

// TestWhyRunTransportErrorsAreCounted checks a host that never answered is an
// error in the summary, not a group.
func TestWhyRunTransportErrorsAreCounted(t *testing.T) {
	fake := transporttest.New().OnFunc(func(host resource.HostID, cmd transport.Command) (transporttest.Reply, bool) {
		if host == "web-02" {
			return transporttest.Reply{Err: errs.New(errs.Unreachable, "fake.exec", "no route")}, true
		}
		return transporttest.Reply{Stdout: whyRunBody(host)}, true
	})
	c := NewClient(ClientOptions{Exec: fake})
	rep, err := c.WhyRun(context.Background(), []resource.HostID{"web-01", "web-02"})
	if err != nil {
		t.Fatalf("WhyRun: %v", err)
	}
	if got := len(rep.Summary.Errors[errs.Unreachable]); got != 1 {
		t.Errorf("unreachable = %d, want 1 (%+v)", got, rep.Summary.Errors)
	}
	if len(rep.Summary.Groups) != 1 || len(rep.Summary.Groups[0].Hosts) != 1 {
		t.Errorf("groups = %+v, want one group holding web-01", rep.Summary.Groups)
	}
	if !strings.Contains(rep.Text(), "1 error") {
		t.Errorf("Text() = %q, want the error count", rep.Text())
	}
}

// TestWhyRunNormalize checks the stripping of timestamps, durations and pids.
func TestWhyRunNormalize(t *testing.T) {
	cases := []struct{ in, want string }{
		{
			in:   "[2026-11-13T10:00:00+00:00] INFO: Starting Chef Infra Client (pid 4242)\n",
			want: "INFO: Starting Chef Infra Client (pid <pid>)",
		},
		{
			in:   "Chef Infra Client finished, 2/45 resources would have been updated in 12.34 seconds\n",
			want: "Chef Infra Client finished, 2/45 resources would have been updated in <dur>",
		},
		{
			in:   "  * file[/etc/motd.chef] action create (up to date)   \r\n",
			want: "  * file[/etc/motd.chef] action create (up to date)",
		},
		{
			in:   "Recipe: lab::default\n\n\n",
			want: "Recipe: lab::default",
		},
	}
	for _, c := range cases {
		if got := NormalizeClientOutput(c.in); got != c.want {
			t.Errorf("NormalizeClientOutput(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// Two hosts that differ only in clock and pid must normalize equal.
	a := NormalizeClientOutput(whyRunBody("web-01"))
	b := NormalizeClientOutput(whyRunBody("web-02"))
	if a != b {
		t.Errorf("the two hosts normalize differently:\n%q\n%q", a, b)
	}
}

// TestWhyRunParseClientSummary checks the summary line becomes columns.
func TestWhyRunParseClientSummary(t *testing.T) {
	cases := []struct {
		in        string
		updated   int
		total     int
		hasCounts bool
		failed    bool
		ok        bool
	}{
		{"Chef Infra Client finished, 2/45 resources would have been updated", 2, 45, true, false, true},
		{"Chef Infra Client finished, 5/12 resources updated in 03 seconds", 5, 12, true, false, true},
		{"Chef Infra Client failed. 0 resources updated in 12 seconds", 0, 0, false, true, true},
		{"Chef Infra Client finished, 1/9 resources would have been updated", 1, 9, true, false, true},
		{"nothing useful here", 0, 0, false, false, false},
	}
	for _, c := range cases {
		got, ok := ParseClientSummary(c.in)
		if ok != c.ok {
			t.Errorf("%q: ok = %v, want %v", c.in, ok, c.ok)
			continue
		}
		if !ok {
			continue
		}
		if got.Updated != c.updated || got.Total != c.total || got.HasCounts != c.hasCounts || got.Failed != c.failed {
			t.Errorf("%q: summary = %+v, want %d/%d counts=%v failed=%v",
				c.in, got, c.updated, c.total, c.hasCounts, c.failed)
		}
	}
}

// TestWhyRunNeedsAnExecutor checks an unconfigured client refuses instead of
// panicking.
func TestWhyRunNeedsAnExecutor(t *testing.T) {
	c := NewClient(ClientOptions{})
	if _, err := c.WhyRun(context.Background(), []resource.HostID{"web-01"}); errs.KindOf(err) != errs.Config {
		t.Fatalf("kind = %q, want config (%v)", errs.KindOf(err), err)
	}
	if _, err := c.WhyRun(context.Background(), nil); errs.KindOf(err) != errs.Config {
		t.Errorf("no hosts kind = %q, want config", errs.KindOf(err))
	}
}
