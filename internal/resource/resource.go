package resource

import (
	"fmt"
	"strings"
	"time"

	"jumpr/internal/errs"
)

// HostID is the canonical identity of a machine: lower-case, as resolved by the inventory.
type HostID string

// KindName names a resource kind, e.g. "host", "unit", "pod", "salt-job".
type KindName string

// Ref points at one resource. Unused fields are empty.
type Ref struct {
	Backend   string // "linux", "salt", "k8s", ...
	Kind      KindName
	Host      HostID // for host-scoped kinds
	Namespace string // k8s namespace, or a backend-defined scope
	Name      string
}

// String renders "backend/kind/host/namespace/name" with empty parts kept, e.g. "linux/unit/web-01//nginx.service".
func (r Ref) String() string {
	return strings.Join([]string{r.Backend, string(r.Kind), string(r.Host), r.Namespace, r.Name}, "/")
}

// ParseRef is the inverse of Ref.String: it accepts exactly five "/"-separated
// parts. Empty parts are allowed and kept.
func ParseRef(s string) (Ref, error) {
	parts := strings.Split(strings.TrimSpace(s), "/")
	if len(parts) != 5 {
		return Ref{}, errs.New(errs.Config, "resource.ParseRef",
			fmt.Sprintf("bad ref %q: want 5 slash-separated parts, got %d", s, len(parts)))
	}
	return Ref{
		Backend:   parts[0],
		Kind:      KindName(parts[1]),
		Host:      HostID(parts[2]),
		Namespace: parts[3],
		Name:      parts[4],
	}, nil
}

// NormalizeHostID canonicalises a host identity: it trims surrounding space,
// drops one trailing dot (an FQDN root) and lower-cases the rest.
func NormalizeHostID(s string) HostID {
	s = strings.TrimSpace(s)
	s = strings.TrimSuffix(s, ".")
	return HostID(strings.ToLower(s))
}

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
	Raw       any // backend-specific payload for detail views; may be nil
	FetchedAt time.Time
	Stale     bool // served from cache or from a failed refresh
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

// Column returns the column with the given ID.
func (k KindSpec) Column(id string) (Column, bool) {
	for _, c := range k.Columns {
		if c.ID == id {
			return c, true
		}
	}
	return Column{}, false
}

// Query is a request for a list of resources.
type Query struct {
	Backend  string // empty = resolve by kind through the registry
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
