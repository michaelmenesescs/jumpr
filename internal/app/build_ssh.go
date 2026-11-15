package app

import (
	"log/slog"
	"os"
	"sync"

	"jumpr/internal/config"
	"jumpr/internal/interact"
	"jumpr/internal/transport/ssh"
)

// agentSocket is the SSH_AUTH_SOCK the pool asks for at every dial. The path
// changes when the agent is reattached (a new login session, a restarted
// agent), so the pool reads a holder instead of a value captured at startup.
type agentSocket struct {
	mu   sync.Mutex
	path string
}

// Get returns the agent socket path; "" means no agent.
func (a *agentSocket) Get() string {
	if a == nil {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.path
}

// Set replaces the agent socket path.
func (a *agentSocket) Set(path string) {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.path = path
	a.mu.Unlock()
}

// newAgentSocket seeds a holder from the caller's environment, falling back to
// the process environment (only internal/app and cmd may read it, BUILD-PLAN
// §6.1).
func newAgentSocket(o Options) *agentSocket {
	path := o.Env["SSH_AUTH_SOCK"]
	if path == "" {
		path = os.Getenv("SSH_AUTH_SOCK")
	}
	return &agentSocket{path: path}
}

// sshStack is the SSH half of the composition root: a resolver over the
// configured ssh_config files, the shared connection pool and the executor
// built on that pool. The pool is the only thing that owns connections, so the
// caller closes the stack (and thereby the pool) when it is done.
type sshStack struct {
	Resolver  *ssh.Resolver
	Pool      *ssh.Pool
	Transport *ssh.Transport
}

// buildSSH builds the stack with the caller's prompter and an agent socket
// holder seeded from the environment. It is what `jumpr doctor` uses.
func buildSSH(cfg config.Config, o Options, log *slog.Logger) (*sshStack, error) {
	return buildSSHWith(cfg, o, log, o.Prompter, newAgentSocket(o))
}

// buildSSHWith builds the resolver, pool and transport described by cfg, asking
// prompter for unknown host keys and private-key passphrases and reading the
// agent socket from agent at every dial. Nothing is dialled here: the pool
// connects on first use. The caller must Close the stack.
func buildSSHWith(cfg config.Config, o Options, log *slog.Logger, prompter interact.Prompter, agent *agentSocket) (*sshStack, error) {
	resolver, err := ssh.NewResolver(cfg.SSH.ConfigFiles, o.Home, defaultUser(o))
	if err != nil {
		return nil, err
	}

	pool := ssh.NewPool(ssh.PoolOptions{
		Resolver: resolver,
		Dial: ssh.DialOptions{
			Auth: ssh.AuthOptions{
				AgentSocket: agent.Get,
				Prompter:    prompter,
			},
			HostKeys: ssh.HostKeyOptions{
				Files:    cfg.SSH.KnownHosts,
				Prompter: prompter,
			},
			ConnectTimeout: cfg.SSH.ConnectTimeout.Duration(),
			Keepalive:      cfg.SSH.Keepalive.Duration(),
			Log:            log,
		},
		MaxConnsPerBastion: cfg.SSH.MaxConnsPerBastion,
		MaxChannelsPerConn: cfg.SSH.MaxChannelsPerConn,
		IdleTimeout:        cfg.SSH.IdleTimeout.Duration(),
	})

	return &sshStack{
		Resolver: resolver,
		Pool:     pool,
		Transport: ssh.New(pool, ssh.Options{
			ExecTimeout: cfg.SSH.ExecTimeout.Duration(),
			MaxOutput:   int(cfg.SSH.MaxOutput),
			Log:         log,
		}),
	}, nil
}

// Close releases every pooled connection. It is safe to call once.
func (s *sshStack) Close() error {
	if s == nil || s.Pool == nil {
		return nil
	}
	return s.Pool.Close()
}

// defaultUser is the ssh_config fallback user: what Options carries when the
// caller passes an environment, else the process user.
func defaultUser(o Options) string {
	if o.Env != nil {
		if u := o.Env["USER"]; u != "" {
			return u
		}
	}
	return os.Getenv("USER")
}
