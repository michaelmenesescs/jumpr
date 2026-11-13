package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestLoadAlertmanagerSection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := "alertmanager:\n" +
		"  - name: eu\n" +
		"    url: https://alertmanager.example.internal\n" +
		"  - name: us\n" +
		"    url: https://alertmanager-us.example.internal\n" +
		"    connection: us-sso\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := Load(path, testHome)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []AlertmanagerConfig{
		{Name: "eu", URL: "https://alertmanager.example.internal"},
		{Name: "us", URL: "https://alertmanager-us.example.internal", Connection: "us-sso"},
	}
	if !reflect.DeepEqual(cfg.Alertmanager, want) {
		t.Errorf("alertmanager = %+v, want %+v", cfg.Alertmanager, want)
	}
	if err := cfg.Validate(); err != nil {
		t.Errorf("Validate: %v", err)
	}
	if got := Default().Alertmanager; len(got) != 0 {
		t.Errorf("Default().Alertmanager = %v, want empty", got)
	}
}

func TestValidateAlertmanagerErrors(t *testing.T) {
	cases := []struct {
		name  string
		cfg   []AlertmanagerConfig
		field string
	}{
		{
			name:  "no name",
			cfg:   []AlertmanagerConfig{{URL: "https://alertmanager.example.internal"}},
			field: "alertmanager[0].name",
		},
		{
			name: "duplicate name",
			cfg: []AlertmanagerConfig{
				{Name: "eu", URL: "https://alertmanager.example.internal"},
				{Name: "eu", URL: "https://alertmanager-2.example.internal"},
			},
			field: "alertmanager[1].name",
		},
		{
			name:  "not a URL",
			cfg:   []AlertmanagerConfig{{Name: "eu", URL: "alertmanager.example.internal"}},
			field: "alertmanager[0].url",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			cfg.Alertmanager = tc.cfg
			err := cfg.Validate()
			if err == nil {
				t.Fatal("Validate returned no error")
			}
			if !strings.Contains(err.Error(), tc.field) {
				t.Errorf("error %q does not name %q", err, tc.field)
			}
		})
	}
}
