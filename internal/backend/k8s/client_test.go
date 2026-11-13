package k8s

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"jumper/internal/backend"
	"jumper/internal/errs"
	"jumper/internal/resource"
)

// testKubeconfig is the sanitised two-context kubeconfig of testdata/.
func testKubeconfig(t *testing.T) string {
	t.Helper()
	path, err := filepath.Abs(filepath.Join("testdata", "kubeconfig"))
	if err != nil {
		t.Fatalf("abs testdata: %v", err)
	}
	return path
}

// ctxSpec is one context a test kubeconfig defines.
type ctxSpec struct {
	name      string
	server    string
	namespace string
}

// writeKubeconfig writes a kubeconfig for ctxs, current-context set to current,
// into a temporary directory and returns its path.
func writeKubeconfig(t *testing.T, current string, ctxs ...ctxSpec) string {
	t.Helper()
	var b strings.Builder
	b.WriteString("apiVersion: v1\nkind: Config\n")
	fmt.Fprintf(&b, "current-context: %s\nclusters:\n", current)
	for _, c := range ctxs {
		fmt.Fprintf(&b, "  - name: %s-cluster\n    cluster:\n      server: %s\n      insecure-skip-tls-verify: true\n", c.name, c.server)
	}
	b.WriteString("users:\n")
	for _, c := range ctxs {
		fmt.Fprintf(&b, "  - name: %s-user\n    user:\n      token: REDACTED\n", c.name)
	}
	b.WriteString("contexts:\n")
	for _, c := range ctxs {
		fmt.Fprintf(&b, "  - name: %s\n    context:\n      cluster: %s-cluster\n      user: %s-user\n", c.name, c.name, c.name)
		if c.namespace != "" {
			fmt.Fprintf(&b, "      namespace: %s\n", c.namespace)
		}
	}
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatalf("write kubeconfig: %v", err)
	}
	return path
}

// versionServer is a minimal API server that answers GET /version.
func versionServer(t *testing.T, version string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/version", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/version" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"major":"1","minor":"34","gitVersion":%q,"gitCommit":"0000000",`+
			`"gitTreeState":"clean","buildDate":"2026-11-01T00:00:00Z",`+
			`"goVersion":"go1.27","compiler":"gc","platform":"darwin/arm64"}`, version)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestClientContexts lists the kubeconfig's contexts with the current one
// marked (SPEC §8.2).
func TestClientContexts(t *testing.T) {
	c := newTestClient(t, ClientOptions{
		Kubeconfig: testKubeconfig(t),
		Protected:  []string{"beta"},
	})
	ctxs, err := c.Contexts()
	if err != nil {
		t.Fatalf("Contexts: %v", err)
	}
	if len(ctxs) != 2 {
		t.Fatalf("got %d contexts, want 2: %+v", len(ctxs), ctxs)
	}
	alpha, beta := ctxs[0], ctxs[1]
	if alpha.Name != "alpha" || beta.Name != "beta" {
		t.Fatalf("contexts not sorted by name: %+v", ctxs)
	}
	if !alpha.Current || beta.Current {
		t.Errorf("current flags = alpha:%v beta:%v, want alpha", alpha.Current, beta.Current)
	}
	if got, want := alpha.Cluster, "alpha-cluster"; got != want {
		t.Errorf("alpha cluster = %q, want %q", got, want)
	}
	if got, want := alpha.User, "alpha-user"; got != want {
		t.Errorf("alpha user = %q, want %q", got, want)
	}
	if got, want := alpha.Namespace, "alpha-ns"; got != want {
		t.Errorf("alpha namespace = %q, want %q", got, want)
	}
	if alpha.Protected {
		t.Error("alpha must not be protected")
	}
	if !beta.Protected {
		t.Error("beta is in contexts_protected and must be marked protected")
	}
}

// TestClientSwitchContext selects another context and rebuilds the client.
func TestClientSwitchContext(t *testing.T) {
	c := newTestClient(t, ClientOptions{Kubeconfig: testKubeconfig(t)})
	var switched []string
	c.SetOnSwitch(func(name string) { switched = append(switched, name) })

	if got, want := c.CurrentContext(), "alpha"; got != want {
		t.Fatalf("CurrentContext = %q, want %q", got, want)
	}
	protected, err := c.SwitchContext(context.Background(), "beta")
	if err != nil {
		t.Fatalf("SwitchContext: %v", err)
	}
	if protected {
		t.Error("beta is not protected")
	}
	if got, want := c.CurrentContext(), "beta"; got != want {
		t.Errorf("CurrentContext = %q, want %q", got, want)
	}
	cfg, err := c.RESTConfig()
	if err != nil {
		t.Fatalf("RESTConfig: %v", err)
	}
	if got, want := cfg.Host, "https://beta.example.internal:6443"; got != want {
		t.Errorf("host = %q, want %q", got, want)
	}
	if got, want := cfg.QPS, float32(defaultQPS); got != want {
		t.Errorf("QPS = %v, want %v", got, want)
	}
	if got, want := cfg.Burst, defaultBurst; got != want {
		t.Errorf("burst = %d, want %d", got, want)
	}
	if got, want := cfg.Timeout, requestTimeout; got != want {
		t.Errorf("timeout = %s, want %s", got, want)
	}
	if len(switched) != 1 || switched[0] != "beta" {
		t.Errorf("onSwitch calls = %v, want [beta]", switched)
	}

	// The contexts listing follows the switch.
	ctxs, err := c.Contexts()
	if err != nil {
		t.Fatalf("Contexts: %v", err)
	}
	for _, kc := range ctxs {
		if kc.Current != (kc.Name == "beta") {
			t.Errorf("context %q current = %v after the switch", kc.Name, kc.Current)
		}
	}
}

// TestClientSwitchProtected reports the protected flag so the session can go
// read-only (SPEC §16).
func TestClientSwitchProtected(t *testing.T) {
	c := newTestClient(t, ClientOptions{
		Kubeconfig: testKubeconfig(t),
		Protected:  []string{"beta"},
	})
	protected, err := c.SwitchContext(context.Background(), "beta")
	if err != nil {
		t.Fatalf("SwitchContext: %v", err)
	}
	if !protected {
		t.Error("beta is in contexts_protected: want protected")
	}
}

// TestClientSwitchUnknownContext changes nothing and is a config error.
func TestClientSwitchUnknownContext(t *testing.T) {
	c := newTestClient(t, ClientOptions{Kubeconfig: testKubeconfig(t)})
	if _, err := c.SwitchContext(context.Background(), "gamma"); err == nil {
		t.Fatal("switching to an unknown context must fail")
	} else if got := errs.KindOf(err); got != errs.Config {
		t.Errorf("kind = %q, want %q", got, errs.Config)
	}
	if got, want := c.CurrentContext(), "alpha"; got != want {
		t.Errorf("CurrentContext = %q after a failed switch, want %q", got, want)
	}
}

// TestClientSwitchRebuildsClients proves the switch really rebuilds the
// client: each context talks to its own server.
func TestClientSwitchRebuildsClients(t *testing.T) {
	alpha := versionServer(t, "v1.34.1")
	beta := versionServer(t, "v1.34.2")
	path := writeKubeconfig(t, "alpha",
		ctxSpec{name: "alpha", server: alpha.URL},
		ctxSpec{name: "beta", server: beta.URL},
	)
	c := newTestClient(t, ClientOptions{Kubeconfig: path})

	got, err := c.ServerVersion(context.Background())
	if err != nil {
		t.Fatalf("ServerVersion(alpha): %v", err)
	}
	if got != "v1.34.1" {
		t.Errorf("alpha version = %q, want v1.34.1", got)
	}
	if _, err := c.SwitchContext(context.Background(), "beta"); err != nil {
		t.Fatalf("SwitchContext: %v", err)
	}
	got, err = c.ServerVersion(context.Background())
	if err != nil {
		t.Fatalf("ServerVersion(beta): %v", err)
	}
	if got != "v1.34.2" {
		t.Errorf("beta version = %q, want v1.34.2", got)
	}
}

// TestClientKubeconfigFromEnv uses the $KUBECONFIG value internal/app passes in
// (BUILD-PLAN §6.1: the package never reads the environment itself).
func TestClientKubeconfigFromEnv(t *testing.T) {
	c := newTestClient(t, ClientOptions{KubeconfigEnv: testKubeconfig(t)})
	ctxs, err := c.Contexts()
	if err != nil {
		t.Fatalf("Contexts: %v", err)
	}
	if len(ctxs) != 2 {
		t.Fatalf("got %d contexts, want 2", len(ctxs))
	}
}

// TestClientExplicitKubeconfigMissing: a path from config.yaml that does not
// exist is a config error, not a silently unconfigured backend.
func TestClientExplicitKubeconfigMissing(t *testing.T) {
	_, err := NewClient(ClientOptions{Kubeconfig: filepath.Join(t.TempDir(), "nope")})
	if err == nil {
		t.Fatal("a missing explicit kubeconfig must fail")
	}
	if got := errs.KindOf(err); got != errs.Config {
		t.Errorf("kind = %q, want %q", got, errs.Config)
	}
}

// TestBackendHealthUnconfigured: no kubeconfig anywhere is Unconfigured, the
// grey dot, not Down.
func TestBackendHealthUnconfigured(t *testing.T) {
	b := NewWithClient(newTestClient(t, ClientOptions{Home: t.TempDir()}))
	h := b.Health(context.Background())
	if h.State != backend.HealthUnconfigured {
		t.Fatalf("state = %v, want HealthUnconfigured (%s)", h.State, h.Detail)
	}
	if !strings.Contains(h.Detail, ".kube/config") {
		t.Errorf("detail = %q, want the kubeconfig path", h.Detail)
	}
	if h.Checked.IsZero() {
		t.Error("Checked must be set")
	}
}

// TestBackendHealthVersion: GET /version with a 5 s timeout, and OK shows the
// server version (SPEC §8.2).
func TestBackendHealthVersion(t *testing.T) {
	srv := versionServer(t, "v1.34.1")
	path := writeKubeconfig(t, "alpha", ctxSpec{name: "alpha", server: srv.URL, namespace: "prod"})
	b := NewWithClient(newTestClient(t, ClientOptions{Kubeconfig: path}))

	h := b.Health(context.Background())
	if h.State != backend.HealthOK {
		t.Fatalf("state = %v, want HealthOK (%s)", h.State, h.Detail)
	}
	if !strings.Contains(h.Detail, "v1.34.1") {
		t.Errorf("detail = %q, want the server version", h.Detail)
	}
	if !strings.Contains(h.Detail, "alpha") {
		t.Errorf("detail = %q, want the context name", h.Detail)
	}
}

// TestBackendHealthAuth: 401 is Down with kind auth.
func TestBackendHealthAuth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"kind":"Status","apiVersion":"v1","status":"Failure",`+
			`"message":"Unauthorized","reason":"Unauthorized","code":401}`)
	}))
	t.Cleanup(srv.Close)
	path := writeKubeconfig(t, "alpha", ctxSpec{name: "alpha", server: srv.URL})
	b := NewWithClient(newTestClient(t, ClientOptions{Kubeconfig: path}))

	h := b.Health(context.Background())
	if h.State != backend.HealthDown {
		t.Fatalf("state = %v, want HealthDown (%s)", h.State, h.Detail)
	}
	if !strings.HasPrefix(h.Detail, string(errs.Auth)+": ") {
		t.Errorf("detail = %q, want it to start with %q", h.Detail, errs.Auth+": ")
	}
}

// TestBackendHealthUnreachable: a server that is not there is Down, and the
// reason is not auth.
func TestBackendHealthUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()
	path := writeKubeconfig(t, "alpha", ctxSpec{name: "alpha", server: url})
	b := NewWithClient(newTestClient(t, ClientOptions{Kubeconfig: path}))

	h := b.Health(context.Background())
	if h.State != backend.HealthDown {
		t.Fatalf("state = %v, want HealthDown (%s)", h.State, h.Detail)
	}
	if strings.HasPrefix(h.Detail, string(errs.Auth)) {
		t.Errorf("detail = %q, must not be auth", h.Detail)
	}
}

// TestBackendHealthTimeout: the probe gives up after 5 s even when the server
// never answers.
func TestBackendHealthTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	path := writeKubeconfig(t, "alpha", ctxSpec{name: "alpha", server: srv.URL})
	b := NewWithClient(newTestClient(t, ClientOptions{Kubeconfig: path}))

	start := time.Now()
	h := b.Health(context.Background())
	if h.State != backend.HealthDown {
		t.Fatalf("state = %v, want HealthDown (%s)", h.State, h.Detail)
	}
	if !strings.HasPrefix(h.Detail, string(errs.Timeout)) {
		t.Errorf("detail = %q, want it to start with %q", h.Detail, errs.Timeout)
	}
	if elapsed := time.Since(start); elapsed > healthTimeout+2*time.Second {
		t.Errorf("probe took %s, want about %s", elapsed, healthTimeout)
	}
}

// TestNamespace covers `:ns`: the context's namespace by default, a name, and
// "all" for every namespace.
func TestNamespace(t *testing.T) {
	srv := versionServer(t, "v1.34.1")
	path := writeKubeconfig(t, "alpha", ctxSpec{name: "alpha", server: srv.URL, namespace: "prod"})
	c := newTestClient(t, ClientOptions{Kubeconfig: path})

	if got := c.Namespace(); got != "" {
		// Before the first use the namespace is unknown; once the client is
		// built it comes from the context.
		t.Logf("namespace before the first request = %q", got)
	}
	if _, err := c.ServerVersion(context.Background()); err != nil {
		t.Fatalf("ServerVersion: %v", err)
	}
	if got, want := c.Namespace(), "prod"; got != want {
		t.Errorf("namespace = %q, want %q (the context's)", got, want)
	}
	if err := c.SetNamespace("staging"); err != nil {
		t.Fatalf("SetNamespace: %v", err)
	}
	if got, want := c.Namespace(), "staging"; got != want {
		t.Errorf("namespace = %q, want %q", got, want)
	}
	if err := c.SetNamespace("all"); err != nil {
		t.Fatalf("SetNamespace(all): %v", err)
	}
	if got := c.Namespace(); got != "" {
		t.Errorf("namespace after `all` = %q, want empty (every namespace)", got)
	}
	err := c.SetNamespace("Bad_Namespace")
	if err == nil {
		t.Fatal("an invalid namespace must fail")
	}
	if got := errs.KindOf(err); got != errs.Config {
		t.Errorf("kind = %q, want %q", got, errs.Config)
	}
}

// TestNamespaceDefaultAfterSwitch: without an explicit `:ns`, switching
// contexts adopts the new context's namespace.
func TestNamespaceDefaultAfterSwitch(t *testing.T) {
	srv := versionServer(t, "v1.34.1")
	path := writeKubeconfig(t, "alpha",
		ctxSpec{name: "alpha", server: srv.URL, namespace: "alpha-ns"},
		ctxSpec{name: "beta", server: srv.URL, namespace: "beta-ns"},
	)
	c := newTestClient(t, ClientOptions{Kubeconfig: path})
	if _, err := c.ServerVersion(context.Background()); err != nil {
		t.Fatalf("ServerVersion: %v", err)
	}
	if got, want := c.Namespace(), "alpha-ns"; got != want {
		t.Fatalf("namespace = %q, want %q", got, want)
	}
	if _, err := c.SwitchContext(context.Background(), "beta"); err != nil {
		t.Fatalf("SwitchContext: %v", err)
	}
	if got, want := c.Namespace(), "beta-ns"; got != want {
		t.Errorf("namespace = %q, want %q", got, want)
	}
}

// TestBackendListHasNoKinds: until task 106 adds them, listing says so instead
// of pretending the cluster is empty.
func TestBackendListHasNoKinds(t *testing.T) {
	b := NewWithClient(newTestClient(t, ClientOptions{Kubeconfig: testKubeconfig(t)}))
	if kinds := b.Kinds(); len(kinds) != 0 {
		t.Fatalf("Kinds = %v, want none until task 106", kinds)
	}
	if _, err := b.List(context.Background(), resource.Query{Kind: "pod"}); err == nil {
		t.Fatal("List must fail while no kind is served")
	} else if got := errs.KindOf(err); got != errs.Config {
		t.Errorf("kind = %q, want %q", got, errs.Config)
	}
	if _, err := b.Get(context.Background(), resource.Ref{Kind: "pod", Name: "web-0"}); err == nil {
		t.Fatal("Get must fail while no kind is served")
	}
	if got := b.Links(resource.Resource{}); got != nil {
		t.Errorf("Links = %v, want nil", got)
	}
	if got, want := b.Name(), "k8s"; got != want {
		t.Errorf("Name = %q, want %q", got, want)
	}
}

// TestClassify maps client-go failures onto error kinds.
func TestClassify(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want errs.Kind
	}{
		{"nil", nil, ""},
		{"unauthorized", fmt.Errorf("the server has asked for the client to provide credentials"), errs.Auth},
		{"exec plugin", fmt.Errorf("getting credentials: exec: executable gcloud not found"), errs.Auth},
		{"refused", fmt.Errorf("Get \"https://x/version\": dial tcp 1.2.3.4:6443: connect: connection refused"), errs.Unreachable},
		{"deadline", context.DeadlineExceeded, errs.Timeout},
		{"remote", fmt.Errorf("the server could not find the requested resource"), errs.Remote},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Classify(tc.err); got != tc.want {
				t.Errorf("Classify(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

// newTestClient builds a client that logs nowhere.
func newTestClient(t *testing.T, o ClientOptions) *Client {
	t.Helper()
	c, err := NewClient(o)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}
