package inventory

import (
	"context"
	"path"
	"time"

	"jumpr/internal/errs"
	"jumpr/internal/resource"
	sshcfg "jumpr/internal/transport/ssh"
)

// SSHConfigSource lists every literal Host in the ssh_config files.
type SSHConfigSource struct {
	Resolver *sshcfg.Resolver
	// ExcludePatterns are globs matched against the alias; matches are skipped
	// (bastions, for example).
	ExcludePatterns []string
	// Now stamps SeenAt; nil means time.Now.
	Now func() time.Time
}

// NewSSHConfigSource wraps a resolver as an inventory source.
func NewSSHConfigSource(r *sshcfg.Resolver) *SSHConfigSource {
	return &SSHConfigSource{Resolver: r}
}

// Name is both the source name and its label namespace.
func (s *SSHConfigSource) Name() string { return "ssh" }

// Load resolves every alias in the config. Individual hosts that fail to
// resolve are skipped; only cancellation aborts the load.
func (s *SSHConfigSource) Load(ctx context.Context) ([]Host, error) {
	if s.Resolver == nil {
		return nil, errs.New(errs.Config, "inventory.ssh", "no ssh_config resolver")
	}
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	stamp := now()
	aliases := s.Resolver.Hosts()
	hosts := make([]Host, 0, len(aliases))
	for _, alias := range aliases {
		if err := ctx.Err(); err != nil {
			return nil, errs.Wrap(errs.Canceled, "inventory.ssh", err)
		}
		if s.excluded(alias) {
			continue
		}
		spec, err := s.Resolver.Resolve(alias)
		if err != nil {
			continue
		}
		h := Host{
			ID:     resource.NormalizeHostID(alias),
			Addr:   alias,
			Labels: make(map[string]string),
			SeenAt: stamp,
		}
		if spec.HostName != "" {
			if hn := resource.NormalizeHostID(spec.HostName); hn != h.ID {
				h.Aliases = append(h.Aliases, string(hn))
			}
		}
		if spec.User != "" {
			h.Labels["user"] = spec.User
		}
		if len(spec.Jumps) > 0 && spec.Jumps[0] != "" {
			h.Labels["jump"] = spec.Jumps[0]
		}
		hosts = append(hosts, h)
	}
	return hosts, nil
}

func (s *SSHConfigSource) excluded(alias string) bool {
	for _, pat := range s.ExcludePatterns {
		if ok, err := path.Match(pat, alias); err == nil && ok {
			return true
		}
	}
	return false
}
