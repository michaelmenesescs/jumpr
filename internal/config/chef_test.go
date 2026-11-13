package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestLoadChefSection checks the `chef:` section is decoded (SPEC §8.7).
func TestLoadChefSection(t *testing.T) {
	body := "chef:\n" +
		"  workstation: chef.lab.example.internal\n" +
		"  knife: /usr/local/bin/knife\n" +
		"  sudo: true\n" +
		"  timeout: 45s\n"
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := Load(path, t.TempDir())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := ChefConfig{
		Workstation: "chef.lab.example.internal",
		Knife:       "/usr/local/bin/knife",
		Sudo:        true,
		Timeout:     Duration(45 * time.Second),
	}
	if !reflect.DeepEqual(cfg.Chef, want) {
		t.Errorf("chef = %+v, want %+v", cfg.Chef, want)
	}
}

// TestChefDefaults checks the defaults: no workstation (Chef unconfigured), the
// knife command, no sudo, a positive timeout.
func TestChefDefaults(t *testing.T) {
	want := ChefConfig{Knife: "knife", Sudo: false, Timeout: Duration(30 * time.Second)}
	if got := Default().Chef; !reflect.DeepEqual(got, want) {
		t.Errorf("Default().Chef = %+v, want %+v", got, want)
	}
}

// TestValidateChefSection checks every rule Validate applies to the section.
func TestValidateChefSection(t *testing.T) {
	cases := []struct {
		name string
		set  func(*Config)
		want string // substring of the error; empty means valid
	}{
		{"defaults", func(c *Config) {}, ""},
		{"configured", func(c *Config) {
			c.Chef.Workstation = "chef.lab.example.internal"
			c.Chef.Knife = "/opt/chef/bin/knife"
		}, ""},
		{"empty knife", func(c *Config) { c.Chef.Knife = "  " }, "chef.knife"},
		{"knife with arguments", func(c *Config) { c.Chef.Knife = "sudo knife" }, "chef.knife"},
		{"workstation with a space", func(c *Config) { c.Chef.Workstation = "chef lab" }, "chef.workstation"},
		{"zero timeout", func(c *Config) { c.Chef.Timeout = 0 }, "chef.timeout"},
	}
	for _, c := range cases {
		cfg := Default()
		c.set(&cfg)
		err := cfg.Validate()
		if c.want == "" {
			if err != nil {
				t.Errorf("%s: unexpected error: %v", c.name, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("%s: want an error naming %q", c.name, c.want)
			continue
		}
		if got := err.Error(); !strings.Contains(got, c.want) {
			t.Errorf("%s: error = %q, want it to name %q", c.name, got, c.want)
		}
	}
}
