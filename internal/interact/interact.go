// Package interact defines the questions jumpr can ask the user and the
// Prompter interface every front end implements.
package interact

import "context"

type Kind int

const (
	HostKeyUnknown Kind = iota // Body holds host, key type and SHA256 fingerprint; Choices: "once", "always", "reject"
	Passphrase                 // Secret = true; Answer.Text is the passphrase
	Confirm                    // Choices: "yes", "no"
)

type Prompt struct {
	Kind    Kind
	Title   string
	Body    string
	Secret  bool
	Choices []string
}

type Answer struct {
	Choice   string
	Text     string
	Canceled bool
}

// Prompter asks the user. Implementations must return errs.Interaction when nobody can answer
// (no UI, detached past the timeout). Safe for concurrent use; prompts are shown one at a time.
type Prompter interface {
	Ask(ctx context.Context, p Prompt) (Answer, error)
}
