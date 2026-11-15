// Release tooling for jumpr: a reproducible darwin build, the archives a user
// downloads and the checksum file that goes with them. It is a Go program, not
// a script (AGENTS.md rule 4): `go run ./cmd/sitrep-lab release --version v1.2.3`.
//
// Nothing here uploads or tags anything. It builds, it packs, it prints sizes,
// and it refuses to start from a tree it cannot account for.
package devtools

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"jumpr/internal/errs"
	"jumpr/internal/render"
)

// versionRe is the shape a release version must have: a leading v, three
// numbers, and an optional pre-release or build suffix (`v1.2.3`, `v0.0.0-test`).
var versionRe = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.\-]+)?$`)

// releaseArches are the darwin architectures a release carries.
var releaseArches = []string{"arm64", "amd64"}

// releaseTime is the modification time stamped into every archive entry and the
// gzip header, so two runs of the same commit produce the same bytes.
var releaseTime = time.Unix(0, 0).UTC()

// Cmd is one command the release runs.
type Cmd struct {
	Name   string   // executable to run, e.g. "go"
	Args   []string // its arguments
	Dir    string   // working directory; "" = the process's
	Env    []string // KEY=VALUE pairs added to the inherited environment
	Stdout io.Writer
	Stderr io.Writer
}

// Runner runs one command and returns its error. The default is exec.CommandContext;
// tests replace it to keep the release off the toolchain.
type Runner func(ctx context.Context, c Cmd) error

// ReleaseOptions configure one release run.
type ReleaseOptions struct {
	// RepoDir is the repository root: where go.mod, README.md and docs live.
	// Required.
	RepoDir string
	// Version is the version being released (`v1.2.3`). Required.
	Version string
	// OutDir receives the archives and SHA256SUMS; "" means <RepoDir>/dist.
	OutDir string
	// Archs are the darwin architectures to build; empty means arm64 and amd64.
	Archs []string
	// Sign is a codesign identity; empty leaves the binaries unsigned. macOS
	// signing is an open question (BUILD-PLAN §12), so it is opt-in.
	Sign string
	// SkipChecks skips the clean-tree check and `make check`. It exists for
	// tests and CI that has already verified the tree.
	SkipChecks bool
	// Go and Make are the tool names; empty means "go" and "make".
	Go, Make string
	// Run runs one command. Required: internal/ may not open a subprocess
	// (BUILD-PLAN §6.1), so the command supplies the runner.
	Run Runner
}

// Release builds jumpr for each darwin architecture, writes one .tar.gz per
// architecture plus SHA256SUMS into the output directory, and reports the sizes
// on out. It never uploads, never tags and never pushes.
//
// Unless SkipChecks is set it first refuses a tree with uncommitted changes and
// then runs `make check`, so a release can only be cut from a tree that passed
// the same gate as a commit.
func Release(ctx context.Context, o ReleaseOptions, out io.Writer) error {
	if out == nil {
		out = io.Discard
	}
	if o.RepoDir == "" {
		return errs.New(errs.Config, "devtools.release", "no repository directory")
	}
	if o.Version == "" {
		return errs.New(errs.Config, "devtools.release", "release needs --version vX.Y.Z")
	}
	if !versionRe.MatchString(o.Version) {
		return errs.New(errs.Config, "devtools.release",
			fmt.Sprintf("version %q is not vMAJOR.MINOR.PATCH (for example v1.2.3)", o.Version))
	}
	if o.Go == "" {
		o.Go = "go"
	}
	if o.Make == "" {
		o.Make = "make"
	}
	if o.Run == nil {
		return errs.New(errs.Config, "devtools.release", "no command runner")
	}
	archs := o.Archs
	if len(archs) == 0 {
		archs = releaseArches
	}
	for _, arch := range archs {
		switch arch {
		case "arm64", "amd64":
		default:
			return errs.New(errs.Config, "devtools.release",
				fmt.Sprintf("unknown architecture %q: jumpr ships darwin/arm64 and darwin/amd64", arch))
		}
	}
	dist := o.OutDir
	if dist == "" {
		dist = filepath.Join(o.RepoDir, "dist")
	}
	if !o.SkipChecks {
		if err := checkTree(ctx, o, out); err != nil {
			return err
		}
	}

	// The payload every archive carries, besides the binary. A missing file is
	// an error: a release without its docs or examples is not shippable.
	payload := []archiveFile{
		{Name: "README.md", Path: filepath.Join(o.RepoDir, "README.md")},
		{Name: "docs/USER-GUIDE.md", Path: filepath.Join(o.RepoDir, "docs", "USER-GUIDE.md")},
		{Name: "docs/KEYS.md", Path: filepath.Join(o.RepoDir, "docs", "KEYS.md")},
	}
	for _, name := range []string{"config", "connections", "policy", "keymap"} {
		payload = append(payload, archiveFile{
			Name: "examples/" + name + ".yaml",
			Path: filepath.Join(o.RepoDir, "internal", "app", "templates", name+".yaml"),
		})
	}
	plugins, err := filepath.Glob(filepath.Join(o.RepoDir, "examples", "plugins", "*.yaml"))
	if err != nil {
		return errs.Wrap(errs.Internal, "devtools.release", err)
	}
	if len(plugins) == 0 {
		return errs.New(errs.Config, "devtools.release", "no example plugins found in examples/plugins")
	}
	sort.Strings(plugins)
	for _, p := range plugins {
		payload = append(payload, archiveFile{Name: "examples/plugins/" + filepath.Base(p), Path: p})
	}
	for _, f := range payload {
		if _, err := os.Stat(f.Path); err != nil {
			return errs.New(errs.Config, "devtools.release",
				fmt.Sprintf("release payload %s is missing: %v", f.Name, err))
		}
	}

	if err := os.MkdirAll(dist, 0o755); err != nil {
		return errs.Wrap(errs.Internal, "devtools.release", err)
	}
	work, err := os.MkdirTemp("", "jumpr-release-")
	if err != nil {
		return errs.Wrap(errs.Internal, "devtools.release", err)
	}
	defer os.RemoveAll(work)

	sizes := make([]int64, 0, len(archs))
	binSizes := make([]int64, 0, len(archs))
	names := make([]string, 0, len(archs))
	for _, arch := range archs {
		binary := filepath.Join(work, "jumpr-"+arch)
		if err := build(ctx, o, arch, binary, out); err != nil {
			return err
		}
		info, err := os.Stat(binary)
		if err != nil {
			return errs.New(errs.Internal, "devtools.release",
				fmt.Sprintf("%s build did not produce %s", o.Go, binary))
		}
		if o.Sign != "" {
			if err := sign(ctx, o, binary, out); err != nil {
				return err
			}
		}
		name := fmt.Sprintf("jumpr-%s-darwin-%s.tar.gz", o.Version, arch)
		path := filepath.Join(dist, name)
		files := append([]archiveFile{{Name: "jumpr", Path: binary, Mode: 0o755}}, payload...)
		if err := writeArchive(path, files); err != nil {
			return err
		}
		archInfo, err := os.Stat(path)
		if err != nil {
			return errs.Wrap(errs.Internal, "devtools.release", err)
		}
		sizes = append(sizes, archInfo.Size())
		binSizes = append(binSizes, info.Size())
		names = append(names, name)
		fmt.Fprintf(out, "%s  %s (binary %s)\n", relPath(o.RepoDir, path), render.Bytes(archInfo.Size()),
			render.Bytes(info.Size()))
	}

	sums := make([]string, 0, len(names))
	for _, name := range names {
		path := filepath.Join(dist, name)
		digest, err := fileSHA256(path)
		if err != nil {
			return err
		}
		sums = append(sums, fmt.Sprintf("%s  %s\n", digest, relPath(o.RepoDir, path)))
	}
	// The sums always cover the archives in name order, so the file is stable.
	sort.Strings(sums)
	sumPath := filepath.Join(dist, "SHA256SUMS")
	if err := os.WriteFile(sumPath, []byte(strings.Join(sums, "")), 0o644); err != nil {
		return errs.Wrap(errs.Internal, "devtools.release", err)
	}
	fmt.Fprintf(out, "%s  %d archive(s), %s in total\n", relPath(o.RepoDir, sumPath), len(names), render.Bytes(sumSizes(sizes)))
	fmt.Fprintf(out, "check them with: shasum -a 256 -c %s\n", relPath(o.RepoDir, sumPath))
	return nil
}

// checkTree refuses a dirty tree and then runs `make check`.
func checkTree(ctx context.Context, o ReleaseOptions, out io.Writer) error {
	var status strings.Builder
	err := o.Run(ctx, Cmd{
		Name: "git", Args: []string{"status", "--porcelain"},
		Dir: o.RepoDir, Stdout: &status, Stderr: out,
	})
	if err != nil {
		return errs.Wrap(errs.Internal, "devtools.release", fmt.Errorf("git status: %w", err))
	}
	if dirty := strings.TrimSpace(status.String()); dirty != "" {
		return errs.New(errs.Config, "devtools.release",
			fmt.Sprintf("refusing to release from a tree with uncommitted changes:\n%s", dirty))
	}
	fmt.Fprintf(out, "make check\n")
	if err := o.Run(ctx, Cmd{Name: o.Make, Args: []string{"check"}, Dir: o.RepoDir, Stdout: out, Stderr: out}); err != nil {
		return errs.Wrap(errs.Internal, "devtools.release", fmt.Errorf("make check failed: %w", err))
	}
	return nil
}

// build cross-compiles one darwin architecture with the flags a release uses:
// no cgo, no build paths, stripped symbols, and the version in main.version.
func build(ctx context.Context, o ReleaseOptions, arch, binary string, out io.Writer) error {
	args := []string{
		"build",
		"-trimpath",
		"-ldflags", "-s -w -X main.version=" + o.Version,
		"-o", binary,
		"./cmd/jumpr",
	}
	env := []string{"CGO_ENABLED=0", "GOOS=darwin", "GOARCH=" + arch}
	if err := o.Run(ctx, Cmd{Name: o.Go, Args: args, Dir: o.RepoDir, Env: env, Stdout: out, Stderr: out}); err != nil {
		return errs.Wrap(errs.Internal, "devtools.release",
			fmt.Errorf("%s build darwin/%s failed: %w", o.Go, arch, err))
	}
	return nil
}

// sign runs codesign on one binary. macOS signing needs an identity the user
// owns, so it only happens when --sign asked for one.
func sign(ctx context.Context, o ReleaseOptions, binary string, out io.Writer) error {
	if err := o.Run(ctx, Cmd{
		Name: "codesign", Args: []string{"--force", "--sign", o.Sign, binary},
		Dir: o.RepoDir, Stdout: out, Stderr: out,
	}); err != nil {
		return errs.Wrap(errs.Internal, "devtools.release",
			fmt.Errorf("codesign with identity %q failed: %w", o.Sign, err))
	}
	return nil
}

// archiveFile is one member of a release archive.
type archiveFile struct {
	Name string      // path inside the archive, forward slashes
	Path string      // source file on disk
	Mode os.FileMode // 0 means a regular 0644 file
}

// writeArchive writes a gzip'd tar of files, in the given order, with fixed
// ownership and timestamps: the same input always produces the same bytes.
func writeArchive(path string, files []archiveFile) error {
	f, err := os.Create(path)
	if err != nil {
		return errs.Wrap(errs.Internal, "devtools.release", err)
	}
	if err := writeArchiveTo(f, files); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return errs.Wrap(errs.Internal, "devtools.release", err)
	}
	return nil
}

// writeArchiveTo packs files into w.
func writeArchiveTo(w io.Writer, files []archiveFile) error {
	gz, err := gzip.NewWriterLevel(w, gzip.BestCompression)
	if err != nil {
		return errs.Wrap(errs.Internal, "devtools.release", err)
	}
	gz.Header.ModTime = time.Time{} // no wall-clock stamp in the gzip header
	gz.Header.OS = 255              // unknown, like the tar format itself

	tw := tar.NewWriter(gz)
	for _, af := range files {
		data, err := os.ReadFile(af.Path)
		if err != nil {
			return errs.Wrap(errs.Internal, "devtools.release",
				fmt.Errorf("read %s: %w", af.Path, err))
		}
		mode := int64(0o644)
		if af.Mode != 0 {
			mode = int64(af.Mode.Perm())
		}
		hdr := &tar.Header{
			Name:     af.Name,
			Mode:     mode,
			Size:     int64(len(data)),
			Typeflag: tar.TypeReg,
			ModTime:  releaseTime,
			Format:   tar.FormatUSTAR,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return errs.Wrap(errs.Internal, "devtools.release", err)
		}
		if _, err := tw.Write(data); err != nil {
			return errs.Wrap(errs.Internal, "devtools.release", err)
		}
	}
	if err := tw.Close(); err != nil {
		return errs.Wrap(errs.Internal, "devtools.release", err)
	}
	if err := gz.Close(); err != nil {
		return errs.Wrap(errs.Internal, "devtools.release", err)
	}
	return nil
}

// fileSHA256 returns the hex digest of a file.
func fileSHA256(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", errs.Wrap(errs.Internal, "devtools.release", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// relPath renders path relative to the repository root when it is inside it, so
// SHA256SUMS names files the way `shasum -c` expects from there.
func relPath(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil || strings.HasPrefix(rel, "..") {
		return path
	}
	return rel
}

// sumSizes adds sizes.
func sumSizes(ns []int64) int64 {
	var total int64
	for _, n := range ns {
		total += n
	}
	return total
}
