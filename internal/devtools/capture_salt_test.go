package devtools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"jumpr/internal/resource"
	"jumpr/internal/transport"
	"jumpr/internal/transport/transporttest"
)

// TestScrubValues checks the pillar value scrubber: every leaf becomes
// REDACTED-<type>, keys and the document's shape stay, and the output is
// valid JSON.
func TestScrubValues(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
		deny []string
	}{
		{
			name: "flat object",
			in:   `{"env":"lab","db_password":"not-a-real-secret","port":5432,"tls":true,"empty":null}`,
			want: []string{
				`"env":"REDACTED-string"`,
				`"db_password":"REDACTED-string"`,
				`"port":"REDACTED-number"`,
				`"tls":"REDACTED-bool"`,
				`"empty":"REDACTED-null"`,
			},
			deny: []string{"not-a-real-secret", "5432", "lab"},
		},
		{
			name: "nested object and array",
			in:   `{"common":{"credential":{"user":"svc","pass":"hunter2"},"roles":["web","db"]}}`,
			want: []string{
				`"credential":{"pass":"REDACTED-string","user":"REDACTED-string"}`,
				`"roles":["REDACTED-string","REDACTED-string"]`,
				`"common":`,
			},
			deny: []string{"svc", "hunter2", "web", "db"},
		},
		{
			name: "per-minion shape",
			in:   `{"web-01":{"env":"lab"},"db-01":{"env":"lab"}}`,
			want: []string{`"web-01":{"env":"REDACTED-string"}`, `"db-01":{"env":"REDACTED-string"}`},
			deny: []string{"lab"},
		},
		{
			name: "already scrubbed is left alone",
			in:   `{"a":"REDACTED-string","b":{"c":"REDACTED-number"}}`,
			want: []string{`"a":"REDACTED-string"`, `"c":"REDACTED-number"`},
			deny: []string{`"REDACTED-string":"`, "REDACTED-bool"},
		},
		{
			name: "not json is returned unchanged",
			in:   "ERROR: not a map\n",
			want: []string{"ERROR: not a map\n"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := string(ScrubValues([]byte(tt.in)))
			for _, want := range tt.want {
				if !strings.Contains(got, want) {
					t.Errorf("ScrubValues(%q) = %q; want it to contain %q", tt.in, got, want)
				}
			}
			for _, deny := range tt.deny {
				if strings.Contains(got, deny) {
					t.Errorf("ScrubValues(%q) = %q; want it without %q", tt.in, got, deny)
				}
			}
		})
	}
}

// TestScrubValuesIdempotent runs the scrubber twice: the capture rewrites
// files in place, so a second pass must change nothing.
func TestScrubValuesIdempotent(t *testing.T) {
	in := `{"env":"lab","count":3,"nested":{"flag":false,"list":[1,"two",null]}}`
	once := ScrubValues([]byte(in))
	twice := ScrubValues(once)
	if string(once) != string(twice) {
		t.Fatalf("ScrubValues is not idempotent:\n once: %q\ntwice: %q", once, twice)
	}
}

// TestScrubValuesThenSanitizePillar is the combined pillar path: the scrubber
// removes every value first, so nothing in the lab's pillar (the fake secret
// included) can reach the committed file.
func TestScrubValuesThenSanitizePillar(t *testing.T) {
	const pillar = `{"web-01":{"env":"lab","db_password":"not-a-real-secret","addr":"192.168.139.5"}}`
	got := string(Sanitize(ScrubValues([]byte(pillar)), testHostMap()))
	if strings.Contains(got, "not-a-real-secret") || strings.Contains(got, "192.168.139.5") {
		t.Fatalf("scrubbed pillar still carries a value: %q", got)
	}
	if !strings.Contains(got, `"web-01"`) || !strings.Contains(got, `"db_password":"REDACTED-string"`) {
		t.Fatalf("scrubbed pillar lost its shape: %q", got)
	}
}

// saltFake scripts the Salt CLI and the two lab-only shell changes the capture
// makes, so the whole plan runs without a lab.
func saltFake() *transporttest.Fake {
	const jid = "20260101010101000001"
	listJob := `{"jid":"` + jid + `","Function":"test.ping","Arguments":[],"Target":"*",` +
		`"Target-type":"glob","User":"root","StartTime":"2026, Jan 01 01:01:01.000000",` +
		`"Minions":["web-01","web-02"],"Result":{"web-01":true,"web-02":true}}`
	reply := func(s string) (transporttest.Reply, bool) { return transporttest.Reply{Stdout: s, RC: 0}, true }
	f := transporttest.New()
	f.OnFunc(func(host resource.HostID, cmd transport.Command) (transporttest.Reply, bool) {
		script := cmd.Script
		switch {
		case strings.Contains(script, "systemctl") && strings.Contains(script, "salt-minion"):
			return transporttest.Reply{RC: 0}, true
		case strings.Contains(script, "cp -p /etc/motd"):
			return transporttest.Reply{RC: 0}, true
		case strings.Contains(script, "mv /etc/motd.jumpr-capture"):
			return transporttest.Reply{RC: 0}, true
		case strings.Contains(script, "salt --version"):
			return reply("salt 3007.1 (Chlorine)\n")
		case strings.Contains(script, "jobs.list_job"):
			return reply(listJob)
		case strings.Contains(script, "jobs.active"):
			return reply(`{"20260101010101000002":{"Function":"test.sleep","Arguments":["20"],` +
				`"Target":"web-01,web-02","Target-type":"list","User":"root"}}`)
		case strings.Contains(script, "jobs.list_jobs"):
			return reply(`{"` + jid + `":{"Function":"test.ping","Arguments":[],"Target":"*",` +
				`"Target-type":"glob","User":"root","StartTime":"2026, Jan 01 01:01:01.000000"}}`)
		case strings.Contains(script, "--preview-target") && !strings.Contains(script, "--out=json"):
			return reply("web-01:\n    - web-02:\n    - web-03:\n")
		case strings.Contains(script, "--preview-target"):
			return reply(`["web-02","web-03"]`)
		case strings.Contains(script, "salt-key") && strings.Contains(script, "-F"):
			return reply(`{"minions":{},"minions_pre":{},"minions_rejected":{}}`)
		case strings.Contains(script, "salt-key"):
			return reply(`{"minions":["web-01","web-02"],"minions_pre":[],"minions_rejected":[]}`)
		case strings.Contains(script, "manage.status"):
			return reply(`{"up":["web-01","web-02"],"down":[]}`)
		case strings.Contains(script, "manage.versions"):
			return reply(`{"web-01":{"salt":3007.1},"web-02":{"salt":3007.1}}`)
		case strings.Contains(script, "cache.grains"):
			return reply(`{"web-01":{"role":["web"],"dc":"lab"}}`)
		case strings.Contains(script, "config.get"):
			return reply(`{"webs":"G@role:web","k8s":"G@role:k8s"}`)
		case strings.Contains(script, "state.event"):
			return reply("salt/job/20260101010101000003/new\tsalt\tweb-01\n")
		case strings.Contains(script, "--async"):
			return reply("jid: " + jid + "\n")
		case strings.Contains(script, "state.show_highstate"):
			return reply(`{"web-01":{"nginx":{"pkg.installed":[]}}}`)
		case strings.Contains(script, "state.sls"):
			return reply(`["Rendering SLS 'nosuch' failed: not found"]`)
		case strings.Contains(script, "pillar.items"):
			return reply(`{"web-01":{"env":"lab","db_password":"not-a-real-secret"}}`)
		case strings.Contains(script, "state.show_top"):
			return reply(`{"base":{"*":["common"],"role:web":["nginx"]}}`)
		case strings.Contains(script, "state.show_sls"):
			return reply(`{"nginx":{"nginx":{"pkg.installed":[]}}}`)
		case strings.Contains(script, "mine.get"):
			return reply(`{"web-01":["192.168.139.5"]}`)
		case strings.Contains(script, "fileserver.envs"):
			return reply(`["base"]`)
		case strings.Contains(script, "fileserver.file_list"):
			return reply(`["top.sls","common.sls","nginx.sls"]`)
		}
		return transporttest.Reply{}, false
	})
	return f
}

// TestCaptureSaltWritesGoldenFiles runs the whole capture plan against a
// scripted lab and checks every required file lands, sanitized and scrubbed.
func TestCaptureSaltWritesGoldenFiles(t *testing.T) {
	outDir := t.TempDir()
	fake := saltFake()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var log strings.Builder
	err := CaptureSalt(ctx, SaltOptions{
		Exec:         fake,
		OutDir:       outDir,
		LocalUser:    "developer",
		Poll:         time.Millisecond,
		Wait:         20 * time.Millisecond,
		NoReturnWait: 2 * time.Millisecond,
		EventWindow:  time.Second,
	}, &log)
	if err != nil {
		t.Fatalf("CaptureSalt: %v\nlog:\n%s", err, log.String())
	}

	want := []string{
		"version.txt", "key-list.json", "key-finger.json", "manage-status.json",
		"manage-versions.json", "cache-grains.json", "nodegroups.json",
		"preview-compound.txt", "preview-compound.json",
		"async-submit.txt", "list-job-ping.json",
		"jobs-active.json", "jobs-list.json", "cmd-run-all.json",
		"state-test-clean.json", "state-test-changes.json", "state-render-error.json",
		"show-top.json", "show-sls-nginx.json", "show-highstate.json",
		"pillar-items.json", "mine.json", "fileserver-envs.json",
		"fileserver-file-list.json", "event-sample.txt", "no-return.json",
	}
	for _, name := range want {
		path := filepath.Join(outDir, name)
		b, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("capture did not write %s: %v", name, err)
			continue
		}
		if len(b) == 0 {
			t.Errorf("%s is empty", name)
		}
		if strings.Contains(string(b), "sr-") || strings.Contains(string(b), "developer") {
			t.Errorf("%s still carries a real identity: %q", name, b)
		}
	}

	pillar, err := os.ReadFile(filepath.Join(outDir, "pillar-items.json"))
	if err != nil {
		t.Fatalf("pillar-items.json: %v", err)
	}
	if strings.Contains(string(pillar), "not-a-real-secret") {
		t.Errorf("pillar-items.json carries the pillar secret: %q", pillar)
	}
	if !strings.Contains(string(pillar), `"db_password":"REDACTED-string"`) {
		t.Errorf("pillar-items.json is not scrubbed: %q", pillar)
	}

	readme, err := os.ReadFile(filepath.Join(outDir, "README.md"))
	if err != nil {
		t.Fatalf("README.md: %v", err)
	}
	for _, name := range want {
		if !strings.Contains(string(readme), name+": ") {
			t.Errorf("README.md has no command line for %s", name)
		}
	}
	if !strings.Contains(string(readme), "## Deviations\n\nNone.") {
		t.Errorf("README.md has no empty Deviations section: %q", readme)
	}

	// The lab-only changes were both made and undone.
	calls := fake.Calls()
	var stopped, started, drifted, restored bool
	for _, c := range calls {
		switch {
		case strings.Contains(c.Command.Script, "systemctl stop salt-minion"):
			stopped = true
		case strings.Contains(c.Command.Script, "systemctl start salt-minion"):
			started = true
		case strings.Contains(c.Command.Script, "cp -p /etc/motd"):
			drifted = true
		case strings.Contains(c.Command.Script, "mv /etc/motd.jumpr-capture"):
			restored = true
		}
	}
	if !stopped || !started {
		t.Errorf("no-return capture did not stop and restart salt-minion (stop=%v start=%v)", stopped, started)
	}
	if !drifted || !restored {
		t.Errorf("state drift was not created and restored (drift=%v restore=%v)", drifted, restored)
	}
}

// TestCaptureSaltRecordsDeviations checks requirement 4: a failing command is
// recorded under Deviations instead of aborting the run.
func TestCaptureSaltRecordsDeviations(t *testing.T) {
	outDir := t.TempDir()
	f := transporttest.New() // no rules: every command exits 127
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	err := CaptureSalt(ctx, SaltOptions{
		Exec:         f,
		OutDir:       outDir,
		Poll:         time.Millisecond,
		Wait:         time.Millisecond,
		NoReturnWait: time.Millisecond,
		EventWindow:  time.Second,
	}, &strings.Builder{})
	if err == nil {
		t.Fatal("CaptureSalt returned nil although every command failed")
	}
	readme, rerr := os.ReadFile(filepath.Join(outDir, "README.md"))
	if rerr != nil {
		t.Fatalf("README.md: %v", rerr)
	}
	if !strings.Contains(string(readme), "## Deviations\n\n- version.txt: ") {
		t.Fatalf("README.md does not record the failures: %q", readme)
	}
}

// TestSaltCaptureReadmeDocumentsEveryFile keeps the committed README in step
// with the capture plan: every file the tool writes is listed with the command
// that produces it. UPDATE_GOLDEN=1 rewrites it from the plan.
func TestSaltCaptureReadmeDocumentsEveryFile(t *testing.T) {
	plan, err := saltPlan(saltCaptureEventWindow)
	if err != nil {
		t.Fatalf("saltPlan: %v", err)
	}
	path := filepath.Join("..", "..", "testdata", "salt", "README.md")
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		lines := make([]string, 0, len(plan))
		for _, c := range plan {
			lines = append(lines, c.Name+": "+c.Cmd)
		}
		if err := writeCapture(path, saltReadme(lines, nil)); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	readme, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("testdata/salt/README.md: %v", err)
	}
	for _, c := range plan {
		if !strings.Contains(string(readme), c.Name+": ") {
			t.Errorf("testdata/salt/README.md has no line for %s", c.Name)
		}
	}
	if strings.Contains(string(readme), "fake-secret") {
		t.Errorf("testdata/salt/README.md mentions a secret")
	}
}
