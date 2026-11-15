package devtools

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"jumpr/internal/transport/transporttest"
)

// TestCaptureChef checks a capture run writes one file per planned command, the
// README lists them, and a failing command is recorded as a deviation instead of
// stopping the run (task 113).
func TestCaptureChef(t *testing.T) {
	fake := transporttest.New().
		On("knife --version", transporttest.Reply{Stdout: "Chef Infra Client: 18.4.12\n"}).
		On("knife status", transporttest.Reply{Stdout: `[{"name":"sr-web-01","ohai_time":1794570900}]`}).
		On("knife node list", transporttest.Reply{Stdout: `["sr-web-01"]`}).
		On("knife role list", transporttest.Reply{Stdout: `["web"]`}).
		On("knife environment list", transporttest.Reply{Stdout: `["production"]`}).
		On("knife environment show", transporttest.Reply{Stdout: `{"name":"production"}`}).
		On("knife node show", transporttest.Reply{Stdout: `{"name":"sr-web-01"}`}).
		On("knife search", transporttest.Reply{Stdout: `{"total":1,"start":0,"rows":[{"sr-web-01":"ubuntu"}]}`}).
		On("knife role show", transporttest.Reply{RC: 1, Stderr: "ERROR: Cannot find a role named 'web'"})

	dir := t.TempDir()
	var out bytes.Buffer
	err := CaptureChef(context.Background(), ChefOptions{Exec: fake, OutDir: dir, LocalUser: "tester"}, &out)
	if err == nil {
		t.Fatal("want an error naming the failed capture")
	}
	if !strings.Contains(err.Error(), "1 capture(s) failed") {
		t.Errorf("error = %q, want it to count the failed capture", err)
	}

	status, err := os.ReadFile(filepath.Join(dir, "status.json"))
	if err != nil {
		t.Fatalf("status.json: %v", err)
	}
	if !strings.Contains(string(status), "web-01.lab.example.internal") {
		t.Errorf("captured status not sanitized:\n%s", status)
	}
	if strings.Contains(string(status), "sr-web-01") {
		t.Errorf("captured status kept a lab-only name:\n%s", status)
	}

	readme, err := os.ReadFile(filepath.Join(dir, "README.md"))
	if err != nil {
		t.Fatalf("README: %v", err)
	}
	for _, want := range []string{"status.json: knife status -F json", "## Deviations", "role-show-web.json"} {
		if !strings.Contains(string(readme), want) {
			t.Errorf("README missing %q:\n%s", want, readme)
		}
	}
	if strings.Contains(string(readme), "tester") {
		t.Errorf("README kept the local user name:\n%s", readme)
	}
}

// TestCaptureChefNeedsATransport checks the capture refuses to run without an
// executor.
func TestCaptureChefNeedsATransport(t *testing.T) {
	if err := CaptureChef(context.Background(), ChefOptions{}, &bytes.Buffer{}); err == nil {
		t.Fatal("want a config error")
	}
}
