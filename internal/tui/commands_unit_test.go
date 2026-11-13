package tui_test

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"jumper/internal/fanout"
	"jumper/internal/group"
	"jumper/internal/resource"
	"jumper/internal/targets"
	"jumper/internal/transport"
	"jumper/internal/transport/transporttest"
	"jumper/internal/tui"
)

// unitHosts is the five-host target set the unit tests run on.
func unitHosts() []resource.HostID {
	return []resource.HostID{"web-01", "web-02", "web-03", "web-04", "web-05"}
}

// openedFanout is what a `:unitdiff` run handed to the fan-out view.
type openedFanout struct {
	title     string
	cmd       transport.Command
	hosts     []resource.HostID
	normalize func(string) string
}

// captureFanout returns an opener that records its arguments; nothing is
// rendered, because these tests are about the command, not the frame.
func captureFanout(got *openedFanout) tui.FanoutOpener {
	return func(_ *tui.Env, title string, cmd transport.Command, hosts []resource.HostID, normalize func(string) string) tui.View {
		*got = openedFanout{title: title, cmd: cmd, hosts: hosts, normalize: normalize}
		return nil
	}
}

// unitEnv is an Env whose session target set is hosts.
func unitEnv(hosts []resource.HostID) *tui.Env {
	return &tui.Env{Session: &tui.SessionState{Name: "main", Targets: targets.Set{Label: "test", Hosts: hosts}}}
}

// unitCommands registers the unit commands and returns the registry.
func unitCommands(t *testing.T, open tui.FanoutOpener) *tui.Commands {
	t.Helper()
	c := tui.NewCommands()
	if err := tui.RegisterUnitCommands(c, open); err != nil {
		t.Fatalf("RegisterUnitCommands: %v", err)
	}
	return c
}

// runCommand runs `:name args` and returns the message its tea.Cmd emits.
func runCommand(t *testing.T, c *tui.Commands, env *tui.Env, name, args string) tea.Msg {
	t.Helper()
	cmd, ok := c.Lookup(name)
	if !ok {
		t.Fatalf("command %q is not registered", name)
	}
	msg := cmd.Run(env, args)
	if msg == nil {
		return nil
	}
	return msg()
}

// unitBody is one host's `systemctl cat` output. The drop-in argument adds the
// extra `# /etc/systemd/system/...d/override.conf` block systemctl prints.
func unitBody(trailing string, dropIn bool) string {
	body := "# /usr/lib/systemd/system/nginx.service\n" +
		"[Unit]\n" +
		"Description=The nginx HTTP and reverse proxy server\n" +
		"[Service]\n" +
		"ExecStart=/usr/sbin/nginx\n" + trailing
	if dropIn {
		body += "# /etc/systemd/system/nginx.service.d/override.conf\n" +
			"[Service]\n" +
			"LimitNOFILE=8192\n"
	}
	return body
}

// TestUnitDiffGroupsIdenticalFiles is the acceptance behaviour: on five hosts
// where four carry the same file (one of them with stray trailing spaces) and
// one carries an extra drop-in, the fan-out headline is "4 identical · 1 differ".
func TestUnitDiffGroupsIdenticalFiles(t *testing.T) {
	ex := transporttest.New()
	ex.OnHost("web-01", "systemctl cat", transporttest.Reply{Stdout: unitBody("", false)})
	ex.OnHost("web-02", "systemctl cat", transporttest.Reply{Stdout: unitBody("   \n\t\n", false)})
	ex.OnHost("web-03", "systemctl cat", transporttest.Reply{Stdout: unitBody("", false)})
	ex.OnHost("web-04", "systemctl cat", transporttest.Reply{Stdout: unitBody("", false)})
	ex.OnHost("web-05", "systemctl cat", transporttest.Reply{Stdout: unitBody("", true)})

	var got openedFanout
	c := unitCommands(t, captureFanout(&got))
	env := unitEnv(unitHosts())

	msg := runCommand(t, c, env, "unitdiff", "nginx.service")
	if _, ok := msg.(tui.PushViewMsg); !ok {
		t.Fatalf(":unitdiff emitted %T, want PushViewMsg", msg)
	}
	if got.title != "unit-file nginx.service" {
		t.Errorf("title = %q, want %q", got.title, "unit-file nginx.service")
	}
	if got.normalize == nil {
		t.Fatal(":unitdiff passed no normalizer")
	}
	if want := []string{"systemctl", "cat", "nginx.service", "--no-pager"}; !strings.Contains(got.cmd.Script, strings.Join(want, " ")) {
		t.Errorf("command = %s, want systemctl cat nginx.service --no-pager", got.cmd.Script)
	}
	if len(got.hosts) != len(unitHosts()) {
		t.Errorf("hosts = %v, want the session targets", got.hosts)
	}
	if got.cmd.Sudo {
		t.Error("systemctl cat needs no sudo")
	}

	// Group the fake executor's answers the way the fan-out view does.
	g := group.NewGrouper(len(unitHosts()), got.normalize)
	for hr := range fanout.Run(context.Background(), ex, got.hosts, got.cmd, fanout.Options{}) {
		if hr.Err != nil {
			g.AddError(hr.Host, hr.Err)
			continue
		}
		g.Add(hr.Host, string(hr.Result.Stdout), hr.Result.RC)
	}
	sum := g.Summary()
	if head := sum.Headline(); head != "4 identical · 1 differ" {
		t.Fatalf("headline = %q, want %q (groups: %d)", head, "4 identical · 1 differ", len(sum.Groups))
	}
	if len(sum.Groups) != 2 {
		t.Fatalf("%d groups, want 2", len(sum.Groups))
	}
	if hosts := sum.Groups[0].Hosts; len(hosts) != 4 || hosts[3] != "web-04" {
		t.Errorf("majority group = %v, want the four identical hosts", hosts)
	}
	if len(sum.Groups[1].Hosts) != 1 || sum.Groups[1].Hosts[0] != "web-05" {
		t.Errorf("differing group = %v, want web-05", sum.Groups[1].Hosts)
	}
	if !strings.Contains(sum.Groups[1].Output, "override.conf") {
		t.Errorf("the differing group lost its drop-in header:\n%s", sum.Groups[1].Output)
	}
}

// TestUnitDiffNormalizerDropsOnlyTrailingSpace pins the normalizer: trailing
// whitespace goes, every other byte — the `# /path` headers included — stays.
func TestUnitDiffNormalizerDropsOnlyTrailingSpace(t *testing.T) {
	var got openedFanout
	c := unitCommands(t, captureFanout(&got))
	runCommand(t, c, unitEnv(unitHosts()), "unitdiff", "nginx.service")

	in := "# /etc/systemd/system/nginx.service.d/override.conf   \nLimitNOFILE=8192 \t\r\n\n\n"
	out := got.normalize(in)
	if want := "# /etc/systemd/system/nginx.service.d/override.conf\nLimitNOFILE=8192"; out != want {
		t.Fatalf("normalize = %q, want %q", out, want)
	}

	// A host with a drop-in must never normalize onto one without.
	withDropIn := got.normalize(unitBody("", true))
	without := got.normalize(unitBody("", false))
	if withDropIn == without {
		t.Fatal("a drop-in host normalized onto a host without one")
	}
}

// TestUnitDiffMissingUnitGroup keeps hosts whose unit does not exist out of the
// identical group: the probe prints "no such unit" itself, because the fan-out
// groups stdout and systemctl puts the "No files found" line on stderr.
func TestUnitDiffMissingUnitGroup(t *testing.T) {
	ex := transporttest.New()
	ex.OnHost("web-01", "systemctl cat", transporttest.Reply{Stdout: unitBody("", false)})
	ex.OnHost("web-02", "systemctl cat", transporttest.Reply{Stdout: "no such unit\n", RC: 0})
	ex.OnHost("web-03", "systemctl cat", transporttest.Reply{Stdout: "no such unit\n", RC: 0})

	var got openedFanout
	c := unitCommands(t, captureFanout(&got))
	runCommand(t, c, unitEnv(unitHosts()[:3]), "unitdiff", "nginx.service")

	if !strings.Contains(got.cmd.Script, "no such unit") {
		t.Fatalf("the probe does not report a missing unit on stdout: %s", got.cmd.Script)
	}

	g := group.NewGrouper(3, got.normalize)
	for hr := range fanout.Run(context.Background(), ex, got.hosts, got.cmd, fanout.Options{}) {
		if hr.Err != nil {
			g.AddError(hr.Host, hr.Err)
			continue
		}
		g.Add(hr.Host, string(hr.Result.Stdout), hr.Result.RC)
	}
	sum := g.Summary()
	if len(sum.Groups) != 2 {
		t.Fatalf("%d groups, want 2: %s", len(sum.Groups), sum.Headline())
	}
	if strings.TrimSpace(sum.Groups[0].Output) != "no such unit" || len(sum.Groups[0].Hosts) != 2 {
		t.Errorf("majority group = %q over %v, want \"no such unit\" on web-02 and web-03",
			sum.Groups[0].Output, sum.Groups[0].Hosts)
	}
}

// TestUnitDiffValidatesUnit checks the argument boundary of both commands: a
// unit name that could not survive one shell word is refused before any view.
func TestUnitDiffValidatesUnit(t *testing.T) {
	for _, tc := range []struct {
		name string
		args string
	}{
		{"empty", ""},
		{"two words", "nginx.service db.service"},
		{"semicolon", "nginx.service; reboot"},
		{"path", "../../etc/passwd"},
		{"quote", "nginx'service"},
	} {
		for _, cmdName := range []string{"unitdiff", "unit"} {
			t.Run(tc.name+"/"+cmdName, func(t *testing.T) {
				c := unitCommands(t, nil)
				msg := runCommand(t, c, unitEnv(unitHosts()), cmdName, tc.args)
				flash, ok := msg.(tui.FlashMsg)
				if !ok {
					t.Fatalf("%s %q emitted %T, want a flash", cmdName, tc.args, msg)
				}
				if flash.Level != tui.FlashError {
					t.Errorf("flash level = %v, want error", flash.Level)
				}
			})
		}
	}
}

// TestUnitDiffNeedsTargets refuses to diff nothing.
func TestUnitDiffNeedsTargets(t *testing.T) {
	for _, cmdName := range []string{"unitdiff", "unit"} {
		c := unitCommands(t, nil)
		msg := runCommand(t, c, unitEnv(nil), cmdName, "nginx.service")
		flash, ok := msg.(tui.FlashMsg)
		if !ok {
			t.Fatalf("%s emitted %T, want a flash", cmdName, msg)
		}
		if !strings.Contains(flash.Text, "no targets") {
			t.Errorf("%s flash = %q, want the no-targets hint", cmdName, flash.Text)
		}
	}
}

// TestUnitFleetOpensQuery is the second half of the task: `:unit <unit>` opens
// the unit-fleet kind for the session targets with the unit in the arguments.
func TestUnitFleetOpensQuery(t *testing.T) {
	c := unitCommands(t, nil)
	hosts := unitHosts()
	msg := runCommand(t, c, unitEnv(hosts), "unit", "nginx.service")
	qmsg, ok := msg.(tui.OpenQueryMsg)
	if !ok {
		t.Fatalf(":unit emitted %T, want OpenQueryMsg", msg)
	}
	q := qmsg.Query
	if q.Kind != "unit-fleet" {
		t.Errorf("kind = %q, want unit-fleet", q.Kind)
	}
	if got := q.Args["unit"]; got != "nginx.service" {
		t.Errorf("Args[unit] = %q, want nginx.service", got)
	}
	if len(q.Hosts) != len(hosts) {
		t.Fatalf("hosts = %v, want the session targets", q.Hosts)
	}
	for i, h := range hosts {
		if q.Hosts[i] != h {
			t.Errorf("host %d = %q, want %q", i, q.Hosts[i], h)
		}
	}
}
