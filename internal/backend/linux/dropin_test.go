package linux

import (
	"context"
	"strings"
	"testing"

	"jumpr/internal/resource"
	"jumpr/internal/transport"
)

// dropInRecorder is an Executor that remembers every command it was handed.
// A preview must never hand it one.
type dropInRecorder struct {
	calls []transport.Command
}

func (r *dropInRecorder) Name() string { return "recorder" }

func (r *dropInRecorder) Exec(_ context.Context, _ resource.HostID, cmd transport.Command) (transport.ExecResult, error) {
	r.calls = append(r.calls, cmd)
	return transport.ExecResult{}, nil
}

// dropInCat is a `systemctl cat` output with an existing override.
const dropInCat = `# /usr/lib/systemd/system/nginx.service
[Unit]
Description=nginx

# /etc/systemd/system/nginx.service.d/override.conf
[Service]
LimitNOFILE=65535`

func TestDropInPathIsTheOverrideFile(t *testing.T) {
	if got, want := DropInPath(" nginx.service "), "/etc/systemd/system/nginx.service.d/override.conf"; got != want {
		t.Errorf("DropInPath = %q, want %q", got, want)
	}
	if got, want := DropInPath("sshd@web-01.service"), "/etc/systemd/system/sshd@web-01.service.d/override.conf"; got != want {
		t.Errorf("DropInPath of a template = %q, want %q", got, want)
	}
}

func TestDropInBodyReadsOneFile(t *testing.T) {
	body, ok := DropInBody(dropInCat, "/etc/systemd/system/nginx.service.d/override.conf")
	if !ok {
		t.Fatal("DropInBody did not find the override")
	}
	if body != "[Service]\nLimitNOFILE=65535" {
		t.Errorf("body = %q", body)
	}
	unit, ok := DropInBody(dropInCat, "/usr/lib/systemd/system/nginx.service")
	if !ok || unit != "[Unit]\nDescription=nginx" {
		t.Errorf("unit body = %q, ok = %v", unit, ok)
	}
	if _, ok := DropInBody(dropInCat, "/etc/systemd/system/nginx.service.d/none.conf"); ok {
		t.Error("DropInBody found a file that is not in the output")
	}
	if _, ok := DropInBody(dropInCat, ""); ok {
		t.Error("DropInBody of an empty path reported true")
	}
}

func TestDropInValidate(t *testing.T) {
	cases := []struct {
		name string
		text string
		want int
		has  []string
	}{
		{
			name: "clean service section",
			text: "[Service]\nNice=5\nLimitNOFILE=65535\n",
			want: 0,
		},
		{
			name: "comments and blank lines are fine",
			text: "[Service]\n# ExecStart=/bin/false\n\n; also ignored\nNice=5\n",
			want: 0,
		},
		{
			name: "no section header",
			text: "Nice=5\nLimitNOFILE=65535\n",
			want: 1,
			has:  []string{"no section header"},
		},
		{
			name: "unknown section",
			text: "[Servce]\nNice=5\n",
			want: 1,
			has:  []string{"line 1", "unknown section [Servce]"},
		},
		{
			name: "line without equals",
			text: "[Service]\nNice 5\n",
			want: 1,
			has:  []string{"line 2", "not a key=value line"},
		},
		{
			name: "ExecStart without a reset",
			text: "[Service]\nExecStart=/usr/bin/nginx -g 'daemon off;'\n",
			want: 1,
			has:  []string{"line 2", "empty ExecStart="},
		},
		{
			name: "ExecStart with a reset",
			text: "[Service]\nExecStart=\nExecStart=/usr/bin/nginx -g 'daemon off;'\n",
			want: 0,
		},
		{
			name: "the reset must come first",
			text: "[Service]\nExecStart=/bin/one\nExecStart=\nExecStart=/bin/two\n",
			want: 1,
			has:  []string{"line 2"},
		},
		{
			name: "the reset is per section",
			text: "[Service]\nExecStart=\n[Unit]\nExecStart=/bin/late\n",
			want: 1,
			has:  []string{"line 4"},
		},
		{
			name: "CRLF is handled",
			text: "[Service]\r\nNice 5\r\n",
			want: 1,
			has:  []string{"line 2"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ValidateDropIn(tc.text)
			if len(got) != tc.want {
				t.Fatalf("warnings = %d %v, want %d", len(got), got, tc.want)
			}
			joined := strings.Join(got, "\n")
			for _, want := range tc.has {
				if !strings.Contains(joined, want) {
					t.Errorf("warnings %v do not mention %q", got, want)
				}
			}
		})
	}
}

func TestDropInPreviewReplacesTheExistingDropIn(t *testing.T) {
	p := PreviewDropIn("nginx.service", dropInCat, "[Service]\nLimitNOFILE=65535\nNice=5\n")

	if p.Unit != "nginx.service" || p.Path != "/etc/systemd/system/nginx.service.d/override.conf" {
		t.Errorf("preview = %+v", p)
	}
	if p.Text != "[Service]\nLimitNOFILE=65535\nNice=5" {
		t.Errorf("text = %q, want the draft with no trailing blank line", p.Text)
	}
	if len(p.Warnings) != 0 {
		t.Errorf("warnings = %v, want none", p.Warnings)
	}
	for _, want := range []string{"+Nice=5", "--- systemctl cat nginx.service", "+++ systemctl cat nginx.service (with the drop-in)"} {
		if !strings.Contains(p.Diff, want) {
			t.Errorf("diff does not contain %q:\n%s", want, p.Diff)
		}
	}
	// The block is replaced in place: the file header is context, never added.
	if strings.Contains(p.Diff, "+# /etc/systemd/system/nginx.service.d/override.conf") {
		t.Errorf("diff adds the drop-in file instead of replacing the block:\n%s", p.Diff)
	}
	if n := strings.Count(p.Diff, "@@"); n != 2 {
		t.Errorf("diff has %d hunk markers, want one hunk:\n%s", n/2, p.Diff)
	}
}

func TestDropInPreviewAppendsANewDropIn(t *testing.T) {
	cat := "# /usr/lib/systemd/system/nginx.service\n[Unit]\nDescription=nginx"
	p := PreviewDropIn("nginx.service", cat, "[Service]\nNice=5")

	if !strings.Contains(p.Diff, "+# /etc/systemd/system/nginx.service.d/override.conf") {
		t.Errorf("diff does not add the drop-in file:\n%s", p.Diff)
	}
	if !strings.Contains(p.Diff, "+Nice=5") {
		t.Errorf("diff does not add the draft:\n%s", p.Diff)
	}
	if !strings.Contains(p.Diff, " Description=nginx") {
		t.Errorf("diff lost the unit file context:\n%s", p.Diff)
	}
}

func TestDropInPreviewUnchangedDraftHasNoDiff(t *testing.T) {
	p := PreviewDropIn("nginx.service", dropInCat, "[Service]\nLimitNOFILE=65535")
	if p.Diff != "" {
		t.Errorf("diff = %q, want empty for an unchanged draft", p.Diff)
	}
}

func TestDropInPreviewApplyBlock(t *testing.T) {
	p := PreviewDropIn("nginx.service", dropInCat, "[Service]\nNice=5\n")
	for _, want := range []string{
		"to apply by hand",
		"sudo systemctl edit nginx.service",
		"sudo mkdir -p /etc/systemd/system/nginx.service.d",
		"sudo tee /etc/systemd/system/nginx.service.d/override.conf >/dev/null <<'EOF'",
		"[Service]\nNice=5\nEOF",
		"sudo systemctl daemon-reload",
		"sudo systemctl restart nginx.service",
	} {
		if !strings.Contains(p.Apply, want) {
			t.Errorf("apply block does not contain %q:\n%s", want, p.Apply)
		}
	}
}

func TestDropInPreviewWarns(t *testing.T) {
	p := PreviewDropIn("nginx.service", dropInCat, "[Service]\nExecStart=/bin/true")
	if len(p.Warnings) != 1 || !strings.Contains(p.Warnings[0], "empty ExecStart=") {
		t.Errorf("warnings = %v, want the ExecStart warning", p.Warnings)
	}
}

// TestDropInPreviewRunsNoCommand is the zero-footprint rule of the task: a
// preview is text, so the executor is never handed a command and no action.Spec
// exists for applying one.
func TestDropInPreviewRunsNoCommand(t *testing.T) {
	rec := &dropInRecorder{}
	b := New(Options{Exec: rec})

	PreviewDropIn("nginx.service", dropInCat, "[Service]\nNice=5")
	ValidateDropIn("[Service]\nNice=5")

	if len(rec.calls) != 0 {
		t.Fatalf("preview ran %d commands, want none: %+v", len(rec.calls), rec.calls)
	}
	sel := []resource.Resource{{Ref: resource.Ref{Backend: "linux", Kind: KindUnitDetail, Host: "web-01", Name: "nginx.service"}}}
	for _, spec := range b.Actions(sel) {
		id := strings.ToLower(spec.ID + " " + spec.Title)
		if strings.Contains(id, "dropin") || strings.Contains(id, "drop-in") || strings.Contains(id, "override") {
			t.Errorf("the planner offers a drop-in action: %+v", spec)
		}
	}
}
