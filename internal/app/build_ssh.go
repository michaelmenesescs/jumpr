package app

import (
	"log/slog"
	"os"

	"jumper/internal/config"
	"jumper/internal/transport/ssh"
)

// sshStack is the SSH half of the composition root: a resolver over the
// configured ssh_config files, the shared connection pool and the executor
// built on that pool. The pool is the only thing that owns connections, so the
// caller closes the stack (and thereby the pool) when it is done.
type sshStack struct {
	Resolver  *ssh.Resolver
	Pool      *ssh.Pool
	Transport *ssh.Transport
}

// buildSSH builds the resolver, pool and transport described by cfg. Nothing is
// dialled here: the pool connects on first use. Prompts (unknown host key,
// private-key passphrase) go to o.Prompter. The caller must Close the stack.
func buildSSH(cfg config.Config, o Options, log *slog.Logger) (*sshStack, error) {
	resolver, err := ssh.NewResolver(cfg.SSH.ConfigFiles, o.Home, defaultUser(o))
	if err != nil {
		return nil, err
	}

	pool := ssh.NewPool(ssh.PoolOptions{
		Resolver: resolver,
		Dial: ssh.DialOptions{
			Auth: ssh.AuthOptions{
				AgentSocket: func() string { return os.Getenv("SSH_AUTH_SOCK") },
				Prompter:    o.Prompter,
			},
			HostKeys: ssh.HostKeyOptions{
				Files:    cfg.SSH.KnownHosts,
				Prompter: o.Prompter,
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
