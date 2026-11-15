package tui

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"jumpr/internal/errs"
	"jumpr/internal/interact"
)

// defaultPromptTimeout is used when NewPrompter is given a non-positive
// timeout; it matches the interact.timeout default of the config.
const defaultPromptTimeout = 2 * time.Minute

// promptOp is the Op of every error the Prompter returns.
const promptOp = "interact"

// Prompter implements interact.Prompter for the TUI. Ask is called from
// background goroutines (SSH handshakes), never from Update: it sends a
// PromptMsg to the program, which shows a modal dialog, and waits for the
// answer on the prompt's reply channel.
//
// send is program.Send, supplied by internal/app. Prompts are shown one at a
// time: Ask holds a mutex for the whole time its question is on screen, so a
// fan-out of connections never stacks dialogs.
type Prompter struct {
	send    func(tea.Msg)
	timeout time.Duration

	// promptMu serializes questions: held while one prompt is on screen.
	promptMu sync.Mutex

	// stateMu guards attached, attachc and once.
	stateMu  sync.Mutex
	attached bool
	attachc  chan struct{} // closed while attached; replaced when detached
	once     map[string]interact.Answer
}

var _ interact.Prompter = (*Prompter)(nil)

// NewPrompter returns a prompter that hands prompts to send and gives up after
// timeout (or the caller's context). It starts attached: before M5 the TUI is
// always on the terminal, and app calls SetAttached(false) while detached.
func NewPrompter(send func(tea.Msg), timeout time.Duration) *Prompter {
	if timeout <= 0 {
		timeout = defaultPromptTimeout
	}
	c := make(chan struct{})
	close(c)
	return &Prompter{
		send:     send,
		timeout:  timeout,
		attached: true,
		attachc:  c,
		once:     make(map[string]interact.Answer),
	}
}

// SetAttached records whether a client is looking at the session. While
// detached, Ask waits up to the timeout for a client before failing with
// errs.Interaction.
func (p *Prompter) SetAttached(on bool) {
	p.stateMu.Lock()
	defer p.stateMu.Unlock()
	if p.attached == on {
		return
	}
	p.attached = on
	if on {
		close(p.attachc)
		return
	}
	p.attachc = make(chan struct{})
}

// Ask shows prompt and waits for the answer, ctx.Done() or the timeout. It
// fails with errs.Interaction when nobody can answer (no send function, no
// client attached in time, no answer in time, or a canceled context; the
// context error is kept as the cause).
func (p *Prompter) Ask(ctx context.Context, prompt interact.Prompt) (interact.Answer, error) {
	if err := ctx.Err(); err != nil {
		return interact.Answer{}, promptCtxErr(err)
	}
	if p == nil || p.send == nil {
		return interact.Answer{}, errs.New(errs.Interaction, promptOp,
			fmt.Sprintf("%s: no UI to ask", promptName(prompt)))
	}
	key := hostKeyCacheKey(prompt)

	p.promptMu.Lock()
	defer p.promptMu.Unlock()

	// The cache is checked under the prompt mutex as well: an identical
	// host-key question answered with "once" while this one queued is already
	// decided.
	if key != "" {
		if ans, ok := p.cachedOnce(key); ok {
			return ans, nil
		}
	}
	if err := p.waitAttached(ctx); err != nil {
		return interact.Answer{}, err
	}

	reply := make(chan interact.Answer, 1)
	p.send(PromptMsg{Prompt: prompt, Reply: reply})

	timer := time.NewTimer(p.timeout)
	defer timer.Stop()
	select {
	case ans := <-reply:
		if key != "" && !ans.Canceled && strings.EqualFold(strings.TrimSpace(ans.Choice), "once") {
			p.rememberOnce(key, ans)
		}
		return ans, nil
	case <-ctx.Done():
		return interact.Answer{}, promptCtxErr(ctx.Err())
	case <-timer.C:
		return interact.Answer{}, errs.New(errs.Interaction, promptOp,
			fmt.Sprintf("%s: no answer within %s", promptName(prompt), p.timeout))
	}
}

// waitAttached blocks until a client is attached, the context ends or the
// timeout passes.
func (p *Prompter) waitAttached(ctx context.Context) error {
	p.stateMu.Lock()
	ch, attached := p.attachc, p.attached
	p.stateMu.Unlock()
	if attached {
		return nil
	}
	timer := time.NewTimer(p.timeout)
	defer timer.Stop()
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return promptCtxErr(ctx.Err())
	case <-timer.C:
		return errs.New(errs.Interaction, promptOp, "no client attached")
	}
}

// cachedOnce returns the cached "once" answer for key, if any.
func (p *Prompter) cachedOnce(key string) (interact.Answer, bool) {
	p.stateMu.Lock()
	defer p.stateMu.Unlock()
	ans, ok := p.once[key]
	return ans, ok
}

// rememberOnce caches a "once" answer for the rest of the process's life.
func (p *Prompter) rememberOnce(key string, ans interact.Answer) {
	p.stateMu.Lock()
	defer p.stateMu.Unlock()
	p.once[key] = ans
}

// hostKeyCacheKey is the cache key for a "once" host-key answer. The prompt
// body holds the host, the key type and the SHA256 fingerprint, so an
// identical body means the same host key; an empty body never matches.
func hostKeyCacheKey(prompt interact.Prompt) string {
	if prompt.Kind != interact.HostKeyUnknown {
		return ""
	}
	return prompt.Body
}

// promptCtxErr reports a finished context as errs.Interaction, keeping the
// context error as the cause so errors.Is(err, context.Canceled) still holds.
func promptCtxErr(err error) error {
	return errs.Wrap(errs.Interaction, promptOp, err)
}

// promptName is the title used in error messages, with a per-kind fallback.
func promptName(prompt interact.Prompt) string {
	if prompt.Title != "" {
		return prompt.Title
	}
	switch prompt.Kind {
	case interact.HostKeyUnknown:
		return "Unknown host key"
	case interact.Passphrase:
		return "Passphrase"
	case interact.Confirm:
		return "Confirm"
	}
	return "prompt"
}
