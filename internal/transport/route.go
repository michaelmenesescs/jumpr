package transport

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"

	"jumper/internal/errs"
	"jumper/internal/resource"
)

// Transport names a RouteRule may select. Native is the in-process SSH client;
// SystemSSH is the fallback that shells out to /usr/bin/ssh.
const (
	Native    = "native"
	SystemSSH = "system-ssh"
)

// RouteRule sends the hosts matching Match to Transport. Match is a glob in
// path.Match syntax, matched against the host ID (for example
// "*.legacy.example.internal"); a bare host name matches only that host.
type RouteRule struct {
	Match     string
	Transport string
}

// RouterBackend is a transport that can do everything a Router delegates:
// run commands, stream a command's output and open a shell. Both the native
// SSH transport and the system-ssh fallback implement it.
type RouterBackend interface {
	Executor
	Streamer
	ShellOpener
}

// Router implements Executor, Streamer and ShellOpener by delegating every
// call to the native backend or to the system-ssh backend, decided per host by
// the configured glob rules. It is safe for concurrent use: the routing table
// is read-only after NewRouter.
type Router struct {
	native   RouterBackend
	fallback RouterBackend
	rules    []routeRule
}

// routeRule is one compiled RouteRule.
type routeRule struct {
	match    string
	fallback bool
}

var (
	_ Executor    = (*Router)(nil)
	_ Streamer    = (*Router)(nil)
	_ ShellOpener = (*Router)(nil)
)

// NewRouter returns a Router over native and fallback. native is required; a
// rule that selects system-ssh without a fallback backend, an unknown transport
// name and a pattern that does not compile are all errs.Config errors.
func NewRouter(native, fallback RouterBackend, rules []RouteRule) (*Router, error) {
	if native == nil {
		return nil, errs.New(errs.Config, "transport.router", "the native transport is required")
	}
	r := &Router{native: native, fallback: fallback}
	for i, rule := range rules {
		name := strings.TrimSpace(rule.Transport)
		switch name {
		case "", Native:
			// An empty transport means native: the rule exists to say so.
			name = Native
		case SystemSSH:
		default:
			return nil, errs.New(errs.Config, "transport.router",
				fmt.Sprintf("ssh.transports[%d].transport: %q is not native or system-ssh", i, rule.Transport))
		}
		match := strings.TrimSpace(rule.Match)
		if match == "" {
			return nil, errs.New(errs.Config, "transport.router",
				fmt.Sprintf("ssh.transports[%d].match: a host pattern is required", i))
		}
		if _, err := path.Match(match, "host"); err != nil {
			return nil, errs.New(errs.Config, "transport.router",
				fmt.Sprintf("ssh.transports[%d].match: %v", i, err))
		}
		if name == SystemSSH && fallback == nil {
			return nil, errs.New(errs.Config, "transport.router",
				fmt.Sprintf("ssh.transports[%d]: %q needs the system-ssh transport", i, match))
		}
		r.rules = append(r.rules, routeRule{match: match, fallback: name == SystemSSH})
	}
	return r, nil
}

// Name implements Executor. Routing is decided per host, so the router itself
// is named "router"; the transport a call actually used is ExecResult.Via.
func (r *Router) Name() string { return "router" }

// Exec implements Executor: it runs cmd on host through the transport its
// rules select. A native ProxyCommand rejection is returned with the
// ssh.transports stanza that would route the host to system ssh.
func (r *Router) Exec(ctx context.Context, host resource.HostID, cmd Command) (ExecResult, error) {
	backend, fallback := r.pick(host)
	res, err := backend.Exec(ctx, host, cmd)
	return res, r.annotate(err, host, fallback)
}

// Start implements Streamer.
func (r *Router) Start(ctx context.Context, host resource.HostID, cmd Command) (Stream, error) {
	backend, fallback := r.pick(host)
	st, err := backend.Start(ctx, host, cmd)
	return st, r.annotate(err, host, fallback)
}

// OpenShell implements ShellOpener.
func (r *Router) OpenShell(ctx context.Context, host resource.HostID, req PTYRequest) (Shell, error) {
	backend, fallback := r.pick(host)
	sh, err := backend.OpenShell(ctx, host, req)
	return sh, r.annotate(err, host, fallback)
}

// Via reports the transport name a call on host would use, without running
// anything: "native" or "system-ssh".
func (r *Router) Via(host resource.HostID) string {
	if _, fallback := r.pick(host); fallback {
		return SystemSSH
	}
	return Native
}

// pick returns the backend for host and whether it is the fallback. The last
// matching rule wins, so a later, more specific pattern can undo an earlier
// broad one.
func (r *Router) pick(host resource.HostID) (RouterBackend, bool) {
	fallback := false
	for _, rule := range r.rules {
		if ok, _ := path.Match(rule.match, string(host)); ok {
			fallback = rule.fallback
		}
	}
	if fallback && r.fallback != nil {
		return r.fallback, true
	}
	return r.native, false
}

// annotate adds the config stanza that fixes a native ProxyCommand rejection.
// Errors of the fallback backend are returned unchanged.
func (r *Router) annotate(err error, host resource.HostID, fallback bool) error {
	if err == nil || fallback {
		return err
	}
	if errs.KindOf(err) != errs.Config {
		return err
	}
	if !strings.Contains(strings.ToLower(err.Error()), "proxycommand") {
		return err
	}
	return WithSystemSSHSuggestion(err, host)
}

// WithSystemSSHSuggestion returns err with the config stanza that routes host
// through the system-ssh transport appended to its message. Any other error is
// returned unchanged. The *errs.Error is copied, never mutated.
func WithSystemSSHSuggestion(err error, host resource.HostID) error {
	if err == nil {
		return nil
	}
	var e *errs.Error
	if !errors.As(err, &e) {
		return err
	}
	if strings.Contains(e.Msg, "ssh:\n  transports:") {
		return err
	}
	cp := *e
	cp.Msg = e.Msg + "\nadd to config.yaml:\n" + SystemSSHStanza(string(host))
	return &cp
}

// SystemSSHStanza renders the config snippet that routes host to the system-ssh
// transport: three YAML lines with the host as the match pattern.
func SystemSSHStanza(host string) string {
	return fmt.Sprintf("ssh:\n  transports:\n    - match: %q\n      transport: system-ssh", host)
}
