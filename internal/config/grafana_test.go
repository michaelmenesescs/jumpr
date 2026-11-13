package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// TestLoadGrafanaSection checks the `grafana:` section is decoded (SPEC §8.4),
// including the connection profile name.
func TestLoadGrafanaSection(t *testing.T) {
	body := "grafana:\n" +
		"  - name: eu\n" +
		"    url: https://grafana.example.internal\n" +
		"  - name: us\n" +
		"    url: https://grafana-us.example.internal\n" +
		"    connection: us-sso\n"
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := Load(path, t.TempDir())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []GrafanaConfig{
		{Name: "eu", URL: "https://grafana.example.internal"},
		{Name: "us", URL: "https://grafana-us.example.internal", Connection: "us-sso"},
	}
	if !reflect.DeepEqual(cfg.Grafana, want) {
		t.Errorf("grafana = %+v, want %+v", cfg.Grafana, want)
	}
	if got := Default().Grafana; len(got) != 0 {
		t.Errorf("Default().Grafana = %v, want empty", got)
	}
}

// TestValidateGrafanaSection checks every rule Validate applies to a Grafana
// entry.
func TestValidateGrafanaSection(t *testing.T) {
	base := func() Config {
		c := Default()
		return c
	}
	cases := []struct {
		name string
		set  func(*Config)
		want string // substring of the error; empty means valid
	}{
		{"one server", func(c *Config) {
			c.Grafana = []GrafanaConfig{{Name: "eu", URL: "https://grafana.example.internal"}}
		}, ""},
		{"without a name", func(c *Config) {
			c.Grafana = []GrafanaConfig{{URL: "https://grafana.example.internal"}}
		}, "grafana[0].name"},
		{"configured twice", func(c *Config) {
			c.Grafana = []GrafanaConfig{
				{Name: "eu", URL: "https://grafana.example.internal"},
				{Name: "eu", URL: "https://grafana.example.internal"},
			}
		}, "grafana[1].name"},
		{"without a scheme", func(c *Config) {
			c.Grafana = []GrafanaConfig{{Name: "eu", URL: "grafana.example.internal"}}
		}, "grafana[0].url"},
		{"not a URL", func(c *Config) {
			c.Grafana = []GrafanaConfig{{Name: "eu", URL: "://nope"}}
		}, "grafana[0].url"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base()
			tc.set(&cfg)
			err := cfg.Validate()
			if tc.want == "" {
				if err != nil {
					t.Fatalf("Validate: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("Validate accepted an invalid grafana section")
			}
			if !contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

// contains is a local substring helper so the test does not import strings for
// one call.
func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
