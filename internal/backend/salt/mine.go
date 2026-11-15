package salt

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"jumpr/internal/errs"
	"jumpr/internal/redact"
	"jumpr/internal/resource"
)

// Mine and grains reads (SPEC §8.6 "Grains" and "Mine").
//
// Both are read on the master and never on a minion unless the user asks for
// it: the grains explorer and the mine browser serve the master's cache
// (`salt-run cache.grains`, `salt-run cache.mine`), and only `:grains --live`
// runs `grains.items` on the minion itself, to detect a stale cache.
const (
	opMine        = "salt.mine"
	opMineValue   = "salt.mine.value"
	opGrainsCache = "salt.grains.cache"
	opGrainsLive  = "salt.grains.live"

	// mineValueWidth is the widest compact mine value a row carries.
	mineValueWidth = 80
	// mineCap is the most of one mine value that is ever rendered. Mine data
	// is whatever an admin configured and can be large; 256 KiB is a screen,
	// not a file.
	mineCap = 256 << 10
)

// mineSecretPatterns are the extra redaction rules for mine values, which are
// JSON by construction: the built-in assignments rule expects `key=value` or
// `key: value` and a quoted JSON key (`"password": "…"`) slips past it.
func mineSecretPatterns() []string {
	return []string{
		`(?i)"(pass(word|wd|phrase)?|secret|token|api[_-]?key|private[_-]?key)"\s*:\s*("[^"]*"|[^,}\s]+)`,
	}
}

// mineRedactor returns the redactor mine values pass through before they are
// shown: the built-ins plus the JSON key shape.
func mineRedactor() *redact.Redactor {
	red, err := redact.New(mineSecretPatterns())
	if err != nil {
		return nil
	}
	return red
}

// MineEntry is one mine function value of one minion, as the master's cache
// holds it: the value is compacted to one line of at most mineValueWidth cells
// and redacted; Size is the byte length of the cached JSON value.
type MineEntry struct {
	Minion   string
	Function string
	Value    string
	Size     int64
}

// GrainDiff is one grain key whose cached and live value differ. A side the
// other does not have is "" (missing).
type GrainDiff struct {
	Key    string
	Cached string
	Live   string
}

// GrainValue is one distinct value of a grain and the number of minions that
// report it.
type GrainValue struct {
	Value string
	Count int
}

// Mine reads the master's mine cache for the whole fleet: one entry per minion
// and mine function, sorted by minion and then function. Values are compacted
// and redacted here; nothing larger than one cell's worth of text is kept.
func (b *Backend) Mine(ctx context.Context) ([]MineEntry, error) {
	out, err := b.mineCache(ctx, "*")
	if err != nil {
		return nil, err
	}
	red := mineRedactor()
	entries := make([]MineEntry, 0, len(out))
	for _, minion := range sortedKeys(out) {
		var funcs map[string]json.RawMessage
		if err := json.Unmarshal(out[minion], &funcs); err != nil {
			// A mine entry that is not an object is skipped, never fatal.
			continue
		}
		for _, fun := range sortedKeys(funcs) {
			raw := bytes.TrimSpace(funcs[fun])
			entries = append(entries, MineEntry{
				Minion:   minion,
				Function: fun,
				Value:    compactMine(GrainText(raw), red),
				Size:     int64(len(raw)),
			})
		}
	}
	return entries, nil
}

// MineValue returns one mine value in full: pretty-printed, redacted and cut
// at mineCap. It is read from the master's cache with a target of that minion
// alone, so the rest of the fleet is never fetched for one value.
func (b *Backend) MineValue(ctx context.Context, minion, function string) (string, error) {
	minion = strings.TrimSpace(minion)
	function = strings.TrimSpace(function)
	if minion == "" || !minionIDRe.MatchString(minion) {
		return "", errs.New(errs.Config, opMineValue, fmt.Sprintf("invalid minion id %q", minion))
	}
	if function == "" || !funRe.MatchString(function) {
		return "", errs.New(errs.Config, opMineValue, fmt.Sprintf("invalid mine function %q", function))
	}
	out, err := b.mineCache(ctx, minion)
	if err != nil {
		return "", err
	}
	raw, ok := mineRaw(out, minion, function)
	if !ok {
		return "", errs.New(errs.NotAvailable, opMineValue,
			"no cached value for "+function).WithHost(minion)
	}
	red := mineRedactor()
	text := prettyJSON(raw)
	if red != nil {
		return red.Cap(text, mineCap), nil
	}
	return text, nil
}

// mineCache reads the master's mine cache for one target expression and
// decodes the minion → function → value object.
func (b *Backend) mineCache(ctx context.Context, tgt string) (map[string]json.RawMessage, error) {
	if b.runner == nil {
		return nil, errs.New(errs.Config, opMine, "salt backend not configured: no runner")
	}
	out, err := b.runner.Run(ctx, Call{
		Tool:   toolRun,
		Fun:    "cache.mine",
		Kwargs: map[string]string{"tgt": tgt},
	})
	if err != nil {
		return nil, err
	}
	return DecodeObject(out)
}

// mineRaw picks one mine value out of a decoded cache reply. The documented
// shape is minion → function → value; a flat function → value reply (a master
// that answered for a single minion) is accepted as well.
func mineRaw(out map[string]json.RawMessage, minion, function string) (json.RawMessage, bool) {
	if raw, ok := out[minion]; ok {
		var funcs map[string]json.RawMessage
		if err := json.Unmarshal(raw, &funcs); err == nil {
			if v, ok := funcs[function]; ok {
				return v, true
			}
		}
	}
	if len(out) == 1 {
		for _, raw := range out {
			var funcs map[string]json.RawMessage
			if err := json.Unmarshal(raw, &funcs); err == nil {
				if v, ok := funcs[function]; ok {
					return v, true
				}
			}
		}
	}
	if v, ok := out[function]; ok {
		return v, true
	}
	return nil, false
}

// AllGrains reads the whole grains cache of the fleet (`cache.grains tgt='*'`).
// The grains explorer uses it for the distribution of one grain across every
// minion; nothing is read from the minions themselves.
func (b *Backend) AllGrains(ctx context.Context) (map[string]Grains, error) {
	all, err := b.cacheGrains(ctx, "*")
	if err != nil {
		return nil, err
	}
	if len(all) == 0 {
		return nil, errs.New(errs.NotAvailable, opGrainsCache, EmptyCacheHint)
	}
	return all, nil
}

// MinionGrains reads one minion's grains from the master's cache. A minion the
// cache does not know is a NotAvailable error naming the live read, never an
// empty map that looks like a minion without grains.
func (b *Backend) MinionGrains(ctx context.Context, minion string) (Grains, error) {
	minion = strings.TrimSpace(minion)
	if minion == "" || !minionIDRe.MatchString(minion) {
		return nil, errs.New(errs.Config, opGrainsCache, fmt.Sprintf("invalid minion id %q", minion))
	}
	all, err := b.cacheGrains(ctx, minion)
	if err != nil {
		return nil, err
	}
	if g, ok := all[minion]; ok {
		return g, nil
	}
	if len(all) == 1 {
		for _, g := range all {
			return g, nil
		}
	}
	return nil, errs.New(errs.NotAvailable, opGrainsCache,
		"no cached grains; try :grains --live "+minion).WithHost(minion)
}

// cacheGrains reads the grains cache for one target expression.
func (b *Backend) cacheGrains(ctx context.Context, tgt string) (map[string]Grains, error) {
	if b.runner == nil {
		return nil, errs.New(errs.Config, opGrainsCache, "salt backend not configured: no runner")
	}
	out, err := b.runner.Run(ctx, Call{
		Tool:   toolRun,
		Fun:    "cache.grains",
		Kwargs: map[string]string{"tgt": tgt},
	})
	if err != nil {
		return nil, err
	}
	raw, err := DecodeObject(out)
	if err != nil {
		return nil, err
	}
	all := make(map[string]Grains, len(raw))
	for minion, v := range raw {
		var g Grains
		if err := json.Unmarshal(v, &g); err != nil {
			continue
		}
		if g == nil {
			g = Grains{}
		}
		all[minion] = g
	}
	return all, nil
}

// LiveGrains runs grains.items on one minion, as a job: the job engine answers
// with the minion's own grains, and a minion that never returns is a timeout,
// never an empty map. The caller diffs the answer against the cache to find a
// stale one.
func (b *Backend) LiveGrains(ctx context.Context, minion string) (Grains, error) {
	minion = strings.TrimSpace(minion)
	if minion == "" || !minionIDRe.MatchString(minion) {
		return nil, errs.New(errs.Config, opGrainsLive, fmt.Sprintf("invalid minion id %q", minion))
	}
	if b.runner == nil {
		return nil, errs.New(errs.Config, opGrainsLive, "salt backend not configured: no runner")
	}
	job, err := b.runner.Start(ctx, Submit{
		Targets: []Target{{Type: "list", Expr: minion}},
		Fun:     "grains.items",
		Expect:  []string{minion},
	})
	if err != nil {
		return nil, err
	}
	defer job.Cancel()

	var (
		raw     json.RawMessage
		missing error
	)
	for mr := range job.Results() {
		returned := bytes.TrimSpace(mr.Return)
		if mr.NoReturn || len(returned) == 0 || isNull(returned) || NoReturn(returned) {
			if missing == nil {
				missing = errs.New(errs.Timeout, opGrainsLive, "no return from "+mr.Minion)
			}
			continue
		}
		// Anything but an object is Salt reporting a failure.
		if !bytes.HasPrefix(returned, []byte("{")) {
			if missing == nil {
				missing = errs.New(errs.Remote, opGrainsLive,
					"grains.items did not return an object").WithHost(mr.Minion)
			}
			continue
		}
		raw = mr.Return
	}
	if len(raw) == 0 {
		if missing != nil {
			return nil, missing
		}
		return nil, errs.New(errs.Parse, opGrainsLive, "grains.items returned nothing").WithHost(minion)
	}
	var g Grains
	if err := json.Unmarshal(raw, &g); err != nil {
		return nil, errs.New(errs.Parse, opGrainsLive, "grains.items did not return an object").WithHost(minion)
	}
	if g == nil {
		g = Grains{}
	}
	return g, nil
}

// DiffGrains compares a cached grain set with a live one and returns the keys
// whose value changed, appeared or disappeared, sorted by key. An empty result
// means the cache is up to date.
func DiffGrains(cached, live Grains) []GrainDiff {
	keys := make([]string, 0, len(cached)+len(live))
	seen := make(map[string]bool, len(cached)+len(live))
	for _, set := range []Grains{cached, live} {
		for k := range set {
			if !seen[k] {
				seen[k] = true
				keys = append(keys, k)
			}
		}
	}
	sort.Strings(keys)
	var out []GrainDiff
	for _, k := range keys {
		was, inCache := cached[k]
		now, inLive := live[k]
		left, right := GrainText(was), GrainText(now)
		switch {
		case !inLive:
			out = append(out, GrainDiff{Key: k, Cached: left})
		case !inCache:
			out = append(out, GrainDiff{Key: k, Live: right})
		case left != right:
			out = append(out, GrainDiff{Key: k, Cached: left, Live: right})
		}
	}
	return out
}

// GrainDistribution counts, across every minion of the cache, how often each
// distinct value of one grain appears. A list grain counts once per minion and
// element. Rows are ordered by count (descending), then value.
func GrainDistribution(all map[string]Grains, key string) []GrainValue {
	counts := make(map[string]int, len(all))
	for _, g := range all {
		seen := make(map[string]bool)
		for _, v := range grainValues(g, key) {
			if v == "" || seen[v] {
				continue
			}
			seen[v] = true
			counts[v]++
		}
	}
	out := make([]GrainValue, 0, len(counts))
	for v, n := range counts {
		out = append(out, GrainValue{Value: v, Count: n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Value < out[j].Value
	})
	return out
}

// MinionsWithGrain returns the sorted minion IDs whose grain key holds value.
// A list grain matches on any of its elements.
func MinionsWithGrain(all map[string]Grains, key, value string) []string {
	var out []string
	for minion, g := range all {
		for _, v := range grainValues(g, key) {
			if v == value {
				out = append(out, minion)
				break
			}
		}
	}
	sort.Strings(out)
	return out
}

// GrainText renders one grain or mine value as one line: a string as it is, a
// number or bool the way JSON spells it, a list of scalars comma-joined and
// anything nested as compact JSON.
func GrainText(raw json.RawMessage) string {
	if s, ok := scalarValue(raw); ok {
		return s
	}
	if list, ok := scalarList(raw); ok {
		return strings.Join(list, ",")
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return ""
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, trimmed); err == nil {
		return buf.String()
	}
	return string(trimmed)
}

// mineRows is the List path of the salt-mine kind: one row per minion and mine
// function.
func (b *Backend) mineRows(ctx context.Context) ([]resource.Resource, error) {
	entries, err := b.Mine(ctx)
	if err != nil {
		return nil, err
	}
	items := make([]resource.Resource, 0, len(entries))
	for _, e := range entries {
		items = append(items, resource.Resource{
			Ref: resource.Ref{Backend: backendName, Kind: KindMine, Namespace: e.Minion, Name: e.Function},
			Fields: map[string]any{
				"minion":   e.Minion,
				"function": e.Function,
				"value":    e.Value,
				"size":     e.Size,
			},
			FetchedAt: b.now(),
		})
	}
	return items, nil
}

// grainValues is every scalar value one grain key holds on one minion.
func grainValues(g Grains, key string) []string {
	raw := g[key]
	if list, ok := scalarList(raw); ok {
		return list
	}
	if s, ok := scalarValue(raw); ok {
		return []string{s}
	}
	return nil
}

// compactMine renders a mine value as one cell's worth of text: whitespace
// runs collapse to one space, the text is cut at mineValueWidth and the result
// is redacted last, so a truncated secret still matches its pattern.
func compactMine(text string, red *redact.Redactor) string {
	text = strings.Join(strings.Fields(text), " ")
	if r := []rune(text); len(r) > mineValueWidth {
		text = string(r[:mineValueWidth-1]) + "…"
	}
	if red != nil {
		text = red.String(text)
	}
	return text
}

// prettyJSON renders one JSON value as indented text: a bare string as it is,
// anything else with two-space indentation. Unparsable input is returned
// trimmed.
func prettyJSON(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(trimmed, &s); err == nil {
		return s
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return string(trimmed)
	}
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return string(trimmed)
	}
	return string(out)
}

// sortedKeys returns the keys of a raw JSON object, sorted.
func sortedKeys(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
