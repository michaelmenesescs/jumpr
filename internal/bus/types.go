// Package bus is the only path that performs a write. This file holds the
// types of BUILD-PLAN §4.5; the pure decisions (confirm level, rollout shape)
// are in rollout.go and the engine follows in later tasks.
package bus

import (
	"context"
	"time"

	"jumpr/internal/action"
)

// ConfirmLevel is how much confirmation an action needs.
type ConfirmLevel int

const (
	ConfirmYesNo      ConfirmLevel = iota // 1 target: y/N
	ConfirmList                           // 2..N: show the list, y/N
	ConfirmTypedCount                     // > typed_confirm_over (default 20) or any protected target: type the count
)

// String returns the level's name.
func (l ConfirmLevel) String() string {
	switch l {
	case ConfirmYesNo:
		return "yes-no"
	case ConfirmList:
		return "list"
	case ConfirmTypedCount:
		return "typed-count"
	default:
		return "unknown"
	}
}

// Rollout says how the targets are cut into a canary and batches.
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

// Denial says why policy refused an action.
type Denial struct {
	Rule   string // the policy.yaml rule text
	Reason string
}

// Confirmation is the user's answer to a proposal.
type Confirmation struct {
	ProposalID string
	Typed      string // the typed target count for ConfirmTypedCount; ignored otherwise
}

// Phase is the stage a rollout is in.
type Phase int

const (
	PhaseCanary Phase = iota
	PhasePaused       // canary finished OK; waiting for Continue or Abort
	PhaseBatch
	PhaseDone
	PhaseAborted
	PhaseStopped // failure threshold hit
)

// String returns the phase's name.
func (p Phase) String() string {
	switch p {
	case PhaseCanary:
		return "canary"
	case PhasePaused:
		return "paused"
	case PhaseBatch:
		return "batch"
	case PhaseDone:
		return "done"
	case PhaseAborted:
		return "aborted"
	case PhaseStopped:
		return "stopped"
	default:
		return "unknown"
	}
}

// Event is one step of a rollout as it happens.
type Event struct {
	Phase   Phase
	Batch   int                // 1-based during PhaseBatch
	Result  *action.StepResult // set when one step finished
	Summary *Summary           // set on Done, Aborted, Stopped
}

// Summary is the outcome of a finished rollout.
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
