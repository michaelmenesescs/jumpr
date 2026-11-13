package interact

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"jumper/internal/errs"
)

// Static returns a Prompter that answers with the given answers in order and
// fails with errs.Interaction once they run out. It is for tests.
func Static(answers ...Answer) Prompter {
	return &staticPrompter{answers: append([]Answer(nil), answers...)}
}

type staticPrompter struct {
	mu      sync.Mutex
	answers []Answer
	next    int
}

func (s *staticPrompter) Ask(ctx context.Context, p Prompt) (Answer, error) {
	if err := promptCtxErr(ctx); err != nil {
		return Answer{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.next >= len(s.answers) {
		return Answer{}, errs.New(errs.Interaction, "interact", fmt.Sprintf("%s: no static answer left", p.Title))
	}
	a := s.answers[s.next]
	s.next++
	return a, nil
}

// None returns a Prompter that never answers: every Ask fails with
// errs.Interaction, as it does when no client is attached.
func None() Prompter { return nonePrompter{} }

type nonePrompter struct{}

func (nonePrompter) Ask(ctx context.Context, p Prompt) (Answer, error) {
	if err := promptCtxErr(ctx); err != nil {
		return Answer{}, err
	}
	return Answer{}, errs.New(errs.Interaction, "interact", fmt.Sprintf("%s: nobody to ask", p.Title))
}

func promptCtxErr(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return errs.Wrap(errs.Timeout, "interact", err)
		}
		return errs.Wrap(errs.Canceled, "interact", err)
	}
	return nil
}
