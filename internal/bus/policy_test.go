package bus

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"jumper/internal/action"
	"jumper/internal/errs"
	"jumper/internal/resource"
)

// testPolicy is the checked-in policy.yaml used by the selector and target
// tests.
const testPolicy = "testdata/policy.yaml"

// displayPlan builds a one-step plan with the given display string on targets.
func displayPlan(display string, targets ...action.Target) action.Plan {
	if len(targets) == 0 {
		targets = []action.Target{{Host: "web-ewr-001"}}
	}
	steps := make([]action.Step, len(targets))
	for i, t := range targets {
		steps[i] = action.Step{Target: t, Display: display}
	}
	return action.Plan{
		Spec:    action.Spec{ID: "linux.systemctl.restart"},
		Request: action.Request{SpecID: "linux.systemctl.restart", Targets: targets},
		Steps:   steps,
		Via:     "ssh",
	}
}

// hostTarget makes a target with the given labels.
func hostTarget(host string, labels map[string]string) action.Target {
	return action.Target{Host: resource.HostID(host), Labels: labels}
}

// nTargets makes n plain targets.
func nTargets(n int) []action.Target {
	out := make([]action.Target, n)
	for i := range out {
		out[i] = hostTarget("web-ewr-001", nil)
	}
	return out
}

// writePolicy writes a policy file into a temp dir and returns its path.
func writePolicy(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "policy.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write policy: %v", err)
	}
	return path
}

func TestPolicyDefaultDeniesStopSSHD(t *testing.T) {
	p := DefaultPolicy()
	denied := []string{
		"sudo -n systemctl stop sshd.service",
		"sudo -n -- systemctl disable ssh",
		"systemctl mask sshd",
		"sudo -n rm -rf /",
		"reboot",
		"shutdown -h now",
		"mkfs.ext4 /dev/sda1",
		"dd if=/dev/zero of=/dev/sda bs=1M",
	}
	for _, display := range denied {
		d := p.Check(displayPlan(display), false)
		if d == nil {
			t.Fatalf("%q: want denied, got allowed", display)
		}
		if d.Rule == "" || d.Reason == "" {
			t.Fatalf("%q: denial needs a rule and a reason, got %+v", display, d)
		}
	}
	if d := p.Check(displayPlan("sudo -n systemctl stop sshd.service"), false); d.Reason != "never cut off SSH" {
		t.Fatalf("stop sshd reason = %q, want %q", d.Reason, "never cut off SSH")
	}
}

func TestPolicyDefaultAllowsRestartNginx(t *testing.T) {
	p := DefaultPolicy()
	allowed := []string{
		"sudo -n -- systemctl restart nginx.service",
		"sudo -n systemctl reload nginx",
		"systemctl status sshd",
		"uptime",
		"rm -rf /var/tmp/build",
		"dd if=/dev/zero of=/tmp/file bs=1M",
	}
	for _, display := range allowed {
		if d := p.Check(displayPlan(display), false); d != nil {
			t.Fatalf("%q: want allowed, got %+v", display, d)
		}
	}
	if p.Check(displayPlan("journalctl -u nginx"), false) != nil {
		t.Fatal("journalctl: want allowed")
	}
}

func TestPolicyReadOnlyDeniesEverything(t *testing.T) {
	p := DefaultPolicy()
	if p.ReadOnly() {
		t.Fatal("default policy must not be read-only")
	}
	d := p.Check(displayPlan("systemctl restart nginx"), true)
	if d == nil {
		t.Fatal("readonly override: want denied, got allowed")
	}
	if d.Rule != "readonly: true" {
		t.Fatalf("rule = %q, want %q", d.Rule, "readonly: true")
	}

	path := writePolicy(t, "readonly: true\n")
	ro, err := LoadPolicy(path)
	if err != nil {
		t.Fatalf("LoadPolicy: %v", err)
	}
	if !ro.ReadOnly() {
		t.Fatal("policy file says readonly, ReadOnly() is false")
	}
	if d := ro.Check(displayPlan("systemctl restart nginx"), false); d == nil {
		t.Fatal("read-only policy: want denied, got allowed")
	}
}

func TestPolicyMaxTargets(t *testing.T) {
	p, err := LoadPolicy(testPolicy)
	if err != nil {
		t.Fatalf("LoadPolicy: %v", err)
	}
	if d := p.Check(displayPlan("systemctl restart nginx", nTargets(5)...), false); d != nil {
		t.Fatalf("5 targets: want allowed, got %+v", d)
	}
	d := p.Check(displayPlan("systemctl restart nginx", nTargets(6)...), false)
	if d == nil {
		t.Fatal("6 targets: want denied, got allowed")
	}
	if d.Rule != "max_targets: 5" {
		t.Fatalf("rule = %q, want %q", d.Rule, "max_targets: 5")
	}

	// The count comes from the request when a plan carries no steps.
	plan := action.Plan{
		Spec:    action.Spec{ID: "generic.run"},
		Request: action.Request{Targets: nTargets(6)},
	}
	if d := p.Check(plan, false); d == nil {
		t.Fatal("request targets over the limit: want denied, got allowed")
	}
}

func TestPolicySpecRuleWithSelector(t *testing.T) {
	p, err := LoadPolicy(testPolicy)
	if err != nil {
		t.Fatalf("LoadPolicy: %v", err)
	}
	prod := hostTarget("db-ewr-001", map[string]string{"env": "prod", "role": "db"})
	staging := hostTarget("db-ewr-002", map[string]string{"env": "staging", "role": "db"})

	keyDelete := action.Plan{
		Spec:    action.Spec{ID: "salt.key.delete"},
		Request: action.Request{SpecID: "salt.key.delete", Targets: []action.Target{prod}},
	}
	d := p.Check(keyDelete, false)
	if d == nil {
		t.Fatal("salt.key.delete in prod: want denied, got allowed")
	}
	if !strings.Contains(d.Rule, "salt.key.delete") {
		t.Fatalf("rule = %q, want it to name the spec", d.Rule)
	}
	if d.Reason != "key deletes in prod go through change management" {
		t.Fatalf("reason = %q", d.Reason)
	}

	keyDelete.Request.Targets = []action.Target{staging}
	if d := p.Check(keyDelete, false); d != nil {
		t.Fatalf("salt.key.delete outside prod: want allowed, got %+v", d)
	}

	other := action.Plan{
		Spec:    action.Spec{ID: "salt.state.apply"},
		Request: action.Request{SpecID: "salt.state.apply", Targets: []action.Target{prod}},
	}
	if d := p.Check(other, false); d != nil {
		t.Fatalf("another spec in prod: want allowed, got %+v", d)
	}
}

func TestPolicySpecRuleGlob(t *testing.T) {
	path := writePolicy(t, "deny:\n  - spec: 'salt.*'\n    reason: no salt writes here\n")
	p, err := LoadPolicy(path)
	if err != nil {
		t.Fatalf("LoadPolicy: %v", err)
	}
	plan := displayPlan("salt-call state.apply")
	plan.Spec.ID = "salt.state.apply"
	if d := p.Check(plan, false); d == nil {
		t.Fatal("glob spec rule: want denied, got allowed")
	}
	plan.Spec.ID = "linux.systemctl.restart"
	if d := p.Check(plan, false); d != nil {
		t.Fatalf("non-matching spec: want allowed, got %+v", d)
	}
}

func TestPolicyProtectedSelector(t *testing.T) {
	p, err := LoadPolicy(testPolicy)
	if err != nil {
		t.Fatalf("LoadPolicy: %v", err)
	}
	cases := []struct {
		name   string
		labels map[string]string
		want   bool
	}{
		{"prod db", map[string]string{"env": "prod", "role": "db"}, true},
		{"prod web", map[string]string{"env": "prod", "role": "web"}, false},
		{"protected label", map[string]string{"protected": "true"}, true},
		{"nothing", nil, false},
	}
	for _, c := range cases {
		if got := p.Protected(hostTarget("h1", c.labels)); got != c.want {
			t.Fatalf("%s: Protected = %v, want %v", c.name, got, c.want)
		}
	}

	// The built-in policy protects the "protected" label.
	if !DefaultPolicy().Protected(hostTarget("h1", map[string]string{"protected": "yes"})) {
		t.Fatal("default policy must protect the protected label")
	}
	if DefaultPolicy().Protected(hostTarget("h1", map[string]string{"env": "prod", "role": "db"})) {
		t.Fatal("default policy has no prod-db selector")
	}
}

func TestPolicyRootTransportDenied(t *testing.T) {
	path := writePolicy(t, "allow_root_transport: false\n")
	p, err := LoadPolicy(path)
	if err != nil {
		t.Fatalf("LoadPolicy: %v", err)
	}
	for _, via := range []string{"salt", "salt-ssh"} {
		plan := displayPlan("systemctl restart nginx")
		plan.Via = via
		d := p.Check(plan, false)
		if d == nil {
			t.Fatalf("via %q: want denied, got allowed", via)
		}
		if d.Rule != "allow_root_transport: false" {
			t.Fatalf("via %q: rule = %q", via, d.Rule)
		}
	}
	plan := displayPlan("systemctl restart nginx")
	plan.Via = "ssh"
	if d := p.Check(plan, false); d != nil {
		t.Fatalf("via ssh: want allowed, got %+v", d)
	}

	saltPlan := displayPlan("systemctl restart nginx")
	saltPlan.Via = "salt"
	if d := DefaultPolicy().Check(saltPlan, false); d != nil {
		t.Fatalf("default policy allows Salt by default, got %+v", d)
	}
}

func TestPolicyCheckLine(t *testing.T) {
	p := DefaultPolicy()
	if d := p.CheckLine("sudo -n systemctl stop sshd"); d == nil {
		t.Fatal("stop sshd line: want denied, got allowed")
	}
	if d := p.CheckLine("ls -l /var/log"); d != nil {
		t.Fatalf("harmless line: want allowed, got %+v", d)
	}

	// Spec rules never apply to a line.
	loaded, err := LoadPolicy(testPolicy)
	if err != nil {
		t.Fatalf("LoadPolicy: %v", err)
	}
	if d := loaded.CheckLine("salt.key.delete web-ewr-001"); d != nil {
		t.Fatalf("spec rule fired on a line: %+v", d)
	}

	// A user pattern rule applies to lines too.
	user, err := LoadPolicy(writePolicy(t, "deny:\n  - pattern: '\\bkill\\s+-9\\b'\n    reason: no SIGKILL here\n"))
	if err != nil {
		t.Fatalf("LoadPolicy: %v", err)
	}
	d := user.CheckLine("kill -9 1234")
	if d == nil || d.Reason != "no SIGKILL here" {
		t.Fatalf("user pattern on a line: got %+v", d)
	}
}

func TestPolicyUnknownKeyIsError(t *testing.T) {
	cases := map[string]string{
		"unknown key":          "protectd: []\n",
		"unknown sync key":     "sync:\n  raww: confirm\n",
		"disable_builtin":      "deny:\n  - spec: salt.key.delete\n    disable_builtin: no-rm\n",
		"bad pattern":          "deny:\n  - pattern: 'systemctl ('\n",
		"both pattern, spec":   "deny:\n  - pattern: 'x'\n    spec: y\n",
		"neither":              "deny:\n  - reason: nothing to match\n",
		"bad selector":         "deny:\n  - spec: a.b\n    selector: '=oops'\n",
		"bad protected":        "protected: ['=oops']\n",
		"bad sync raw":         "sync:\n  raw: maybe\n",
		"negative max_targets": "max_targets: -1\n",
	}
	for name, body := range cases {
		_, err := LoadPolicy(writePolicy(t, body))
		if err == nil {
			t.Fatalf("%s: want error, got nil", name)
		}
		if k := errs.KindOf(err); k != errs.Config {
			t.Fatalf("%s: kind = %q, want %q", name, k, errs.Config)
		}
	}
}

func TestPolicyLoadMissingFileUsesDefaults(t *testing.T) {
	p, err := LoadPolicy(filepath.Join(t.TempDir(), "absent.yaml"))
	if err != nil {
		t.Fatalf("LoadPolicy: %v", err)
	}
	if p.ReadOnly() {
		t.Fatal("default policy must not be read-only")
	}
	if p.SyncRaw() != "confirm" {
		t.Fatalf("sync raw = %q, want confirm", p.SyncRaw())
	}
	if p.Check(displayPlan("systemctl restart nginx"), false) != nil {
		t.Fatal("default policy must allow a restart")
	}
	if p.Check(displayPlan("sudo -n systemctl stop sshd"), false) == nil {
		t.Fatal("default policy must deny stop sshd")
	}
}

func TestPolicyReload(t *testing.T) {
	p, err := LoadPolicy(testPolicy)
	if err != nil {
		t.Fatalf("LoadPolicy: %v", err)
	}
	path := writePolicy(t, "readonly: true\nshell_in_readonly: true\nsync:\n  raw: deny\n")
	if err := p.Reload(path); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if !p.ReadOnly() || !p.ShellInReadOnly() || p.SyncRaw() != "deny" {
		t.Fatalf("reload did not swap: readonly=%v shell=%v sync=%q",
			p.ReadOnly(), p.ShellInReadOnly(), p.SyncRaw())
	}
	// A broken file leaves the previous values in force.
	if err := p.Reload(writePolicy(t, "sync:\n  raw: maybe\n")); err == nil {
		t.Fatal("Reload of a broken file: want error, got nil")
	}
	if !p.ReadOnly() || p.SyncRaw() != "deny" {
		t.Fatal("failed reload changed the policy")
	}
	// A missing file restores the defaults.
	if err := p.Reload(filepath.Join(t.TempDir(), "absent.yaml")); err != nil {
		t.Fatalf("Reload missing: %v", err)
	}
	if p.ReadOnly() || p.SyncRaw() != "confirm" {
		t.Fatal("reload of a missing file did not restore the defaults")
	}
}
