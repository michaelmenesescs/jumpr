package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"jumpr/internal/errs"
)

// TestLoadKubernetesSection covers the `kubernetes:` keys of BUILD-PLAN §11:
// an explicit kubeconfig path and the contexts that make a session read-only.
func TestLoadKubernetesSection(t *testing.T) {
	body := "kubernetes:\n" +
		"  kubeconfig: ~/.kube/work\n" +
		"  contexts_protected: [prod, staging]\n"
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := Load(path, testHome)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := KubernetesConfig{
		// The loader keeps `~`; the k8s client expands it with the home
		// directory internal/app passes in.
		Kubeconfig:        "~/.kube/work",
		ContextsProtected: []string{"prod", "staging"},
	}
	if !reflect.DeepEqual(cfg.Kubernetes, want) {
		t.Errorf("kubernetes = %+v, want %+v", cfg.Kubernetes, want)
	}
}

// TestKubernetesDefaults: the section is optional and defaults to the standard
// kubeconfig rules with no protected context.
func TestKubernetesDefaults(t *testing.T) {
	cfg := Default()
	if cfg.Kubernetes.Kubeconfig != "" {
		t.Errorf("kubernetes.kubeconfig = %q, want empty", cfg.Kubernetes.Kubeconfig)
	}
	if len(cfg.Kubernetes.ContextsProtected) != 0 {
		t.Errorf("kubernetes.contexts_protected = %v, want empty", cfg.Kubernetes.ContextsProtected)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("defaults must validate: %v", err)
	}
}

// TestValidateKubernetesProtected: an empty or repeated context name is a
// config error naming the entry.
func TestValidateKubernetesProtected(t *testing.T) {
	for _, tc := range []struct {
		name      string
		protected []string
		wantErr   bool
	}{
		{"none", nil, false},
		{"two", []string{"prod", "staging"}, false},
		{"blank", []string{" "}, true},
		{"repeated", []string{"prod", "prod"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			cfg.Kubernetes.ContextsProtected = tc.protected
			err := cfg.Validate()
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Validate: want an error for %v", tc.protected)
				}
				if got := errs.KindOf(err); got != errs.Config {
					t.Errorf("kind = %q, want %q", got, errs.Config)
				}
				return
			}
			if err != nil {
				t.Fatalf("Validate: %v", err)
			}
		})
	}
}
