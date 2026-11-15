package tui

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"jumpr/internal/backend/linux"
	"jumpr/internal/errs"
	"jumpr/internal/redact"
	"jumpr/internal/render"
	"jumpr/internal/resource"
	"jumpr/internal/shq"
	"jumpr/internal/transport"
	"jumpr/internal/tui/keymap"
	"jumpr/internal/tui/theme"
)

const (
	// fileOp is the Op of every error built here.
	fileOp = "tui.file"
	// fileReadLimit caps one file read at 1 MiB, exactly `head -c 1048576`.
	fileReadLimit = 1 << 20
	// fileTimeout bounds one file read or checksum fan-out.
	fileTimeout = 60 * time.Second
	// fileTimeLayout is the header's mtime form.
	fileTimeLayout = "2006-01-02 15:04:05 -0700"
)

// sensitivePatterns are the built-in path patterns whose content needs a y/N
// confirmation and passes through internal/redact. They are matched against the
// cleaned path and its base name.
var sensitivePatterns = []string{
	"/etc/shadow",
	"*.key",
	"*.pem",
	"id_*",
	"*/.ssh/*",
	"*secret*",
	"*token*",
}

// readOnlyBackend is the slice of the Linux backend the read-only viewer and the
// pkg probe need: one already-quoted command, fanned out by the backend itself.
// The command text is built here with internal/shq; the backend supplies the
// executor, the concurrency limit and the timeout.
type readOnlyBackend interface {
	ExecReadOnly(ctx context.Context, hosts []resource.HostID, cmd transport.Command) <-chan transport.HostResult
}

// readOnlyFor finds the backend that can run read-only commands: the Linux
// backend in production, a registered stub in tests.
func readOnlyFor(env *Env) (readOnlyBackend, error) {
	if env == nil || env.Registry == nil {
		return nil, errs.New(errs.Config, fileOp, "no backends configured")
	}
	if b, ok := env.Registry.Get("linux"); ok {
		if r, ok := b.(readOnlyBackend); ok {
			return r, nil
		}
	}
	for _, b := range env.Registry.Backends() {
		if r, ok := b.(readOnlyBackend); ok {
			return r, nil
		}
	}
	return nil, errs.New(errs.NotAvailable, fileOp, "no backend can run read-only commands")
}

// RegisterFileCommands registers the read-only file viewer and the pkg probe.
func RegisterFileCommands(c *Commands) error {
	if c == nil {
		return nil
	}
	for _, cmd := range fileCommands() {
		if err := c.Register(cmd); err != nil {
			return err
		}
	}
	return nil
}

// fileCommands is the command table: `:file` and `:probe pkg`.
func fileCommands() []Command {
	return []Command{
		{
			Name:     "file",
			Aliases:  []string{"cat"},
			Usage:    `:file <path> [--host h] [--tail N]`,
			Help:     "read-only file viewer (sensitive paths ask first)",
			Run:      runFileCommand,
			Complete: func(*Env, string) []string { return nil },
		},
		{
			Name:     "probe",
			Usage:    ":probe pkg <name> [--host h]",
			Help:     "print one package's version across the fleet",
			Run:      runProbeCommand,
			Complete: func(*Env, string) []string { return []string{"pkg"} },
		},
	}
}

// fileArgs is one parsed `:file` command line.
type fileArgs struct {
	Path      string
	Hosts     []resource.HostID
	Tail      int
	Sensitive bool
}

// runFileCommand plans a read-only file view.
func runFileCommand(env *Env, args string) tea.Cmd {
	fa, err := parseFileArgs(args)
	if err != nil {
		return Flash(FlashError, "%v", err)
	}
	return fileCommandFor(env, fa, false)
}

// fileCommandFor answers the sensitive-path question first, then plans either
// the single-host viewer or the multi-host checksum fan-out.
func fileCommandFor(env *Env, fa fileArgs, confirmed bool) tea.Cmd {
	if pat, sensitive := sensitivePath(fa.Path); sensitive && !confirmed {
		fa.Sensitive = true
		next := fileCommandFor(env, fa, true)
		text := fa.Path + " matches the sensitive path pattern " + strconv.Quote(pat) +
			"; its content is redacted.\nread it anyway?"
		var t *theme.Theme
		if env != nil {
			t = env.Theme
		}
		return func() tea.Msg {
			return OpenDialogMsg{Dialog: NewConfirmDialog(t, "sensitive path", text, next)}
		}
	}
	if len(fa.Hosts) > 1 {
		return checksumCmd(env, fa)
	}
	return readFileCmd(env, fa)
}

// parseFileArgs parses `<path> [--host h] [--tail N]`. A path with spaces must
// be quoted, or it is rejected rather than guessed at.
func parseFileArgs(args string) (fileArgs, error) {
	toks := splitCommandArgs(args)
	var (
		fa        fileArgs
		pathParts []string
	)
	for i := 0; i < len(toks); i++ {
		switch toks[i] {
		case "--host", "-H":
			if i+1 >= len(toks) {
				return fileArgs{}, errs.New(errs.Config, fileOp, "--host needs a comma-separated host list")
			}
			i++
			for _, h := range strings.Split(toks[i], ",") {
				if h = strings.TrimSpace(h); h != "" {
					fa.Hosts = append(fa.Hosts, resource.NormalizeHostID(h))
				}
			}
		case "--tail", "-n":
			if i+1 >= len(toks) {
				return fileArgs{}, errs.New(errs.Config, fileOp, "--tail needs a line count")
			}
			i++
			n, err := strconv.Atoi(toks[i])
			if err != nil || n <= 0 {
				return fileArgs{}, errs.New(errs.Config, fileOp,
					fmt.Sprintf("--tail %q is not a positive line count", toks[i]))
			}
			fa.Tail = n
		default:
			pathParts = append(pathParts, toks[i])
		}
	}
	switch len(pathParts) {
	case 0:
		return fileArgs{}, errs.New(errs.Config, fileOp, "usage: :file <path> [--host h] [--tail N]")
	case 1:
		fa.Path = pathParts[0]
	default:
		return fileArgs{}, errs.New(errs.Config, fileOp, "quote a path with spaces: :file \"<path>\"")
	}
	if err := validFilePath(fa.Path); err != nil {
		return fileArgs{}, err
	}
	fa.Hosts = dedupeIDs(fa.Hosts)
	if len(fa.Hosts) == 0 {
		return fileArgs{}, errs.New(errs.Config, fileOp, "no host: pass --host <name>")
	}
	return fa, nil
}

// validFilePath rejects a path that could not survive one shell argument or one
// text view line.
func validFilePath(p string) error {
	if strings.TrimSpace(p) == "" {
		return errs.New(errs.Config, fileOp, "path is empty")
	}
	if strings.ContainsAny(p, "\n\r\x00") {
		return errs.New(errs.Config, fileOp, "path contains a control character")
	}
	return nil
}

// dedupeIDs normalizes and de-duplicates hosts, keeping the first occurrence.
func dedupeIDs(in []resource.HostID) []resource.HostID {
	seen := make(map[resource.HostID]bool, len(in))
	out := make([]resource.HostID, 0, len(in))
	for _, h := range in {
		if h == "" || seen[h] {
			continue
		}
		seen[h] = true
		out = append(out, h)
	}
	return out
}

// sensitivePath reports the built-in pattern p matches, if any. Patterns are
// checked against the cleaned path and its base name, so `*.pem` catches
// /etc/ssl/private/site.pem, `id_*` catches ~/.ssh/id_ed25519 and `*/.ssh/*`
// catches anything under an .ssh directory at any depth.
func sensitivePath(p string) (string, bool) {
	clean := path.Clean(p)
	base := strings.ToLower(path.Base(clean))
	lower := strings.ToLower(clean)
	for _, pat := range sensitivePatterns {
		var hit bool
		switch pat {
		case "*/.ssh/*":
			hit = strings.Contains(lower, "/.ssh/")
		case "*.key", "*.pem":
			hit = strings.HasSuffix(base, pat[1:])
		case "id_*":
			hit = strings.HasPrefix(base, "id_")
		case "*secret*", "*token*":
			hit = strings.Contains(lower, strings.Trim(pat, "*"))
		default: // a literal path such as /etc/shadow
			hit = lower == pat || strings.HasSuffix(lower, pat)
		}
		if hit {
			return pat, true
		}
	}
	return "", false
}

// fileStatScript is `stat -c '%s %U:%G %a %y' <path>`, the header's source. The
// path is quoted with shq, never interpolated.
func fileStatScript(p string) string {
	return "stat -c '%s %U:%G %a %y' " + shq.Quote(p)
}

// fileBodyScript is the read itself: the last N lines with --tail, otherwise the
// first MiB. Neither form writes anything.
func fileBodyScript(p string, tail int) string {
	if tail > 0 {
		return "tail -n " + strconv.Itoa(tail) + " " + shq.Quote(p)
	}
	return "head -c " + strconv.Itoa(fileReadLimit) + " " + shq.Quote(p)
}

// checksumScript is `sha256sum <path>`, the multi-host drift check.
func checksumScript(p string) string {
	return "sha256sum " + shq.Quote(p)
}

// remoteErr turns a non-zero exit code into an error the flash line can show.
func remoteErr(host resource.HostID, sudo bool, res transport.ExecResult) error {
	if res.RC == 0 {
		return nil
	}
	if e := shq.Classify(res.RC, string(res.Stderr), sudo); e != nil {
		if typed, ok := e.(*errs.Error); ok {
			return typed.WithHost(string(host))
		}
		return errs.Wrap(errs.Remote, fileOp, e).WithHost(string(host))
	}
	msg := fmt.Sprintf("exit %d", res.RC)
	if tail := strings.TrimSpace(string(res.Stderr)); tail != "" {
		msg += ": " + firstLine([]byte(tail))
	}
	return errs.New(errs.Remote, fileOp, msg).WithHost(string(host))
}

// runOne runs one command on one host and returns its result.
func runOne(ctx context.Context, r readOnlyBackend, host resource.HostID, cmd transport.Command) (transport.ExecResult, error) {
	for hr := range r.ExecReadOnly(ctx, []resource.HostID{host}, cmd) {
		if hr.Err != nil {
			return transport.ExecResult{}, hr.Err
		}
		if err := remoteErr(hr.Host, cmd.Sudo, hr.Result); err != nil {
			return transport.ExecResult{}, err
		}
		return hr.Result, nil
	}
	return transport.ExecResult{}, errs.New(errs.Remote, fileOp, "no result").WithHost(string(host))
}

// readFileCmd reads one file and pushes the viewer. It runs entirely inside a
// tea.Cmd: no I/O happens in Update or Render.
func readFileCmd(env *Env, fa fileArgs) tea.Cmd {
	return func() tea.Msg {
		runner, err := readOnlyFor(env)
		if err != nil {
			return FlashMsg{Level: FlashError, Text: err.Error()}
		}
		ctx, cancel := context.WithTimeout(context.Background(), fileTimeout)
		defer cancel()
		host := fa.Hosts[0]

		statRes, err := runOne(ctx, runner, host, transport.Command{Script: fileStatScript(fa.Path), Sudo: true})
		if err != nil {
			return FlashMsg{Level: FlashError, Text: err.Error()}
		}
		stat, statErr := linux.ParseFileStat(string(statRes.Stdout))

		bodyRes, err := runOne(ctx, runner, host, transport.Command{Script: fileBodyScript(fa.Path, fa.Tail), Sudo: true})
		if err != nil {
			return FlashMsg{Level: FlashError, Text: err.Error()}
		}
		body := bodyRes.Stdout
		if linux.IsBinaryData(body) {
			return FlashMsg{Level: FlashWarn, Text: "refusing to display binary file " + fa.Path}
		}

		lines := []string{fa.Path}
		if statErr != nil {
			lines = append(lines, "stat: "+statErr.Error())
		} else {
			head := "size " + render.Bytes(stat.Size) +
				"  owner " + stat.Owner + ":" + stat.Group +
				"  mode " + stat.Mode
			if !stat.MTime.IsZero() {
				head += "  mtime " + stat.MTime.Format(fileTimeLayout)
			}
			lines = append(lines, head)
			if fa.Tail <= 0 && stat.Size > fileReadLimit {
				lines = append(lines, "truncated at 1 MiB")
			}
		}
		lines = append(lines, "")

		content := string(body)
		if fa.Sensitive {
			// A sensitive path's content is redacted before it is ever shown.
			if red, rerr := redact.New(nil); rerr == nil {
				content = red.String(content)
			}
		}
		content = strings.TrimRight(content, "\n")
		if content != "" {
			lines = append(lines, strings.Split(content, "\n")...)
		}
		return PushViewMsg{View: newTextPage(env, "file "+fa.Path, lines)}
	}
}

// checksumCmd is the multi-host form: `sha256sum <path>` on every host, grouped
// by digest, which is the config drift check.
func checksumCmd(env *Env, fa fileArgs) tea.Cmd {
	return func() tea.Msg {
		runner, err := readOnlyFor(env)
		if err != nil {
			return FlashMsg{Level: FlashError, Text: err.Error()}
		}
		ctx, cancel := context.WithTimeout(context.Background(), fileTimeout)
		defer cancel()

		groups := make(map[string][]string)
		var failures []string
		cmd := transport.Command{Script: checksumScript(fa.Path), Sudo: true}
		for hr := range runner.ExecReadOnly(ctx, fa.Hosts, cmd) {
			host := string(hr.Host)
			if hr.Err != nil {
				failures = append(failures, host+"  error  "+hr.Err.Error())
				continue
			}
			if e := remoteErr(hr.Host, cmd.Sudo, hr.Result); e != nil {
				failures = append(failures, host+"  error  "+e.Error())
				continue
			}
			sum, _, ok := linux.ParseChecksumLine(firstLine(hr.Result.Stdout))
			if !ok {
				failures = append(failures, host+"  error  unparsable sha256sum output")
				continue
			}
			groups[sum] = append(groups[sum], host)
		}

		lines := []string{
			"sha256sum " + fa.Path + "  ·  " + strconv.Itoa(len(fa.Hosts)) + " hosts  ·  " +
				strconv.Itoa(len(groups)) + " sums",
			"",
		}
		for _, sum := range sortedByHostCount(groups) {
			hosts := append([]string(nil), groups[sum]...)
			sort.Strings(hosts)
			lines = append(lines, sum+"  "+strconv.Itoa(len(hosts))+" hosts  "+strings.Join(hosts, ", "))
		}
		sort.Strings(failures)
		lines = append(lines, failures...)
		return PushViewMsg{View: newTextPage(env, "sha256sum "+fa.Path, lines)}
	}
}

// sortedByHostCount orders digests biggest group first, then by digest.
func sortedByHostCount(groups map[string][]string) []string {
	sums := make([]string, 0, len(groups))
	for sum := range groups {
		sums = append(sums, sum)
	}
	sort.SliceStable(sums, func(i, j int) bool {
		if len(groups[sums[i]]) != len(groups[sums[j]]) {
			return len(groups[sums[i]]) > len(groups[sums[j]])
		}
		return sums[i] < sums[j]
	})
	return sums
}

// firstLine returns the first line of out, trimmed.
func firstLine(out []byte) string {
	s := string(out)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// runProbeCommand runs one parameterized catalog probe. Only `pkg` exists here;
// `:probe` with no argument says how to use it.
func runProbeCommand(env *Env, args string) tea.Cmd {
	toks := splitCommandArgs(args)
	if len(toks) == 0 {
		return Flash(FlashError, "usage: :probe pkg <name> [--host h]")
	}
	if toks[0] != "pkg" {
		return Flash(FlashError, "unknown probe %q; only pkg is available", toks[0])
	}
	var (
		name  string
		hosts []resource.HostID
	)
	for i := 1; i < len(toks); i++ {
		switch toks[i] {
		case "--host", "-H":
			if i+1 >= len(toks) {
				return Flash(FlashError, "--host needs a comma-separated host list")
			}
			i++
			for _, h := range strings.Split(toks[i], ",") {
				if h = strings.TrimSpace(h); h != "" {
					hosts = append(hosts, resource.NormalizeHostID(h))
				}
			}
		default:
			if name != "" {
				return Flash(FlashError, "usage: :probe pkg <name> [--host h]")
			}
			name = toks[i]
		}
	}
	if name == "" {
		return Flash(FlashError, "usage: :probe pkg <name> [--host h]")
	}
	if strings.HasPrefix(name, "-") || strings.ContainsAny(name, " \t\n\r\x00") {
		return Flash(FlashError, "bad package name %q", name)
	}
	hosts = dedupeIDs(hosts)
	if len(hosts) == 0 {
		return Flash(FlashError, "no host: pass --host <name>")
	}
	return packageProbeCmd(env, name, hosts)
}

// packageProbeCmd prints one package's version across the fleet, grouped by
// version.
func packageProbeCmd(env *Env, name string, hosts []resource.HostID) tea.Cmd {
	return func() tea.Msg {
		runner, err := readOnlyFor(env)
		if err != nil {
			return FlashMsg{Level: FlashError, Text: err.Error()}
		}
		ctx, cancel := context.WithTimeout(context.Background(), fileTimeout)
		defer cancel()

		groups := make(map[string][]string)
		var failures []string
		cmd := transport.Command{Script: linux.PackageVersionCommand(name), Sudo: false}
		for hr := range runner.ExecReadOnly(ctx, hosts, cmd) {
			host := string(hr.Host)
			if hr.Err != nil {
				failures = append(failures, host+"  error  "+hr.Err.Error())
				continue
			}
			if e := remoteErr(hr.Host, cmd.Sudo, hr.Result); e != nil {
				failures = append(failures, host+"  error  "+e.Error())
				continue
			}
			version, verr := linux.ParsePackageVersion(string(hr.Result.Stdout))
			if verr != nil {
				failures = append(failures, host+"  error  no version (package not installed)")
				continue
			}
			groups[version] = append(groups[version], host)
		}

		lines := []string{
			"pkg " + name + "  ·  " + strconv.Itoa(len(hosts)) + " hosts  ·  " +
				strconv.Itoa(len(groups)) + " versions",
			"",
		}
		for _, version := range sortedByHostCount(groups) {
			members := append([]string(nil), groups[version]...)
			sort.Strings(members)
			lines = append(lines, version+"  "+strconv.Itoa(len(members))+" hosts  "+strings.Join(members, ", "))
		}
		sort.Strings(failures)
		lines = append(lines, failures...)
		return PushViewMsg{View: newTextPage(env, "pkg "+name, lines)}
	}
}

// splitCommandArgs splits a command line into words, honouring single and double
// quotes so a path with spaces survives as one argument.
func splitCommandArgs(s string) []string {
	var (
		out    []string
		cur    strings.Builder
		quote  rune
		escape bool
	)
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
	}
	for _, r := range s {
		switch {
		case escape:
			// A backslash inside double quotes (or outside them) takes the next
			// rune literally, so a quoted path can contain a quote itself.
			cur.WriteRune(r)
			escape = false
		case quote == '"' && r == '\\', quote == 0 && r == '\\':
			escape = true
		case quote != 0:
			if r == quote {
				quote = 0
				continue
			}
			cur.WriteRune(r)
		case r == '\'' || r == '"':
			quote = r
		case r == ' ' || r == '\t':
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return out
}

// textPage is the read-only viewer `:file` opens: a caption line and the text,
// scrolled with the usual keys. It does no I/O and starts no work.
type textPage struct {
	crumb string
	keys  *keymap.Map
	lines []string
	top   int
	w     int
	h     int
}

var _ View = (*textPage)(nil)

// newTextPage builds a viewer over lines.
func newTextPage(env *Env, crumb string, lines []string) *textPage {
	keys := keymap.Default()
	if env != nil && env.Keys != nil {
		keys = env.Keys
	}
	return &textPage{crumb: crumb, keys: keys, lines: append([]string(nil), lines...)}
}

// Init does nothing: the caller already has the content.
func (p *textPage) Init() tea.Cmd { return nil }

// Update scrolls, or pops on back.
func (p *textPage) Update(msg tea.Msg) (View, tea.Cmd) {
	k, ok := msg.(tea.KeyMsg)
	if !ok {
		return p, nil
	}
	page := max(1, p.h-1)
	switch key := k.String(); {
	case p.keys.Back.Matches(key):
		return p, Pop()
	case p.keys.Up.Matches(key):
		p.top--
	case p.keys.Down.Matches(key):
		p.top++
	case p.keys.PageUp.Matches(key):
		p.top -= page
	case p.keys.PageDown.Matches(key):
		p.top += page
	case p.keys.Top.Matches(key):
		p.top = 0
	case p.keys.Bottom.Matches(key):
		p.top = len(p.lines)
	}
	p.clamp()
	return p, nil
}

// clamp keeps the scroll position inside the document.
func (p *textPage) clamp() {
	if p.top < 0 {
		p.top = 0
	}
	if p.top > len(p.lines)-1 {
		p.top = max(0, len(p.lines)-1)
	}
}

// Render draws the visible window, exactly w by h.
func (p *textPage) Render(w, h int) string {
	p.w, p.h = w, h
	p.clamp()
	body := h
	visible := make([]string, 0, body)
	for i := p.top; i < len(p.lines) && len(visible) < body; i++ {
		visible = append(visible, p.lines[i])
	}
	return render.Fit(strings.Join(visible, "\n"), w, h)
}

// Crumb is the pane breadcrumb.
func (p *textPage) Crumb() string { return p.crumb }

// Hints are the keys the viewer answers.
func (p *textPage) Hints() []KeyHint {
	return []KeyHint{
		{Key: "up/down pgup/pgdown", Text: "scroll"},
		{Key: "esc", Text: "back"},
	}
}

// Subject is empty: the page is about a path, not a resource.
func (p *textPage) Subject() Subject { return Subject{} }

// Capturing is false: the pane keeps its prefix key.
func (p *textPage) Capturing() bool { return false }

// CopyLines returns the plain text of the page.
func (p *textPage) CopyLines() []string { return append([]string(nil), p.lines...) }
