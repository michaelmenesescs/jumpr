package ssh

import (
	"context"
	"errors"
	"net"
	"os"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"

	"jumper/internal/interact"
)

const (
	// agentDialTimeout bounds the local unix-socket dial to the agent. A dead
	// agent must not stall the UI.
	agentDialTimeout = 2 * time.Second
	// passphraseTries is how often the user is asked for one key's passphrase.
	passphraseTries = 3
)

// AuthOptions configures AuthMethods. AgentSocket is a function because the
// agent socket path changes when the ssh-agent is reattached.
type AuthOptions struct {
	AgentSocket func() string
	Prompter    interact.Prompter
	ReadFile    func(string) ([]byte, error)
}

// Auth loads identity files lazily and remembers what each file yielded, so a
// passphrase is asked for once per file while the AuthMethods value lives.
type Auth struct {
	spec HostSpec
	opts AuthOptions

	files sync.Map // identity file path -> *keyEntry
}

type keyEntry struct {
	once    sync.Once
	signers []ssh.Signer
	err     error
}

func newAuth(spec HostSpec, o AuthOptions) *Auth {
	if o.ReadFile == nil {
		o.ReadFile = os.ReadFile
	}
	return &Auth{spec: spec, opts: o}
}

// AuthMethods returns the authentication methods for spec, in order: the ssh
// agent first (unless IdentitiesOnly narrows it to the listed keys), then the
// identity files. The returned cleanup closes any agent connection and must be
// called once the connection attempt is over.
func AuthMethods(spec HostSpec, o AuthOptions) ([]ssh.AuthMethod, func()) {
	a := newAuth(spec, o)

	var mu sync.Mutex
	var conns []net.Conn
	cleanup := func() {
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			c.Close()
		}
		conns = nil
	}

	var methods []ssh.AuthMethod

	socket := ""
	if o.AgentSocket != nil {
		socket = o.AgentSocket()
	}
	if socket != "" {
		if conn, err := net.DialTimeout("unix", socket, agentDialTimeout); err == nil {
			conns = append(conns, conn)
			ag := agent.NewClient(conn)
			if spec.IdentitiesOnly {
				// IdentitiesOnly narrows the *agent's* keys to those named by
				// an identity file's .pub; the identity files themselves are
				// still offered below. The filtered set is resolved now
				// because Go stops the authentication loop at a publickey
				// callback that yields no keys, which would hide the files:
				// an agent without the listed key must not make the host
				// unreachable.
				if signers, err := a.agentSignersFiltered(ag)(); err == nil && len(signers) > 0 {
					methods = append(methods, ssh.PublicKeysCallback(func() ([]ssh.Signer, error) {
						return signers, nil
					}))
				}
			} else {
				methods = append(methods, ssh.PublicKeysCallback(ag.Signers))
			}
		}
	}

	if len(spec.IdentityFiles) > 0 {
		methods = append(methods, ssh.PublicKeysCallback(a.keyFileSigners))
	}
	return methods, cleanup
}

// agentSignersFiltered returns a signers callback that keeps only the agent
// keys whose public key matches a readable .pub file of spec.IdentityFiles.
func (a *Auth) agentSignersFiltered(ag agent.Agent) func() ([]ssh.Signer, error) {
	return func() ([]ssh.Signer, error) {
		signers, err := ag.Signers()
		if err != nil {
			return nil, err
		}
		allowed := a.allowedPubKeys()
		if len(allowed) == 0 {
			return nil, nil
		}
		out := make([]ssh.Signer, 0, len(signers))
		for _, s := range signers {
			if _, ok := allowed[string(s.PublicKey().Marshal())]; ok {
				out = append(out, s)
			}
		}
		return out, nil
	}
}

// allowedPubKeys reads the .pub next to each identity file. Files that are
// missing or unreadable simply do not allow anything.
func (a *Auth) allowedPubKeys() map[string]struct{} {
	allowed := make(map[string]struct{})
	for _, file := range a.spec.IdentityFiles {
		data, err := a.opts.ReadFile(file + ".pub")
		if err != nil {
			continue
		}
		for len(data) > 0 {
			pub, _, _, rest, err := ssh.ParseAuthorizedKey(data)
			if err != nil {
				break
			}
			allowed[string(pub.Marshal())] = struct{}{}
			data = rest
		}
	}
	return allowed
}

// keyFileSigners lazily loads every identity file on first use.
func (a *Auth) keyFileSigners() ([]ssh.Signer, error) {
	var out []ssh.Signer
	for _, file := range a.spec.IdentityFiles {
		signers, err := a.loadIdentity(file)
		if err != nil {
			return nil, err
		}
		out = append(out, signers...)
	}
	return out, nil
}

// loadIdentity parses one identity file at most once.
func (a *Auth) loadIdentity(file string) ([]ssh.Signer, error) {
	e, _ := a.files.LoadOrStore(file, &keyEntry{})
	entry := e.(*keyEntry)
	entry.once.Do(func() { entry.signers, entry.err = a.parseIdentity(file) })
	return entry.signers, entry.err
}

// parseIdentity returns the signers of one key file. Missing, unreadable and
// unparseable files are skipped (nil, nil), as is a key whose passphrase stays
// unknown after passphraseTries attempts.
func (a *Auth) parseIdentity(file string) ([]ssh.Signer, error) {
	pem, err := a.opts.ReadFile(file)
	if err != nil {
		return nil, nil
	}
	signer, err := ssh.ParsePrivateKey(pem)
	if err == nil {
		return []ssh.Signer{signer}, nil
	}
	var missing *ssh.PassphraseMissingError
	if !errors.As(err, &missing) {
		return nil, nil
	}
	if a.opts.Prompter == nil {
		return nil, nil
	}
	var askErr error
	for i := 0; i < passphraseTries; i++ {
		ans, err := a.opts.Prompter.Ask(context.Background(), interact.Prompt{
			Kind:   interact.Passphrase,
			Title:  "Passphrase for " + file,
			Secret: true,
		})
		if err != nil {
			askErr = err
			break
		}
		if ans.Canceled {
			return nil, nil
		}
		if signer, err = ssh.ParsePrivateKeyWithPassphrase(pem, []byte(ans.Text)); err == nil {
			return []ssh.Signer{signer}, nil
		}
	}
	return nil, askErr
}
