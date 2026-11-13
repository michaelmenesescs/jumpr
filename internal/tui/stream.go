package tui

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"sitrep/internal/backend"
)

// batchWindow bounds how long NextBatch keeps draining a channel after the
// first item arrived. The UI must never wait on a busy stream (§8).
const batchWindow = 50 * time.Millisecond

// listBatchMax is the most Results one ListBatchMsg carries.
const listBatchMax = 200

// NextBatch waits for at least one item on ch, then drains whatever else is
// ready, up to max items or until 50 ms have passed, and returns them as one
// slice. closed is true when ch was closed: it is returned together with the
// items still queued, so a caller can stop asking after Closed.
//
// max < 1 is treated as 1.
func NextBatch[T any](ch <-chan T, max int) (items []T, closed bool) {
	if max < 1 {
		max = 1
	}
	v, ok := <-ch
	if !ok {
		return nil, true
	}
	items = append(items, v)
	if max == 1 {
		return items, false
	}
	limit := time.NewTimer(batchWindow)
	defer limit.Stop()
	for len(items) < max {
		select {
		case v, ok := <-ch:
			if !ok {
				return items, true
			}
			items = append(items, v)
		case <-limit.C:
			return items, false
		}
	}
	return items, false
}

// ListBatch returns a command that reads the next batch of at most 200
// Results from ch and emits it as a ListBatchMsg tagged with seq.
func ListBatch(seq int, ch <-chan backend.Result) tea.Cmd {
	return func() tea.Msg {
		results, closed := NextBatch(ch, listBatchMax)
		return ListBatchMsg{Seq: seq, Results: results, Closed: closed}
	}
}
