// Package action defines the types of a write: what a backend can do, the
// request that asks for it, and the plan the user confirms before anything
// runs. It is types only; execution lives in internal/bus.
package action

import (
	"context"
	"time"

	"jumpr/internal/resource"
)

// Actor says who asked. "human", "plugin:<name>", "agent:<name>".
type Actor string

// Human is the actor for a request typed by the user.
const Human Actor = "human"

// Risk classifies how dangerous a write is.
type Risk int

const (
	RiskNormal      Risk = iota // restart, reload, scale
	RiskDestructive             // stop, kill -9, delete, key delete, state.apply
)

// Param describes one argument a spec accepts.
type Param struct {
	Name     string
	Title    string
	Default  string
	Required bool
	Choices  []string // optional
}

// Spec describes one kind of write a backend offers.
type Spec struct {
	ID        string // globally unique: "linux.systemctl.restart", "salt.state.apply", "generic.run"
	Title     string // menu text: "restart"
	Risk      Risk
	Params    []Param
	HasDryRun bool // Plan can produce a Preview
}

// Target is one host a step acts on.
type Target struct {
	Host   resource.HostID
	Ref    resource.Ref      // the resource acted on, if any
	Labels map[string]string // host labels; policy matches on these
}

// Request asks a planner to produce a plan for a spec on a set of targets.
type Request struct {
	SpecID  string
	Targets []Target
	Params  map[string]string
	Actor   Actor
}

// StepResult is what one step produced.
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
	Title    string // "restart nginx.service on 12 hosts"
	Steps    []Step // one per target, in rollout order
	Preview  string // dry-run output or diff, may be empty
	Via      string // transport name
	Warnings []string
}

// Planner is implemented by backends that can write.
type Planner interface {
	// Actions lists what can be done to this selection. Pure, no I/O.
	Actions(selection []resource.Resource) []Spec
	// Plan resolves a request into steps. It may run read-only remote calls (dry runs) but must not write.
	Plan(ctx context.Context, req Request) (Plan, error)
}
