// Package k8s is the Kubernetes backend (SPEC §8.2): a client-go client built
// from the standard kubeconfig rules, the context switcher, the session
// namespace and the health probe. The kinds come in task 106.
//
// Nothing here reads os.Stdin/Stdout/Stderr/Getenv: the kubeconfig path, the
// $KUBECONFIG value and the home directory are passed in by internal/app
// (BUILD-PLAN §6.1).
package k8s

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/version"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog/v2"

	"jumper/internal/config"
	"jumper/internal/errs"
)

const (
	// dialTimeout bounds the TCP (and TLS) handshake to the API server.
	dialTimeout = 10 * time.Second
	// requestTimeout bounds one API request, exec-plugin auth included.
	requestTimeout = 30 * time.Second
	// healthTimeout bounds one /version probe.
	healthTimeout = 5 * time.Second
	// defaultQPS and defaultBurst are the client-side rate limits: an informer
	// on many kinds must not be throttled into uselessness, and the API server
	// must not be hammered.
	defaultQPS   = 50
	defaultBurst = 100
	// defaultNamespace is the namespace a context without one gets, as kubectl
	// does.
	defaultNamespace = "default"
)

// Context is one kubeconfig context.
type Context struct {
	Name      string
	Cluster   string
	User      string
	Namespace string // the context's default namespace; may be empty
	Current   bool
	Protected bool // listed in kubernetes.contexts_protected
}

// ClientOptions configures a Client. Every value comes from internal/app: the
// package itself reads no environment (BUILD-PLAN §6.1).
type ClientOptions struct {
	// Kubeconfig is kubernetes.kubeconfig: an explicit path, `~` allowed.
	// Empty means the standard rules below.
	Kubeconfig string
	// KubeconfigEnv is the value of $KUBECONFIG from the environment of the
	// client that started the server. Empty means it was not set.
	KubeconfigEnv string
	// Home is the home directory, used for ~/.kube/config and for `~`.
	Home string
	// Protected lists kubernetes.contexts_protected.
	Protected []string
	// Log receives client-go's warnings. Nil discards them.
	Log *slog.Logger
}

// noKubeconfigError says the kubeconfig is missing or empty. It makes the
// backend unconfigured (a grey dot) rather than broken (a red one).
type noKubeconfigError struct {
	source string
	err    error
}

func (e *noKubeconfigError) Error() string { return "no kubeconfig: " + e.source }
func (e *noKubeconfigError) Unwrap() error { return e.err }

// Client holds one kubeconfig's clients and the session's place in it. Every
// method is safe for concurrent use.
type Client struct {
	rules     *clientcmd.ClientConfigLoadingRules
	source    string
	home      string
	protected map[string]bool
	log       *slog.Logger

	mu          sync.Mutex
	built       bool
	buildErr    error
	cfg         *rest.Config
	typed       kubernetes.Interface
	dyn         dynamic.Interface
	disc        discovery.DiscoveryInterface
	contextName string
	namespace   string
	nsExplicit  bool
	onSwitch    func(context string)
}

// NewClient builds a client over o. An explicit kubeconfig path that does not
// exist is an errs.Config error: a typo in config.yaml must not look like an
// unconfigured cluster. A missing $KUBECONFIG or ~/.kube/config is not an
// error; the client is then unconfigured until a file appears.
func NewClient(o ClientOptions) (*Client, error) {
	home := strings.TrimSpace(o.Home)
	explicit := strings.TrimSpace(o.Kubeconfig)

	c := &Client{
		home:      home,
		protected: make(map[string]bool, len(o.Protected)),
		log:       o.Log,
	}
	if c.log == nil {
		c.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	for _, name := range o.Protected {
		if name = strings.TrimSpace(name); name != "" {
			c.protected[name] = true
		}
	}
	c.rules, c.source = loadingRules(explicit, o.KubeconfigEnv, home)
	redirectKlog(c.log)

	if explicit != "" {
		path := config.ExpandHome(explicit, home)
		if _, err := os.Stat(path); err != nil {
			return nil, errs.Wrap(errs.Config, "k8s.client", err)
		}
	}
	return c, nil
}

// loadingRules returns the kubeconfig loading rules and a one-line description
// of where they point, for messages.
func loadingRules(explicit, env, home string) (*clientcmd.ClientConfigLoadingRules, string) {
	if explicit != "" {
		path := config.ExpandHome(explicit, home)
		return &clientcmd.ClientConfigLoadingRules{ExplicitPath: path}, path
	}
	if env = strings.TrimSpace(env); env != "" {
		var paths []string
		for _, p := range filepath.SplitList(env) {
			if p = strings.TrimSpace(p); p != "" {
				paths = append(paths, config.ExpandHome(p, home))
			}
		}
		if len(paths) > 0 {
			return &clientcmd.ClientConfigLoadingRules{Precedence: paths}, strings.Join(paths, string(filepath.ListSeparator))
		}
	}
	path := filepath.Join(home, ".kube", "config")
	return &clientcmd.ClientConfigLoadingRules{ExplicitPath: path}, path
}

// redirectKlog sends klog (client-go's log) to slog instead of stderr, which
// would corrupt the TUI (BUILD-PLAN §6.1 rule 4).
func redirectKlog(log *slog.Logger) {
	if log == nil {
		return
	}
	// LogToStderr(false) is what actually keeps klog off stderr: SetOutput only
	// moves the log files, and klog still mirrors to stderr by default, which
	// would corrupt the TUI (BUILD-PLAN §6.1 rule 4).
	klog.LogToStderr(false)
	klog.SetOutput(&klogWriter{log: log})
}

// klogWriter turns one klog line into one slog line.
type klogWriter struct{ log *slog.Logger }

// Write logs each complete line and reports the whole length, as io.Writer
// requires.
func (w *klogWriter) Write(p []byte) (int, error) {
	for _, line := range strings.Split(strings.TrimRight(string(p), "\n"), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			w.log.Debug("k8s", "line", line)
		}
	}
	return len(p), nil
}

// Source is where the kubeconfig is read from, for messages.
func (c *Client) Source() string { return c.source }

// Protected reports whether name is listed in kubernetes.contexts_protected.
func (c *Client) Protected(name string) bool { return c.protected[strings.TrimSpace(name)] }

// SetOnSwitch installs the hook a context switch calls once the clients are
// rebuilt. The informer factory of task 106 stops its watchers there.
func (c *Client) SetOnSwitch(fn func(context string)) {
	c.mu.Lock()
	c.onSwitch = fn
	c.mu.Unlock()
}

// clientConfig builds a fresh loading client, optionally overriding the
// current context.
func (c *Client) clientConfig(contextName string) clientcmd.ClientConfig {
	overrides := &clientcmd.ConfigOverrides{}
	if contextName != "" {
		overrides.CurrentContext = contextName
	}
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(c.rules, overrides)
}

// connectLocked builds the clients for contextName ("" = the kubeconfig's
// current context). c.mu must be held.
func (c *Client) connectLocked(contextName string) error {
	cc := c.clientConfig(contextName)
	cfg, err := cc.ClientConfig()
	if err != nil {
		return c.loadError(err)
	}
	cfg.Timeout = requestTimeout
	cfg.Dial = (&net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second}).DialContext
	cfg.QPS = defaultQPS
	cfg.Burst = defaultBurst

	typed, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return errs.Wrap(errs.Config, "k8s.client", err)
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return errs.Wrap(errs.Config, "k8s.client", err)
	}
	disc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return errs.Wrap(errs.Config, "k8s.client", err)
	}

	raw, err := cc.RawConfig()
	if err != nil {
		return c.loadError(err)
	}
	// RawConfig does not apply the CurrentContext override, so the selected
	// name is the one that was asked for.
	name := raw.CurrentContext
	if contextName != "" {
		name = contextName
	}
	kc, ok := raw.Contexts[name]
	if !ok {
		return errs.New(errs.Config, "k8s.client",
			fmt.Sprintf("no context %q in %s", name, c.source))
	}
	c.cfg, c.typed, c.dyn, c.disc = cfg, typed, dyn, disc
	c.contextName = name
	c.built = true
	c.buildErr = nil
	if !c.nsExplicit {
		c.namespace = defaultNamespace
		if kc.Namespace != "" {
			c.namespace = kc.Namespace
		}
	}
	return nil
}

// resetLocked drops the built clients so the next use rebuilds them. c.mu must
// be held.
func (c *Client) resetLocked() {
	c.cfg, c.typed, c.dyn, c.disc = nil, nil, nil, nil
	c.built = false
	c.buildErr = nil
	c.contextName = ""
}

// ensureLocked connects on first use and reports the cached failure.
func (c *Client) ensureLocked() error {
	if c.built {
		return c.buildErr
	}
	if err := c.connectLocked(""); err != nil {
		c.built = true
		c.buildErr = err
		return err
	}
	return nil
}

// loadError turns a clientcmd error into an *errs.Error, marking a missing
// kubeconfig so Health can report grey instead of red.
func (c *Client) loadError(err error) error {
	if clientcmd.IsEmptyConfig(err) || errors.Is(err, os.ErrNotExist) {
		return &errs.Error{Kind: errs.Config, Op: "k8s.client",
			Msg: "no kubeconfig: " + c.source,
			Err: &noKubeconfigError{source: c.source, err: err}}
	}
	return &errs.Error{Kind: errs.Config, Op: "k8s.client", Msg: oneLine(err.Error()), Err: err}
}

// Contexts lists the kubeconfig's contexts sorted by name, the current one
// marked.
func (c *Client) Contexts() ([]Context, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	raw, err := c.clientConfig("").RawConfig()
	if err != nil {
		return nil, c.loadError(err)
	}
	current := raw.CurrentContext
	if c.contextName != "" {
		current = c.contextName
	}
	names := make([]string, 0, len(raw.Contexts))
	for name := range raw.Contexts {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) == 0 {
		return nil, errs.New(errs.Config, "k8s.contexts", "no contexts in "+c.source)
	}
	out := make([]Context, 0, len(names))
	for _, name := range names {
		kc := raw.Contexts[name]
		out = append(out, Context{
			Name:      name,
			Cluster:   kc.Cluster,
			User:      kc.AuthInfo,
			Namespace: kc.Namespace,
			Current:   name == current,
			Protected: c.protected[name],
		})
	}
	return out, nil
}

// CurrentContext is the selected context, or "" when it cannot be read.
func (c *Client) CurrentContext() string {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.contextName != "" {
		return c.contextName
	}
	raw, err := c.clientConfig("").RawConfig()
	if err != nil {
		return ""
	}
	return raw.CurrentContext
}

// SwitchContext selects name and rebuilds the clients against it (SPEC §8.2).
// It reports whether the context is protected, which makes the session
// read-only (SPEC §16). A context the kubeconfig does not define is an
// errs.Config error and changes nothing.
func (c *Client) SwitchContext(ctx context.Context, name string) (bool, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return false, errs.New(errs.Config, "k8s.context", "a context needs a name")
	}
	if err := ctx.Err(); err != nil {
		return false, errs.Wrap(errs.KindOf(err), "k8s.context", err)
	}

	c.mu.Lock()
	raw, err := c.clientConfig("").RawConfig()
	if err != nil {
		c.mu.Unlock()
		return false, c.loadError(err)
	}
	if _, ok := raw.Contexts[name]; !ok {
		c.mu.Unlock()
		return false, errs.New(errs.Config, "k8s.context",
			fmt.Sprintf("no context %q in %s", name, c.source))
	}
	// The old clients go first: nothing must answer for the old context once
	// the new one is selected. If the new one cannot be built the previous
	// context is restored — otherwise contextName stays empty, ensureLocked
	// rebuilds against the kubeconfig's current-context, and the session ends
	// up quietly talking to a different cluster than the one it reports.
	prev := c.contextName
	c.resetLocked()
	if err := c.connectLocked(name); err != nil {
		c.resetLocked()
		if prev != "" && prev != name {
			if rerr := c.connectLocked(prev); rerr != nil {
				c.resetLocked()
			}
		}
		c.mu.Unlock()
		return false, err
	}
	hook := c.onSwitch
	protected := c.protected[name]
	c.mu.Unlock()

	if hook != nil {
		hook(name)
	}
	return protected, nil
}

// Namespace is the session namespace; "" means every namespace. Without an
// explicit `:ns` it is the current context's namespace, or "default".
func (c *Client) Namespace() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.namespace
}

// SetNamespace sets the session namespace. "" and "all" mean every namespace;
// anything else must be a valid namespace name.
func (c *Client) SetNamespace(namespace string) error {
	namespace = strings.TrimSpace(namespace)
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case namespace == "", strings.EqualFold(namespace, "all"):
		c.namespace, c.nsExplicit = "", true
		return nil
	}
	if errsList := validation.IsDNS1123Label(namespace); len(errsList) > 0 {
		return errs.New(errs.Config, "k8s.namespace",
			fmt.Sprintf("%q is not a namespace name: %s", namespace, errsList[0]))
	}
	c.namespace, c.nsExplicit = namespace, true
	return nil
}

// RESTConfig returns the rest configuration of the current context.
func (c *Client) RESTConfig() (*rest.Config, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.ensureLocked(); err != nil {
		return nil, err
	}
	return rest.CopyConfig(c.cfg), nil
}

// Clientset returns the typed client of the current context.
func (c *Client) Clientset() (kubernetes.Interface, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.ensureLocked(); err != nil {
		return nil, err
	}
	return c.typed, nil
}

// Dynamic returns the dynamic client of the current context, for kinds without
// a typed client.
func (c *Client) Dynamic() (dynamic.Interface, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.ensureLocked(); err != nil {
		return nil, err
	}
	return c.dyn, nil
}

// Discovery returns the discovery client of the current context.
func (c *Client) Discovery() (discovery.DiscoveryInterface, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.ensureLocked(); err != nil {
		return nil, err
	}
	return c.disc, nil
}

// ServerVersion is GET /version of the current context, bounded by
// healthTimeout.
func (c *Client) ServerVersion(ctx context.Context) (string, error) {
	disc, err := c.Discovery()
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, healthTimeout)
	defer cancel()

	body, err := disc.RESTClient().Get().AbsPath("/version").Do(ctx).Raw()
	if err != nil {
		return "", &errs.Error{Kind: Classify(err), Op: "k8s.version", Msg: oneLine(err.Error()), Err: err}
	}
	var info version.Info
	if err := json.Unmarshal(body, &info); err != nil {
		return "", &errs.Error{Kind: errs.Parse, Op: "k8s.version",
			Msg: "GET /version did not parse", Err: err}
	}
	if info.GitVersion == "" {
		return "", errs.New(errs.Parse, "k8s.version", "GET /version carried no gitVersion")
	}
	return info.GitVersion, nil
}

// Unconfigured reports whether err says there is no kubeconfig to read.
func Unconfigured(err error) bool {
	var nk *noKubeconfigError
	if errors.As(err, &nk) {
		return true
	}
	return err != nil && clientcmd.IsEmptyConfig(err)
}

// Classify maps a client-go error onto an errs.Kind: 401/403 and a failed
// credential plugin are auth, a timeout is timeout, everything else keeps its
// own kind.
func Classify(err error) errs.Kind {
	if err == nil {
		return ""
	}
	if apierrors.IsUnauthorized(err) || apierrors.IsForbidden(err) {
		return errs.Auth
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "unauthorized"), strings.Contains(msg, "forbidden"),
		strings.Contains(msg, "status 401"), strings.Contains(msg, "status 403"),
		strings.Contains(msg, "provide credentials"),
		strings.Contains(msg, "exec: "), strings.Contains(msg, "getting credentials"):
		return errs.Auth
	case strings.Contains(msg, "no such host"), strings.Contains(msg, "connection refused"),
		strings.Contains(msg, "no route to host"), strings.Contains(msg, "i/o timeout"):
		return errs.Unreachable
	}
	if kind := errs.KindOf(err); kind != errs.Internal {
		return kind
	}
	return errs.Remote
}

// oneLine collapses whitespace so an error stays a one-line table cell
// (BUILD-PLAN §7).
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
