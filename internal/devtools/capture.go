// Package devtools holds the repository's development tooling: recording real
// lab output into testdata/ and sanitizing it so it can be committed. Nothing
// here is part of the jumper binary.
//
// Everything takes writers as parameters and returns errors; the command in
// cmd/ owns os.Stdin/Stdout/Stderr and os.Args (BUILD-PLAN §6.1).
package devtools

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"jumper/internal/backend/linux"
	"jumper/internal/errs"
	"jumper/internal/resource"
	"jumper/internal/transport"
)

// labDomain is the suffix every lab host carries (SPEC §22). Captures keep it:
// it is the documentation alias the rest of the repo uses.
const labDomain = ".lab.example.internal"

// captureHosts is the one-host-per-distro table a capture run uses: the lab has
// exactly one machine of each distro the parsers must survive (task 053).
var captureHosts = []CaptureHost{
	{Host: "web-01" + labDomain, Short: "web-01", Distro: "rocky9"},
	{Host: "db-01" + labDomain, Short: "db-01", Distro: "ubuntu2204"},
	{Host: "web-03" + labDomain, Short: "web-03", Distro: "ubuntu2404"},
}

// captureTimeout bounds one remote probe command during a capture.
const captureTimeout = 60 * time.Second

// CaptureHost is one lab machine and the testdata directory its output lands
// in. Host is the inventory ID the transport dials; Short is the machine name
// without the "sr-" prefix, used to build the sanitizer's replacement table.
type CaptureHost struct {
	Host   string
	Short  string
	Distro string
}

// CaptureHosts returns the one-host-per-distro table, as a copy: callers may
// reorder or filter their own run without changing the default.
func CaptureHosts() []CaptureHost {
	return append([]CaptureHost(nil), captureHosts...)
}

// Options configure a Linux capture.
type Options struct {
	// Exec runs the probe commands. Required.
	Exec transport.Executor
	// OutDir is the testdata root; empty means "testdata/linux".
	OutDir string
	// Hosts is the per-distro host table; empty means CaptureHosts().
	Hosts []CaptureHost
	// LocalUser is the developer's local user name, replaced with "sre" in the
	// output. Empty leaves user names alone.
	LocalUser string
	// Timeout bounds one probe command; zero means captureTimeout.
	Timeout time.Duration
}

// CaptureLinux runs every raw command of the Linux probes on one host per
// distro, sanitizes the stdout and writes it to
// <OutDir>/<probe>/<distro>/<name>.txt, printing one line per file written.
//
// A host that fails to run a section is reported and the run continues with the
// remaining sections, so one unreachable machine does not throw away the rest;
// the returned error names every host that failed.
func CaptureLinux(ctx context.Context, o Options, out io.Writer) error {
	if o.Exec == nil {
		return errs.New(errs.Config, "devtools.capture", "no transport executor")
	}
	outDir := o.OutDir
	if outDir == "" {
		outDir = filepath.Join("testdata", "linux")
	}
	hosts := o.Hosts
	if len(hosts) == 0 {
		hosts = CaptureHosts()
	}
	timeout := o.Timeout
	if timeout <= 0 {
		timeout = captureTimeout
	}

	sections := linux.RawCommands()
	keys := make([]string, 0, len(sections))
	for k := range sections {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	hostMap := HostMap(hosts, o.LocalUser)
	var failed []string
	fail := func(host resource.HostID, section string, err error) {
		failed = append(failed, string(host))
		fmt.Fprintf(out, "capture: %s %s: %v\n", host, section, err)
	}

	for _, h := range hosts {
		host := resource.HostID(h.Host)
		for _, k := range keys {
			sec := sections[k]
			res, err := o.Exec.Exec(ctx, host, transport.Command{
				Script:  sec.Cmd,
				Sudo:    sec.Sudo,
				Timeout: timeout,
			})
			if err != nil {
				fail(host, k, err)
				continue
			}
			if res.RC != 0 && len(res.Stdout) == 0 {
				fail(host, k, fmt.Errorf("exit %d: %s", res.RC, strings.TrimSpace(string(res.Stderr))))
				continue
			}
			path := filepath.Join(outDir, sec.Probe, h.Distro, sec.Name+".txt")
			if err := writeCapture(path, Sanitize(res.Stdout, hostMap)); err != nil {
				return err
			}
			fmt.Fprintln(out, path)
		}
	}
	if len(failed) > 0 {
		seen := make(map[string]bool, len(failed))
		uniq := failed[:0]
		for _, name := range failed {
			if !seen[name] {
				seen[name] = true
				uniq = append(uniq, name)
			}
		}
		return errs.New(errs.Remote, "devtools.capture",
			fmt.Sprintf("%d host(s) failed: %s", len(uniq), strings.Join(uniq, ", ")))
	}
	return nil
}

// writeCapture writes one sanitized capture, creating its directory.
func writeCapture(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return errs.Wrap(errs.Internal, "devtools.capture", err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		return errs.Wrap(errs.Internal, "devtools.capture", err)
	}
	return nil
}

// HostMap returns the replacement table Sanitize uses for a capture run: every
// lab machine name, with and without its "sr-" prefix and in its OrbStack
// domains, maps onto the *.lab.example.internal alias, and the local user name
// maps onto "sre".
func HostMap(hosts []CaptureHost, localUser string) map[string]string {
	m := make(map[string]string, len(hosts)*5+1)
	for _, h := range hosts {
		alias := h.Short + labDomain
		names := []string{
			"sr-" + h.Short,
			"sr-" + h.Short + ".orb.local",
			"sr-" + h.Short + ".orbstack.dev",
			"sr-" + h.Short + ".local",
		}
		for _, name := range names {
			m[name] = alias
		}
	}
	if localUser != "" {
		m[localUser] = localUserName
	}
	return m
}
