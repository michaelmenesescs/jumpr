package devtools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"jumpr/internal/backend/salt"
	"jumpr/internal/errs"
	"jumpr/internal/resource"
	"jumpr/internal/shq"
	"jumpr/internal/transport"
)

// Salt capture defaults. A capture is a one-shot developer tool, so the waits
// are generous; tests shorten them with SaltOptions.
const (
	saltCaptureDefaultTimeout = 60 * time.Second
	saltCaptureDefaultPoll    = 500 * time.Millisecond
	saltCaptureDefaultWait    = 45 * time.Second
	saltCaptureNoReturnWait   = 15 * time.Second
	saltCaptureEventWindow    = 10 * time.Second
	saltCaptureSleepJob       = "20"
)

// Lab-only paths and names the capture uses for the drift and no-return files.
const (
	saltCaptureMaster        = "salt.lab.example.internal"
	saltCaptureDriftFile     = "/etc/motd"
	saltCaptureDriftBackup   = "/etc/motd.jumpr-capture"
	saltCaptureStoppedMinion = "web-02"
	saltCaptureDriftMarker   = "# drift added by the capture tool"
)

// saltLabMachines is every machine the Salt lab puts a minion on, master
// included: the sanitizer has to know them all because Salt output prints
// minion ids.
var saltLabMachines = []string{"bastion", "salt", "web-01", "web-02", "web-03", "db-01", "k3s-1", "k3s-2", "chef"}

// SaltOptions configure a Salt capture.
type SaltOptions struct {
	// Exec runs the command lines on the master and on the minions the
	// capture has to touch (the drift and no-return steps). Required.
	Exec transport.Executor
	// Master is the Salt master's host id; empty means
	// salt.lab.example.internal.
	Master resource.HostID
	// OutDir is the testdata directory; empty means "testdata/salt".
	OutDir string
	// LocalUser is the developer's local user name, replaced with "sre".
	LocalUser string
	// Timeout bounds one remote command; zero means 60 s.
	Timeout time.Duration
	// Poll is the job polling interval; zero means 500 ms.
	Poll time.Duration
	// Wait bounds waiting for one job's returns; zero means 45 s.
	Wait time.Duration
	// NoReturnWait is how long the no-return capture waits before reading the
	// job, so the silent minion is visibly missing; zero means 15 s.
	NoReturnWait time.Duration
	// EventWindow is how long the event sample runs; zero means 10 s.
	EventWindow time.Duration
}

// saltSession is the state one capture run needs.
type saltSession struct {
	ctx      context.Context
	ex       transport.Executor
	master   resource.HostID
	runner   *salt.Runner
	timeout  time.Duration
	poll     time.Duration
	wait     time.Duration
	noReturn time.Duration
	hosts    map[string]resource.HostID
	now      func() time.Time
	sleep    func(context.Context, time.Duration) bool

	pingJID string
}

// saltCapture is one file a capture writes.
type saltCapture struct {
	// Name is the file name under the output directory.
	Name string
	// Cmd is the command documented for the file. It may hold "<jid>" for the
	// files read back through jobs.list_job.
	Cmd string
	// Scrub makes the output go through ScrubValues before Sanitize.
	Scrub bool
	// Do produces the bytes and the exact command that produced them.
	Do func(*saltSession) (data []byte, cmd string, err error)
}

// CaptureSalt records the Salt CLI's real output into OutDir so the parsers of
// tasks 068-075 are written against what Salt actually prints.
//
// Every command goes through salt.Runner, i.e. exactly the command line,
// JSON flags and failure rules the jumpr code itself uses; the few captures
// that need plain text (salt --version, --preview-target without JSON, the
// event stream) are built with shq and run on the same master. Output is
// passed through Sanitize, and pillar output additionally through ScrubValues.
//
// A capture that fails is recorded under "Deviations" in the README the run
// writes, and the run continues with the next file so one broken command does
// not throw away the rest. The returned error names every failed capture.
func CaptureSalt(ctx context.Context, o SaltOptions, out io.Writer) error {
	if o.Exec == nil {
		return errs.New(errs.Config, "devtools.capture_salt", "no transport executor")
	}
	outDir := o.OutDir
	if outDir == "" {
		outDir = filepath.Join("testdata", "salt")
	}
	opts := o
	if opts.Master == "" {
		opts.Master = saltCaptureMaster
	}
	if opts.Timeout <= 0 {
		opts.Timeout = saltCaptureDefaultTimeout
	}
	if opts.Poll <= 0 {
		opts.Poll = saltCaptureDefaultPoll
	}
	if opts.Wait <= 0 {
		opts.Wait = saltCaptureDefaultWait
	}
	if opts.NoReturnWait <= 0 {
		opts.NoReturnWait = saltCaptureNoReturnWait
	}
	if opts.EventWindow <= 0 {
		opts.EventWindow = saltCaptureEventWindow
	}

	s := &saltSession{
		ctx:      ctx,
		ex:       opts.Exec,
		master:   opts.Master,
		runner:   salt.NewRunner(opts.Exec, opts.Master, salt.RunnerOptions{Sudo: true, Timeout: opts.Timeout}),
		timeout:  opts.Timeout,
		poll:     opts.Poll,
		wait:     opts.Wait,
		noReturn: opts.NoReturnWait,
		hosts:    saltHostIDs(),
		now:      time.Now,
		sleep:    sleepCtx,
	}

	plan, err := saltPlan(opts.EventWindow)
	if err != nil {
		return err
	}
	hostMap := HostMap(saltCaptureHosts(), opts.LocalUser)

	var (
		lines      []string
		deviations []string
	)
	for _, c := range plan {
		data, cmd, err := c.Do(s)
		if cmd == "" {
			cmd = c.Cmd
		}
		lines = append(lines, c.Name+": "+cmd)
		if err != nil {
			deviations = append(deviations, c.Name+": "+err.Error())
			fmt.Fprintf(out, "capture: %s: %v\n", c.Name, err)
			continue
		}
		if c.Scrub {
			data = ScrubValues(data)
		}
		path := filepath.Join(outDir, c.Name)
		if err := writeCapture(path, Sanitize(data, hostMap)); err != nil {
			return err
		}
		fmt.Fprintln(out, path)
	}

	readme := filepath.Join(outDir, "README.md")
	if err := writeCapture(readme, saltReadme(lines, deviations)); err != nil {
		return err
	}
	fmt.Fprintln(out, readme)

	if len(deviations) > 0 {
		return errs.New(errs.Remote, "devtools.capture_salt",
			fmt.Sprintf("%d capture(s) failed; see %s Deviations", len(deviations), readme))
	}
	return nil
}

// saltPlan builds the ordered capture list. The order matters: the clean state
// run is captured before the drift exists, and every command that reads a job
// back comes after its submission.
func saltPlan(eventWindow time.Duration) ([]saltCapture, error) {
	var plan []saltCapture

	// add records a capture that is one plain Runner call.
	add := func(name string, c salt.Call) error {
		line, err := c.CommandLine()
		if err != nil {
			return err
		}
		plan = append(plan, saltCapture{
			Name: name,
			Cmd:  line,
			Do: func(s *saltSession) ([]byte, string, error) {
				b, err := s.runner.Run(s.ctx, c)
				return b, line, err
			},
		})
		return nil
	}
	// addRaw records a capture that is one plain-text command line.
	addRaw := func(name, line string, timeout time.Duration) {
		plan = append(plan, saltCapture{
			Name: name,
			Cmd:  line,
			Do: func(s *saltSession) ([]byte, string, error) {
				b, err := s.raw(line, timeout)
				return b, line, err
			},
		})
	}

	// Version and keys, minion status and the grains/nodegroups caches.
	addRaw("version.txt", shq.Join("salt", "--version"), 30*time.Second)
	for _, c := range []struct {
		name string
		call salt.Call
	}{
		{"key-list.json", salt.Call{Tool: "salt-key", Flags: []string{"-L"}}},
		{"key-finger.json", salt.Call{Tool: "salt-key", Flags: []string{"-F"}}},
		{"manage-status.json", salt.Call{Tool: "salt-run", Fun: "manage.status"}},
		{"manage-versions.json", salt.Call{Tool: "salt-run", Fun: "manage.versions"}},
		{"cache-grains.json", salt.Call{Tool: "salt-run", Fun: "cache.grains", Kwargs: map[string]string{"tgt": "*"}}},
		{"nodegroups.json", salt.Call{Tool: "salt-run", Fun: "config.get", Args: []string{"nodegroups"}}},
	} {
		if err := add(c.name, c.call); err != nil {
			return nil, err
		}
	}

	// The compound target preview, without and with JSON output.
	preview := salt.Call{Tool: "salt", Target: &salt.Target{Type: "compound", Expr: "G@role:web"}, Flags: []string{"--preview-target"}}
	addRaw("preview-compound.txt", shq.Join("salt", "-C", "G@role:web", "--preview-target"), 30*time.Second)
	if err := add("preview-compound.json", preview); err != nil {
		return nil, err
	}

	// Async submit, then that job's record once it has finished.
	ping := salt.Call{Tool: "salt", Target: &salt.Target{Type: "glob", Expr: "*"}, Fun: "test.ping", Flags: []string{"--async"}}
	pingLine, err := ping.CommandLine()
	if err != nil {
		return nil, err
	}
	plan = append(plan, saltCapture{
		Name: "async-submit.txt",
		Cmd:  pingLine,
		Do: func(s *saltSession) ([]byte, string, error) {
			jid, b, err := s.submit(ping)
			if err != nil {
				return b, pingLine, err
			}
			s.pingJID = jid
			return b, pingLine, nil
		},
	})
	listPingLine, err := listJobCall("<jid>")
	if err != nil {
		return nil, err
	}
	plan = append(plan, saltCapture{
		Name: "list-job-ping.json",
		Cmd:  listPingLine,
		Do: func(s *saltSession) ([]byte, string, error) {
			if s.pingJID == "" {
				return nil, listPingLine, errs.New(errs.Internal, "devtools.capture_salt",
					"no ping job id: async-submit.txt failed")
			}
			s.waitFinished(s.pingJID, s.wait)
			return s.lookup(s.pingJID)
		},
	})

	// A long job captured while it is still active, then the job list.
	sleep := salt.Call{Tool: "salt", Target: &salt.Target{Type: "list", Expr: "web-01,web-02"},
		Fun: "test.sleep", Args: []string{saltCaptureSleepJob}, Flags: []string{"--async"}}
	sleepLine, err := sleep.CommandLine()
	if err != nil {
		return nil, err
	}
	activeLine, err := salt.Call{Tool: "salt-run", Fun: "jobs.active"}.CommandLine()
	if err != nil {
		return nil, err
	}
	plan = append(plan, saltCapture{
		Name: "jobs-active.json",
		Cmd:  activeLine,
		Do: func(s *saltSession) ([]byte, string, error) {
			if _, _, err := s.submit(sleep); err != nil {
				return nil, sleepLine, err
			}
			b, err := s.runner.Run(s.ctx, salt.Call{Tool: "salt-run", Fun: "jobs.active"})
			return b, activeLine, err
		},
	})
	if err := add("jobs-list.json", salt.Call{Tool: "salt-run", Fun: "jobs.list_jobs"}); err != nil {
		return nil, err
	}

	// A command that fails on the minion: cmd.run_all's return carries
	// stdout, stderr and the exit code, captured through list_job.
	runAll := salt.Call{Tool: "salt", Target: &salt.Target{Type: "list", Expr: "web-01,web-02"},
		Fun: "cmd.run_all",
		Kwargs: map[string]string{
			"cmd":          "echo out; echo err >&2; exit 3",
			"python_shell": "True",
		}}
	runAllLine, err := runAll.CommandLine()
	if err != nil {
		return nil, err
	}
	runAllJID, err := listJobCall("<jid>")
	if err != nil {
		return nil, err
	}
	plan = append(plan, saltCapture{
		Name: "cmd-run-all.json",
		Cmd:  runAllJID,
		Do: func(s *saltSession) ([]byte, string, error) {
			return s.submitAndLookup(runAll, []string{"web-01", "web-02"}, s.wait, runAllLine)
		},
	})

	// state.apply test=True on a clean minion, then with one drifted file.
	apply := salt.Call{Tool: "salt", Target: &salt.Target{Type: "list", Expr: "web-01"},
		Fun: "state.apply", Kwargs: map[string]string{"test": "True"}}
	applyLine, err := apply.CommandLine()
	if err != nil {
		return nil, err
	}
	applyJID, err := listJobCall("<jid>")
	if err != nil {
		return nil, err
	}
	plan = append(plan, saltCapture{
		Name: "state-test-clean.json",
		Cmd:  applyJID,
		Do: func(s *saltSession) ([]byte, string, error) {
			return s.submitAndLookup(apply, []string{"web-01"}, s.wait, applyLine)
		},
	})
	plan = append(plan, saltCapture{
		Name: "state-test-changes.json",
		Cmd:  applyJID,
		Do: func(s *saltSession) ([]byte, string, error) {
			return s.withDrift(func() ([]byte, string, error) {
				return s.submitAndLookup(apply, []string{"web-01"}, s.wait, applyLine)
			})
		},
	})

	// A sls that does not exist: Salt's render error.
	nosuch := salt.Call{Tool: "salt", Target: &salt.Target{Type: "list", Expr: "web-01"},
		Fun: "state.sls", Args: []string{"nosuch"}, Kwargs: map[string]string{"test": "True"}}
	if err := add("state-render-error.json", nosuch); err != nil {
		return nil, err
	}

	// The state tree as the master renders it.
	for _, c := range []struct {
		name string
		call salt.Call
	}{
		{"show-top.json", salt.Call{Tool: "salt-run", Fun: "state.show_top"}},
		{"show-sls-nginx.json", salt.Call{Tool: "salt-run", Fun: "state.show_sls", Args: []string{"nginx"}}},
		{"show-highstate.json", salt.Call{Tool: "salt", Target: &salt.Target{Type: "list", Expr: "web-01"}, Fun: "state.show_highstate"}},
	} {
		if err := add(c.name, c.call); err != nil {
			return nil, err
		}
	}

	// Pillar output is scrubbed leaf by leaf before it is written: this
	// capture must never carry the lab's fake secret.
	pillar := salt.Call{Tool: "salt", Target: &salt.Target{Type: "list", Expr: "web-01"}, Fun: "pillar.items"}
	pillarLine, err := pillar.CommandLine()
	if err != nil {
		return nil, err
	}
	plan = append(plan, saltCapture{
		Name:  "pillar-items.json",
		Cmd:   pillarLine,
		Scrub: true,
		Do: func(s *saltSession) ([]byte, string, error) {
			b, err := s.runner.Run(s.ctx, pillar)
			return b, pillarLine, err
		},
	})

	for _, c := range []struct {
		name string
		call salt.Call
	}{
		{"mine.json", salt.Call{Tool: "salt-run", Fun: "mine.get", Args: []string{"*", "network.ip_addrs"}}},
		{"fileserver-envs.json", salt.Call{Tool: "salt-run", Fun: "fileserver.envs"}},
		{"fileserver-file-list.json", salt.Call{Tool: "salt-run", Fun: "fileserver.file_list", Kwargs: map[string]string{"saltenv": "base"}}},
	} {
		if err := add(c.name, c.call); err != nil {
			return nil, err
		}
	}

	// The event bus for one window, with a test.ping on it.
	eventLine := eventCommand(eventWindow)
	plan = append(plan, saltCapture{
		Name: "event-sample.txt",
		Cmd:  eventLine,
		Do: func(s *saltSession) ([]byte, string, error) {
			b, err := s.raw(eventLine, eventWindow+10*time.Second)
			return b, eventLine, err
		},
	})

	// A job that targets a minion whose salt-minion service is stopped: the
	// return stays missing, which is exactly what task 071 must render.
	noReturnLine, err := listJobCall("<jid>")
	if err != nil {
		return nil, err
	}
	plan = append(plan, saltCapture{
		Name: "no-return.json",
		Cmd:  noReturnLine,
		Do: func(s *saltSession) ([]byte, string, error) {
			return s.withStoppedMinion(func() ([]byte, string, error) {
				jid, _, err := s.submit(ping)
				if err != nil {
					return nil, noReturnLine, err
				}
				// Wait the whole window so every responsive minion has
				// returned and the silent one is visibly absent.
				s.sleep(s.ctx, s.noReturn)
				return s.lookup(jid)
			})
		},
	})

	return plan, nil
}

// listJobCall renders the jobs.list_job call for the README and for a file's
// documented command; jid may be a placeholder.
func listJobCall(jid string) (string, error) {
	return salt.Call{Tool: "salt-run", Fun: "jobs.list_job", Args: []string{jid}}.CommandLine()
}

// eventCommand is the plain-text event capture: start a ping one second in,
// then read the event bus for the window.
func eventCommand(window time.Duration) string {
	ping := shq.Join("salt", "--out=json", "--no-color", "*", "test.ping")
	return "(" + shq.Join("sleep", "1") + "; " + ping + " >/dev/null 2>&1) & " +
		shq.Join("timeout", fmt.Sprintf("%d", int(window/time.Second)), "salt-run", "state.event", "pretty=False")
}

// saltCaptureHosts is the sanitizer table for a Salt capture: every lab
// machine, so both `sr-<name>` and its OrbStack domains map onto the
// documentation alias.
func saltCaptureHosts() []CaptureHost {
	hosts := make([]CaptureHost, 0, len(saltLabMachines))
	for _, name := range saltLabMachines {
		hosts = append(hosts, CaptureHost{Host: name + labDomain, Short: name})
	}
	return hosts
}

// saltHostIDs maps a short machine name to the host id the transport dials.
func saltHostIDs() map[string]resource.HostID {
	m := make(map[string]resource.HostID, len(saltLabMachines))
	for _, name := range saltLabMachines {
		m[name] = resource.HostID(name + labDomain)
	}
	return m
}

// raw runs one plain-text command line on the master. Non-zero exit codes with
// output on stdout are data (Salt exits non-zero when a minion fails).
func (s *saltSession) raw(line string, timeout time.Duration) ([]byte, error) {
	res, err := s.ex.Exec(s.ctx, s.master, transport.Command{Script: line, Sudo: true, Timeout: timeout})
	if err != nil {
		return nil, err
	}
	if res.RC != 0 && len(res.Stdout) == 0 {
		return nil, errs.New(errs.Remote, "devtools.capture_salt",
			fmt.Sprintf("exit %d: %s", res.RC, firstNonEmptyLine(string(res.Stderr)))).WithHost(string(s.master))
	}
	return res.Stdout, nil
}

// submit starts one async job and returns its id and the submission output.
func (s *saltSession) submit(c salt.Call) (string, []byte, error) {
	c.Flags = append(append([]string(nil), c.Flags...), "--async")
	out, err := s.runner.Run(s.ctx, c)
	if err != nil {
		return "", out, err
	}
	jid, err := salt.ParseJID(out)
	if err != nil {
		return "", out, err
	}
	return jid, out, nil
}

// waitJob polls jobs.list_job until every expected minion has a result, until
// the job id disappears, or until limit passes. An empty expected set means
// "the first result". The last lookup result is what the caller captures.
func (s *saltSession) waitJob(jid string, expect []string, limit time.Duration) {
	deadline := s.now().Add(limit)
	for {
		info, err := s.runner.Lookup(s.ctx, jid)
		if err == nil && jobDone(info, expect) {
			return
		}
		if !s.now().Before(deadline) {
			return
		}
		if !s.sleep(s.ctx, s.poll) {
			return
		}
	}
}

// waitFinished waits until the master no longer lists jid as active, so the
// record jobs.list_job returns is the finished job and not a snapshot.
func (s *saltSession) waitFinished(jid string, limit time.Duration) {
	deadline := s.now().Add(limit)
	for {
		if !s.sleep(s.ctx, s.poll) {
			return
		}
		active, err := s.runner.Active(s.ctx)
		if err == nil {
			if _, ok := active[jid]; !ok {
				return
			}
		}
		if !s.now().Before(deadline) {
			return
		}
	}
}

// jobDone reports whether the job has all the results this capture wants.
func jobDone(info salt.JobInfo, expect []string) bool {
	if len(expect) == 0 {
		return len(info.Result) > 0
	}
	for _, m := range expect {
		if _, ok := info.Result[m]; !ok {
			return false
		}
	}
	return true
}

// lookup captures jobs.list_job for jid.
func (s *saltSession) lookup(jid string) ([]byte, string, error) {
	c := salt.Call{Tool: "salt-run", Fun: "jobs.list_job", Args: []string{jid}}
	line, err := c.CommandLine()
	if err != nil {
		return nil, "", err
	}
	b, err := s.runner.Run(s.ctx, c)
	return b, line, err
}

// submitAndLookup runs one async job, waits for the expected returns and
// captures jobs.list_job. submitLine documents what was submitted when the
// submit itself fails.
func (s *saltSession) submitAndLookup(c salt.Call, expect []string, limit time.Duration, submitLine string) ([]byte, string, error) {
	jid, _, err := s.submit(c)
	if err != nil {
		return nil, submitLine, err
	}
	s.waitJob(jid, expect, limit)
	return s.lookup(jid)
}

// withDrift adds a comment line to the Salt-managed file on web-01, runs fn,
// and restores the file afterwards whether fn succeeded or not.
func (s *saltSession) withDrift(fn func() ([]byte, string, error)) (data []byte, cmd string, err error) {
	if derr := s.setDrift(); derr != nil {
		return nil, "", derr
	}
	data, cmd, err = fn()
	if uerr := s.clearDrift(); uerr != nil {
		if err == nil {
			err = uerr
		} else {
			err = fmt.Errorf("%v; restoring %s also failed: %w", err, saltCaptureDriftFile, uerr)
		}
	}
	return data, cmd, err
}

// setDrift backs up the managed file and appends a comment line to it.
func (s *saltSession) setDrift() error {
	host, ok := s.hosts["web-01"]
	if !ok {
		return errs.New(errs.Config, "devtools.capture_salt", "web-01 is not a lab machine")
	}
	line := shq.Join("cp", "-p", saltCaptureDriftFile, saltCaptureDriftBackup) +
		" && " + shq.Join("printf", "%s\\n", saltCaptureDriftMarker) + " >> " + shq.Quote(saltCaptureDriftFile)
	return s.shell(s.ctx, host, line)
}

// clearDrift restores the managed file from its backup.
func (s *saltSession) clearDrift() error {
	host, ok := s.hosts["web-01"]
	if !ok {
		return errs.New(errs.Config, "devtools.capture_salt", "web-01 is not a lab machine")
	}
	line := "if [ -f " + shq.Quote(saltCaptureDriftBackup) + " ]; then " +
		shq.Join("mv", saltCaptureDriftBackup, saltCaptureDriftFile) + "; fi"
	ctx, cancel := s.restoreCtx()
	defer cancel()
	return s.shell(ctx, host, line)
}

// withStoppedMinion stops salt-minion on one lab machine, runs fn, and starts
// it again afterwards whether fn succeeded or not.
func (s *saltSession) withStoppedMinion(fn func() ([]byte, string, error)) (data []byte, cmd string, err error) {
	if serr := s.minionService("stop"); serr != nil {
		return nil, "", serr
	}
	data, cmd, err = fn()
	if uerr := s.minionService("start"); uerr != nil {
		if err == nil {
			err = uerr
		} else {
			err = fmt.Errorf("%v; restarting salt-minion on %s also failed: %w", err, saltCaptureStoppedMinion, uerr)
		}
	}
	return data, cmd, err
}

// minionService stops or starts salt-minion on the machine the no-return
// capture silences. Starting it back up uses a context that survives a
// canceled run, so the lab is restored even when the capture is interrupted.
func (s *saltSession) minionService(verb string) error {
	host, ok := s.hosts[saltCaptureStoppedMinion]
	if !ok {
		return errs.New(errs.Config, "devtools.capture_salt",
			fmt.Sprintf("%s is not a lab machine", saltCaptureStoppedMinion))
	}
	ctx := s.ctx
	if verb == "start" {
		var cancel context.CancelFunc
		ctx, cancel = s.restoreCtx()
		defer cancel()
	}
	if err := s.shell(ctx, host, shq.Join("systemctl", verb, "salt-minion")); err != nil {
		return err
	}
	// Give the master a moment to notice the change before the job is
	// submitted (or, on start, before the next capture runs).
	s.sleep(s.ctx, s.poll)
	return nil
}

// shell runs one short already-quoted line as root on a lab machine.
func (s *saltSession) shell(ctx context.Context, host resource.HostID, line string) error {
	res, err := s.ex.Exec(ctx, host, transport.Command{Script: line, Sudo: true, Timeout: s.timeout})
	if err != nil {
		return err
	}
	if res.RC != 0 {
		return errs.New(errs.Remote, "devtools.capture_salt",
			fmt.Sprintf("exit %d: %s", res.RC, firstNonEmptyLine(string(res.Stderr)))).WithHost(string(host))
	}
	return nil
}

// saltReadme renders the capture's README: one line per file with the command
// that produced it, then the deviations the run found.
func saltReadme(lines, deviations []string) []byte {
	var b strings.Builder
	b.WriteString("# Salt golden files\n\n")
	b.WriteString("Captured from the lab's Salt master (`" + saltCaptureMaster + "`) by\n")
	b.WriteString("`go run ./cmd/sitrep-lab capture salt --config-dir lab/jumpr`.\n\n")
	b.WriteString("Every file is the Salt CLI's own output, passed through `devtools.Sanitize`.\n")
	b.WriteString("`pillar-items.json` is additionally passed through `devtools.ScrubValues`, so\n")
	b.WriteString("every leaf value is `REDACTED-<type>` and no pillar value is ever recorded.\n\n")
	b.WriteString("One line per file: the exact command that produced it.\n\n")
	for _, line := range lines {
		b.WriteString(line + "\n")
	}
	b.WriteString("\n## Deviations\n\n")
	if len(deviations) == 0 {
		b.WriteString("None.\n")
		return []byte(b.String())
	}
	for _, d := range deviations {
		b.WriteString("- " + d + "\n")
	}
	return []byte(b.String())
}

// ScrubValues replaces every leaf value in a JSON document with
// REDACTED-<type>, keeping the document's keys and shape. A leaf that is
// already scrubbed is left alone, so the function is idempotent. Input that is
// not JSON is returned unchanged: the capture must record whatever Salt
// printed, and the parsers skip what they do not understand.
func ScrubValues(b []byte) []byte {
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return b
	}
	out, err := json.Marshal(scrubValue(v))
	if err != nil {
		return b
	}
	return append(out, '\n')
}

// scrubValue walks one decoded JSON value.
func scrubValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		m := make(map[string]any, len(t))
		for k, v := range t {
			m[k] = scrubValue(v)
		}
		return m
	case []any:
		a := make([]any, len(t))
		for i, v := range t {
			a[i] = scrubValue(v)
		}
		return a
	case string:
		if strings.HasPrefix(t, "REDACTED-") {
			return t
		}
		return "REDACTED-string"
	case float64, json.Number:
		return "REDACTED-number"
	case bool:
		return "REDACTED-bool"
	case nil:
		return "REDACTED-null"
	}
	return "REDACTED-unknown"
}

// firstNonEmptyLine returns the first non-empty line of s, trimmed.
func firstNonEmptyLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return ""
}

// restoreCtx is the context the "put the lab back" steps use: it keeps the
// run's cancellation out of the way and carries its own timeout, so a restore
// still happens when the capture is interrupted.
func (s *saltSession) restoreCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(s.ctx), s.timeout)
}

// sleepCtx waits d, returning false when ctx ended first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
