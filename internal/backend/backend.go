// Package backend defines the integration contract and the registry that maps
// kind names and aliases to backends.
//
// A backend only reads: writes go through action.Planner and the action bus
// (BUILD-PLAN §4.5). Methods on a Backend must be safe for concurrent use.
package backend

import (
	"context"
	"time"

	"jumpr/internal/resource"
)

type HealthState int

const (
	HealthUnconfigured HealthState = iota // grey dot
	HealthOK                              // green
	HealthDegraded                        // half
	HealthDown                            // red
)

// String renders the state as a stable lower-case word.
func (s HealthState) String() string {
	switch s {
	case HealthOK:
		return "ok"
	case HealthDegraded:
		return "degraded"
	case HealthDown:
		return "down"
	default:
		return "unconfigured"
	}
}

type Health struct {
	State   HealthState
	Detail  string // one line, shown in :health
	Checked time.Time
}

// Result is one batch of a streaming List. A List on 100 hosts yields at least 100 Results.
type Result struct {
	Host  resource.HostID // set for host-scoped kinds
	Items []resource.Resource
	Err   error // *errs.Error; this batch failed, others may still succeed
	Done  int   // units finished so far (hosts, pages)
	Total int   // units expected; 0 = unknown
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

// LinkProvider is optional: extra pivots for rows of *other* backends (host → salt minion, pod → host).
// The root asks every registered backend, so backends never import each other.
type LinkProvider interface {
	ExtraLinks(r resource.Resource) []resource.Link
}
