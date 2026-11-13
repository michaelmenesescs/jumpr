package salt

import (
	"bytes"
	"context"
	"encoding/json"
	"path"
	"strconv"
	"strings"
	"unicode/utf8"
)

// maxLabelLen is the longest grain value kept as a label. Longer values are
// dropped: the cap keeps secrets-looking grains (keys, certificates, long
// free text) out of the inventory.
const maxLabelLen = 64

// builtinLabelKeys are exposed for every minion, whether or not they are in
// the denylist and without the length cap.
var builtinLabelKeys = []string{
	"id", "os", "os_family", "osrelease", "osfinger", "kernelrelease",
	"saltversion", "virtual", "num_cpus", "mem_total",
}

// noisyGrains are never turned into labels: they are long, nested, machine
// specific or secret-looking. Patterns are matched with path.Match.
var noisyGrains = []string{
	"path", "pythonpath", "gpus", "hwaddr_interfaces", "ip_interfaces",
	"ip4_interfaces", "ip6_interfaces", "dns", "locale_info", "systemd",
	"disks", "ssds", "server_id", "machine_id", "uuid", "serialnumber",
	"saltpath", "pythonexecutable", "pythonversion", "zmqversion",
	"lsb_distrib_*", "cpu_flags", "cpu_model", "biosversion",
	"biosreleasedate", "productname", "manufacturer", "master", "shell",
	"ps", "groupname", "username", "gid", "uid", "pid", "cwd",
	"transactional", "efi*", "init",
}

// Grains is one minion's grains, decoded lazily: every value stays raw until
// a caller asks for it.
type Grains map[string]json.RawMessage

// Str returns the string form of key: a string as-is, a number or bool
// formatted the way JSON spells it. It returns "" when the key is missing or
// the value is not a scalar.
func (g Grains) Str(key string) string {
	s, _ := scalarValue(g[key])
	return s
}

// StrList returns key as a list of strings. A JSON array of scalars yields one
// item per element; a single scalar yields one item; anything else yields nil.
// An empty list yields an empty, non-nil slice.
func (g Grains) StrList(key string) []string {
	if list, ok := scalarList(g[key]); ok {
		return list
	}
	if s, ok := scalarValue(g[key]); ok {
		return []string{s}
	}
	return nil
}

// Labels turns one minion's grains into inventory labels. The built-in keys
// are always exposed; every other top-level scalar (or list of scalars) is
// exposed unless it is denied as noisy. extra names grain keys that are always
// exposed, even when the denylist would drop them.
func Labels(g Grains, extra []string) map[string]string {
	out := make(map[string]string, len(g)+len(builtinLabelKeys))
	for _, k := range builtinLabelKeys {
		if s, ok := scalarValue(g[k]); ok {
			out[k] = s
		}
	}
	forced := make(map[string]bool, len(extra))
	for _, k := range extra {
		forced[k] = true
		if _, ok := out[k]; ok {
			continue
		}
		if v, ok := labelValue(g[k]); ok {
			out[k] = v
		}
	}
	for k, raw := range g {
		if forced[k] || isBuiltinKey(k) || isNoisyGrain(k) {
			continue
		}
		if v, ok := labelValue(raw); ok {
			out[k] = v
		}
	}
	return out
}

// KnownGrainKeys returns every grain key that at least one minion reports as a
// scalar or a list of scalars. The targeting compiler uses it to decide
// whether a selector key can become a Salt grain target (`G@role:web`).
func KnownGrainKeys(all map[string]Grains) map[string]bool {
	out := make(map[string]bool)
	for _, g := range all {
		for k, raw := range g {
			if _, ok := scalarValue(raw); ok {
				out[k] = true
				continue
			}
			if _, ok := scalarList(raw); ok {
				out[k] = true
			}
		}
	}
	return out
}

// CachedGrains reads the master's grains cache without contacting a minion.
// The cache is a map of minion id to grains; values that are not objects are
// skipped rather than failing the call.
func (r *Runner) CachedGrains(ctx context.Context) (map[string]Grains, error) {
	out, err := r.Run(ctx, Call{
		Tool:   toolRun,
		Fun:    "cache.grains",
		Kwargs: map[string]string{"tgt": "*"},
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

// labelValue renders a grain as a label value: scalars directly, lists of
// scalars comma-joined. Values longer than maxLabelLen, empty values and
// anything nested are dropped.
func labelValue(raw json.RawMessage) (string, bool) {
	if s, ok := scalarValue(raw); ok {
		return cappedLabel(s)
	}
	if list, ok := scalarList(raw); ok {
		return cappedLabel(strings.Join(list, ","))
	}
	return "", false
}

func cappedLabel(s string) (string, bool) {
	if s == "" || utf8.RuneCountInString(s) > maxLabelLen {
		return "", false
	}
	return s, true
}

// isBuiltinKey reports whether k is already handled as a built-in label.
func isBuiltinKey(k string) bool {
	for _, b := range builtinLabelKeys {
		if k == b {
			return true
		}
	}
	return false
}

// isNoisyGrain matches k against the denylist, including its glob entries.
func isNoisyGrain(k string) bool {
	for _, pat := range noisyGrains {
		if k == pat {
			return true
		}
		if strings.ContainsAny(pat, "*?[") {
			if ok, err := path.Match(pat, k); err == nil && ok {
				return true
			}
		}
	}
	return false
}

// decodeAny decodes one raw JSON value, keeping numbers as written.
func decodeAny(raw json.RawMessage) (any, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, false
	}
	return v, true
}

// scalarAny renders a decoded JSON scalar as a string.
func scalarAny(v any) (string, bool) {
	switch t := v.(type) {
	case string:
		return t, true
	case bool:
		return strconv.FormatBool(t), true
	case json.Number:
		return t.String(), true
	}
	return "", false
}

// scalarValue renders raw as a string when it is a JSON string, number or bool.
func scalarValue(raw json.RawMessage) (string, bool) {
	v, ok := decodeAny(raw)
	if !ok {
		return "", false
	}
	return scalarAny(v)
}

// scalarList renders raw as strings when it is a JSON array whose elements are
// all scalars. Lists with any nested element are rejected.
func scalarList(raw json.RawMessage) ([]string, bool) {
	v, ok := decodeAny(raw)
	if !ok {
		return nil, false
	}
	arr, ok := v.([]any)
	if !ok {
		return nil, false
	}
	out := make([]string, 0, len(arr))
	for _, item := range arr {
		s, ok := scalarAny(item)
		if !ok {
			return nil, false
		}
		out = append(out, s)
	}
	return out, true
}
