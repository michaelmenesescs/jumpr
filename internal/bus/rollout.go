package bus

import (
	"fmt"
	"strconv"
	"strings"

	"jumpr/internal/action"
	"jumpr/internal/errs"
)

// RolloutConfig is the input of the rollout decisions. It mirrors
// config.RolloutConfig field for field, so a caller can build one without this
// package importing internal/config.
type RolloutConfig struct {
	Canary                  int
	BatchPercent            int
	BatchMax                int
	FailureThresholdPercent int
	TypedConfirmOver        int
}

// ConfirmFor decides how much confirmation an action on targets hosts needs.
// More than TypedConfirmOver targets, or any protected target, needs the typed
// count; two or more targets show the list; RiskDestructive lifts a single
// target from y/N to the explicit list.
func ConfirmFor(targets int, anyProtected bool, risk action.Risk, c RolloutConfig) ConfirmLevel {
	switch {
	case targets > c.TypedConfirmOver || anyProtected:
		return ConfirmTypedCount
	case targets >= 2:
		return ConfirmList
	case risk == action.RiskDestructive:
		return ConfirmList
	default:
		return ConfirmYesNo
	}
}

// PlanRollout cuts targets into a canary phase and rolling batches. One target
// is a single batch with no canary. Otherwise the canary is min(canary,
// targets-1), at least 1 when a canary is configured, and the rest is split into
// batches of ceil(remaining × batch percent / 100), clamped to [1, batch max].
func PlanRollout(targets int, c RolloutConfig) Rollout {
	r := Rollout{FailureThreshold: float64(c.FailureThresholdPercent) / 100}
	if targets <= 1 {
		r.BatchSize = 1
		r.Batches = 1
		return r
	}
	canary := c.Canary
	if canary > targets-1 {
		canary = targets - 1
	}
	if canary < 1 && c.Canary >= 1 {
		canary = 1
	}
	if canary < 0 {
		canary = 0
	}
	remaining := targets - canary
	size := ceilPercent(remaining, c.BatchPercent)
	if size < 1 {
		size = 1
	}
	if c.BatchMax >= 1 && size > c.BatchMax {
		size = c.BatchMax
	}
	r.Canary = canary
	r.BatchSize = size
	r.Batches = ceilDiv(remaining, size)
	return r
}

// Batches splits steps into the canary (the first r.Canary steps) and the
// rolling batches, preserving order. A batch size below 1 puts all non-canary
// steps in one batch.
func Batches(steps []action.Step, r Rollout) (canary []action.Step, batches [][]action.Step) {
	if len(steps) == 0 {
		return nil, nil
	}
	n := r.Canary
	if n > len(steps) {
		n = len(steps)
	}
	if n < 0 {
		n = 0
	}
	canary = steps[:n]
	rest := steps[n:]
	if len(rest) == 0 {
		return canary, nil
	}
	size := r.BatchSize
	if size < 1 {
		size = len(rest)
	}
	for len(rest) > 0 {
		k := size
		if k > len(rest) {
			k = len(rest)
		}
		batches = append(batches, rest[:k])
		rest = rest[k:]
	}
	return canary, batches
}

// CheckConfirmation validates the user's answer against the proposal. A denied
// proposal is a Policy error, a confirmation for another proposal an Internal
// one, and ConfirmTypedCount needs the exact decimal target count.
func CheckConfirmation(p Proposal, c Confirmation) error {
	if p.Denied != nil {
		msg := p.Denied.Reason
		if msg == "" {
			msg = "denied by policy"
		}
		return errs.New(errs.Policy, "bus.confirm", msg)
	}
	if c.ProposalID != p.ID {
		return errs.New(errs.Internal, "bus.confirm",
			fmt.Sprintf("confirmation for proposal %q does not match %q", c.ProposalID, p.ID))
	}
	if p.Confirm != ConfirmTypedCount {
		return nil
	}
	want := strconv.Itoa(targetCount(p))
	if strings.TrimSpace(c.Typed) != want {
		return errs.New(errs.Policy, "bus.confirm", "typed count does not match")
	}
	return nil
}

// ShouldStop reports whether the failure rate after the canary has crossed the
// threshold. It is a strict comparison: with 20 hosts and the default 5 %, one
// failure (exactly 5 %) does not stop the rollout and the second one does. The
// fraction is taken over totalAfterCanary, so a partly finished batch cannot
// make the failure rate look larger; doneAfterCanary is the number of hosts
// that have finished so far.
func ShouldStop(failedAfterCanary, doneAfterCanary, totalAfterCanary int, threshold float64) bool {
	_ = doneAfterCanary
	if totalAfterCanary <= 0 {
		return false
	}
	return float64(failedAfterCanary)/float64(totalAfterCanary) > threshold
}

// targetCount is the number of targets a proposal acts on: its steps, one per
// target in rollout order, falling back to the request when a plan carries no
// steps (a zero-target or pre-planning proposal).
func targetCount(p Proposal) int {
	if n := len(p.Plan.Steps); n > 0 {
		return n
	}
	return len(p.Plan.Request.Targets)
}

func ceilDiv(a, b int) int {
	if b <= 0 {
		return 0
	}
	return (a + b - 1) / b
}

func ceilPercent(a, percent int) int {
	if percent <= 0 {
		return 0
	}
	return (a*percent + 99) / 100
}
