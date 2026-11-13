package salt

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"

	"jumper/internal/errs"
	"jumper/internal/resource"
)

// Key states as reported by salt-key -L.
const (
	KeyAccepted = "accepted"
	KeyPending  = "pending"
	KeyRejected = "rejected"
	KeyDenied   = "denied"
)

// Key is one entry of salt-key -L. ID is the minion ID as Salt knows it; it is
// not necessarily a hostname jumper can resolve.
type Key struct {
	ID    string
	State string
}

// ManageStatus is salt-run manage.status: the minions answering and the ones
// that do not.
type ManageStatus struct {
	Up   []string
	Down []string
}

// Versions is salt-run manage.versions reduced to what the table shows: the
// master's version and a version per minion. IDs are not repeated across the
// two maps.
type Versions struct {
	Master         string
	UpToDate       map[string]string
	RequiresUpdate map[string]string
}

// keyStateRank orders rows for the salt-key kind: pending keys are what an
// operator acts on, so they come first.
var keyStateRank = map[string]int{
	KeyPending:  0,
	KeyAccepted: 1,
	KeyRejected: 2,
	KeyDenied:   3,
}

// keyListSections maps the salt-key -L fields onto their states, in the order
// the CLI prints them.
var keyListSections = []struct {
	field string
	state string
}{
	{"minions", KeyAccepted},
	{"minions_pre", KeyPending},
	{"minions_rejected", KeyRejected},
	{"minions_denied", KeyDenied},
}

// ParseKeys decodes salt-key -L. Rows come out grouped by state in the CLI's
// order and sorted by ID inside each state; List reorders them for display.
func ParseKeys(b []byte) ([]Key, error) {
	top, err := decodeTopObject(b, "salt.keys")
	if err != nil {
		return nil, err
	}
	var out []Key
	for _, sec := range keyListSections {
		raw, ok := top[sec.field]
		if !ok {
			continue
		}
		ids, err := parseIDList(raw)
		if err != nil {
			return nil, errs.New(errs.Parse, "salt.keys", sec.field+": "+err.Error())
		}
		sort.Strings(ids)
		for _, id := range ids {
			out = append(out, Key{ID: id, State: sec.state})
		}
	}
	return out, nil
}

// ParseFingerprints decodes salt-key -F. The master's own key ("local") is not
// a minion and is dropped.
func ParseFingerprints(b []byte) (map[string]string, error) {
	top, err := decodeTopObject(b, "salt.keys")
	if err != nil {
		return nil, err
	}
	out := make(map[string]string)
	for _, sec := range keyListSections {
		raw, ok := top[sec.field]
		if !ok {
			continue
		}
		var m map[string]string
		if err := json.Unmarshal(raw, &m); err != nil {
			continue // an unexpected shape costs fingerprints, not the list
		}
		for id, fp := range m {
			out[id] = fp
		}
	}
	return out, nil
}

// ParseManageStatus decodes salt-run manage.status. Unknown or missing
// sections read as empty.
func ParseManageStatus(b []byte) (ManageStatus, error) {
	top, err := decodeTopObject(b, "salt.status")
	if err != nil {
		return ManageStatus{}, err
	}
	var st ManageStatus
	if raw, ok := top["up"]; ok {
		if st.Up, err = parseIDList(raw); err != nil {
			return ManageStatus{}, errs.New(errs.Parse, "salt.status", "up: "+err.Error())
		}
	}
	if raw, ok := top["down"]; ok {
		if st.Down, err = parseIDList(raw); err != nil {
			return ManageStatus{}, errs.New(errs.Parse, "salt.status", "down: "+err.Error())
		}
	}
	sort.Strings(st.Up)
	sort.Strings(st.Down)
	return st, nil
}

// ParseManageVersions decodes salt-run manage.versions. Both the list shape
// (["web-01"], every entry at the master's version) and the map shape
// ({"web-01": "3007.1"}) are accepted; sections it does not know are skipped,
// so a minion without a version has no entry (the cell renders as "—").
func ParseManageVersions(b []byte) (Versions, error) {
	top, err := decodeTopObject(b, "salt.versions")
	if err != nil {
		return Versions{}, err
	}
	v := Versions{UpToDate: map[string]string{}, RequiresUpdate: map[string]string{}}
	var masterRaw json.RawMessage
	if raw, ok := top["Master"]; ok {
		masterRaw = raw
	}
	_ = json.Unmarshal(masterRaw, &v.Master)
	v.UpToDate = parseVersionSection(top["Up to date"], v.Master)
	v.RequiresUpdate = parseVersionSection(top["Minion requires update"], v.Master)
	return v, nil
}

// decodeTopObject decodes the first JSON object in b with its values left raw.
func decodeTopObject(b []byte, op string) (map[string]json.RawMessage, error) {
	var top map[string]json.RawMessage
	if err := DecodeInto(b, &top); err != nil {
		if e, ok := err.(*errs.Error); ok && e.Op == "salt.decode" {
			return nil, errs.New(errs.Parse, op, e.Msg)
		}
		return nil, err
	}
	if top == nil {
		top = map[string]json.RawMessage{}
	}
	return top, nil
}

// parseIDList accepts a JSON array of strings or an object whose keys are IDs.
func parseIDList(raw json.RawMessage) ([]string, error) {
	if isNull(raw) {
		return nil, nil
	}
	var list []string
	if err := json.Unmarshal(raw, &list); err == nil {
		return list, nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err == nil {
		out := make([]string, 0, len(obj))
		for id := range obj {
			out = append(out, id)
		}
		return out, nil
	}
	return nil, fmt.Errorf("want an array or an object")
}

// parseVersionSection decodes one manage.versions section into id → version.
func parseVersionSection(raw json.RawMessage, master string) map[string]string {
	out := make(map[string]string)
	if isNull(raw) {
		return out
	}
	var list []json.RawMessage
	if err := json.Unmarshal(raw, &list); err == nil {
		for _, item := range list {
			var id string
			if err := json.Unmarshal(item, &id); err == nil {
				out[id] = master
				continue
			}
			var entry struct {
				ID      string `json:"id"`
				Version string `json:"version"`
			}
			if err := json.Unmarshal(item, &entry); err == nil && entry.ID != "" {
				out[entry.ID] = entry.Version
			}
		}
		return out
	}
	var obj map[string]string
	if err := json.Unmarshal(raw, &obj); err == nil {
		for id, v := range obj {
			out[id] = v
		}
	}
	return out
}

// isNull reports whether raw is JSON null or empty.
func isNull(raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	return s == "" || s == "null"
}

func minionState(r *minionRow) (string, resource.Status) {
	switch r.key {
	case KeyAccepted:
		if r.up {
			return "up", resource.StatusOK
		}
		return "down", resource.StatusCrit
	case KeyPending, KeyRejected, KeyDenied:
		return "unaccepted", resource.StatusWarn
	}
	if r.up {
		return "up", resource.StatusOK
	}
	return "down", resource.StatusCrit
}

// minionRow is one minion accumulated from the three reads.
type minionRow struct {
	id   string
	key  string
	up   bool
	down bool
}

// minionRows merges salt-key -L, salt-run manage.status and salt-run
// manage.versions, which run in parallel. Grain columns (os, role, dc) stay
// nil until task 069 adds the cache.grains read.
func (b *Backend) minionRows(ctx context.Context) ([]resource.Resource, error) {
	var (
		keysOut, statusOut, versionsOut []byte
		keysErr, statusErr, versionsErr error
	)
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		keysOut, keysErr = b.runner.Run(ctx, Call{Tool: toolKey, Flags: []string{"-L"}})
	}()
	go func() {
		defer wg.Done()
		statusOut, statusErr = b.runner.Run(ctx, Call{Tool: toolRun, Fun: "manage.status"})
	}()
	go func() {
		defer wg.Done()
		versionsOut, versionsErr = b.runner.Run(ctx, Call{Tool: toolRun, Fun: "manage.versions"})
	}()
	wg.Wait()

	if keysErr != nil {
		return nil, keysErr
	}
	if statusErr != nil {
		return nil, statusErr
	}
	keys, err := ParseKeys(keysOut)
	if err != nil {
		return nil, err
	}
	status, err := ParseManageStatus(statusOut)
	if err != nil {
		return nil, err
	}
	// A missing version list costs the version cells, not the list.
	var versions Versions
	if versionsErr == nil {
		if v, perr := ParseManageVersions(versionsOut); perr == nil {
			versions = v
		}
	}

	rows := make(map[string]*minionRow)
	get := func(id string) *minionRow {
		r, ok := rows[id]
		if !ok {
			r = &minionRow{id: id}
			rows[id] = r
		}
		return r
	}
	for _, k := range keys {
		if k.State != KeyAccepted && k.State != KeyPending {
			continue
		}
		get(k.ID).key = k.State
	}
	for _, id := range status.Up {
		get(id).up = true
	}
	for _, id := range status.Down {
		get(id).down = true
	}

	ids := make([]string, 0, len(rows))
	for id := range rows {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	now := b.now()
	out := make([]resource.Resource, 0, len(ids))
	for _, id := range ids {
		r := rows[id]
		state, st := minionState(r)
		fields := map[string]any{
			"minion": id,
			"status": state,
		}
		if r.key != "" {
			fields["key"] = r.key
		}
		if v := versions.UpToDate[id]; v != "" {
			fields["version"] = v
		} else if v := versions.RequiresUpdate[id]; v != "" {
			fields["version"] = v
		}
		if host, ok := b.MinionHost(id); ok {
			fields["host"] = string(host)
		}
		out = append(out, resource.Resource{
			Ref:       resource.Ref{Backend: backendName, Kind: KindMinion, Name: id},
			Fields:    fields,
			Status:    st,
			FetchedAt: now,
		})
	}
	return out, nil
}

// keyRows merges salt-key -L and salt-key -F, which run in parallel.
func (b *Backend) keyRows(ctx context.Context) ([]resource.Resource, error) {
	var (
		listOut, fpOut []byte
		listErr, fpErr error
	)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		listOut, listErr = b.runner.Run(ctx, Call{Tool: toolKey, Flags: []string{"-L"}})
	}()
	go func() {
		defer wg.Done()
		fpOut, fpErr = b.runner.Run(ctx, Call{Tool: toolKey, Flags: []string{"-F"}})
	}()
	wg.Wait()

	if listErr != nil {
		return nil, listErr
	}
	keys, err := ParseKeys(listOut)
	if err != nil {
		return nil, err
	}
	var fps map[string]string
	if fpErr == nil {
		if m, perr := ParseFingerprints(fpOut); perr == nil {
			fps = m
		}
	}
	sort.SliceStable(keys, func(i, j int) bool {
		ri, rj := keyStateRank[keys[i].State], keyStateRank[keys[j].State]
		if ri != rj {
			return ri < rj
		}
		return keys[i].ID < keys[j].ID
	})

	now := b.now()
	out := make([]resource.Resource, 0, len(keys))
	for _, k := range keys {
		fields := map[string]any{
			"id":    k.ID,
			"state": k.State,
		}
		if fp := fps[k.ID]; fp != "" {
			fields["fingerprint"] = fp
		}
		st := resource.StatusOK
		if k.State != KeyAccepted {
			st = resource.StatusWarn
		}
		out = append(out, resource.Resource{
			Ref:       resource.Ref{Backend: backendName, Kind: KindKey, Name: k.ID},
			Fields:    fields,
			Status:    st,
			FetchedAt: now,
		})
	}
	return out, nil
}
