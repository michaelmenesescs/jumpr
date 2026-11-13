package config

import (
	"os"
	"path/filepath"
	"testing"

	"jumper/internal/errs"
)

func TestConnectionsPath(t *testing.T) {
	p := ResolvePaths("/home/tester", "")
	if got, want := p.ConnectionsPath(), filepath.Join("/home/tester/.config/jumper", ConnectionsFile); got != want {
		t.Fatalf("ConnectionsPath() = %q, want %q", got, want)
	}
}

func TestLoadConnections(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "connections.yaml")
	content := "connections:\n  lab:\n    proxy:\n      type: ssh-socks\n      via: bastion.example.internal\n    timeout: 5s\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	profiles, err := LoadConnections(path, dir)
	if err != nil {
		t.Fatalf("LoadConnections: %v", err)
	}
	p := profiles["lab"]
	if p == nil {
		t.Fatal("the lab profile is missing")
	}
	if got := p.Timeout.Duration().String(); got != "5s" {
		t.Fatalf("timeout = %s, want 5s", got)
	}

	if _, err := LoadConnections(filepath.Join(dir, "absent.yaml"), dir); err != nil {
		t.Fatalf("a missing connections.yaml must not be an error: %v", err)
	}

	bad := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(bad, []byte("connections:\n  lab:\n    nope: true\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := LoadConnections(bad, dir); err == nil {
		t.Fatal("an unknown key was accepted")
	} else if kind := errs.KindOf(err); kind != errs.Config {
		t.Fatalf("kind = %q, want config", kind)
	}
}
