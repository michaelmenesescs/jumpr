package ssh

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/skeema/knownhosts"
	"golang.org/x/crypto/ssh"
	xknownhosts "golang.org/x/crypto/ssh/knownhosts"

	"jumper/internal/errs"
	"jumper/internal/interact"
)

// HostKeyOptions configures HostKeyConfig.
type HostKeyOptions struct {
	Files    []string
	Strict   string
	Prompter interact.Prompter
	Append   func(file, line string) error
}

// HostKeyConfig returns a host key callback for spec, the host key algorithms
// worth offering (so a server with several key types does not fail as a key
// change), and an error when known_hosts cannot be read.
func HostKeyConfig(spec HostSpec, o HostKeyOptions) (ssh.HostKeyCallback, []string, error) {
	files := o.Files
	if len(files) == 0 {
		files = spec.KnownHostsFiles
	}
	strict := strings.ToLower(strings.TrimSpace(o.Strict))
	if strict == "" {
		strict = strings.ToLower(strings.TrimSpace(spec.StrictHostKeyChecking))
	}

	// Lookups in known_hosts take a host:port; the file's own form (bare host on
	// port 22, [host]:port otherwise) is what Normalize produces.
	hostPort := net.JoinHostPort(spec.HostName, strconv.Itoa(spec.Port))
	address := knownhosts.Normalize(hostPort)

	existing := make([]string, 0, len(files))
	for _, f := range files {
		if _, err := os.Stat(f); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue // a missing known_hosts is an empty one
			}
			return nil, nil, errs.Wrap(errs.Config, "ssh.hostkey", fmt.Errorf("%s: %w", f, err))
		}
		existing = append(existing, f)
	}

	kh, err := knownhosts.NewDB(existing...)
	if err != nil {
		return nil, nil, errs.Wrap(errs.Config, "ssh.hostkey", err)
	}

	algos := kh.HostKeyAlgorithms(hostPort)

	var mu sync.Mutex
	accepted := make(map[string]struct{}) // keys accepted with "once"

	acceptedKey := func(key ssh.PublicKey) bool {
		mu.Lock()
		defer mu.Unlock()
		_, ok := accepted[string(key.Marshal())]
		return ok
	}
	remember := func(key ssh.PublicKey) {
		mu.Lock()
		defer mu.Unlock()
		accepted[string(key.Marshal())] = struct{}{}
	}

	appendLine := func(line string) error {
		if o.Append != nil && len(files) > 0 {
			return o.Append(files[0], line)
		}
		if len(files) > 0 {
			fh, err := os.OpenFile(files[0], os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
			if err != nil {
				return err
			}
			defer fh.Close()
			if _, err := fh.WriteString(line + "\n"); err != nil {
				return err
			}
			return nil
		}
		return nil
	}

	cb := func(_ string, remote net.Addr, key ssh.PublicKey) error {
		if acceptedKey(key) {
			return nil
		}
		err := kh.HostKeyCallback()(hostPort, remote, key)
		if err == nil {
			return nil
		}
		if knownhosts.IsHostKeyChanged(err) {
			return errs.New(errs.HostKey, "ssh.hostkey", fmt.Sprintf(
				"host key for %s changed (known_hosts line %d); refusing", spec.HostName, changedLine(err)))
		}
		if !knownhosts.IsHostUnknown(err) {
			return errs.Wrap(errs.HostKey, "ssh.hostkey", err)
		}

		switch strict {
		case "yes":
			return errs.New(errs.HostKey, "ssh.hostkey", fmt.Sprintf(
				"host key for %s is not in known_hosts (StrictHostKeyChecking yes)", spec.HostName))
		case "no", "accept-new":
			if err := appendLine(knownhosts.Line([]string{address}, key)); err != nil {
				remember(key) // cannot record it, but the user asked not to check
			}
			return nil
		}

		if o.Prompter == nil {
			return unknownHostKey(spec.HostName, errs.New(errs.Interaction, "ssh.hostkey",
				fmt.Sprintf("host key for %s is unknown and nobody can be asked", spec.HostName)))
		}
		ans, askErr := o.Prompter.Ask(context.Background(), interact.Prompt{
			Kind:  interact.HostKeyUnknown,
			Title: "Unknown host key",
			Body: fmt.Sprintf("host: %s\nkey type: %s\nfingerprint: %s",
				spec.HostName, key.Type(), ssh.FingerprintSHA256(key)),
			Choices: []string{"once", "always", "reject"},
		})
		if askErr != nil {
			return unknownHostKey(spec.HostName, askErr)
		}
		if ans.Canceled {
			return errs.New(errs.HostKey, "ssh.hostkey", fmt.Sprintf(
				"host key for %s rejected", spec.HostName))
		}
		switch strings.ToLower(strings.TrimSpace(ans.Choice)) {
		case "once":
			remember(key)
			return nil
		case "always":
			if err := appendLine(knownhosts.Line([]string{address}, key)); err != nil {
				return errs.Wrap(errs.HostKey, "ssh.hostkey",
					fmt.Errorf("host key for %s accepted but not saved: %w", spec.HostName, err))
			}
			remember(key)
			return nil
		default: // "reject", empty, anything unexpected
			return errs.New(errs.HostKey, "ssh.hostkey", fmt.Sprintf(
				"host key for %s rejected", spec.HostName))
		}
	}
	return cb, algos, nil
}

// unknownHostKey keeps the interaction failure as the cause but reports the
// host key kind, because the dial cannot go on.
func unknownHostKey(host string, cause error) error {
	e := errs.Wrap(errs.HostKey, "ssh.hostkey", cause)
	e.Host = host
	return e
}

// changedLine returns the known_hosts line of the first stored key that did
// not match.
func changedLine(err error) int {
	var keyErr *xknownhosts.KeyError
	if !errors.As(err, &keyErr) {
		return 0
	}
	for _, k := range keyErr.Want {
		if k.Line > 0 {
			return k.Line
		}
	}
	return 0
}
