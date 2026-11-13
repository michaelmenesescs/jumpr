package tui_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"jumper/internal/errs"
	"jumper/internal/interact"
	"jumper/internal/resource"
	"jumper/internal/tui"
)

// replyNow is a fake send that answers every prompt at once with answer.
func replyNow(t *testing.T, answer interact.Answer, seen chan<- interact.Prompt) func(tea.Msg) {
	t.Helper()
	return func(msg tea.Msg) {
		pm, ok := msg.(tui.PromptMsg)
		if !ok {
			t.Errorf("send got %T, want tui.PromptMsg", msg)
			return
		}
		if seen != nil {
			seen <- pm.Prompt
		}
		pm.Reply <- answer
	}
}

func TestPrompterAskReturnsAnswer(t *testing.T) {
	seen := make(chan interact.Prompt, 1)
	p := tui.NewPrompter(replyNow(t, interact.Answer{Choice: "yes"}, seen), 5*time.Second)

	want := interact.Prompt{Kind: interact.Confirm, Title: "kill?", Body: "really?", Choices: []string{"yes", "no"}}
	ans, err := p.Ask(context.Background(), want)
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if ans.Choice != "yes" || ans.Text != "" || ans.Canceled {
		t.Fatalf("Ask returned %+v, want Choice \"yes\"", ans)
	}
	select {
	case got := <-seen:
		if got.Kind != want.Kind || got.Title != want.Title || got.Body != want.Body {
			t.Fatalf("send saw %+v, want %+v", got, want)
		}
	default:
		t.Fatal("Ask never sent a PromptMsg")
	}
}

func TestPrompterAskTimeout(t *testing.T) {
	const timeout = 60 * time.Millisecond
	var sent atomic.Int32
	p := tui.NewPrompter(func(msg tea.Msg) {
		if _, ok := msg.(tui.PromptMsg); !ok {
			t.Errorf("send got %T, want tui.PromptMsg", msg)
		}
		sent.Add(1) // nobody ever answers
	}, timeout)

	start := time.Now()
	_, err := p.Ask(context.Background(), interact.Prompt{Kind: interact.Passphrase, Title: "passphrase", Secret: true})
	elapsed := time.Since(start)
	if errs.KindOf(err) != errs.Interaction {
		t.Fatalf("Ask error is %v (kind %q), want errs.Interaction", err, errs.KindOf(err))
	}
	if elapsed < timeout {
		t.Fatalf("Ask returned after %s, want at least %s", elapsed, timeout)
	}
	if sent.Load() != 1 {
		t.Fatalf("sent %d prompts, want 1", sent.Load())
	}
}

func TestPrompterAskCanceledCtx(t *testing.T) {
	p := tui.NewPrompter(func(tea.Msg) {}, 5*time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	_, err := p.Ask(ctx, interact.Prompt{Kind: interact.Passphrase, Title: "passphrase", Secret: true})
	// The task's contract: a canceled context is errs.Interaction, with the
	// context error kept as the cause.
	if errs.KindOf(err) != errs.Interaction {
		t.Fatalf("Ask error is %v (kind %q), want errs.Interaction", err, errs.KindOf(err))
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Ask error %v does not wrap context.Canceled", err)
	}
}

func TestPrompterDetached(t *testing.T) {
	t.Run("fails while detached", func(t *testing.T) {
		var sent atomic.Int32
		p := tui.NewPrompter(func(tea.Msg) { sent.Add(1) }, 50*time.Millisecond)
		p.SetAttached(false)

		_, err := p.Ask(context.Background(), interact.Prompt{Kind: interact.Passphrase, Title: "passphrase"})
		if errs.KindOf(err) != errs.Interaction {
			t.Fatalf("Ask error is %v (kind %q), want errs.Interaction", err, errs.KindOf(err))
		}
		if sent.Load() != 0 {
			t.Fatalf("sent %d prompts while detached, want 0", sent.Load())
		}
	})

	t.Run("waits for attachment", func(t *testing.T) {
		p := tui.NewPrompter(replyNow(t, interact.Answer{Text: "s3cret"}, nil), 2*time.Second)
		p.SetAttached(false)
		go func() {
			time.Sleep(20 * time.Millisecond)
			p.SetAttached(true)
		}()
		ans, err := p.Ask(context.Background(), interact.Prompt{Kind: interact.Passphrase, Secret: true, Title: "passphrase"})
		if err != nil {
			t.Fatalf("Ask: %v", err)
		}
		if ans.Text != "s3cret" {
			t.Fatalf("Ask returned %+v, want the passphrase", ans)
		}
	})
}

func TestPrompterSerialized(t *testing.T) {
	var mu sync.Mutex
	inFlight, peak := 0, 0
	p := tui.NewPrompter(func(msg tea.Msg) {
		pm, ok := msg.(tui.PromptMsg)
		if !ok {
			t.Errorf("send got %T, want tui.PromptMsg", msg)
			return
		}
		mu.Lock()
		inFlight++
		if inFlight > peak {
			peak = inFlight
		}
		mu.Unlock()
		go func() {
			time.Sleep(20 * time.Millisecond)
			mu.Lock()
			inFlight--
			mu.Unlock()
			pm.Reply <- interact.Answer{Text: "ok"}
		}()
	}, 2*time.Second)

	var wg sync.WaitGroup
	errsOut := make([]error, 2)
	for i := range errsOut {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errsOut[i] = p.Ask(context.Background(), interact.Prompt{
				Kind:   interact.Passphrase,
				Title:  "passphrase",
				Body:   strings.Repeat("connection ", i+1),
				Secret: true,
			})
		}(i)
	}
	wg.Wait()
	for i, err := range errsOut {
		if err != nil {
			t.Fatalf("Ask %d: %v", i, err)
		}
	}
	mu.Lock()
	got := peak
	mu.Unlock()
	if got != 1 {
		t.Fatalf("%d prompts were on screen at once, want 1", got)
	}
}

func TestPrompterHostKeyOnceCached(t *testing.T) {
	var sent atomic.Int32
	p := tui.NewPrompter(func(msg tea.Msg) {
		pm, ok := msg.(tui.PromptMsg)
		if !ok {
			t.Errorf("send got %T, want tui.PromptMsg", msg)
			return
		}
		sent.Add(1)
		pm.Reply <- interact.Answer{Choice: "once"}
	}, 2*time.Second)

	prompt := interact.Prompt{
		Kind:    interact.HostKeyUnknown,
		Title:   "Unknown host key",
		Body:    "host: web-01\nkey type: ssh-ed25519\nfingerprint: SHA256:abc123",
		Choices: []string{"once", "always", "reject"},
	}

	var wg sync.WaitGroup
	answers := make([]interact.Answer, 2)
	for i := range answers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ans, err := p.Ask(context.Background(), prompt)
			if err != nil {
				t.Errorf("Ask %d: %v", i, err)
			}
			answers[i] = ans
		}(i)
	}
	wg.Wait()
	if sent.Load() != 1 {
		t.Fatalf("sent %d prompts for the same host key, want 1", sent.Load())
	}
	for i, ans := range answers {
		if ans.Choice != "once" {
			t.Fatalf("Ask %d returned %+v, want Choice \"once\"", i, ans)
		}
	}

	other := prompt
	other.Body = "host: web-01\nkey type: ssh-ed25519\nfingerprint: SHA256:def456"
	if _, err := p.Ask(context.Background(), other); err != nil {
		t.Fatalf("Ask for a different fingerprint: %v", err)
	}
	if sent.Load() != 2 {
		t.Fatalf("sent %d prompts after a different fingerprint, want 2", sent.Load())
	}
}

// TestInteractRootShowsHostKeyDialog drives the dialog through the root: the
// prompt is shown above everything, "o" is answered with Choice "once" and
// the dialog goes away.
func TestInteractRootShowsHostKeyDialog(t *testing.T) {
	tm := start(t, newRoot(t, resource.Query{Kind: "host", Hosts: ewrHosts(4)}))
	waitFor(t, tm, "web-ewr-001")

	reply := make(chan interact.Answer, 1)
	tm.Send(tui.PromptMsg{
		Prompt: interact.Prompt{
			Kind:    interact.HostKeyUnknown,
			Title:   "Unknown host key",
			Body:    "host: web-ewr-001\nkey type: ssh-ed25519\nfingerprint: SHA256:Zm9vYmFy",
			Choices: []string{"once", "always", "reject"},
		},
		Reply: reply,
	})
	waitFor(t, tm, "Unknown host key", "SHA256:Zm9vYmFy", "known_hosts")

	tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("o")})
	select {
	case ans := <-reply:
		if ans.Choice != "once" {
			t.Fatalf("reply is %+v, want Choice \"once\"", ans)
		}
		if ans.Canceled {
			t.Fatalf("reply is %+v, want it not canceled", ans)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no reply on PromptMsg.Reply")
	}

	screen := finish(t, tm)
	if strings.Contains(screen, "SHA256:Zm9vYmFy") {
		t.Fatalf("host key dialog still on screen after answering:\n%s", screen)
	}
}
