// Package ssh resolves hosts from ssh_config files.
package ssh

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kevinburke/ssh_config"

	"jumpr/internal/errs"
)

const maxChain = 5

// HostSpec is everything needed to dial one host.
type HostSpec struct {
	Alias, HostName, User string
	Port                  int
	IdentityFiles         []string
	IdentitiesOnly        bool
	KnownHostsFiles       []string
	StrictHostKeyChecking string
	ConnectTimeout        time.Duration
	ServerAliveInterval   time.Duration
	Jumps                 []string
	ProxyCommand          string
}

// Resolver answers ssh_config lookups over a fixed list of files.
type Resolver struct {
	cfgs        []*ssh_config.Config
	home        string
	defaultUser string
}

// NewResolver decodes each file; missing files are skipped.
func NewResolver(files []string, home, defaultUser string) (*Resolver, error) {
	r := &Resolver{home: home, defaultUser: defaultUser}
	for _, f := range files {
		fh, err := os.Open(f)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, errs.Wrap(errs.Config, "sshconfig", fmt.Errorf("%s: %w", f, err))
		}
		cfg, err := ssh_config.Decode(fh)
		fh.Close()
		if err != nil {
			return nil, errs.Wrap(errs.Config, "sshconfig", fmt.Errorf("%s: %w", f, err))
		}
		r.cfgs = append(r.cfgs, cfg)
	}
	return r, nil
}

func (r *Resolver) get(alias, key string) string {
	for _, c := range r.cfgs {
		if v, err := c.Get(alias, key); err == nil && v != "" {
			return v
		}
	}
	return ""
}

func (r *Resolver) getAll(alias, key string) []string {
	for _, c := range r.cfgs {
		if v, err := c.GetAll(alias, key); err == nil && len(v) > 0 {
			return v
		}
	}
	return nil
}

func expandTokens(s, host, user string, port int) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '%' || i+1 == len(s) {
			b.WriteByte(s[i])
			continue
		}
		i++
		switch s[i] {
		case '%':
			b.WriteByte('%')
		case 'h':
			b.WriteString(host)
		case 'r':
			b.WriteString(user)
		case 'p':
			b.WriteString(strconv.Itoa(port))
		default:
			b.WriteByte('%')
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

func (r *Resolver) expandPath(s, host, user string, port int) string {
	s = expandTokens(s, host, user, port)
	if s == "~" {
		return r.home
	}
	if strings.HasPrefix(s, "~/") {
		return strings.TrimRight(r.home, "/") + s[1:]
	}
	return s
}

func seconds(v string) time.Duration {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n < 0 {
		return 0
	}
	return time.Duration(n) * time.Second
}

// Resolve returns the dial parameters for alias.
func (r *Resolver) Resolve(alias string) (HostSpec, error) {
	s := HostSpec{Alias: alias, User: r.defaultUser, Port: 22, StrictHostKeyChecking: "ask"}
	if v := r.get(alias, "User"); v != "" {
		s.User = v
	}
	if v := r.get(alias, "Port"); v != "" {
		p, err := strconv.Atoi(v)
		if err != nil || p < 1 || p > 65535 {
			return HostSpec{}, errs.New(errs.Config, "sshconfig", fmt.Sprintf("%s: invalid Port %q", alias, v))
		}
		s.Port = p
	}
	s.HostName = alias
	if v := r.get(alias, "HostName"); v != "" {
		s.HostName = expandTokens(v, alias, s.User, s.Port)
	}
	for _, v := range r.getAll(alias, "IdentityFile") {
		s.IdentityFiles = append(s.IdentityFiles, r.expandPath(v, s.HostName, s.User, s.Port))
	}
	s.IdentitiesOnly = strings.EqualFold(r.get(alias, "IdentitiesOnly"), "yes")
	if v := r.get(alias, "UserKnownHostsFile"); v != "" && !strings.EqualFold(v, "none") {
		for _, f := range strings.Fields(v) {
			s.KnownHostsFiles = append(s.KnownHostsFiles, r.expandPath(f, s.HostName, s.User, s.Port))
		}
	} else if v == "" {
		s.KnownHostsFiles = []string{r.expandPath("~/.ssh/known_hosts", s.HostName, s.User, s.Port)}
	}
	if v := r.get(alias, "StrictHostKeyChecking"); v != "" {
		s.StrictHostKeyChecking = strings.ToLower(v)
	}
	s.ConnectTimeout = seconds(r.get(alias, "ConnectTimeout"))
	s.ServerAliveInterval = seconds(r.get(alias, "ServerAliveInterval"))
	if v := r.get(alias, "ProxyJump"); v != "" && !strings.EqualFold(v, "none") {
		for _, j := range strings.Split(v, ",") {
			if j = strings.TrimSpace(j); j != "" {
				s.Jumps = append(s.Jumps, j)
			}
		}
	}
	if v := r.get(alias, "ProxyCommand"); v != "" && !strings.EqualFold(v, "none") {
		s.ProxyCommand = v
	}
	return s, nil
}

// parseJump splits [user@]host[:port].
func parseJump(j string) (user, host string, port int, err error) {
	if i := strings.LastIndex(j, "@"); i >= 0 {
		user, j = j[:i], j[i+1:]
	}
	host = j
	if strings.HasPrefix(j, "[") {
		if i := strings.Index(j, "]"); i > 0 {
			host = j[1:i]
			rest := j[i+1:]
			if strings.HasPrefix(rest, ":") {
				port, err = strconv.Atoi(rest[1:])
			}
		}
	} else if strings.Count(j, ":") == 1 {
		i := strings.Index(j, ":")
		host = j[:i]
		port, err = strconv.Atoi(j[i+1:])
	}
	if err != nil || host == "" || port < 0 || port > 65535 {
		return "", "", 0, fmt.Errorf("invalid ProxyJump element %q", j)
	}
	return user, host, port, nil
}

// Chain returns the specs to dial in order, ending with alias.
func (r *Resolver) Chain(alias string) ([]HostSpec, error) {
	chain, err := r.chain(alias, "", 0, map[string]bool{})
	if err != nil {
		return nil, err
	}
	if len(chain) > maxChain {
		return nil, errs.New(errs.Config, "sshconfig", fmt.Sprintf("%s: jump chain too long (%d > %d)", alias, len(chain), maxChain))
	}
	return chain, nil
}

func (r *Resolver) chain(alias, user string, port int, stack map[string]bool) ([]HostSpec, error) {
	if stack[alias] {
		return nil, errs.New(errs.Config, "sshconfig", fmt.Sprintf("%s: ProxyJump loop", alias))
	}
	spec, err := r.Resolve(alias)
	if err != nil {
		return nil, err
	}
	if user != "" {
		spec.User = user
	}
	if port != 0 {
		spec.Port = port
	}
	stack[alias] = true
	defer delete(stack, alias)
	var out []HostSpec
	for _, j := range spec.Jumps {
		u, h, p, err := parseJump(j)
		if err != nil {
			return nil, errs.Wrap(errs.Config, "sshconfig", fmt.Errorf("%s: %w", alias, err))
		}
		sub, err := r.chain(h, u, p, stack)
		if err != nil {
			return nil, err
		}
		out = append(out, sub...)
		if len(out) > maxChain {
			return nil, errs.New(errs.Config, "sshconfig", fmt.Sprintf("%s: jump chain too long", alias))
		}
	}
	return append(out, spec), nil
}

// Hosts lists all literal Host patterns, sorted and de-duplicated.
func (r *Resolver) Hosts() []string {
	seen := map[string]bool{}
	for _, c := range r.cfgs {
		for _, h := range c.Hosts {
			for _, p := range h.Patterns {
				s := p.String()
				if s == "" || strings.ContainsAny(s, "*?!") {
					continue
				}
				seen[s] = true
			}
		}
	}
	out := make([]string, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
