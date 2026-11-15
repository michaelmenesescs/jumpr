package salt

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"

	"jumpr/internal/errs"
	"jumpr/internal/resource"
)

// slsNameRe is the shape of one sls name Salt can be asked to render: dotted,
// no slashes, no leading dash.
var slsNameRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*$`)

// SlsFile is one sls in one environment: the name Salt addresses
// ("nginx.conf") and the file it lives in ("nginx/conf.sls").
type SlsFile struct {
	Name string
	Env  string
	Path string
}

// Tree is the state tree: the environments the master has, every sls in them
// and, for the minions a query names, how many of them include each sls in
// their top file.
type Tree struct {
	Envs  []string
	Files []SlsFile
	// Minions are the query's hosts mapped onto minion IDs and sorted.
	Minions []string
	// Assigned counts the minions whose top includes an sls, keyed by
	// SlsKey(env, name). It is only meaningful when AssignedOK is set.
	Assigned   map[string]int
	AssignedOK bool
	// TopErr is why the assignment counts are missing; the tree itself is
	// still usable without them.
	TopErr error
	// Errors are the environments whose file list could not be read.
	Errors []string
}

// SlsKey is the map key of one sls of one environment.
func SlsKey(env, name string) string { return env + "\x00" + name }

// SlsName derives the sls name of a file path: only *.sls files become sls
// names, an "init.sls" is its directory and the top file is not an sls.
func SlsName(p string) (string, bool) {
	p = strings.TrimPrefix(strings.TrimSpace(p), "/")
	if !strings.HasSuffix(p, ".sls") {
		return "", false
	}
	p = strings.TrimSuffix(p, ".sls")
	if p == "" {
		return "", false
	}
	parts := strings.Split(p, "/")
	if len(parts) == 1 && parts[0] == "top" {
		return "", false
	}
	if parts[len(parts)-1] == "init" {
		if len(parts) == 1 {
			return "init", true
		}
		parts = parts[:len(parts)-1]
	}
	return strings.Join(parts, "."), true
}

// ParseFileServerEnvs decodes salt-run fileserver.envs. The runner answers
// with the names; the sources=True object form is accepted as well.
func ParseFileServerEnvs(b []byte) ([]string, error) {
	var list []string
	if err := DecodeInto(b, &list); err == nil {
		return uniqueSorted(list), nil
	}
	var obj map[string]json.RawMessage
	if err := DecodeInto(b, &obj); err != nil {
		return nil, errs.Wrap(errs.Parse, "salt.fileserver", err)
	}
	envs := make([]string, 0, len(obj))
	for name := range obj {
		envs = append(envs, name)
	}
	return uniqueSorted(envs), nil
}

// ParseFileServerFiles decodes salt-run fileserver.file_list for one
// environment.
func ParseFileServerFiles(b []byte) ([]string, error) {
	var list []string
	if err := DecodeInto(b, &list); err == nil {
		return cleanPaths(list), nil
	}
	var obj map[string]json.RawMessage
	if err := DecodeInto(b, &obj); err != nil {
		return nil, errs.Wrap(errs.Parse, "salt.fileserver", err)
	}
	for _, key := range []string{"files", "base"} {
		raw, ok := obj[key]
		if !ok {
			continue
		}
		if err := json.Unmarshal(raw, &list); err == nil {
			return cleanPaths(list), nil
		}
	}
	return nil, errs.New(errs.Parse, "salt.fileserver", "fileserver.file_list answered neither a list nor {files: …}")
}

// cleanPaths trims and de-duplicates a file list.
func cleanPaths(list []string) []string {
	out := make([]string, 0, len(list))
	seen := make(map[string]bool, len(list))
	for _, p := range list {
		p = strings.TrimPrefix(strings.TrimSpace(p), "/")
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

// uniqueSorted trims, de-duplicates and sorts a string list.
func uniqueSorted(list []string) []string {
	out := make([]string, 0, len(list))
	seen := make(map[string]bool, len(list))
	for _, s := range list {
		if s = strings.TrimSpace(s); s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// ShowTop is salt's per-minion state.show_top: the sls each minion includes,
// by environment.
type ShowTop struct {
	Minions []string
	Top     map[string]map[string][]string
	// Errors are the minions that did not answer or could not render their
	// top file.
	Errors map[string][]string
}

// SLS is every sls of one minion, across environments, sorted and unique.
func (t ShowTop) SLS(minion string) []string {
	byEnv := t.Top[minion]
	if len(byEnv) == 0 {
		return nil
	}
	envs := make([]string, 0, len(byEnv))
	for env := range byEnv {
		envs = append(envs, env)
	}
	sort.Strings(envs)
	out := make([]string, 0, len(byEnv))
	seen := make(map[string]bool)
	for _, env := range envs {
		for _, name := range byEnv[env] {
			if name == "" || seen[name] {
				continue
			}
			seen[name] = true
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// ParseShowTop decodes state.show_top. A minion that did not answer (JSON
// false) or answered with a string is kept as an error, never dropped.
func ParseShowTop(b []byte) (ShowTop, error) {
	st := ShowTop{Top: map[string]map[string][]string{}, Errors: map[string][]string{}}
	top, err := DecodeObject(b)
	if err != nil {
		return st, err
	}
	minions := make([]string, 0, len(top))
	for m := range top {
		minions = append(minions, m)
	}
	sort.Strings(minions)
	for _, minion := range minions {
		raw := bytes.TrimSpace(top[minion])
		if len(raw) == 0 || isNull(raw) {
			continue
		}
		if raw[0] != '{' {
			if NoReturn(raw) {
				st.Errors[minion] = []string{"no return"}
			} else {
				st.Errors[minion] = []string{errorText(raw)}
			}
			continue
		}
		var envs map[string]json.RawMessage
		if err := json.Unmarshal(raw, &envs); err != nil {
			st.Errors[minion] = []string{err.Error()}
			continue
		}
		byEnv := make(map[string][]string, len(envs))
		for env, rv := range envs {
			var list []string
			if err := json.Unmarshal(rv, &list); err != nil {
				// The master-side shape (env -> target -> sls) carries no
				// per-minion assignment; skip what we cannot attribute.
				continue
			}
			byEnv[env] = list
		}
		st.Top[minion] = byEnv
	}
	st.Minions = minions
	return st, nil
}

// StateDef is one rendered state of one minion: the show_sls/show_highstate
// row jumpr shows.
type StateDef struct {
	Minion string
	ID     string
	Module string
	Fun    string
	SLS    string
	Env    string
	Name   string
	// Requisites summarizes require/watch style arguments, e.g.
	// "require pkg:nginx; watch file:/etc/nginx/nginx.conf".
	Requisites string
}

// Function is "module.fun".
func (d StateDef) Function() string {
	if d.Module == "" {
		return d.Fun
	}
	if d.Fun == "" {
		return d.Module
	}
	return d.Module + "." + d.Fun
}

// ShowError is one minion's render error, as Salt reports it.
type ShowError struct {
	Minion string
	Text   string
}

// ShowState is the flattened result of state.show_sls/state.show_highstate:
// the rows to render plus the render errors, which are shown and not treated
// as a failure.
type ShowState struct {
	Minions []string
	Defs    []StateDef
	Errors  []ShowError
}

// requisiteKeys are the argument names summarized as requisites, in the order
// they are rendered.
var requisiteKeys = []string{
	"require", "watch", "onchanges", "onfail", "prereq",
	"require_in", "watch_in", "listen", "listen_in",
}

// ParseShowState flattens a rendered state tree. A list of strings (a render
// error) is kept as an error; it never fails the parse.
func ParseShowState(b []byte) (ShowState, error) {
	var st ShowState
	data, err := FirstJSON(b)
	if err != nil {
		return st, err
	}
	if trimmed := bytes.TrimSpace(data); len(trimmed) > 0 && trimmed[0] == '[' {
		var list []json.RawMessage
		if err := json.Unmarshal(trimmed, &list); err != nil {
			return st, errs.Wrap(errs.Parse, "salt.show", err)
		}
		for _, item := range list {
			st.Errors = append(st.Errors, ShowError{Text: errorText(item)})
		}
		return st, nil
	}
	top, err := DecodeObject(b)
	if err != nil {
		return st, err
	}
	minions := make([]string, 0, len(top))
	for m := range top {
		minions = append(minions, m)
	}
	sort.Strings(minions)
	for _, minion := range minions {
		raw := bytes.TrimSpace(top[minion])
		if len(raw) == 0 || isNull(raw) {
			continue
		}
		switch raw[0] {
		case '{':
			defs, derr := parseStateObject(minion, raw)
			if derr != nil {
				st.Errors = append(st.Errors, ShowError{Minion: minion, Text: derr.Error()})
				continue
			}
			st.Defs = append(st.Defs, defs...)
		case '[':
			var list []json.RawMessage
			if err := json.Unmarshal(raw, &list); err != nil {
				st.Errors = append(st.Errors, ShowError{Minion: minion, Text: err.Error()})
				continue
			}
			for _, item := range list {
				st.Errors = append(st.Errors, ShowError{Minion: minion, Text: errorText(item)})
			}
		case '"':
			var s string
			if json.Unmarshal(raw, &s) == nil {
				st.Errors = append(st.Errors, ShowError{Minion: minion, Text: s})
			}
		case 'f':
			if NoReturn(raw) {
				st.Errors = append(st.Errors, ShowError{Minion: minion, Text: "no return"})
			}
		}
	}
	st.Minions = minions
	return st, nil
}

// parseStateObject flattens one minion's rendered states, in state-id order.
func parseStateObject(minion string, raw json.RawMessage) ([]StateDef, error) {
	var states map[string]json.RawMessage
	if err := json.Unmarshal(raw, &states); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(states))
	for id := range states {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var out []StateDef
	for _, id := range ids {
		trimmed := bytes.TrimSpace(states[id])
		if len(trimmed) == 0 || trimmed[0] != '{' {
			continue
		}
		var mods map[string]json.RawMessage
		if err := json.Unmarshal(trimmed, &mods); err != nil {
			continue
		}
		sls, env := "", ""
		if r, ok := mods["__sls__"]; ok {
			_ = json.Unmarshal(r, &sls)
		}
		if r, ok := mods["__env__"]; ok {
			_ = json.Unmarshal(r, &env)
		}
		names := make([]string, 0, len(mods))
		for key := range mods {
			if strings.HasPrefix(key, "__") {
				continue
			}
			names = append(names, key)
		}
		sort.Strings(names)
		for _, key := range names {
			def, ok := parseStateDecl(minion, id, key, mods[key])
			if !ok {
				continue
			}
			def.SLS, def.Env = sls, env
			out = append(out, def)
		}
	}
	return out, nil
}

// parseStateDecl builds one row from a state module declaration: the module
// key, the argument list whose first element is the function name.
func parseStateDecl(minion, id, key string, raw json.RawMessage) (StateDef, bool) {
	def := StateDef{Minion: minion, ID: id, Module: key, Name: id}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || isNull(trimmed) {
		return def, true
	}
	var list []json.RawMessage
	if err := json.Unmarshal(trimmed, &list); err != nil {
		return StateDef{}, false
	}
	rest := list
	if len(list) > 0 {
		var fun string
		if json.Unmarshal(list[0], &fun) == nil {
			def.Fun = fun
			rest = list[1:]
		}
	}
	if def.Fun == "" {
		if mod, fun, ok := strings.Cut(key, "."); ok {
			def.Module, def.Fun = mod, fun
		}
	}
	var requisites []string
	for _, elem := range rest {
		t := bytes.TrimSpace(elem)
		if len(t) == 0 || t[0] != '{' {
			continue
		}
		var args map[string]json.RawMessage
		if json.Unmarshal(t, &args) != nil {
			continue
		}
		if v, ok := args["name"]; ok {
			var s string
			if json.Unmarshal(v, &s) == nil && s != "" {
				def.Name = s
			}
		}
		requisites = append(requisites, requisiteSummary(args)...)
	}
	def.Requisites = strings.Join(requisites, "; ")
	return def, true
}

// requisiteSummary renders the require/watch style arguments of one state.
func requisiteSummary(args map[string]json.RawMessage) []string {
	var out []string
	for _, key := range requisiteKeys {
		raw, ok := args[key]
		if !ok {
			continue
		}
		items := requisiteItems(raw)
		if len(items) == 0 {
			continue
		}
		out = append(out, key+" "+strings.Join(items, ", "))
	}
	return out
}

// requisiteItems renders a requisite value: [{"pkg": "nginx"}, …] or a dict.
func requisiteItems(raw json.RawMessage) []string {
	var list []json.RawMessage
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil
	}
	var out []string
	for _, item := range list {
		var obj map[string]string
		if err := json.Unmarshal(item, &obj); err == nil {
			keys := make([]string, 0, len(obj))
			for k := range obj {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				out = append(out, k+":"+obj[k])
			}
			continue
		}
		var s string
		if json.Unmarshal(item, &s) == nil && s != "" {
			out = append(out, s)
		}
	}
	return out
}

// Envs lists the environments the master's fileserver serves.
func (b *Backend) Envs(ctx context.Context) ([]string, error) {
	if b.runner == nil {
		return nil, errs.New(errs.Config, "salt.fileserver", "salt backend not configured: no runner")
	}
	out, err := b.runner.Run(ctx, Call{Tool: toolRun, Fun: "fileserver.envs"})
	if err != nil {
		return nil, err
	}
	return ParseFileServerEnvs(out)
}

// FileList lists one environment's files.
func (b *Backend) FileList(ctx context.Context, env string) ([]string, error) {
	if b.runner == nil {
		return nil, errs.New(errs.Config, "salt.fileserver", "salt backend not configured: no runner")
	}
	env = strings.TrimSpace(env)
	if env == "" {
		return nil, errs.New(errs.Config, "salt.fileserver", "empty environment")
	}
	out, err := b.runner.Run(ctx, Call{
		Tool:   toolRun,
		Fun:    "fileserver.file_list",
		Kwargs: map[string]string{"saltenv": env},
	})
	if err != nil {
		return nil, err
	}
	return ParseFileServerFiles(out)
}

// TopFile is state.show_top for the given minions, in one call. An empty list
// asks the master for every minion's top.
func (b *Backend) TopFile(ctx context.Context, minions []string) (ShowTop, error) {
	if b.runner == nil {
		return ShowTop{}, errs.New(errs.Config, "salt.show_top", "salt backend not configured: no runner")
	}
	call := Call{Tool: toolRun, Fun: "state.show_top"}
	if len(minions) > 0 {
		target := &Target{Type: "list", Expr: strings.Join(minions, ",")}
		call = Call{Tool: toolSalt, Target: target, Fun: "state.show_top"}
	}
	out, err := b.runner.Run(ctx, call)
	if err != nil {
		return ShowTop{}, err
	}
	return ParseShowTop(out)
}

// ShowSls renders one sls for one minion.
func (b *Backend) ShowSls(ctx context.Context, minion, sls string) (ShowState, error) {
	minion = strings.TrimSpace(minion)
	sls = strings.TrimSpace(sls)
	if minion == "" {
		return ShowState{}, errs.New(errs.Config, "salt.show_sls", "show_sls needs a minion")
	}
	if !slsNameRe.MatchString(sls) {
		return ShowState{}, errs.New(errs.Config, "salt.show_sls", fmt.Sprintf("invalid sls name %q", sls))
	}
	out, err := b.runner.Run(ctx, Call{
		Tool:   toolSalt,
		Target: &Target{Type: "list", Expr: minion},
		Fun:    "state.show_sls",
		Args:   []string{sls},
	})
	if err != nil {
		return ShowState{}, err
	}
	return ParseShowState(out)
}

// ShowHighstate renders the highstate of one minion.
func (b *Backend) ShowHighstate(ctx context.Context, minion string) (ShowState, error) {
	minion = strings.TrimSpace(minion)
	if minion == "" {
		return ShowState{}, errs.New(errs.Config, "salt.show_highstate", "show_highstate needs a minion")
	}
	out, err := b.runner.Run(ctx, Call{
		Tool:   toolSalt,
		Target: &Target{Type: "list", Expr: minion},
		Fun:    "state.show_highstate",
	})
	if err != nil {
		return ShowState{}, err
	}
	return ParseShowState(out)
}

// SlsSource reads one file of the state tree with cp.get_file_str, which the
// master's own minion serves. A file the master cannot read is an
// errs.NotAvailable error, not an empty string.
func (b *Backend) SlsSource(ctx context.Context, minion, file string) (string, error) {
	if b.runner == nil {
		return "", errs.New(errs.Config, "salt.source", "salt backend not configured: no runner")
	}
	minion = strings.TrimSpace(minion)
	if minion == "" {
		return "", errs.New(errs.Config, "salt.source", "source needs a minion")
	}
	clean, ok := SaltPath(file)
	if !ok {
		return "", errs.New(errs.Config, "salt.source", fmt.Sprintf("invalid salt path %q", file))
	}
	out, err := b.runner.Run(ctx, Call{
		Tool:   toolSalt,
		Target: &Target{Type: "list", Expr: minion},
		Fun:    "cp.get_file_str",
		Args:   []string{"salt://" + clean},
	})
	if err != nil {
		return "", err
	}
	top, err := DecodeObject(out)
	if err != nil {
		return "", err
	}
	raw, ok := top[minion]
	if !ok && len(top) == 1 {
		for _, v := range top {
			raw, ok = v, true
		}
	}
	if !ok {
		return "", errs.New(errs.NotAvailable, "salt.source", "source not readable")
	}
	trimmed := bytes.TrimSpace(raw)
	var s string
	if len(trimmed) > 0 && trimmed[0] == '"' {
		if json.Unmarshal(trimmed, &s) == nil {
			return s, nil
		}
	}
	if NoReturn(trimmed) || string(trimmed) == "false" {
		return "", errs.New(errs.NotAvailable, "salt.source", "source not readable")
	}
	return "", errs.New(errs.NotAvailable, "salt.source", "source not readable: "+errorText(trimmed))
}

// SaltPath validates a path inside the state tree. It accepts a plain relative
// path and the salt:// form; it rejects anything that could escape the
// fileserver root.
func SaltPath(p string) (string, bool) {
	p = strings.TrimSpace(p)
	p = strings.TrimPrefix(p, "salt://")
	if p == "" {
		return "", false
	}
	clean := path.Clean(p)
	if clean == "." || clean == "/" || strings.HasPrefix(clean, "/") {
		return "", false
	}
	if clean == ".." || strings.HasPrefix(clean, "../") || strings.Contains(clean, "/../") {
		return "", false
	}
	return clean, true
}

// MasterMinion is the minion ID that serves the master's own file roots: the
// salt.id label of the master's inventory host, else the host's short name,
// which is Salt's default minion ID.
func (b *Backend) MasterMinion() (string, bool) {
	if b.master == "" {
		return "", false
	}
	if b.inv != nil {
		if h, ok := b.inv.Resolve(string(b.master)); ok {
			if id := strings.TrimSpace(h.Labels["salt.id"]); id != "" {
				return id, true
			}
		}
	}
	if short := shortName(string(b.master)); short != "" {
		return short, true
	}
	return "", false
}

// MinionOf maps a session host onto its minion ID.
func (b *Backend) MinionOf(host resource.HostID) (string, bool) { return b.HostMinion(host) }

// Tree reads the state tree for a query. Sessions without targets get no
// assignment counts and no state.show_top call.
func (b *Backend) Tree(ctx context.Context, q resource.Query) (Tree, error) {
	t := Tree{Assigned: map[string]int{}}
	if b.runner == nil {
		return t, errs.New(errs.Config, "salt.tree", "salt backend not configured: no runner")
	}
	envs, err := b.Envs(ctx)
	if err != nil {
		return t, err
	}
	t.Envs = envs

	only := strings.TrimSpace(q.Args["env"])
	selected := envs
	if only != "" {
		selected = nil
		for _, env := range envs {
			if env == only {
				selected = append(selected, env)
			}
		}
		if len(selected) == 0 {
			// The master may serve an env it has not listed; ask anyway.
			selected = []string{only}
		}
	}

	seen := make(map[string]bool)
	for _, env := range selected {
		paths, ferr := b.FileList(ctx, env)
		if ferr != nil {
			t.Errors = append(t.Errors, fmt.Sprintf("%s: %v", env, ferr))
			continue
		}
		for _, p := range paths {
			name, ok := SlsName(p)
			if !ok {
				continue
			}
			key := SlsKey(env, name)
			if seen[key] {
				continue
			}
			seen[key] = true
			t.Files = append(t.Files, SlsFile{Name: name, Env: env, Path: p})
		}
	}
	sort.Slice(t.Files, func(i, j int) bool {
		a, c := t.Files[i], t.Files[j]
		if a.Env != c.Env {
			return a.Env < c.Env
		}
		return a.Name < c.Name
	})

	t.Minions = b.targetMinions(q.Hosts)
	if len(t.Minions) == 0 {
		return t, nil
	}
	top, terr := b.TopFile(ctx, t.Minions)
	if terr != nil {
		t.TopErr = terr
		return t, nil
	}
	t.AssignedOK = true
	for _, minion := range t.Minions {
		for env, list := range top.Top[minion] {
			if only != "" && env != only {
				continue
			}
			for _, name := range list {
				t.Assigned[SlsKey(env, name)]++
			}
		}
	}
	return t, nil
}

// targetMinions maps the query's hosts onto sorted, unique minion IDs.
func (b *Backend) targetMinions(hosts []resource.HostID) []string {
	out := make([]string, 0, len(hosts))
	seen := make(map[string]bool, len(hosts))
	for _, h := range hosts {
		m, ok := b.HostMinion(h)
		if !ok || m == "" || seen[m] {
			continue
		}
		seen[m] = true
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

// slsRows is the List path of the salt-sls kind: one row per sls.
func (b *Backend) slsRows(ctx context.Context, q resource.Query) ([]resource.Resource, error) {
	t, err := b.Tree(ctx, q)
	if err != nil {
		return nil, err
	}
	items := make([]resource.Resource, 0, len(t.Files))
	for _, f := range t.Files {
		fields := map[string]any{
			"sls":  f.Name,
			"env":  f.Env,
			"path": f.Path,
		}
		if t.AssignedOK {
			fields["assigned"] = int64(t.Assigned[SlsKey(f.Env, f.Name)])
		}
		items = append(items, resource.Resource{
			Ref:       resource.Ref{Backend: backendName, Kind: KindSls, Namespace: f.Env, Name: f.Name},
			Fields:    fields,
			FetchedAt: b.now(),
		})
	}
	return items, nil
}
