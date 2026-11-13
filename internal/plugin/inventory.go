package plugin

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"jumper/internal/errs"
	"jumper/internal/inventory"
	"jumper/internal/redact"
	"jumper/internal/resource"
	"jumper/internal/transport"
	"jumper/internal/transport/httpx"
)

const (
	// inventoryOp is the errs.Error op of every failure in this file.
	inventoryOp = "plugin.inventory"
	// nameColumn is the row column that names a host.
	nameColumn = "name"
	// stderrTailBytes caps the remote stderr a failure message carries.
	stderrTailBytes = 200
)

// InventoryOptions configures a `target: inventory` plugin.
type InventoryOptions struct {
	// Exec runs the plugin's command. Required when the definition has no
	// request.
	Exec transport.Executor
	// Host is the configured host the command runs on. Required with Exec.
	Host resource.HostID
	// Client is the client of the connection profile named in the definition.
	// Required when the definition has a request.
	Client *httpx.Client
	// Params are the parameter values the command or URL template gets.
	Params map[string]string
	// Timeout caps one load. Zero means HTTPTimeout.
	Timeout time.Duration
	// Now stamps SeenAt; nil means time.Now.
	Now func() time.Time
}

// InventorySource turns a `target: inventory` plugin into an inventory source
// (SPEC §9, §12): a host command on a configured host, or an HTTP request,
// whose parsed rows provide a name plus label columns. It is the source named
// plugin.<name>.
//
// InventorySource is safe for concurrent use: Load keeps no state.
type InventorySource struct {
	def  Definition
	opts InventoryOptions
	http *HTTP
}

// NewInventory validates an inventory plugin definition and returns its source.
func NewInventory(def Definition, opts InventoryOptions) (*InventorySource, error) {
	if def.Target != TargetInventory {
		return nil, errs.New(errs.Config, inventoryOp,
			fmt.Sprintf("plugin %q: target %q is not inventory", def.Name, def.Target))
	}
	if def.Name == "" {
		return nil, errs.New(errs.Config, inventoryOp, "an inventory plugin needs a name")
	}
	s := &InventorySource{def: def, opts: opts}
	switch r := def.Request; {
	case r != nil && strings.TrimSpace(def.Command) != "":
		return nil, errs.New(errs.Config, inventoryOp,
			fmt.Sprintf("plugin %q: an inventory plugin takes a command or a request, not both", def.Name))
	case r != nil:
		h, err := NewHTTP(def, opts.Client)
		if err != nil {
			return nil, err
		}
		h.Timeout = opts.Timeout
		h.Host = string(opts.Host)
		s.http = h
	case strings.TrimSpace(def.Command) == "":
		return nil, errs.New(errs.Config, inventoryOp,
			fmt.Sprintf("plugin %q: an inventory plugin needs a command or a request", def.Name))
	case opts.Exec == nil:
		return nil, errs.New(errs.Config, inventoryOp,
			fmt.Sprintf("plugin %q: no executor for the command", def.Name))
	case opts.Host == "":
		return nil, errs.New(errs.Config, inventoryOp,
			fmt.Sprintf("plugin %q: no configured host to run the command on", def.Name))
	}
	return s, nil
}

// Name is the source name and its label namespace: plugin.<name>.
func (s *InventorySource) Name() string { return "plugin." + s.def.Name }

// Load runs the plugin and turns its rows into hosts. The order is by ID, so a
// refresh is deterministic.
func (s *InventorySource) Load(ctx context.Context) ([]inventory.Host, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, errs.Wrap(errs.KindOf(err), inventoryOp, err)
	}
	rows, err := s.rows(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now
	if s.opts.Now != nil {
		now = s.opts.Now
	}
	return hostsFromRows(rows, now()), nil
}

// rows obtains and parses the plugin's output.
func (s *InventorySource) rows(ctx context.Context) ([]map[string]any, error) {
	if s.http != nil {
		return s.http.Run(ctx, s.opts.Params)
	}
	script, err := RenderCommand(s.def.Command, s.opts.Params, string(s.opts.Host))
	if err != nil {
		return nil, errs.New(errs.Config, inventoryOp, fmt.Sprintf("plugin %q: command: %s", s.def.Name, err))
	}
	timeout := s.opts.Timeout
	if timeout <= 0 {
		timeout = HTTPTimeout
	}
	res, err := s.opts.Exec.Exec(ctx, s.opts.Host, transport.Command{
		Script:  script,
		Sudo:    s.def.Privileged,
		Timeout: timeout,
	})
	if err != nil {
		var e *errs.Error
		if errors.As(err, &e) {
			return nil, err
		}
		return nil, errs.Wrap(errs.KindOf(err), inventoryOp, err)
	}
	if res.RC != 0 {
		msg := fmt.Sprintf("plugin %q: command exited %d", s.def.Name, res.RC)
		if tail := stderrTail(res.Stderr); tail != "" {
			msg += ": " + tail
		}
		return nil, errs.New(errs.Remote, inventoryOp, msg)
	}
	rows, perr := Parse(s.def.Parse, res.Stdout)
	if perr != nil {
		return nil, errs.New(errs.KindOf(perr), inventoryOp,
			fmt.Sprintf("plugin %q: output: %s", s.def.Name, perr))
	}
	return rows, nil
}

// hostsFromRows turns parsed rows into hosts. The name column is `name`; `host`
// works as well, so one parse can serve both targets. Every other column is a
// label; a row without a usable name is skipped rather than becoming a host.
func hostsFromRows(rows []map[string]any, seen time.Time) []inventory.Host {
	out := make([]inventory.Host, 0, len(rows))
	at := make(map[resource.HostID]int, len(rows))
	for _, row := range rows {
		name := firstNonEmpty(rowString(row, nameColumn), rowString(row, hostColumn))
		id := resource.NormalizeHostID(name)
		if id == "" {
			continue
		}
		if i, ok := at[id]; ok {
			for k, v := range rowLabels(row) {
				if _, set := out[i].Labels[k]; !set {
					out[i].Labels[k] = v
				}
			}
			continue
		}
		h := inventory.Host{
			ID:     id,
			Addr:   name,
			Labels: rowLabels(row),
			SeenAt: seen,
		}
		h.Labels["host"] = string(id)
		at[id] = len(out)
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// rowLabels returns the label columns of one row: every column except the name
// ones, with a non-empty string value. The host key is filled in by the caller.
func rowLabels(row map[string]any) map[string]string {
	labels := make(map[string]string, len(row))
	for k, v := range row {
		if k == nameColumn || k == hostColumn {
			continue
		}
		if sv := labelValue(v); sv != "" {
			labels[k] = sv
		}
	}
	return labels
}

// rowString returns row[key] as a trimmed string, or "".
func rowString(row map[string]any, key string) string {
	return strings.TrimSpace(labelValue(row[key]))
}

// labelValue renders one parsed value as a label. Nested objects are not labels
// and become "".
func labelValue(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case bool:
		return strconv.FormatBool(x)
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case []string:
		return strings.Join(x, ",")
	case []any:
		parts := make([]string, 0, len(x))
		for _, e := range x {
			if s := labelValue(e); s != "" {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, ",")
	case map[string]any:
		return ""
	default:
		return fmt.Sprint(x)
	}
}

// firstNonEmpty returns the first non-empty argument.
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// stderrTail caps and redacts the remote stderr a failure message carries.
func stderrTail(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > stderrTailBytes {
		s = strings.TrimSpace(s[len(s)-stderrTailBytes:])
	}
	if s == "" {
		return ""
	}
	red, err := redact.New(nil)
	if err != nil {
		return ""
	}
	return red.String(s)
}

// InventorySource is an inventory.Source.
var _ inventory.Source = (*InventorySource)(nil)
