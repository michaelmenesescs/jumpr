package salt

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"jumpr/internal/errs"
	"jumpr/internal/resource"
	"jumpr/internal/shq"
	"jumpr/internal/transport"
)

// The event kinds of the master's bus, as jumpr names them (task 096). They
// come from the tag, never from the payload, so a payload jumpr cannot read
// still lands in the right kind.
const (
	EventJobNew      = "job-new"
	EventJobRet      = "job-ret"
	EventAuth        = "auth"
	EventMinionStart = "minion-start"
	EventBeacon      = "beacon"
	EventKey         = "key"
	EventRunner      = "runner"
	EventOther       = "other"
)

const (
	// eventsOp is the Op of every error built here.
	eventsOp = "salt.Events"
	// eventSubBuffer is how far one pane may fall behind before the hub drops
	// events for it. A paused pane keeps consuming; a stuck one must never
	// stall the stream.
	eventSubBuffer = 256
	// jobTagPrefix, minionTagPrefix and beaconTagPrefix are the tag roots the
	// kind and the identity are read from.
	jobTagPrefix    = "salt/job/"
	minionTagPrefix = "salt/minion/"
	beaconTagPrefix = "salt/beacon/"
	runTagPrefix    = "salt/run/"
)

// eventTimeLayouts are the `_stamp` shapes Salt prints, most precise first.
var eventTimeLayouts = []string{
	"2006-01-02T15:04:05.999999999",
	"2006-01-02T15:04:05.999999",
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02T15:04:05",
}

// Event is one line of the master's event bus. Tag is the Salt event tag;
// Kind is jumpr's classification of it; Minion, JID and Fun are filled from
// the tag and the payload when it names them. Data is the raw JSON payload.
type Event struct {
	Tag    string
	Time   time.Time
	Kind   string
	Minion string
	JID    string
	Fun    string
	Data   json.RawMessage
}

// EventCommand is the shell line that streams the master's event bus. It runs
// until it is canceled: it is a stream, not a call with a deadline.
func EventCommand() string {
	return shq.Join(toolRun, "state.event", "pretty=False")
}

// OwnRunner reports whether fun is one of the runner functions jumpr runs
// itself while polling, so the event screen can hide jumpr's own noise.
func OwnRunner(fun string) bool {
	switch strings.TrimSpace(fun) {
	case "manage.status", "manage.versions", "cache.grains", "cache.mine",
		"jobs.active", "jobs.list_job", "jobs.list_jobs":
		return true
	}
	return false
}

// Events subscribes to the master's event bus. The channel closes when the
// stream ends (a master restart closes it) or when ctx is done; an error is
// returned only when no stream can be started.
//
// One stream serves the whole session: several panes subscribe to the same
// hub, because `state.event` holds one of the master's few connection slots
// for as long as it runs.
func (b *Backend) Events(ctx context.Context) (<-chan Event, error) {
	if b == nil || b.master == "" || b.runner == nil {
		return nil, errs.New(errs.Config, eventsOp, "salt backend not configured: no master or no runner")
	}
	st := b.streamer()
	if st == nil {
		return nil, errs.New(errs.Config, eventsOp, "no transport streamer configured")
	}
	h := b.hub()
	if err := h.start(st, b.master, transport.Command{
		Script: EventCommand(),
		Sudo:   b.runner.sudo,
	}); err != nil {
		return nil, err
	}
	return h.subscribe(ctx), nil
}

// hub returns the session's one event hub, creating it on first use. The
// backend is safe for concurrent use, so the hub is created once.
func (b *Backend) hub() *eventHub {
	b.eventsMu.Lock()
	defer b.eventsMu.Unlock()
	if b.events == nil {
		b.events = newEventHub()
	}
	return b.events
}

// streamer is the transport Streamer the runner executes on. The Salt backend
// is built over the SSH transport, which streams the event bus the same way it
// follows a journal; an executor that cannot stream means no events.
func (b *Backend) streamer() transport.Streamer {
	if b.runner == nil || b.runner.ex == nil {
		return nil
	}
	st, _ := b.runner.ex.(transport.Streamer)
	return st
}

// eventHub is the session's single `state.event` stream. It starts with the
// first subscriber and stops with the last, so opening and closing event panes
// neither hoards connection slots nor restarts the stream needlessly.
type eventHub struct {
	mu      sync.Mutex
	subs    map[int]chan Event
	nextID  int
	cancel  context.CancelFunc
	running bool
}

// newEventHub returns an idle hub.
func newEventHub() *eventHub {
	return &eventHub{subs: make(map[int]chan Event)}
}

// start launches the stream unless one is already running. The stream is tied
// to the hub, not to the caller's context: one pane closing must not take the
// bus away from the others.
func (h *eventHub) start(st transport.Streamer, host resource.HostID, cmd transport.Command) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.running {
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	stream, err := st.Start(ctx, host, cmd)
	if err != nil {
		cancel()
		return err
	}
	h.cancel = cancel
	h.running = true
	go h.loop(ctx, stream)
	return nil
}

// loop reads the stream until it ends, forwarding the lines that parse.
func (h *eventHub) loop(ctx context.Context, st transport.Stream) {
	for {
		select {
		case <-ctx.Done():
			h.finish()
			return
		case line, ok := <-st.Lines():
			if !ok {
				// The command ended by itself: a master restart, or a
				// dropped connection. Every pane sees the stream close.
				h.finish()
				return
			}
			ev, ok := ParseEventLine(line)
			if !ok {
				continue
			}
			h.broadcast(ev)
		}
	}
}

// finish marks the stream dead and closes every subscriber's channel, so each
// pane can offer to reconnect.
func (h *eventHub) finish() {
	h.mu.Lock()
	h.running = false
	if h.cancel != nil {
		h.cancel()
		h.cancel = nil
	}
	subs := h.subs
	h.subs = make(map[int]chan Event, len(subs))
	h.mu.Unlock()
	for _, ch := range subs {
		close(ch)
	}
}

// subscribe adds one pane. The channel closes when the stream ends or ctx is
// done.
func (h *eventHub) subscribe(ctx context.Context) <-chan Event {
	ch := make(chan Event, eventSubBuffer)
	h.mu.Lock()
	id := h.nextID
	h.nextID++
	h.subs[id] = ch
	h.mu.Unlock()
	go func() {
		<-ctx.Done()
		h.unsubscribe(id)
	}()
	return ch
}

// unsubscribe drops one pane; the last one out stops the stream.
func (h *eventHub) unsubscribe(id int) {
	h.mu.Lock()
	ch, ok := h.subs[id]
	if ok {
		delete(h.subs, id)
	}
	var cancel context.CancelFunc
	if ok && len(h.subs) == 0 && h.running {
		cancel = h.cancel
	}
	h.mu.Unlock()
	if ok {
		close(ch)
	}
	if cancel != nil {
		cancel()
	}
}

// broadcast hands ev to every pane. A pane that cannot keep up loses the event
// rather than slowing the stream down.
func (h *eventHub) broadcast(ev Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, ch := range h.subs {
		select {
		case ch <- ev:
		default:
		}
	}
}

// ParseEventLine parses one line of `state.event pretty=False` output and
// reports whether it was an event. A line without a tag, or whose payload is
// not a JSON object, is skipped: remote output never causes a panic.
func ParseEventLine(line []byte) (Event, bool) {
	tag, payload, ok := splitEventLine(string(line))
	if !ok {
		return Event{}, false
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(payload), &fields); err != nil {
		return Event{}, false
	}
	data := make(json.RawMessage, len(payload))
	copy(data, payload)
	ev := Event{
		Tag:    tag,
		Time:   stampOf(fields),
		Kind:   EventKind(tag),
		Minion: minionOf(tag, fields),
		JID:    jidOf(tag, fields),
		Fun:    stringField(fields, "fun"),
		Data:   data,
	}
	return ev, true
}

// splitEventLine separates the tag from the JSON payload at the first tab, or
// at the first space when the line has none.
func splitEventLine(line string) (tag, payload string, ok bool) {
	line = strings.TrimRight(line, "\r\n")
	if i := strings.IndexByte(line, '\t'); i > 0 {
		return strings.TrimSpace(line[:i]), strings.TrimSpace(line[i+1:]), true
	}
	if i := strings.IndexByte(line, ' '); i > 0 {
		return strings.TrimSpace(line[:i]), strings.TrimSpace(line[i+1:]), true
	}
	return "", "", false
}

// EventKind classifies a Salt event tag.
func EventKind(tag string) string {
	switch {
	case strings.HasPrefix(tag, jobTagPrefix) && strings.Contains(tag, "/ret/"):
		return EventJobRet
	case strings.HasPrefix(tag, jobTagPrefix) && strings.HasSuffix(tag, "/new"):
		return EventJobNew
	case tag == "salt/auth" || strings.HasPrefix(tag, "salt/auth/"):
		return EventAuth
	case tag == "minion_start" || strings.HasPrefix(tag, minionTagPrefix) && strings.HasSuffix(tag, "/start"):
		return EventMinionStart
	case strings.HasPrefix(tag, beaconTagPrefix):
		return EventBeacon
	case tag == "salt/key" || strings.HasPrefix(tag, "salt/key/"):
		return EventKey
	case strings.HasPrefix(tag, runTagPrefix):
		return EventRunner
	}
	return EventOther
}

// minionOf is the minion an event is about: the payload's id when it has one,
// else the one the tag names.
func minionOf(tag string, fields map[string]json.RawMessage) string {
	if id := stringField(fields, "id"); id != "" {
		return id
	}
	if strings.HasPrefix(tag, jobTagPrefix) {
		if rest, ok := strings.CutPrefix(tag, jobTagPrefix); ok {
			if _, after, found := strings.Cut(rest, "/ret/"); found && after != "" {
				return after
			}
		}
		return ""
	}
	for _, prefix := range []string{minionTagPrefix, beaconTagPrefix} {
		rest, ok := strings.CutPrefix(tag, prefix)
		if !ok || rest == "" {
			continue
		}
		if id, _, found := strings.Cut(rest, "/"); found && id != "" {
			return id
		}
		// A tag with no trailing segment names the minion itself.
		return rest
	}
	return ""
}

// jidOf is the job or runner id of an event, from the payload or the tag.
func jidOf(tag string, fields map[string]json.RawMessage) string {
	if jid := stringField(fields, "jid"); jid != "" {
		return jid
	}
	for _, prefix := range []string{jobTagPrefix, runTagPrefix} {
		rest, ok := strings.CutPrefix(tag, prefix)
		if !ok {
			continue
		}
		if jid, _, found := strings.Cut(rest, "/"); found && jid != "" {
			return jid
		}
	}
	return ""
}

// stampOf is the event time from the payload's `_stamp`.
func stampOf(fields map[string]json.RawMessage) time.Time {
	raw := stringField(fields, "_stamp")
	if raw == "" {
		return time.Time{}
	}
	for _, layout := range eventTimeLayouts {
		if t, err := time.Parse(layout, raw); err == nil {
			return t
		}
	}
	return time.Time{}
}

// stringField reads one string field of a payload, empty when absent or of
// another type.
func stringField(fields map[string]json.RawMessage, key string) string {
	raw, ok := fields[key]
	if !ok {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return strings.TrimSpace(s)
}
