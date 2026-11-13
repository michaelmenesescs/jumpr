package salt

import (
	"context"
	"encoding/base64"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"jumper/internal/errs"
	"jumper/internal/inventory"
	"jumper/internal/resource"
	"jumper/internal/transport"
	"jumper/internal/transport/transporttest"
)

// exeInv is the executor tests' inventory: one host per minion, mapped through
// the salt.id label so HostMinion needs no salt-key call.
type exeInv struct {
	hosts []inventory.Host
	byID  map[string]inventory.Host
}

func newExeInv(minions ...string) *exeInv {
	inv := &exeInv{byID: make(map[string]inventory.Host, len(minions))}
	for _, m := range minions {
		h := inventory.Host{
			ID:      exeHost(m),
			Aliases: []string{m},
			Labels:  map[string]string{"salt.id": m},
		}
		inv.hosts = append(inv.hosts, h)
		inv.byID[string(h.ID)] = h
	}
	return inv
}

func (i *exeInv) Hosts() []inventory.Host { return append([]inventory.Host(nil), i.hosts...) }

func (i *exeInv) Select(resource.Selector) []inventory.Host { return nil }

func (i *exeInv) Resolve(name string) (inventory.Host, bool) {
	h, ok := i.byID[name]
	return h, ok
}

func (i *exeInv) Refresh(context.Context) map[string]error { return nil }

func (i *exeInv) Age() time.Duration { return 0 }

// exeHost is the inventory host of a minion in these tests.
func exeHost(minion string) resource.HostID {
	return resource.HostID(minion + ".lab.example.internal")
}

// exeMinions returns n minion IDs and the hosts they belong to.
func exeMinions(n int) ([]string, []resource.HostID) {
	minions := make([]string, n)
	hosts := make([]resource.HostID, n)
	for i := 0; i < n; i++ {
		minions[i] = fmt.Sprintf("web-%03d", i)
		hosts[i] = exeHost(minions[i])
	}
	return minions, hosts
}

// newTestExecutor wires an Executor to the scripted master with test-speed job
// options. The caller owns the returned executor; cleanup closes it.
func newTestExecutor(t *testing.T, m *scriptedMaster, inv inventory.Inventory, o ExecOptions) *Executor {
	t.Helper()
	r := NewRunner(m.fake, master, RunnerOptions{})
	b := New(Options{Runner: r, Inventory: inv, Master: master})
	e := NewExecutor(r, b, o)
	e.jobOpts = fastOptions()
	t.Cleanup(e.Close)
	return e
}

// b64Re pulls the base64 payload out of the recorded Salt command line.
var b64Re = regexp.MustCompile(`echo ([A-Za-z0-9+/=]+) \| base64 -d \| sh`)

// submissionLine returns the last async salt line the fake master saw.
func submissionLine(t *testing.T, m *scriptedMaster) string {
	t.Helper()
	var line string
	for _, c := range m.fake.Calls() {
		if strings.Contains(c.Command.Script, "--async") {
			line = c.Command.Script
		}
	}
	if line == "" {
		t.Fatal("no async salt submission was recorded")
	}
	return line
}

// decodePayload decodes the base64 payload of a recorded command line.
func decodePayload(t *testing.T, line string) string {
	t.Helper()
	m := b64Re.FindStringSubmatch(line)
	if m == nil {
		t.Fatalf("no base64 payload in %q", line)
	}
	raw, err := base64.StdEncoding.DecodeString(m[1])
	if err != nil {
		t.Fatalf("payload %q is not standard base64: %v", m[1], err)
	}
	return string(raw)
}

// drain reads a batch channel to its close.
func drain(ch <-chan transport.HostResult) []transport.HostResult {
	var out []transport.HostResult
	for r := range ch {
		out = append(out, r)
	}
	return out
}

// TestExecutorScriptIsBase64: the script travels base64-encoded, behind the
// LC_ALL prefix, in one cmd.run_all submission.
func TestExecutorScriptIsBase64(t *testing.T) {
	const script = "uptime\nuname -a | cut -d' ' -f1"
	minions, hosts := exeMinions(2)
	m := newScriptedMaster([]string{jidA}, func(jid string, _ int) transporttest.Reply {
		return jobReply(t, jid, minions, okMinion("web-000", "up"), okMinion("web-001", "up"))
	})
	e := newTestExecutor(t, m, newExeInv(minions...), ExecOptions{})

	got := drain(e.ExecBatch(context.Background(), hosts, transport.Command{Script: script}))
	if len(got) != 2 {
		t.Fatalf("got %d results, want 2", len(got))
	}
	line := submissionLine(t, m)
	if !strings.Contains(line, "cmd.run_all") {
		t.Errorf("line %q does not call cmd.run_all", line)
	}
	for _, want := range []string{"python_shell=True", "ignore_retcode=True", "timeout=60"} {
		if !strings.Contains(line, want) {
			t.Errorf("line %q is missing %q", line, want)
		}
	}
	if decoded := decodePayload(t, line); decoded != lcAllPrefix+script {
		t.Errorf("decoded script = %q, want %q", decoded, lcAllPrefix+script)
	}
	if n := strings.Count(line, "-L "); n != 1 {
		t.Errorf("line %q has %d list targets, want 1 for a 2-minion batch", line, n)
	}
}

// TestExecutorHostileScriptsRoundTrip: nothing between jumper and the minion
// may change a byte of the script.
func TestExecutorHostileScriptsRoundTrip(t *testing.T) {
	scripts := []string{
		`echo "double" 'single'`,
		`echo $(hostname) && echo $HOME`,
		"line one\nline two\n",
		`echo a=b c=d`,
		`printf 'héllo ☃ 世界\n'`,
		`awk '{print $1}' /etc/hostname`,
	}
	e := &Executor{opt: ExecOptions{}}
	for _, script := range scripts {
		call, cerr := e.call(script, 30*time.Second)
		if cerr != nil {
			t.Fatalf("call(%q): %v", script, cerr)
		}
		line, err := call.CommandLine()
		if err != nil {
			t.Fatalf("CommandLine(%q): %v", script, err)
		}
		if got := decodePayload(t, line); got != lcAllPrefix+script {
			t.Errorf("decoded = %q, want %q", got, lcAllPrefix+script)
		}
		if !strings.Contains(line, "timeout=30") {
			t.Errorf("line %q is missing timeout=30", line)
		}
	}
}

// TestExecutorResultMapping: the cmd.run_all dict becomes RC, stdout and stderr.
func TestExecutorResultMapping(t *testing.T) {
	minions, hosts := exeMinions(1)
	m := newScriptedMaster([]string{jidA}, func(jid string, _ int) transporttest.Reply {
		return jobReply(t, jid, minions, minion{
			ID:      "web-000",
			Return:  `{"pid": 4242, "retcode": 3, "stdout": "out\n", "stderr": "bad\n"}`,
			Retcode: 0,
			Success: true,
		})
	})
	e := newTestExecutor(t, m, newExeInv(minions...), ExecOptions{})

	got := drain(e.ExecBatch(context.Background(), hosts, transport.Command{Script: "false"}))
	if len(got) != 1 {
		t.Fatalf("got %d results, want 1", len(got))
	}
	r := got[0]
	if r.Err != nil {
		t.Fatalf("unexpected error: %v", r.Err)
	}
	if r.Result.RC != 3 {
		t.Errorf("RC = %d, want 3", r.Result.RC)
	}
	if string(r.Result.Stdout) != "out\n" {
		t.Errorf("Stdout = %q, want %q", r.Result.Stdout, "out\n")
	}
	if string(r.Result.Stderr) != "bad\n" {
		t.Errorf("Stderr = %q, want %q", r.Result.Stderr, "bad\n")
	}
	if want := "salt:" + master; r.Result.Via != want {
		t.Errorf("Via = %q, want %q", r.Result.Via, want)
	}
}

// TestExecutorStringReturnIsRemoteError: a string return is a Salt-level
// failure, not output.
func TestExecutorStringReturnIsRemoteError(t *testing.T) {
	minions, hosts := exeMinions(1)
	const msg = "'cmd.run_all' is not available"
	m := newScriptedMaster([]string{jidA}, func(jid string, _ int) transporttest.Reply {
		return jobReply(t, jid, minions, minion{
			ID:     "web-000",
			Return: strconv.Quote(msg),
		})
	})
	e := newTestExecutor(t, m, newExeInv(minions...), ExecOptions{})

	got := drain(e.ExecBatch(context.Background(), hosts, transport.Command{Script: "uptime"}))
	if len(got) != 1 || got[0].Err == nil {
		t.Fatalf("got %+v, want one result with an error", got)
	}
	if k := errs.KindOf(got[0].Err); k != errs.Remote {
		t.Errorf("kind = %q, want remote", k)
	}
	if !strings.Contains(got[0].Err.Error(), "'cmd.run_all' is not available") {
		t.Errorf("error %q does not carry the Salt message", got[0].Err)
	}
}

// TestExecutorNoReturnIsTimeout: a minion Salt marks as not returned is a
// timeout, never a silent drop.
func TestExecutorNoReturnIsTimeout(t *testing.T) {
	minions, hosts := exeMinions(1)
	m := newScriptedMaster([]string{jidA}, func(jid string, _ int) transporttest.Reply {
		return jobReply(t, jid, minions, minion{ID: "web-000", Return: "false"})
	})
	e := newTestExecutor(t, m, newExeInv(minions...), ExecOptions{})

	got := drain(e.ExecBatch(context.Background(), hosts, transport.Command{Script: "uptime"}))
	if len(got) != 1 || got[0].Err == nil {
		t.Fatalf("got %+v, want one result with an error", got)
	}
	if k := errs.KindOf(got[0].Err); k != errs.Timeout {
		t.Errorf("kind = %q, want timeout", k)
	}
	if !strings.Contains(got[0].Err.Error(), "no return from minion") {
		t.Errorf("error %q does not say the minion did not return", got[0].Err)
	}
}

// TestExecutorExecBatchOneJobForManyHosts: 150 hosts are one Salt job.
func TestExecutorExecBatchOneJobForManyHosts(t *testing.T) {
	const n = 150
	minions, hosts := exeMinions(n)
	results := make([]minion, 0, n)
	for _, m := range minions {
		results = append(results, okMinion(m, "up"))
	}
	m := newScriptedMaster([]string{jidA}, func(jid string, _ int) transporttest.Reply {
		return jobReply(t, jid, minions, results...)
	})
	e := newTestExecutor(t, m, newExeInv(minions...), ExecOptions{})

	got := drain(e.ExecBatch(context.Background(), hosts, transport.Command{Script: "uptime"}))
	if len(got) != n {
		t.Fatalf("got %d results, want %d", len(got), n)
	}
	for _, r := range got {
		if r.Err != nil || r.Result.RC != 0 {
			t.Fatalf("host %s: %+v", r.Host, r)
		}
	}
	if subs := m.submitCount(); subs != 1 {
		t.Errorf("submitted %d jobs, want 1", subs)
	}
}

// TestExecutorMicroBatching: concurrent Exec calls with the same command share
// one submission.
func TestExecutorMicroBatching(t *testing.T) {
	const n = 50
	minions, hosts := exeMinions(n)
	results := make([]minion, 0, n)
	for _, m := range minions {
		results = append(results, okMinion(m, "up"))
	}
	m := newScriptedMaster([]string{jidA}, func(jid string, _ int) transporttest.Reply {
		return jobReply(t, jid, minions, results...)
	})
	e := newTestExecutor(t, m, newExeInv(minions...), ExecOptions{BatchWindow: 200 * time.Millisecond})

	start := make(chan struct{})
	var wg sync.WaitGroup
	errsCh := make(chan error, n)
	for _, h := range hosts {
		wg.Add(1)
		go func(host resource.HostID) {
			defer wg.Done()
			<-start
			res, err := e.Exec(context.Background(), host, transport.Command{Script: "uptime"})
			if err != nil {
				errsCh <- fmt.Errorf("%s: %w", host, err)
				return
			}
			if res.RC != 0 {
				errsCh <- fmt.Errorf("%s: rc %d", host, res.RC)
			}
		}(h)
	}
	close(start)
	wg.Wait()
	close(errsCh)
	for err := range errsCh {
		t.Error(err)
	}
	if subs := m.submitCount(); subs != 1 {
		t.Errorf("submitted %d jobs, want 1 for %d concurrent Exec calls", subs, n)
	}
}

// TestExecutorCanceledCaller: a caller whose context ends is told at once, and
// the late result is dropped.
func TestExecutorCanceledCaller(t *testing.T) {
	minions, hosts := exeMinions(1)
	m := newScriptedMaster([]string{jidA}, func(jid string, _ int) transporttest.Reply {
		return jobReply(t, jid, minions, okMinion("web-000", "up"))
	})
	e := newTestExecutor(t, m, newExeInv(minions...), ExecOptions{BatchWindow: time.Second})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := e.Exec(ctx, hosts[0], transport.Command{Script: "sleep 30"})
		done <- err
	}()
	time.Sleep(10 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if k := errs.KindOf(err); k != errs.Canceled {
			t.Fatalf("kind = %q (%v), want canceled", k, err)
		}
	case <-time.After(time.Second):
		t.Fatal("Exec did not return after its context was canceled")
	}
}

// TestExecutorNonMinionHost: a host Salt does not manage is unreachable and
// nothing is submitted.
func TestExecutorNonMinionHost(t *testing.T) {
	minions, _ := exeMinions(1)
	m := newScriptedMaster([]string{jidA}, func(jid string, _ int) transporttest.Reply {
		return jobReply(t, jid, minions, okMinion("web-000", "up"))
	})
	e := newTestExecutor(t, m, newExeInv(minions...), ExecOptions{})

	stranger := resource.HostID("db-999.lab.example.internal")
	_, err := e.Exec(context.Background(), stranger, transport.Command{Script: "uptime"})
	if k := errs.KindOf(err); k != errs.Unreachable {
		t.Fatalf("kind = %q (%v), want unreachable", k, err)
	}
	if !strings.Contains(err.Error(), "not a salt minion") {
		t.Errorf("error %q does not say the host is not a minion", err)
	}

	got := drain(e.ExecBatch(context.Background(), []resource.HostID{stranger}, transport.Command{Script: "uptime"}))
	if len(got) != 1 || errs.KindOf(got[0].Err) != errs.Unreachable {
		t.Fatalf("ExecBatch = %+v, want one unreachable result", got)
	}
	if subs := m.submitCount(); subs != 0 {
		t.Errorf("submitted %d jobs, want 0", subs)
	}
}

// TestExecutorScriptTooLarge: a payload over the limit is refused before
// anything reaches the master.
func TestExecutorScriptTooLarge(t *testing.T) {
	minions, hosts := exeMinions(1)
	m := newScriptedMaster([]string{jidA}, func(jid string, _ int) transporttest.Reply {
		return jobReply(t, jid, minions, okMinion("web-000", "up"))
	})
	e := newTestExecutor(t, m, newExeInv(minions...), ExecOptions{})

	huge := strings.Repeat("x", 80<<10)
	_, err := e.Exec(context.Background(), hosts[0], transport.Command{Script: huge})
	if k := errs.KindOf(err); k != errs.Config {
		t.Fatalf("kind = %q (%v), want config", k, err)
	}
	if !strings.Contains(err.Error(), "script too large for the Salt transport") {
		t.Errorf("error %q does not name the size limit", err)
	}
	if subs := m.submitCount(); subs != 0 {
		t.Errorf("submitted %d jobs, want 0", subs)
	}
}

// TestExecutorStdinRefused: the Salt transport has no stdin channel.
func TestExecutorStdinRefused(t *testing.T) {
	minions, hosts := exeMinions(1)
	m := newScriptedMaster([]string{jidA}, func(jid string, _ int) transporttest.Reply {
		return jobReply(t, jid, minions, okMinion("web-000", "up"))
	})
	e := newTestExecutor(t, m, newExeInv(minions...), ExecOptions{})

	_, err := e.Exec(context.Background(), hosts[0], transport.Command{Script: "cat", Stdin: []byte("data")})
	if k := errs.KindOf(err); k != errs.Config {
		t.Fatalf("kind = %q (%v), want config", k, err)
	}
	if !strings.Contains(err.Error(), "stdin") {
		t.Errorf("error %q does not mention stdin", err)
	}
}
