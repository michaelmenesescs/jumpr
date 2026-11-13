package devtools

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeRunner records every command and simulates the toolchain: git reports the
// scripted status, `go build` writes the file named by -o, everything else
// succeeds unless failOn matches.
type fakeRunner struct {
	cmds   []Cmd
	status string // `git status --porcelain` output
	failOn string // a command line containing this text fails
	lines  []string
}

func (f *fakeRunner) run(ctx context.Context, c Cmd) error {
	f.cmds = append(f.cmds, c)
	line := c.Name + " " + strings.Join(c.Args, " ")
	f.lines = append(f.lines, line)
	if f.failOn != "" && strings.Contains(line, f.failOn) {
		return errors.New("fake failure: " + line)
	}
	switch {
	case c.Name == "git":
		if c.Stdout != nil {
			io.WriteString(c.Stdout, f.status)
		}
	case c.Name == "go" && len(c.Args) > 0 && c.Args[0] == "build":
		for i := 0; i+1 < len(c.Args); i++ {
			if c.Args[i] == "-o" {
				if err := os.WriteFile(c.Args[i+1], []byte("fake binary"), 0o755); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// hasLine reports whether a recorded command line contains want.
func (f *fakeRunner) hasLine(want string) bool {
	for _, l := range f.lines {
		if strings.Contains(l, want) {
			return true
		}
	}
	return false
}

// repo makes a throwaway repository root with the files a release packs.
func repo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"README.md":                               "# jumper\n",
		"docs/USER-GUIDE.md":                      "# user guide\n",
		"docs/KEYS.md":                            "# key bindings\n",
		"internal/app/templates/config.yaml":      "ui:\n  color: auto\n",
		"internal/app/templates/connections.yaml": "# connections\n",
		"internal/app/templates/policy.yaml":      "# policy\n",
		"internal/app/templates/keymap.yaml":      "open: [enter]\n",
		"examples/plugins/nginx-status.yaml":      "name: nginx-status\n",
		"examples/plugins/ntp-offset.yaml":        "name: ntp-offset\n",
	}
	for name, body := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", name, err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return root
}

// releaseOpts is a release of the throwaway repo with a fake toolchain.
func releaseOpts(t *testing.T, f *fakeRunner) ReleaseOptions {
	t.Helper()
	return ReleaseOptions{
		RepoDir:    repo(t),
		Version:    "v1.2.3",
		SkipChecks: true,
		Run:        f.run,
	}
}

// archiveEntries lists the members of a .tar.gz with their modes.
func archiveEntries(t *testing.T, path string) map[string]int64 {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("gzip %s: %v", path, err)
	}
	defer gz.Close()
	entries := map[string]int64{}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return entries
		}
		if err != nil {
			t.Fatalf("tar %s: %v", path, err)
		}
		entries[hdr.Name] = hdr.Mode
	}
}

func TestReleaseWritesArchivesAndSums(t *testing.T) {
	f := &fakeRunner{}
	o := releaseOpts(t, f)
	var out strings.Builder
	if err := Release(context.Background(), o, &out); err != nil {
		t.Fatalf("Release: %v", err)
	}
	dist := filepath.Join(o.RepoDir, "dist")
	for _, arch := range []string{"arm64", "amd64"} {
		path := filepath.Join(dist, "jumper-v1.2.3-darwin-"+arch+".tar.gz")
		entries := archiveEntries(t, path)
		want := []string{
			"jumper", "README.md", "docs/USER-GUIDE.md", "docs/KEYS.md",
			"examples/config.yaml", "examples/connections.yaml",
			"examples/policy.yaml", "examples/keymap.yaml",
			"examples/plugins/nginx-status.yaml", "examples/plugins/ntp-offset.yaml",
		}
		for _, name := range want {
			mode, ok := entries[name]
			if !ok {
				t.Errorf("%s is missing %s", filepath.Base(path), name)
				continue
			}
			if name == "jumper" && mode&0o111 == 0 {
				t.Errorf("the binary is not executable (mode %o)", mode)
			}
		}
		if len(entries) != len(want) {
			t.Errorf("%s has %d entries, want %d: %v", filepath.Base(path), len(entries), len(want), entries)
		}
	}

	sums, err := os.ReadFile(filepath.Join(dist, "SHA256SUMS"))
	if err != nil {
		t.Fatalf("read SHA256SUMS: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(sums)), "\n")
	if len(lines) != 2 {
		t.Fatalf("SHA256SUMS has %d lines, want one per archive", len(lines))
	}
	for _, line := range lines {
		fields := strings.SplitN(line, "  ", 2)
		if len(fields) != 2 {
			t.Fatalf("bad line %q", line)
		}
		if !strings.HasPrefix(fields[1], "dist/jumper-v1.2.3-darwin-") {
			t.Errorf("path %q, want it relative to the repository root", fields[1])
		}
		digest, err := fileSHA256(filepath.Join(o.RepoDir, fields[1]))
		if err != nil {
			t.Fatalf("%s: %v", fields[1], err)
		}
		if digest != fields[0] {
			t.Errorf("%s: checksum %s, want %s", fields[1], fields[0], digest)
		}
	}
	// The report carries the sizes of the archives.
	if !strings.Contains(out.String(), "jumper-v1.2.3-darwin-arm64.tar.gz") {
		t.Errorf("report is missing the archive sizes:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "SHA256SUMS") || !strings.Contains(out.String(), "2 archive(s)") {
		t.Errorf("report is missing the total:\n%s", out.String())
	}
}

func TestReleaseBuildFlags(t *testing.T) {
	f := &fakeRunner{}
	o := releaseOpts(t, f)
	if err := Release(context.Background(), o, io.Discard); err != nil {
		t.Fatalf("Release: %v", err)
	}
	var builds []Cmd
	for _, c := range f.cmds {
		if c.Name == "go" {
			builds = append(builds, c)
		}
	}
	if len(builds) != 2 {
		t.Fatalf("ran %d go commands, want one build per architecture", len(builds))
	}
	for i, arch := range []string{"arm64", "amd64"} {
		c := builds[i]
		args := strings.Join(c.Args, " ")
		for _, want := range []string{"build", "-trimpath", "-ldflags -s -w -X main.version=v1.2.3", "./cmd/jumper"} {
			if !strings.Contains(args, want) {
				t.Errorf("go build args %q are missing %q", args, want)
			}
		}
		env := strings.Join(c.Env, " ")
		for _, want := range []string{"CGO_ENABLED=0", "GOOS=darwin", "GOARCH=" + arch} {
			if !strings.Contains(env, want) {
				t.Errorf("go build env %q is missing %q", env, want)
			}
		}
		if c.Dir != o.RepoDir {
			t.Errorf("go build ran in %q, want the repository root", c.Dir)
		}
	}
}

func TestReleaseRefusesDirtyTree(t *testing.T) {
	f := &fakeRunner{status: " M docs/SPEC.md\n"}
	o := releaseOpts(t, f)
	o.SkipChecks = false
	err := Release(context.Background(), o, io.Discard)
	if err == nil {
		t.Fatal("Release accepted a dirty tree")
	}
	if !strings.Contains(err.Error(), "uncommitted changes") || !strings.Contains(err.Error(), "docs/SPEC.md") {
		t.Errorf("error = %v, want the dirty path", err)
	}
	if f.hasLine("make check") || f.hasLine("go build") {
		t.Errorf("a dirty tree must stop before make check and the build: %v", f.lines)
	}
	if _, err := os.Stat(filepath.Join(o.RepoDir, "dist")); err == nil {
		t.Error("a refused release wrote dist/")
	}
}

func TestReleaseRunsMakeCheckFirst(t *testing.T) {
	f := &fakeRunner{}
	o := releaseOpts(t, f)
	o.SkipChecks = false
	if err := Release(context.Background(), o, io.Discard); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if !f.hasLine("git status --porcelain") {
		t.Error("the clean-tree check did not run")
	}
	if !f.hasLine("make check") {
		t.Fatal("make check did not run")
	}
	for i, l := range f.lines {
		if strings.HasPrefix(l, "go build") {
			if i == 0 {
				t.Fatal("the build ran before make check")
			}
			break
		}
	}
}

func TestReleaseFailsWhenMakeCheckFails(t *testing.T) {
	f := &fakeRunner{failOn: "make check"}
	o := releaseOpts(t, f)
	o.SkipChecks = false
	err := Release(context.Background(), o, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "make check") {
		t.Fatalf("error = %v, want the make check failure", err)
	}
	if f.hasLine("go build") {
		t.Error("the build ran after make check failed")
	}
}

func TestReleaseFailsWhenBuildFails(t *testing.T) {
	f := &fakeRunner{failOn: "go build"}
	o := releaseOpts(t, f)
	err := Release(context.Background(), o, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "darwin/arm64") {
		t.Fatalf("error = %v, want the architecture named", err)
	}
	entries, statErr := os.ReadDir(filepath.Join(o.RepoDir, "dist"))
	if statErr == nil && len(entries) > 0 {
		t.Errorf("a failed build left %d files in dist/, want none", len(entries))
	}
}

func TestReleaseNeedsVersion(t *testing.T) {
	f := &fakeRunner{}
	for _, version := range []string{"", "1.2.3", "v1.2", "release-1"} {
		o := releaseOpts(t, f)
		o.Version = version
		if err := Release(context.Background(), o, io.Discard); err == nil {
			t.Errorf("version %q was accepted", version)
		}
	}
}

func TestReleaseRejectsUnknownArch(t *testing.T) {
	f := &fakeRunner{}
	o := releaseOpts(t, f)
	o.Archs = []string{"riscv64"}
	if err := Release(context.Background(), o, io.Discard); err == nil {
		t.Fatal("an unknown architecture was accepted")
	}
	if len(f.cmds) != 0 {
		t.Error("an unknown architecture ran a command")
	}
}

func TestReleaseSignsWhenAsked(t *testing.T) {
	f := &fakeRunner{}
	o := releaseOpts(t, f)
	o.Sign = "Developer ID Application: Example (ABCDE12345)"
	o.Archs = []string{"arm64"}
	if err := Release(context.Background(), o, io.Discard); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if !f.hasLine("codesign --force --sign Developer ID Application: Example (ABCDE12345)") {
		t.Fatalf("codesign did not run: %v", f.lines)
	}
	// A binary that could not be signed must not ship.
	f2 := &fakeRunner{failOn: "codesign"}
	o.Run = f2.run
	if err := Release(context.Background(), o, io.Discard); err == nil {
		t.Fatal("Release shipped an unsigned binary after codesign failed")
	}
}

func TestReleaseIsReproducible(t *testing.T) {
	f := &fakeRunner{}
	o := releaseOpts(t, f)
	o.Archs = []string{"arm64"}
	if err := Release(context.Background(), o, io.Discard); err != nil {
		t.Fatalf("Release: %v", err)
	}
	first, err := os.ReadFile(filepath.Join(o.RepoDir, "dist", "jumper-v1.2.3-darwin-arm64.tar.gz"))
	if err != nil {
		t.Fatalf("read archive: %v", err)
	}
	if err := Release(context.Background(), o, io.Discard); err != nil {
		t.Fatalf("second Release: %v", err)
	}
	second, err := os.ReadFile(filepath.Join(o.RepoDir, "dist", "jumper-v1.2.3-darwin-arm64.tar.gz"))
	if err != nil {
		t.Fatalf("read archive: %v", err)
	}
	if string(first) != string(second) {
		t.Error("two runs of the same release produced different bytes")
	}
}

func TestReleaseReportsMissingPayload(t *testing.T) {
	f := &fakeRunner{}
	o := releaseOpts(t, f)
	if err := os.Remove(filepath.Join(o.RepoDir, "examples", "plugins", "nginx-status.yaml")); err != nil {
		t.Fatalf("remove plugin: %v", err)
	}
	if err := os.Remove(filepath.Join(o.RepoDir, "examples", "plugins", "ntp-offset.yaml")); err != nil {
		t.Fatalf("remove plugin: %v", err)
	}
	err := Release(context.Background(), o, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "plugins") {
		t.Fatalf("error = %v, want the missing example plugins", err)
	}
}

func TestReleaseNeedsRepoDir(t *testing.T) {
	if err := Release(context.Background(), ReleaseOptions{Version: "v1.0.0"}, io.Discard); err == nil {
		t.Fatal("a release without a repository directory was accepted")
	}
}

func TestReleaseNeedsRunner(t *testing.T) {
	o := releaseOpts(t, &fakeRunner{})
	o.Run = nil
	if err := Release(context.Background(), o, io.Discard); err == nil {
		t.Fatal("a release without a command runner was accepted")
	}
}
