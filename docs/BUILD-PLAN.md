# sitrep — Build plan (end to end)

> Companion to `docs/SPEC.md` v0.2. The spec says **what**; this file says **how and in what order**.
> The task files in `docs/tasks/` are the unit of work. This file is the contract they share.
> Status: 2026-11-12. Audience: the implementing agent and whoever reviews its work.

**The one rule:** the Go interfaces in §4, the message types in §5, the wire protocol in §6 and the error kinds in
§7 are a contract. If a task seems to need a change to any of them, **stop and ask** (write the question into
`docs/HANDOFF.md` under "Blocked", do not improvise). Everything else is yours to implement as the task says.

---

## 1. Decisions that shape everything

| # | Decision | Why |
|---|---|---|
| D1 | Module path is `sitrep` (`go mod init sitrep`). Imports look like `sitrep/internal/resource`. | No remote yet, and the final host is undecided. Renaming later is one `go mod edit -module` plus a mechanical import rewrite. |
| D2 | `go 1.27` in `go.mod`. Build with `CGO_ENABLED=0`. Primary target `darwin/arm64`; `darwin/amd64` must compile. | Constraint from the spec. Toolchain on the build Mac is go 1.27.2. |
| D3 | **Bubble Tea v1** (`github.com/charmbracelet/bubbletea` v1.3.x), Lip Gloss v1, Bubbles v1. Not the v2 line (`charm.land/...`). | v1 is frozen and stable, and it is the API a fast model knows best (`tea.KeyMsg`, `View() string`). v2 has a different import path and API; mixing them is the most likely way to break the build. Revisit after v1.0 of sitrep. |
| D4 | **The session process owns everything and renders.** A session is one long-lived `tea.Program` whose input and output are plain `io.Reader`/`io.Writer`. Standalone mode wires them to the real terminal. Server mode wires them to a unix socket; the client is a dumb byte pump (the tmux design). | One code path for both modes. No state-sync protocol to design or get wrong. The client is about 200 lines. See §6. |
| D5 | Detach/reattach ships in **M5**, after the tool is already useful. M0–M4 run standalone. From task 001 on, the code obeys the **detach-safety rules** (§6.1) so M5 is plumbing, not a rewrite. Task 025 proves it early by running the whole app over an in-memory pipe. | Keeps the early milestones shippable; removes the risk of painting into a corner. |
| D6 | A pane hosts either a **view stack** (sitrep views) or a **terminal** (remote shell over SSH, later `kubectl exec`, local shell). Terminal panes run the byte stream through an in-process VT emulator (`github.com/charmbracelet/x/vt`) and draw its screen as text inside the pane rectangle. | This is how tmux works, and it is the only way to show a shell next to a table in one frame. The emulator is wrapped behind our own small interface (§4.9) so it can be swapped. |
| D7 | Default prefix key is `C-a`. When the client starts inside tmux (`$TMUX` set), the default is `C-b` (`mux.prefix_in_tmux`). The status line always shows the active prefix. Every mux function is also a `:` command. | The user's tmux prefix is `C-a`, so `C-a` would be eaten by tmux when nested. Their tmux does not use `C-b`, so it passes through. Open question Q1. |
| D8 | Remote reads use **text formats that exist on every supported distro** when JSON is not universal. `systemctl list-units` is parsed from `--plain --no-legend` (JSON needs systemd ≥ 246; RHEL 8 has 239, Ubuntu 20.04 has 245). `journalctl -o json`, `ip -j`, `lsblk -J`, `findmnt -J` are JSON everywhere we support. | One parser per probe, no version sniffing. Reliable before clever. |
| D9 | **Salt comes right after the action bus (M4)**, and Salt commands run as **async jobs** (`salt --async` then poll `jobs.lookup_jid`). The job is the unit: the jobs browser, state runs and Salt-as-transport all sit on one job engine. | The Salt CLI's synchronous mode loses slow minions silently. Async + poll gives per-minion progress, clean timeouts, and the jobs browser for free. |
| D10 | Types shared by many packages live in leaf packages with no internal imports: `errs`, `resource`, `action`, `transport`. Backends never import `tui`, `mux`, `bus` or each other. | Prevents import cycles, which a fast model tends to "fix" by moving code around. |
| D11 | **All third-party dependencies for M0–M7 are added once, in task 001**, and held in `go.mod` by a blank-import file. Later tasks do not run `go get`. Tasks 098, 105, 115 add the few later ones. | Lets tasks run in parallel without `go.mod`/`go.sum` merge conflicts, and stops surprise dependencies. |
| D12 | No Go `plugin` package, no cgo, no code generation, no `init()` registration magic. Wiring is explicit in `internal/app`. | Predictable for the implementer and the reviewer. |
| D13 | **Every helper and dev tool is Go.** Lab controller `cmd/sitrep-lab` (`up|down|status|ssh-config|fault <name>|reset`, in-machine provisioning snippets embedded with `go:embed`), dev tool `cmd/sitrep-dev` (`capture`, `sanitize`, `release`), golden updates via `go test -update`, example auth helper in `examples/`. No bash or Python helper scripts; the Makefile only calls `go`. Shell that sitrep sends to remote nodes is the product, not a helper. | User requirement (2026-11-12). One language, one toolchain, testable tools. |

## 2. Milestones

Each milestone ends with something you can use and a demo script. "Lab" means the OrbStack lab described in
SPEC §22; all lab commands assume `go run ./cmd/sitrep-lab up` has run and `lab/ssh_config` exists (written by `sitrep-lab ssh-config`).

| M | Name | Tasks | You can now… | Demo (run from repo root) |
|---|---|---|---|---|
| M0 | Skeleton | 001–016 | Navigate fake data in a k9s-style shell: `:kind selector`, `/` filter, breadcrumbs, health dots, help. | `make build && ./sitrep --demo` → `:hosts role=web` → `/ewr-00` → `Enter` → `Esc` → `?` → `:q` |
| M1 | Multiplexer (in-process) | 017–025 | Split panes, windows, prefix key, zoom, resize, copy mode; layout survives restart. | `./sitrep --demo` → `C-a %` → `:alerts` in the new pane → `C-a c` → `C-a 0` → `C-a z` → `C-a [` select, `y` → quit and restart: layout is back |
| M2 | SSH + Linux core | 026–053 | Reach lab hosts through the bastion; hosts/units/journal/procs/sockets views; fleet probes grouped by identical output; an interactive SSH shell in a pane. | `./sitrep --config-dir lab/sitrep` → `:hosts` → `Enter` on `web-01` → `u` → `j` → `:probe failed-units` → `C-a %` → `:ssh web-02` |
| M3 | Actions + investigations | 054–065 | Restart a unit with plan → confirm → canary → rolling; policy deny; everything recorded in Markdown; knowledge panel; `:run`; guarded synchronized input. | Run `go run ./cmd/sitrep-lab fault haproxy-443`, then follow SPEC W1 without metrics: `:hosts role=web` → `:inv open nginx 443` → `:probe failed-units` → open the differing host → `u` → `x` restart nginx (fails at the canary) → `s` sockets → `F` → `x` stop haproxy → restart nginx → `T`; then `go run ./cmd/sitrep-lab fault haproxy-443 --undo` |
| M4 | Salt core | 066–076 | Minions and keys, grains inventory, selector ↔ Salt target, live and historical jobs grouped per minion, `state.apply test=True` grouped by identical diff, Salt as fan-out transport, Salt actions through the bus. **First fully usable release (v0.5).** | `:minions` → `:jobs` → `:hosts role=web` → `:salt state` (dry run, grouped) → `x` apply → `:run --via salt 'uptime'` |
| M5 | Session server | 077–083 | Detach and reattach; sessions survive closing the terminal; works inside tmux. | `./sitrep` → open panes, start `journal` follow → `C-a d` → close terminal → new terminal → `./sitrep attach` → follow kept running. `./sitrep ls` |
| M6 | systemd + Linux depth | 084–091 | isd parity: unit status/show/cat, dependencies, timers, drop-in preview, unit-file diff across hosts; disks, network, cgroups, dmesg, files, packages, `/proc` drill-in. | `:units` on `web-01` → `Enter` nginx → tabs; `:hosts role=web` → `:unitdiff nginx.service` |
| M7 | Salt depth | 092–097 | State tree browser, redacted pillar, mine, key management, live event stream, nodegroups, beacons/schedules, salt-ssh awareness, `salt-call` local mode. | `:states` → `:pillar web-01` → `:keys` accept a pending key → `:events` |
| M8 | HTTP + Prometheus/Alertmanager | 098–104 | Connection profiles with proxy/TLS/auth helper; alerts → investigation; PromQL and sparklines; silences. SPEC W1 complete. | `sitrep doctor conn lab-sso https://prom.lab.example.internal` → `:alerts` → `Enter` |
| M9 | Kubernetes | 105–112 | k9s parity for daily use; logs; exec into a pane; node ↔ host pivot. | `:pods` → crash-looping pod → `l` logs → `h` node as host → `u` kubelet |
| M10 | Chef + YAML plugins | 113–118 | knife reads, drift, `chef-client --why-run` through the bus; declarative plugins; save `:run` as a plugin. | `:chef nodes` → `:plugins` → run `nginx-status` |
| M11 | Grafana + Elasticsearch | 119–122 | Dashboards rendered in the TUI; ES log search and tail inside an investigation. | `:dash` → open → `:logs web-01` |
| M12 | Hardening | 123–127 | 2,000-host scale test, `init`/`doctor`, system-ssh fallback, crash safety, release build, user docs. | `make scale-test`; `sitrep doctor` |

Order rationale: M1 before SSH because every later view is drawn inside a pane and it is cheap while the code is
small. M3 before Salt because every Salt write must go through the bus. M5 after M4 because detach adds no new
capability to debug with, only persistence, and by then the model is proven by task 025.

## 3. Package layout (exact)

```
cmd/sitrep/                    main.go: subcommand dispatch only. No logic.
cmd/sitrep-lab/                lab controller (Go; owned by the lab track, not by these tasks)
cmd/sitrep-dev/                dev tool: capture, sanitize, release (Go; tasks 053, 067, 127)
examples/auth-helper-cookie/   example auth helper (Go; task 099)
internal/app/                  composition root: config → transports → backends → bus → session. Run().
internal/config/               paths, schema structs, load, defaults, validate
internal/errs/                 error kinds (§7)
internal/resource/             KindSpec, Column, Ref, Resource, Link, Query, Selector, TimeWindow, HostID
internal/action/               Spec, Request, Target, Step, Plan, StepResult, Planner (types only)
internal/backend/              Backend, Watcher, Result, Event, Health, Registry
internal/backend/fake/         deterministic fake backend used by --demo and tests
internal/backend/linux/        probes, parsers, kinds, actions
internal/backend/salt/         runner, targeting, jobs, states, keys, executor
internal/backend/k8s/          (M9)   chef/ (M10)   prom/ (M8)   grafana/ elastic/ (M11)
internal/transport/            Command, ExecResult, Executor, Streamer, ShellOpener, Shell (types only)
internal/transport/transporttest/  scripted fake Executor/Streamer/ShellOpener
internal/transport/ssh/        sshconfig.go auth.go hostkeys.go dial.go pool.go exec.go shell.go
internal/transport/ssh/sshtest/    in-process SSH server for tests
internal/transport/sysssh/     /usr/bin/ssh fallback (M12)
internal/transport/httpx/      connection profiles, proxy, TLS, auth helper (M8)
internal/shq/                  shell quoting, sudo wrapping
internal/probe/                batch script compiler, section splitter
internal/fanout/               concurrent runner over an Executor
internal/group/                grouping of identical outputs, summary, unified diff
internal/inventory/            Host, Source, merge, identity, cache; sources in source_*.go
internal/targets/              current target set, saved sets
internal/bus/                  bus, policy, confirm level, rollout engine
internal/redact/               secret redaction
internal/safe/                  safe.Go: panic-recovering goroutine starter (task 126)
internal/labtest/               helpers for //go:build lab tests (task 035)
internal/devtools/              logic behind cmd/sitrep-dev (capture, sanitize, release)
internal/bus/generic/           the `:run` planner (task 063)
internal/investigation/        investigation file, audit file, timeline, recorder, knowledge index
internal/plugin/               YAML plugins (M10)
internal/mux/                  layout tree, pane, window, session state, prefix machine, copy mode, persistence
internal/mux/vt/               terminal emulator adapter
internal/mux/termpane/         terminal pane content (uses vt + transport.Shell)
internal/server/               protocol codec, server, client attach, daemon start (M5)
internal/clip/                 clipboard (pbcopy, OSC 52)
internal/interact/             Prompt, Answer, Prompter: how non-UI code asks the user something
internal/render/               table, fit, sparkline, bar, diff colouring
internal/tui/                  root App model, Env, View interface, view stack, command bar, status line, dialogs
internal/tui/theme/            colours and styles
internal/tui/keymap/           key bindings and hints
internal/tui/views/            concrete views (table, text, host, fanout, journal, …)
internal/deps/                 deps.go: blank imports that pin dependencies (build tag `deps`)
lab/                           generated lab files (ssh_config, kubeconfig) and the sitrep lab config; see SPEC §22
testdata/                      golden inputs, sanitized
docs/                          SPEC, BUILD-PLAN, HANDOFF, INTERVIEW, tasks/
```

Import direction (an arrow means "may import"):

```
cmd → app → tui → mux → (mux/termpane, mux/vt)
            tui → bus → action, investigation, redact
            tui → backend(s) → transport, probe, fanout, group, inventory, resource, action, errs
       app → server
leaf packages (import nothing internal except errs): errs, resource, shq, redact, render, clip, config, interact
```

`backend/*` must not import `tui`, `mux`, `bus`, `server` or `app`. `tui/views` may import backends only for
their typed `Raw` payloads; prefer `resource.Resource.Fields`.

## 4. Core interfaces (the contract)

Signatures and doc comments below are normative. Private fields, helpers and constructors are up to the task.
A task file may add a method, field, constructor or small optional interface **by name** (for example `Run.Results()`
in task 057, `Env.Redact` in task 072); such additions are part of the contract once that task is done. Nothing
listed here may be removed, renamed or re-typed.
`tea` is `github.com/charmbracelet/bubbletea`.

### 4.1 `internal/errs`

```go
package errs

// Kind classifies a failure so the UI can label it and callers can branch on it.
type Kind string

const (
	Timeout      Kind = "timeout"           // deadline exceeded (connect, exec, HTTP, salt job)
	Canceled     Kind = "canceled"          // context canceled by the user or by shutdown
	Unreachable  Kind = "unreachable"       // DNS, refused, no route, bastion down
	Auth         Kind = "auth"              // SSH auth failed, HTTP 401/403, kube auth
	HostKey      Kind = "hostkey"           // unknown or mismatched host key
	Sudo         Kind = "sudo"              // `sudo -n` needs a password / not permitted
	NotAvailable Kind = "not-available"     // command or API missing on the remote (rc 127, 404)
	Remote       Kind = "remote"            // remote command ran and failed in a way the caller did not expect
	Parse        Kind = "parse"             // output did not parse
	Policy       Kind = "policy"            // denied by policy.yaml or read-only mode
	Interaction  Kind = "needs-interaction" // needs a prompt but no client is attached / prompt timed out
	Config       Kind = "config"            // invalid or missing configuration
	Protocol     Kind = "protocol"          // session-server protocol error or version mismatch
	Internal     Kind = "internal"          // bug; anything unclassified
)

// Error is the only error type that crosses package boundaries.
type Error struct {
	Kind Kind
	Op   string // "ssh.dial", "salt.lookup_jid", ...
	Host string // optional
	Msg  string // short, user-facing, already redacted
	Err  error  // wrapped cause, may be nil
}

func (e *Error) Error() string
func (e *Error) Unwrap() error

// New builds an Error. Wrap builds one around a cause. WithHost returns a copy with Host set.
func New(kind Kind, op, msg string) *Error
func Wrap(kind Kind, op string, err error) *Error
func (e *Error) WithHost(host string) *Error

// KindOf returns the Kind of err. It unwraps; it maps context.DeadlineExceeded to Timeout and
// context.Canceled to Canceled; anything else is Internal. KindOf(nil) is "".
func KindOf(err error) Kind
```

### 4.2 `internal/resource`

```go
package resource

// HostID is the canonical identity of a machine: lower-case, as resolved by the inventory.
type HostID string

// KindName names a resource kind, e.g. "host", "unit", "pod", "salt-job".
type KindName string

// Ref points at one resource. Unused fields are empty.
type Ref struct {
	Backend   string   // "linux", "salt", "k8s", ...
	Kind      KindName
	Host      HostID   // for host-scoped kinds
	Namespace string   // k8s namespace, or a backend-defined scope
	Name      string
}

// String renders "backend/kind/host/namespace/name" with empty parts kept, e.g. "linux/unit/web-01//nginx.service".
func (r Ref) String() string

type Status int

const (
	StatusUnknown Status = iota
	StatusOK
	StatusWarn
	StatusCrit
	StatusNA // not available / not applicable
)

// Resource is one row. Fields holds one value per column ID. Allowed value types:
// string, int64, float64, bool, time.Time, time.Duration, []float64 (sparkline), nil (renders as "—").
type Resource struct {
	Ref       Ref
	Fields    map[string]any
	Labels    map[string]string
	Status    Status
	Raw       any       // backend-specific payload for detail views; may be nil
	FetchedAt time.Time
	Stale     bool      // served from cache or from a failed refresh
}

type ColumnKind int

const (
	ColText ColumnKind = iota
	ColNumber
	ColBytes
	ColPercent
	ColDuration
	ColTime
	ColSpark
	ColStatus
)

type Align int

const (
	AlignLeft Align = iota
	AlignRight
)

type Column struct {
	ID    string // key into Resource.Fields
	Title string // header text, upper case
	Width int    // fixed cell width; 0 = share the remaining width
	Align Align
	Kind  ColumnKind
}

type Scope int

const (
	ScopeGlobal    Scope = iota // one list for the whole backend (alerts, salt jobs)
	ScopeHost                   // per host (unit, process, socket)
	ScopeNamespace              // per k8s namespace
)

type KindSpec struct {
	Name        KindName
	Aliases     []string // accepted in the command bar, e.g. "po" for "pod"
	Title       string
	Scope       Scope
	Columns     []Column
	DefaultSort string // column ID; prefix "-" for descending
	Watchable   bool
}

// TimeWindow is the investigation time range every view respects.
type TimeWindow struct {
	From  time.Time
	To    time.Time // ignored when ToNow
	ToNow bool
	Label string // what the user typed, e.g. "-1h"
}

// ParseTimeWindow accepts "-30m", "-1h", "-2d", "2026-11-12 13:00..14:00",
// "2026-11-12T13:00:00Z..now". now is injected for tests.
func ParseTimeWindow(s string, now time.Time) (TimeWindow, error)
func (w TimeWindow) Bounds(now time.Time) (from, to time.Time)

// Query is a request for a list of resources.
type Query struct {
	Backend  string            // empty = resolve by kind through the registry
	Kind     KindName
	Selector Selector          // which hosts / labels
	Hosts    []HostID          // explicit host list; wins over Selector when non-empty
	Args     map[string]string // kind-specific, e.g. {"unit": "nginx.service"}
	Window   TimeWindow
}

// Link is a pivot from one resource to another list.
type Link struct {
	Label string // "journal", "pods on node"
	Key   string // suggested key binding, may be empty
	To    Query
}
```

Selector (same package, `selector.go`):

```go
type Op int

const (
	OpEq       Op = iota // key=value
	OpNotEq              // key!=value
	OpMatch              // key~regex   (unanchored, RE2)
	OpNotMatch           // key!~regex
	OpExists             // key
	OpNotExists          // !key
)

type Term struct {
	Key   string
	Op    Op
	Value string
}

// Selector is a conjunction (AND) of terms plus named sets.
// Text form: `role=web env=prod dc~ewr|lax !maint host~^db @saved-set @investigation`.
// Terms are separated by spaces. Values with spaces use double quotes: name="a b".
type Selector struct {
	Terms []Term
	Sets  []string // names without the leading "@"
	Raw   string
}

func ParseSelector(s string) (Selector, error)
func (s Selector) IsZero() bool
// Matches reports whether labels satisfy every term. Sets are ignored here; internal/targets expands them.
func (s Selector) Matches(labels map[string]string) bool
func (s Selector) String() string // canonical text form; ParseSelector(s.String()) round-trips
```

### 4.3 `internal/transport`

```go
package transport

// Command is one remote shell invocation. It runs as: sh -c <Script>,
// or `sudo -n sh -c <Script>` when Sudo is set. Script is passed as a single quoted argument (see internal/shq).
type Command struct {
	Script    string
	Sudo      bool
	Stdin     []byte        // optional
	Timeout   time.Duration // 0 = executor default
	MaxOutput int           // bytes per stream; 0 = executor default (2 MiB)
}

// ExecResult is what came back. RC is the remote exit code (-1 if the command never ran).
// A non-zero RC is NOT an error at this layer.
type ExecResult struct {
	Host      resource.HostID
	Stdout    []byte
	Stderr    []byte
	RC        int
	Truncated bool          // output hit MaxOutput
	Duration  time.Duration
	Via       string        // "ssh", "ssh via bastion.lab.example.internal", "salt:salt.lab.example.internal"
}

// Executor runs commands on hosts. Implementations: native SSH, Salt, system ssh, fake.
// Exec must respect ctx, must never block past the timeout, and must return a *errs.Error
// (Timeout, Unreachable, Auth, HostKey, Sudo, Canceled) for transport-level failures.
// Safe for concurrent use.
type Executor interface {
	Name() string
	Exec(ctx context.Context, host resource.HostID, cmd Command) (ExecResult, error)
}

// Streamer starts a long-running command and delivers stdout line by line (journal follow, salt events).
type Streamer interface {
	Start(ctx context.Context, host resource.HostID, cmd Command) (Stream, error)
}

type Stream interface {
	// Lines is closed when the command ends or the stream is closed. Lines exclude the trailing newline.
	Lines() <-chan []byte
	// Wait blocks until the command ends and returns its result (Stdout empty; Stderr is the capped tail).
	Wait() (ExecResult, error)
	Close() error
}

type PTYRequest struct {
	Term    string // e.g. "xterm-256color"
	Cols    int
	Rows    int
	Command string // empty = login shell
}

// HostResult is one host's outcome in a multi-host run.
type HostResult struct {
	Host   resource.HostID
	Result ExecResult
	Err    error // *errs.Error for transport failures
}

// BatchExecutor is optional: executors that can run one command on many hosts as a single operation
// (Salt). fanout.Run uses it when present. Every host yields exactly one HostResult; the channel is closed.
type BatchExecutor interface {
	ExecBatch(ctx context.Context, hosts []resource.HostID, cmd Command) <-chan HostResult
}

// ShellOpener opens an interactive PTY session on a host.
type ShellOpener interface {
	OpenShell(ctx context.Context, host resource.HostID, req PTYRequest) (Shell, error)
}

// Shell is the byte stream of a remote PTY. Read returns terminal output; Write sends keystrokes.
type Shell interface {
	io.ReadWriteCloser
	Resize(cols, rows int) error
	Wait() error // returns when the remote side exits
}
```

### 4.4 `internal/backend`

```go
package backend

type HealthState int

const (
	HealthUnconfigured HealthState = iota // grey dot
	HealthOK                              // green
	HealthDegraded                        // half
	HealthDown                            // red
)

type Health struct {
	State   HealthState
	Detail  string // one line, shown in :health
	Checked time.Time
}

// Result is one batch of a streaming List. A List on 100 hosts yields at least 100 Results.
type Result struct {
	Host  resource.HostID     // set for host-scoped kinds
	Items []resource.Resource
	Err   error               // *errs.Error; this batch failed, others may still succeed
	Done  int                 // units finished so far (hosts, pages)
	Total int                 // units expected; 0 = unknown
}

type EventType int

const (
	EventAdded EventType = iota
	EventModified
	EventDeleted
)

type Event struct {
	Type     EventType
	Resource resource.Resource
}

// Backend is implemented once per integration. All methods are safe for concurrent use.
type Backend interface {
	Name() string
	Kinds() []resource.KindSpec
	// List streams results and closes the channel when finished or when ctx is done.
	// It returns an error only when nothing can be started (unknown kind, backend unconfigured).
	// Per-host failures arrive as Result.Err. List must not block: do the work in goroutines.
	List(ctx context.Context, q resource.Query) (<-chan Result, error)
	Get(ctx context.Context, ref resource.Ref) (resource.Resource, error)
	// Links returns pivots available from r. Pure function, no I/O.
	Links(r resource.Resource) []resource.Link
	Health(ctx context.Context) Health
}

// Watcher is optional: live updates for Watchable kinds.
type Watcher interface {
	Watch(ctx context.Context, q resource.Query) (<-chan Event, error)
}

// Registry maps kind names and aliases to backends.
type Registry struct{ /* private */ }

func NewRegistry() *Registry
func (r *Registry) Register(b Backend) error                          // error on duplicate kind name or alias
func (r *Registry) Replace(b Backend) error                           // swap a backend of the same name (plugin reload, task 117)
func (r *Registry) Get(name string) (Backend, bool)                   // by backend name
func (r *Registry) Backends() []Backend                               // registration order
func (r *Registry) Lookup(kindOrAlias string) (Backend, resource.KindSpec, bool)
func (r *Registry) Kinds() []resource.KindSpec                        // sorted by name

// LinkProvider is optional: extra pivots for rows of *other* backends (host → salt minion, pod → host).
// The root asks every registered backend, so backends never import each other.
type LinkProvider interface {
	ExtraLinks(r resource.Resource) []resource.Link
}
```

Writes are **not** on `Backend`. A backend that can write also implements `action.Planner` (§4.5).

### 4.5 `internal/action` and `internal/bus`

```go
package action

// Actor says who asked. "human", "plugin:<name>", "agent:<name>".
type Actor string

const Human Actor = "human"

type Risk int

const (
	RiskNormal      Risk = iota // restart, reload, scale
	RiskDestructive             // stop, kill -9, delete, key delete, state.apply
)

type Param struct {
	Name     string
	Title    string
	Default  string
	Required bool
	Choices  []string // optional
}

// Spec describes one kind of write a backend offers.
type Spec struct {
	ID      string // globally unique: "linux.systemctl.restart", "salt.state.apply", "generic.run"
	Title   string // menu text: "restart"
	Risk    Risk
	Params  []Param
	HasDryRun bool // Plan can produce a Preview
}

type Target struct {
	Host   resource.HostID
	Ref    resource.Ref      // the resource acted on, if any
	Labels map[string]string // host labels; policy matches on these
}

type Request struct {
	SpecID  string
	Targets []Target
	Params  map[string]string
	Actor   Actor
}

type StepResult struct {
	Target   Target
	RC       int
	Output   string // combined, size-capped, NOT yet redacted
	Err      error
	Duration time.Duration
}

// Step is the work for one target. Display is the exact command or API call, shown before confirmation
// and written to the record. Run performs it. Run must honour ctx and must not prompt.
type Step struct {
	Target  Target
	Display string
	Run     func(ctx context.Context) StepResult
}

// Plan is everything the user sees before confirming.
type Plan struct {
	Spec     Spec
	Request  Request
	Title    string   // "restart nginx.service on 12 hosts"
	Steps    []Step   // one per target, in rollout order
	Preview  string   // dry-run output or diff, may be empty
	Via      string   // transport name
	Warnings []string
}

// Planner is implemented by backends that can write.
type Planner interface {
	// Actions lists what can be done to this selection. Pure, no I/O.
	Actions(selection []resource.Resource) []Spec
	// Plan resolves a request into steps. It may run read-only remote calls (dry runs) but must not write.
	Plan(ctx context.Context, req Request) (Plan, error)
}
```

```go
package bus

type ConfirmLevel int

const (
	ConfirmYesNo      ConfirmLevel = iota // 1 target: y/N
	ConfirmList                           // 2..N: show the list, y/N
	ConfirmTypedCount                     // > typed_confirm_over (default 20) or any protected target: type the count
)

type Rollout struct {
	Canary           int // hosts in the canary phase (0 = no canary when 1 target)
	BatchSize        int
	Batches          int
	FailureThreshold float64 // fraction of failed hosts after the canary that stops the rollout
}

// Proposal is a planned, policy-checked action waiting for confirmation.
type Proposal struct {
	ID      string
	Plan    action.Plan
	Confirm ConfirmLevel
	Rollout Rollout
	Denied  *Denial // non-nil: cannot be executed; show the reason
}

type Denial struct {
	Rule   string // the policy.yaml rule text
	Reason string
}

type Confirmation struct {
	ProposalID string
	Typed      string // the typed target count for ConfirmTypedCount; ignored otherwise
}

type Phase int

const (
	PhaseCanary Phase = iota
	PhasePaused // canary finished OK; waiting for Continue or Abort
	PhaseBatch
	PhaseDone
	PhaseAborted
	PhaseStopped // failure threshold hit
)

type Event struct {
	Phase   Phase
	Batch   int                // 1-based during PhaseBatch
	Result  *action.StepResult // set when one step finished
	Summary *Summary           // set on Done, Aborted, Stopped
}

type Summary struct {
	OK, Failed, Skipped int
	Duration            time.Duration
}

// Run is a rollout in progress.
type Run interface {
	Events() <-chan Event // closed after the final event
	Continue()            // leave PhasePaused
	Abort()               // stop before the next batch; in-flight steps finish
}

// Recorder persists what happened. Implemented by internal/investigation.
type Recorder interface {
	RecordAction(p Proposal, s Summary, results []action.StepResult) error
	RecordDenied(p Proposal) error
}

// Bus is the only way to perform a write.
type Bus interface {
	// Propose finds the planner for req.SpecID, applies policy, builds the plan and decides the confirm level.
	Propose(ctx context.Context, req action.Request) (Proposal, error)
	// Execute validates the confirmation and starts the rollout in the background.
	// Returns *errs.Error{Kind: Policy} if the proposal was denied or the confirmation is wrong.
	Execute(ctx context.Context, p Proposal, c Confirmation) (Run, error)
	// Register adds a planner. Spec IDs must be unique across planners.
	Register(name string, pl action.Planner) error
}
```

Nothing outside `internal/bus` may call `action.Step.Run`.

### 4.6 `internal/fanout` and `internal/group`

```go
package fanout

type Options struct {
	Concurrency int // max hosts in flight; 0 = 64
}

// HostResult is an alias so callers can keep writing fanout.HostResult.
type HostResult = transport.HostResult

// Run executes cmd on every host with bounded concurrency. Results arrive in completion order.
// The channel is closed when all hosts are done or ctx is canceled; every host yields exactly one HostResult
// (hosts not started before cancellation yield Err with Kind Canceled). Duplicate hosts run once.
// If ex implements transport.BatchExecutor, Run delegates to it. Callers must drain the channel.
func Run(ctx context.Context, ex transport.Executor, hosts []resource.HostID, cmd transport.Command, opt Options) <-chan HostResult
```

```go
package group

type Group struct {
	Key    string            // hash of the normalized output + rc
	Output string            // representative output (first seen)
	RC     int
	Hosts  []resource.HostID // sorted
}

type HostErr struct {
	Host resource.HostID
	Err  error
}

type Summary struct {
	Total  int                     // hosts expected
	Done   int                     // hosts reported (ok + errors)
	Groups []Group                 // largest first; Groups[0] is the majority. Ties: lexical by Key.
	Errors map[errs.Kind][]HostErr // transport failures, not part of any group
}

// Headline renders "1,481 identical · 19 differ · 4 errors". Parts with a zero count are omitted,
// except "identical". While Done < Total it is prefixed with "812/1,512 · ".
func (s Summary) Headline() string

// Grouper accumulates results. Not safe for concurrent use; feed it from one goroutine.
type Grouper struct{ /* private */ }

// normalize may be nil. It is applied to output before hashing (e.g. strip timestamps).
func NewGrouper(total int, normalize func(string) string) *Grouper
func (g *Grouper) Add(host resource.HostID, output string, rc int)
func (g *Grouper) AddError(host resource.HostID, err error)
func (g *Grouper) Summary() Summary

// UnifiedDiff returns a unified diff of two texts with 3 lines of context. Empty string when equal.
func UnifiedDiff(aName, a, bName, b string) string
```

### 4.7 `internal/inventory` and `internal/targets`

```go
package inventory

type Host struct {
	ID      resource.HostID
	Addr    string            // what to dial over SSH (ssh_config alias or FQDN)
	Aliases []string          // short name, FQDN, IPs, k8s node name, minion id, prom instance
	Labels  map[string]string // namespaced ("salt.role") plus unprefixed shortcuts when unambiguous; always has "host"
	Sources []string
	SeenAt  time.Time
}

// Source produces hosts from one place. Load must respect ctx and return quickly or fail.
type Source interface {
	Name() string // also the label namespace: "ssh", "static", "salt", "k8s", "prom", "chef"
	Load(ctx context.Context) ([]Host, error)
}

type Inventory interface {
	Hosts() []Host                                // sorted by ID; a copy
	Select(sel resource.Selector) []Host          // Sets in sel are ignored here
	Resolve(nameOrAlias string) (Host, bool)      // after identity rewrites
	Refresh(ctx context.Context) map[string]error // per source name; successful sources still apply
	Age() time.Duration                           // since the last successful refresh of any source
}
```

```go
package targets

// Set is the current target set of a session: what `:run`, probes and actions apply to.
type Set struct {
	Label    string            // "role=web dc=ewr", "@investigation", "3 selected"
	Selector resource.Selector // zero when built from an explicit list
	Hosts    []resource.HostID // resolved, sorted, de-duplicated
}

// Resolve expands sel against inv, including @sets. "@investigation" resolves through invHosts (may be nil).
func Resolve(inv inventory.Inventory, store *Store, sel resource.Selector, invHosts []resource.HostID) (Set, error)

// Store reads and writes saved sets as <root>/sets/<name>.txt, one host per line, "#" comments.
type Store struct{ /* private */ }
```

### 4.8 `internal/tui`

```go
package tui

// Subject is what a view is "about". It drives the knowledge panel and investigation entries.
type Subject struct {
	Hosts []resource.HostID
	Unit  string
	Alert string
	Kind  resource.KindName
	Ref   resource.Ref
}

type KeyHint struct {
	Key  string // "enter", "u", "C-a %"
	Text string // "open host"
}

// View is one screen inside a view pane. Views are values owned by a ViewStack.
// Update and Render must never block or do I/O; start work by returning a tea.Cmd.
type View interface {
	Init() tea.Cmd
	Update(msg tea.Msg) (View, tea.Cmd)
	// Render returns exactly h lines, each exactly w cells wide (use render.Fit).
	Render(w, h int) string
	Crumb() string     // breadcrumb text, e.g. "hosts (24)"
	Hints() []KeyHint  // shown in the hint bar while focused
	Subject() Subject
	// Capturing is true while the view owns the keyboard (filter input, text prompt).
	// While true, the pane sends every key except the prefix key to the view.
	Capturing() bool
	// CopyLines returns the full plain text of the view for copy mode (no ANSI).
	CopyLines() []string
}

// Env is the shared, read-mostly environment handed to every view and command.
type Env struct {
	Registry  *backend.Registry
	Inventory inventory.Inventory
	Bus       bus.Bus                 // nil before M3
	Exec      transport.Executor      // default fan-out executor; nil in --demo
	Shells    transport.ShellOpener   // nil in --demo
	Session   *SessionState           // targets, window, investigation, readonly
	Theme     *theme.Theme
	Keys      *keymap.Map
	Now       func() time.Time
}

// SessionState is the per-session context every pane shares. Only the root model mutates it.
type SessionState struct {
	Name          string
	Targets       targets.Set
	Window        resource.TimeWindow
	Investigation *investigation.Meta // nil = none open
	ReadOnly      bool
}

// Factory builds the view for a query. Registered per kind; kinds without one get the generic table view.
type Factory func(env *Env, q resource.Query) (View, error)

// Command is a `:` command.
type Command struct {
	Name    string   // "hosts", "split", "run"
	Aliases []string
	Usage   string   // ":run [--via ssh|salt] <command>"
	Help    string   // one line
	// Run returns a tea.Cmd that emits navigation or mux messages (§5). It must not block.
	Run func(env *Env, args string) tea.Cmd
	// Complete returns candidates for the argument text; may be nil.
	Complete func(env *Env, args string) []string
}
```

**Growing structs.** `Env`, `SessionState` and the message list in §5 mention packages that do not exist in early
milestones (`inventory`, `targets`, `bus`, `investigation`, `fanout`, `transport`). The task that introduces a
package adds the fields and messages that reference it; until then they are simply absent. Names and shapes are
fixed here; do not invent alternatives.

### 4.8.1 `internal/interact`

```go
package interact

type Kind int

const (
	HostKeyUnknown Kind = iota // Body holds host, key type and SHA256 fingerprint; Choices: "once", "always", "reject"
	Passphrase                 // Secret = true; Answer.Text is the passphrase
	Confirm                    // Choices: "yes", "no"
)

type Prompt struct {
	Kind    Kind
	Title   string
	Body    string
	Secret  bool
	Choices []string
}

type Answer struct {
	Choice   string
	Text     string
	Canceled bool
}

// Prompter asks the user. Implementations must return errs.Interaction when nobody can answer
// (no UI, detached past the timeout). Safe for concurrent use; prompts are shown one at a time.
type Prompter interface {
	Ask(ctx context.Context, p Prompt) (Answer, error)
}
```

### 4.9 `internal/mux`

```go
package mux

type PaneID int
type WindowID int

// Content is what lives in a pane: a view stack or a terminal.
type Content interface {
	Init() tea.Cmd
	Update(msg tea.Msg) (Content, tea.Cmd)
	// View returns exactly h lines of exactly w cells (the pane interior, borders excluded).
	View(w, h int) string
	Title() string
	SetFocus(focused bool)
	// RawKeys is true for terminals: every key except the prefix key is forwarded to Update untouched,
	// and the global keys (":", "?", "/") are not intercepted.
	RawKeys() bool
	CopyLines() []string // plain text incl. scrollback, oldest first
	State() ContentState // for persistence
	Close() error        // release remote sessions, cancel contexts
}

// ContentState is what is needed to recreate a pane after a restart.
type ContentState struct {
	Type     string   `json:"type"`     // "view" | "term"
	Commands []string `json:"commands"` // view: the `:` command line of each stack entry, bottom first
	Host     string   `json:"host"`     // term: host to reconnect to
}

// Rect is a pane rectangle in screen cells, borders included.
type Rect struct{ X, Y, W, H int }

type Direction int

const (
	Left Direction = iota
	Right
	Up
	Down
)

// Layout is a binary tree of splits. Pure data, no I/O, no tea. All methods return a new or mutated
// layout deterministically so they can be table-tested.
type Layout struct{ /* private */ }

func NewLayout(first PaneID) *Layout
func (l *Layout) SplitH(target, new PaneID) error            // new pane to the right of target, 50/50
func (l *Layout) SplitV(target, new PaneID) error            // new pane below target, 50/50
func (l *Layout) Close(id PaneID) (next PaneID, empty bool)  // sibling takes the space; next = pane to focus
func (l *Layout) Resize(id PaneID, d Direction, cells int, total Rect)
func (l *Layout) Neighbor(id PaneID, d Direction, total Rect) (PaneID, bool)
func (l *Layout) Rects(total Rect) map[PaneID]Rect           // tiles total exactly; min pane size 10x3
func (l *Layout) Panes() []PaneID                            // depth-first, left/top first
func (l *Layout) Swap(a, b PaneID)
func (l *Layout) Even(horizontal bool)                       // equalize sizes along one axis

// vt adapter (package internal/mux/vt)
// Emulator turns a terminal byte stream into a screen of cells.
type Emulator interface {
	io.Writer                      // feed remote output
	Resize(cols, rows int)
	Render() string                // rows joined by "\n", with SGR colours, exactly rows x cols
	Cursor() (x, y int, visible bool)
	ScrollbackLines() []string     // plain text, oldest first
	// Responses returns bytes the emulator wants to send back to the remote side (DA, cursor reports).
	// It must be drained continuously by the owner.
	Responses() io.Reader
	Close() error
}
```

### 4.10 `internal/investigation`

```go
package investigation

type EntryKind string

const (
	EntryOpen    EntryKind = "open"
	EntryView    EntryKind = "view"
	EntryProbe   EntryKind = "probe"
	EntryJournal EntryKind = "journal"
	EntryAction  EntryKind = "action"
	EntryDenied  EntryKind = "denied"
	EntryNote    EntryKind = "note"
	EntryFinding EntryKind = "finding"
	EntryResolve EntryKind = "resolve"
	EntryInput   EntryKind = "input" // synchronized input lines
)

// Entry is one timeline bullet. Text is Markdown, one line. Output is attached collapsed.
type Entry struct {
	Time   time.Time
	Kind   EntryKind
	Actor  action.Actor
	Text   string
	Output string // optional; redacted and size-capped by the writer
}

// Log appends entries to the open investigation, or to the daily audit file when none is open
// (audit files only receive action, denied and input entries). Safe for concurrent use.
// Log implements bus.Recorder.
type Log interface {
	Append(e Entry) error
	Current() (Meta, bool)
}

type Meta struct {
	ID      string // "2026-11-12-api-5xx-ewr"
	Title   string
	Status  string // open | resolved | abandoned
	Opened  time.Time
	Window  resource.TimeWindow
	Targets []resource.HostID
	Alerts  []string
	Tags    []string
	Path    string
}
```

## 5. Bubble Tea message types

All in `internal/tui/msgs.go` unless noted. Messages are plain structs. A message that belongs to one pane is
wrapped in `mux.PaneMsg` so the root can route it.

```go
// package mux
// PaneMsg routes Msg to one pane's Content. Use mux.ForPane(id, cmd) to wrap the result of a tea.Cmd.
type PaneMsg struct {
	Pane PaneID
	Msg  tea.Msg
}

func ForPane(id PaneID, cmd tea.Cmd) tea.Cmd

// OpMsg asks the multiplexer to do something. Op names are constants in mux/ops.go (task 018):
// "split-h", "split-v", "kill-pane", "zoom", "focus", "resize", "swap", "layout-next", "new-window",
// "kill-window", "select-window", "next-window", "prev-window", "window-list", "rename-window",
// "rename-session", "session-list", "ssh", "copy-mode", "sync", "command", "detach", "help", "send-prefix".
type OpMsg struct {
	Op  string
	Arg string
}
```

```go
// package tui

// Navigation (emitted by views and commands; handled by the focused view pane's stack)
type PushViewMsg struct{ View View }
type PopViewMsg struct{}
type ReplaceViewMsg struct{ View View }
type OpenQueryMsg struct{ Query resource.Query } // resolve through the registry + factories, then push

// Feedback
type FlashLevel int // FlashInfo, FlashOK, FlashWarn, FlashError
type FlashMsg struct {
	Level FlashLevel
	Text  string
}

// Dialogs (modal, owned by the root)
type OpenDialogMsg struct{ Dialog Dialog }
type CloseDialogMsg struct{}

// Command bar
type RunCommandMsg struct{ Line string } // execute a `:` command line as if typed

// Session state changes (root updates Env.Session, then rebroadcasts to all panes)
type TargetsChangedMsg struct{ Set targets.Set }
type WindowChangedMsg struct{ Window resource.TimeWindow }
type InvestigationChangedMsg struct{ Meta *investigation.Meta } // nil = closed

// Health polling (root → status line)
type HealthMsg struct {
	Backend string
	Health  backend.Health
}

// Streaming data into a view. Seq guards against stale streams after a refresh.
type ListBatchMsg struct {
	Seq     int
	Results []backend.Result
	Closed  bool
}
type FanoutBatchMsg struct {
	Seq     int
	Results []fanout.HostResult
	Closed  bool
}
type LinesMsg struct {
	Seq    int
	Lines  []string
	Closed bool
	Err    error
}

// Action flow (dialogs)
type ProposalMsg struct {
	Proposal bus.Proposal
	Err      error
}
type RunEventsMsg struct {
	Events []bus.Event
	Closed bool
}

// Mux requests are mux.OpMsg (package mux, see below): emitted by `:` commands and the prefix table,
// executed by mux.Model, or handed to the root when the mux cannot do them itself (detach, command bar, help).

// Interaction requests from non-UI code (host key, passphrase). The root shows a dialog and replies.
type PromptMsg struct {
	Prompt interact.Prompt       // package internal/interact
	Reply  chan<- interact.Answer
}
```

**Streaming pattern (the only one allowed).** A view never receives one message per host. It starts the work in a
`tea.Cmd`, keeps the channel, and asks for the next batch with `tui.NextBatch`:

```go
// NextBatch waits for at least one item on ch, then drains whatever else is ready, up to max items or
// until 50 ms have passed, and returns them as one slice. closed is true when ch was closed.
func NextBatch[T any](ch <-chan T, max int) (items []T, closed bool)
```

The view's `Update` handles the batch message and returns another `NextBatch` command until `Closed`. Nobody calls
`tea.Program.Send` except `internal/app` (window size, prompts) and `internal/server`.

## 6. Session server (M5) and the rules that keep M0–M4 compatible

```
 terminal ── sitrep (client) ══ unix socket ══ sitrep server (one per user)
   raw mode, alt screen,          frames          ├─ session "main":  tea.Program ⇄ mux ⇄ panes
   pumps bytes, SIGWINCH                          ├─ session "db-incident": …
                                                  └─ shared: SSH pool, inventory, backends, bus, investigations
                                                              │ SSH / HTTPS (outbound only)
                                                              ▼ nodes: nothing installed, nothing listening
```

- **One server per user**, started on demand by the client (`sitrep` re-executes itself as `sitrep server` with
  `Setsid`, stdio to `~/.cache/sitrep/server.log`). Lock: `flock` on `~/.cache/sitrep/server.lock`.
- **Socket:** `~/.cache/sitrep/sitrep.sock`, directory mode `0700`, socket mode `0600`. Override with
  `$SITREP_SOCKET` or `-S <path>` (tests use a temp dir; macOS limits socket paths to 103 bytes — check and fail with
  a `Config` error).
- **Rendering happens in the server.** Each session runs one `tea.Program` for its whole life with
  `tea.WithInput(sessionIn)` and `tea.WithOutput(sessionOut)`. `sessionIn`/`sessionOut` are switchable pipes:
  attached → the client connection; detached → input blocks, output is discarded. Work in progress (fan-outs,
  journal follows, shells, rollouts paused at a canary) keeps running while detached.
- **The client** puts the terminal in raw mode, enters the alt screen, hides the cursor, enables mouse reporting,
  then copies stdin → `STDIN` frames and `STDOUT` frames → stdout, and sends `RESIZE` on `SIGWINCH`. On exit it
  restores the terminal. It never parses keys. Detach is decided by the server (prefix `d`), which sends `EXIT`.
- **One client per session at a time.** A second attach to the same session detaches the first (`reason:
  "replaced"`), like `tmux attach -d`. Multi-client mirroring is out of scope.
- **On attach** the server stores the client's size and environment (`TERM`, `COLORTERM`, `SSH_AUTH_SOCK`, `TMUX`),
  injects `tea.WindowSizeMsg`, and forces a full repaint. New SSH connections use the newest `SSH_AUTH_SOCK`.
- **Colour profile** is fixed per server from config `ui.color` (`auto` = from the environment of the client that
  started the server). Documented limitation.
- **Prompts while detached** (host key, key passphrase) wait up to `interact.timeout` (default 2 min) for a client,
  then fail with `errs.Interaction`.
- **Persistence:** every session autosaves its layout (`mux.SessionFile`, task 024) to
  `~/.cache/sitrep/sessions/<name>.json` on change (debounced 1 s). When the server starts it restores saved
  sessions lazily on first attach. Terminal panes come back as "disconnected — press r to reconnect".
- **Standalone mode** (`sitrep --standalone`, and the only mode before M5): same `app.Run`, wired to the real TTY.
  From M5 on the default is server mode; `--standalone` stays for debugging and for tests.

### 6.1 Detach-safety rules (apply from task 001)

1. Only `cmd/sitrep` and `internal/app` may touch `os.Stdin`, `os.Stdout`, `os.Stderr`, `os.Exit`, `os.Getenv`.
   Everything else gets readers, writers and values passed in.
2. No `tea.ExecProcess`, no `tea.Suspend`. Anything interactive runs in a terminal pane.
3. Never read the terminal size from the OS; use the last `tea.WindowSizeMsg`.
4. No `fmt.Print*` or `log.Print*` to stdout/stderr anywhere under `internal/`. Use the `*slog.Logger` passed in.
5. Long work belongs to the session context, not to a view: closing a pane cancels its own context only.
6. Clipboard goes through `internal/clip`. Opening a browser or `$EDITOR` goes through `internal/app` hooks.
7. No package-level mutable state except in `internal/tui/theme` (the colour profile).

### 6.2 Wire protocol

Frame = `type (1 byte)` + `length (4 bytes, big endian)` + `payload`. Maximum payload 1 MiB; larger is a
`Protocol` error and the connection is closed. JSON payloads are UTF-8, unknown fields ignored.

| Type | Dir | Name | Payload |
|---|---|---|---|
| `0x01` | C→S | `HELLO` | JSON `Hello` (must be the first frame) |
| `0x02` | S→C | `WELCOME` | JSON `Welcome` |
| `0x03` | C→S | `STDIN` | raw bytes from the terminal |
| `0x04` | S→C | `STDOUT` | raw bytes for the terminal |
| `0x05` | C→S | `RESIZE` | JSON `{"cols":132,"rows":40}` |
| `0x06` | C→S | `DETACH` | empty (client asks to detach, e.g. on SIGHUP) |
| `0x07` | S→C | `EXIT` | JSON `{"reason":"detached|ended|replaced|shutdown|error","message":"…"}` |
| `0x08` | S→C | `RESULT` | JSON reply to a control command (`ls`, `kill-session`, `kill-server`, `new -d`) |

```go
// package server
const ProtocolVersion = 1

type Hello struct {
	Protocol int               `json:"protocol"`
	Version  string            `json:"version"`  // binary version, informational
	Command  string            `json:"command"`  // "attach" | "new" | "ls" | "kill-session" | "kill-server"
	Session  string            `json:"session"`  // name; "" = most recently used, or create "main"
	Detached bool              `json:"detached"` // new -d: create without attaching
	Cols     int               `json:"cols"`
	Rows     int               `json:"rows"`
	Env      map[string]string `json:"env"`      // TERM, COLORTERM, SSH_AUTH_SOCK, TMUX only
}

type Welcome struct {
	Protocol int    `json:"protocol"`
	Version  string `json:"version"`
	Session  string `json:"session"`
	Created  bool   `json:"created"`
}

type SessionInfo struct { // RESULT of "ls"
	Name     string    `json:"name"`
	Windows  int       `json:"windows"`
	Attached bool      `json:"attached"`
	Created  time.Time `json:"created"`
	Activity time.Time `json:"activity"`
}
```

Version rule: if `Hello.Protocol != ProtocolVersion` the server replies `EXIT{reason:"error"}` with the message
`server speaks protocol N; run "sitrep kill-server" and start again`.

## 7. Error handling rules

- Cross-package errors are `*errs.Error` (§4.1). Inside a package use what you like, convert at the boundary.
- Every table cell that could not be filled shows the error **kind** (`timeout`, `sudo`, `not-available`), never a
  blank and never a stack trace. The detail goes in the flash line or the `e` (errors) dialog.
- Never `panic` for input you did not control (remote output, config, user typing). `internal/app` recovers panics
  at the top level, restores the terminal, and writes `~/.cache/sitrep/crash/<timestamp>.txt`.
- Remote stderr is data, not an error, unless the exit code or a known pattern says so:

| Pattern | Kind |
|---|---|
| rc 127, or stderr contains `command not found` / `No such file or directory` for the command itself | `NotAvailable` |
| stderr contains `sudo: a password is required` or `sudo: a terminal is required` or `is not in the sudoers file` | `Sudo` |
| SSH handshake `unable to authenticate`, `no supported methods remain` | `Auth` |
| knownhosts `key mismatch` / `key is unknown` | `HostKey` |
| `dial tcp … i/o timeout`, context deadline | `Timeout` |
| `connection refused`, `no route to host`, `no such host`, `administratively prohibited` | `Unreachable` |

- Error messages are redacted (`internal/redact`) before they are stored or shown.

## 8. Concurrency model

- **UI goroutine:** Bubble Tea's. `Update` and `View`/`Render` do no I/O, take no locks that I/O goroutines hold
  for long, and finish in well under 16 ms. Target: a 2,000-row table renders only its visible rows.
- **Work:** every remote call runs in a goroutine started by a `tea.Cmd` or by a long-lived owner (pool, job
  poller, informer). Results reach the UI only as messages produced by `NextBatch` (§5), at most ~20 per second
  per stream.
- **Contexts:** `app` creates the root context. Each session derives one; each pane derives one from the session;
  each view derives one per load (`Seq` increments on refresh and the old context is canceled). Cancel on pane
  close, view pop, refresh, quit.
- **Timeouts (defaults, all configurable):** SSH connect 10 s, exec 30 s, journal/stream idle none (follow) but
  connect 10 s, Salt job 60 s, HTTP 15 s, health check 5 s. A call without a deadline is a bug.
- **Limits:** global fan-out concurrency 64; per bastion 4 connections × 8 channels; per target host 1 connection
  with up to 4 concurrent sessions; health checks every 30 s, one in flight per backend.
- **Shared state:** the SSH pool, inventory, registry, bus and investigation log are safe for concurrent use and
  guard themselves with a mutex. Views own their data and are touched only by the UI goroutine.
- **Channels:** the producer closes. Buffered (64) between workers and `NextBatch`. Senders `select` on
  `ctx.Done()` so nobody blocks forever after a cancel.
- `go test -race ./...` must pass. Tests that start goroutines must stop them (cancel + wait).

## 9. Test strategy per layer

| Layer | How | Needs lab |
|---|---|---|
| Pure logic (selector, layout tree, grouping, targeting compiler, prefix machine, protocol codec, policy, rollout math) | Table-driven unit tests; aim for every branch. | no |
| Parsers | Golden tests: input file in `testdata/<backend>/<probe>/<distro>.txt`, expected rows asserted in Go. Each parser task ships at least one inline sample; `go run ./cmd/sitrep-dev capture` (tasks 053, 067) adds real captures from Rocky 9, Ubuntu 22.04 and 24.04. Truncated and garbage inputs must not panic (fuzz-style table). | capture only |
| SSH transport | In-process SSH server (`sshtest`, task 030) with a command table, a bastion mode, a channel limit and latency/failure injection. | no |
| Backends | `transporttest.Fake` executor scripted with golden outputs. | no |
| Bus | Fake planner with scripted step results; assert phases, thresholds, abort, recorder calls. | no |
| Investigations | Write to `t.TempDir()`, compare Markdown with golden files; redaction table tests. | no |
| TUI | `teatest` with a fixed 120×36 size and the fake backend; assert on text present in the final frame, not on exact ANSI. | no |
| Session server | Client and server in one test process over a temp socket; plus one PTY-driven end-to-end test (task 082). | no |
| End to end | `//go:build lab` tests named `TestLab…`, run with `make lab-test`. They read `lab/ssh_config` and skip with a clear message when the lab is down. One per milestone demo. | **yes** |
| Scale | Fake executor with 2,000 hosts, latency and failure injection; benchmark and memory ceiling (task 123). | no |

Rules: `go test ./...` never needs the lab, the network, an SSH agent or a real TTY. Lab tests never write to
nodes except through the bus, and each one undoes its fault injection in `t.Cleanup`.

Standard commands (Makefile, task 001):

```
make build      # CGO_ENABLED=0 go build -o sitrep ./cmd/sitrep
make test       # go test -race ./...
make lint       # gofmt -l (must print nothing) + go vet ./...
make lab-test   # go test -tags lab -run 'TestLab' ./... -count=1   (requires lab/ssh_config)
make check      # lint + test + build
```

## 10. Dependencies (pinned major versions)

Added in task 001 (exact versions; do not upgrade without asking):

| Module | Version | Used for | First used |
|---|---|---|---|
| `github.com/charmbracelet/bubbletea` | v1.3.10 | TUI runtime | M0 |
| `github.com/charmbracelet/lipgloss` | v1.1.0 | styles, layout | M0 |
| `github.com/charmbracelet/bubbles` | v1.0.0 | `textinput`, `viewport`, `spinner` only | M0 |
| `github.com/charmbracelet/x/exp/teatest` | pseudo-version current on 2026-11-01 | TUI tests | M0 |
| `github.com/charmbracelet/x/ansi` | v0.11.x | strip/truncate ANSI, cell width | M0 |
| `github.com/mattn/go-runewidth` | v0.0.x | cell width | M0 |
| `gopkg.in/yaml.v3` | v3.0.1 | config, frontmatter | M0 |
| `golang.org/x/crypto` | v0.59.x | `ssh`, `ssh/agent`, `ssh/knownhosts` | M2 |
| `github.com/kevinburke/ssh_config` | v1.6.0 | read `~/.ssh/config` | M2 |
| `github.com/skeema/knownhosts` | v1.3.x | host key algorithms from known_hosts | M2 |
| `github.com/gliderlabs/ssh` | v0.3.8 | **tests only**: in-process SSH server | M2 |
| `github.com/pmezard/go-difflib` | v1.0.0 | unified diffs | M2 |
| `github.com/charmbracelet/x/vt` | v0.1.0 | terminal emulator for terminal panes | M2 |
| `github.com/creack/pty` | v1.1.24 | local PTY (local shell pane, e2e test) | M5 |
| `golang.org/x/sys` | v0.50.x | `unix` (flock, ioctl, signals) | M5 |
| `golang.org/x/term` | latest v0.x | raw mode in the client | M5 |
| `golang.org/x/sync` | v0.25.x | `errgroup`, `singleflight`, `semaphore` | M2 |

Added later, each by the first task of its milestone:

| Module | Version | Task |
|---|---|---|
| `golang.org/x/net` | v0.62.x (`proxy`) | 098 |
| `k8s.io/client-go`, `k8s.io/api`, `k8s.io/apimachinery` | v0.37.x | 105 |
| `sigs.k8s.io/yaml` | whatever client-go pulls in | 105 |
| `github.com/itchyny/gojq` | v0.12.x | 115 |

Standard library for everything else: `net/http`, `encoding/json`, `log/slog`, `regexp`, `text/template`,
`os/exec`. No vendor SDKs for Prometheus, Grafana, Elasticsearch. No cobra/viper (flag parsing is `flag`), no
testify (plain `testing`), no logging library.

## 11. Configuration schema (v0.2)

`~/.config/sitrep/config.yaml` (override the directory with `--config-dir` or `$SITREP_CONFIG_DIR`). Every key is
optional; defaults shown.

```yaml
ui:
  color: auto              # auto | truecolor | 256 | 16
mux:
  prefix: C-a
  prefix_in_tmux: C-b      # used when the client runs inside tmux; set equal to prefix to disable
  scrollback: 10000        # lines per terminal pane
  max_sync_panes: 8        # raw synchronized input limit
ssh:
  config_files: [~/.ssh/config]
  known_hosts: []          # empty = from ssh_config, else ~/.ssh/known_hosts
  concurrency: 64
  max_conns_per_bastion: 4
  max_channels_per_conn: 8
  connect_timeout: 10s
  exec_timeout: 30s
  idle_timeout: 5m
  keepalive: 30s
  max_output: 2097152
inventory:
  cache_ttl: 15m
  sources:
    - type: ssh_config
    # - type: static
    #   path: ~/.config/sitrep/hosts.yaml
    # - type: salt
  identity:
    rewrites: []           # [{from: '^(.*):9100$', to: '$1'}]
knowledge:
  root: ~/sitrep
rollout:
  canary: 1
  batch_percent: 10
  batch_max: 50
  failure_threshold_percent: 5
  typed_confirm_over: 20
salt:
  master: ""               # host (inventory ID or ssh alias); empty = Salt unconfigured
  sudo: true
  timeout: 60s
  poll_interval: 2s
  list_chunk: 200          # max minions per -L target
  suggest_over: 200        # suggest the Salt transport above this many targets
  allow_salt_ssh: false
interact:
  timeout: 2m
```

`policy.yaml`, `connections.yaml`, `keymap.yaml`, `plugins/*.yaml` are specified in SPEC §11, §7.2, §13, §12 and
loaded by their own tasks.

## 12. Risks and how they are retired early

| Risk | Why it is risky | De-risk |
|---|---|---|
| **Native SSH through a bastion at fan-out scale** | `x/crypto/ssh` has no built-in ProxyJump, pooling or limits. sshd `MaxSessions` (default 10) caps session channels per connection; `MaxStartups` (default 10:30:100) drops bursts of unauthenticated connections; one TCP connection to the bastion carries every forwarded channel. | Tasks 030–033 build an in-process server that enforces a channel limit, and the pool is tested against it. Task 035 runs a 9-host × 5-round fan-out through the lab bastion with `MaxSessions 10`. `max_conns_per_bastion` and `max_channels_per_conn` are config, not constants. Connect bursts are smoothed by the per-bastion semaphore. |
| **known_hosts with several key types** | Plain `x/crypto/ssh/knownhosts` fails with "key mismatch" when the server offers a key type other than the one stored. | Use `skeema/knownhosts` to set `HostKeyAlgorithms` (task 029). Test with an ed25519-only entry against a server that also has RSA. |
| **Terminal panes (VT emulator)** | `charmbracelet/x/vt` is v0.1.0; the emulator must have its response pipe drained or writes block; full-screen programs (vim, top, less) stress it. | Task 049 is a spike with a hard go/no-go test list behind our own `vt.Emulator` interface. Fallback: `github.com/vito/midterm` behind the same interface (ask before switching). |
| **Detach/reattach** | Running Bubble Tea without a real TTY: no raw mode, no size, no colour detection; repaint after reattach. | Task 025 (M1) runs the full app over `io.Pipe` with injected size and asserts frames. The detach-safety rules (§6.1) apply from task 001. M5 then adds only the socket. |
| **Bubble Tea message floods** | 1,500 results as 1,500 messages freeze the UI. | `NextBatch` is the only streaming pattern (task 011) and the scale test (123) enforces it. |
| **Salt CLI output variability** | Minions that do not return, mixed stderr noise, non-JSON lines, state returns that are a list of error strings instead of a dict, version differences. | Async job engine (task 071) so "no return" is explicit. Tolerant decoder (task 066). Real captures from the lab in `testdata/salt/` (task 067) before the parsers that depend on them. |
| **Salt argument parsing** | The Salt CLI YAML-parses arguments; `a=b` words become kwargs; quoting through SSH + sudo + salt is three layers. | Salt-as-transport sends the script base64-encoded: `cmd.run_all cmd='echo <b64> \| base64 -d \| sh' python_shell=True` (task 073), with a round-trip test of hostile strings. |
| **Shell quoting / injection** | User text and labels flow into remote command lines. | One package (`internal/shq`, task 027) with fuzz-style tests; nothing else builds command strings by concatenation. |
| **client-go size and build time** | Adds ~30–40 MB and slow builds. | Isolated in `internal/backend/k8s`; task 105 records the binary size before/after and fails if > 80 MB. Accepted in the spec. |
| **A fast implementer drifting from the design** | Small local "fixes" that change contracts. | Contracts are in this file; tasks name exact files; AGENTS.md says stop-and-ask; every task has runnable acceptance commands. |

## 13. Conventions for the implementer

- One package per directory as listed in §3. File names in tasks are exact.
- Exported identifiers have doc comments. No `utils`/`common`/`helpers` packages.
- Tests live next to the code (`x_test.go`, package `x` or `x_test`). Table-driven. No sleeps longer than 50 ms;
  use channels and contexts with short timeouts.
- Time is injected (`Now func() time.Time`) wherever output depends on it.
- User-visible strings: sentence case, no trailing period in flashes, numbers with thousands separators.
- Hosts in examples and testdata: `*.lab.example.internal` or `*.example.internal`. IPs from `192.0.2.0/24`,
  `198.51.100.0/24` or `10.x` lab ranges only.
- Helper tooling is Go only (D13). Golden files are refreshed with `go test ./... -run <Test> -update` (a `-update` test flag defined in the package test), never with a script.
- Commit message: `task NNN: <what changed>`; one commit per task.
