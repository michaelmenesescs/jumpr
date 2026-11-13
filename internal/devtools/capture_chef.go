package devtools

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"jumper/internal/backend/chef"
	"jumper/internal/errs"
	"jumper/internal/resource"
	"jumper/internal/shq"
	"jumper/internal/transport"
)

// Chef capture defaults. A capture is a one-shot developer tool, so the timeout
// is generous.
const (
	chefCaptureDefaultTimeout = 60 * time.Second
	// chefNodeDetail and chefNodeFailing are the two nodes the detail capture
	// records: a healthy one and the lab's deliberately failing db-01.
	chefNodeDetail  = "web-01"
	chefNodeFailing = "db-01"
	// chefRoleSample and chefEnvSample are the named objects captured whole.
	chefRoleSample = "web"
	chefEnvSample  = "production"
	// chefSearchNode is the lab-wide node query used for the search captures.
	chefSearchNode = "*:*"
)

// chefCaptureWorkstation is the lab's Chef workstation: it runs chef-zero and
// has knife configured for user sre (lab/README.md).
const chefCaptureWorkstation = "chef" + labDomain

// ChefOptions configure a Chef capture.
type ChefOptions struct {
	// Exec runs the knife command lines on the workstation. Required.
	Exec transport.Executor
	// Workstation is the Chef workstation's host id; empty means
	// chef.lab.example.internal.
	Workstation resource.HostID
	// OutDir is the testdata directory; empty means "testdata/chef".
	OutDir string
	// LocalUser is the developer's local user name, replaced with "sre".
	LocalUser string
	// Timeout bounds one knife call; zero means 60 s.
	Timeout time.Duration
}

// chefCapture is one file a capture writes.
type chefCapture struct {
	// Name is the file name under the output directory.
	Name string
	// Cmd is the command documented for the file.
	Cmd string
	// Do produces the bytes.
	Do func(context.Context, *chef.Runner) ([]byte, error)
}

// CaptureChef records knife's real output into OutDir so the parsers of tasks
// 113-114 are written against what knife actually prints (SPEC §8.7).
//
// Every JSON command goes through chef.Runner, i.e. exactly the command line,
// JSON flags and failure rules jumper itself uses. The one plain-text capture
// (knife --version) is built with shq and run on the same workstation. Output
// passes through Sanitize.
//
// A capture that fails is recorded under "Deviations" in the README the run
// writes, and the run continues with the next file. The returned error names
// every failed capture.
func CaptureChef(ctx context.Context, o ChefOptions, out io.Writer) error {
	if o.Exec == nil {
		return errs.New(errs.Config, "devtools.capture_chef", "no transport executor")
	}
	ws := o.Workstation
	if ws == "" {
		ws = chefCaptureWorkstation
	}
	outDir := o.OutDir
	if outDir == "" {
		outDir = filepath.Join("testdata", "chef")
	}
	timeout := o.Timeout
	if timeout <= 0 {
		timeout = chefCaptureDefaultTimeout
	}
	runner := chef.NewRunner(o.Exec, ws, chef.RunnerOptions{Timeout: timeout})

	var (
		lines      []string
		deviations []string
	)
	for _, c := range chefPlan() {
		data, err := c.Do(ctx, runner)
		lines = append(lines, c.Name+": "+c.Cmd)
		if err != nil {
			deviations = append(deviations, c.Name+": "+err.Error())
			fmt.Fprintf(out, "capture: %s: %v\n", c.Name, err)
			continue
		}
		path := filepath.Join(outDir, c.Name)
		if err := writeCapture(path, Sanitize(data, HostMap(saltCaptureHosts(), o.LocalUser))); err != nil {
			return err
		}
		fmt.Fprintln(out, path)
	}

	readme := filepath.Join(outDir, "README.md")
	if err := writeCapture(readme, chefReadme(lines, deviations)); err != nil {
		return err
	}
	fmt.Fprintln(out, readme)

	if len(deviations) > 0 {
		return errs.New(errs.Remote, "devtools.capture_chef",
			fmt.Sprintf("%d capture(s) failed; see %s Deviations", len(deviations), readme))
	}
	return nil
}

// chefPlan builds the ordered capture list.
func chefPlan() []chefCapture {
	var plan []chefCapture

	// add records a capture that is one chef.Runner call with JSON output.
	add := func(name string, args ...string) {
		plan = append(plan, chefCapture{
			Name: name,
			Cmd:  chefLine(name, args...),
			Do: func(ctx context.Context, r *chef.Runner) ([]byte, error) {
				return r.Run(ctx, args...)
			},
		})
	}
	// addRaw records a capture that writes the output of one plain-text line.
	addRaw := func(name, line string) {
		plan = append(plan, chefCapture{
			Name: name,
			Cmd:  line,
			Do: func(ctx context.Context, r *chef.Runner) ([]byte, error) {
				return rawOn(ctx, r, line)
			},
		})
	}

	addRaw("version.txt", shq.Join("knife", "--version"))
	add("status.json", "status")
	add("node-list.json", "node", "list")
	add("role-list.json", "role", "list")
	add("env-list.json", "environment", "list")
	add("node-show.json", "node", "show", chefNodeDetail, "-l")
	add("node-show-db-01.json", "node", "show", chefNodeFailing, "-l")
	add("role-show-web.json", "role", "show", chefRoleSample)
	add("environment-show-production.json", "environment", "show", chefEnvSample)
	add("search.json", "search", "node", chefSearchNode, "-a", "platform")
	add("search-run-list.json", "search", "node", chefSearchNode, "-a", "run_list")
	return plan
}

// chefLine documents the command line a capture runs. The arguments are
// constants of this file, so a failure here would be a build-time bug; the
// fallback keeps the README readable instead of panicking.
func chefLine(name string, args ...string) string {
	line, err := chef.NewRunner(nil, "", chef.RunnerOptions{}).CommandLine(args...)
	if err != nil {
		return name + ": " + err.Error()
	}
	return line
}

// rawOn runs one plain-text line on the runner's workstation, using the same
// executor and timeout the JSON captures use.
func rawOn(ctx context.Context, r *chef.Runner, line string) ([]byte, error) {
	return r.RunShell(ctx, line)
}

// chefReadme renders the capture README: one line per file with its command,
// then the deviations, if any.
func chefReadme(lines, deviations []string) []byte {
	var b strings.Builder
	b.WriteString("# Chef capture files\n\n")
	b.WriteString("Captured from the lab's Chef workstation with\n")
	b.WriteString("`go run ./cmd/sitrep-lab capture chef --config-dir lab/jumper`.\n\n")
	b.WriteString("Every file is knife's own output on the lab's chef-zero, passed through\n")
	b.WriteString("`devtools.Sanitize`. One line per file: the exact command that produced it.\n\n")
	b.WriteString(strings.Join(lines, "\n"))
	b.WriteString("\n")
	if len(deviations) > 0 {
		b.WriteString("\n## Deviations\n\n")
		for _, d := range deviations {
			b.WriteString("- " + d + "\n")
		}
	}
	return []byte(b.String())
}
