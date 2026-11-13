package app

import (
	"context"
	"testing"

	"jumper/internal/config"
	"jumper/internal/redact"
	"jumper/internal/resource"
	"jumper/internal/transport/transporttest"
)

// TestChefBackendUnconfigured checks the builder returns nil when no
// workstation is configured, so callers can skip registration.
func TestChefBackendUnconfigured(t *testing.T) {
	b, err := ChefBackend(config.Default(), nil, nil, nil)
	if err != nil {
		t.Fatalf("ChefBackend: %v", err)
	}
	if b != nil {
		t.Fatalf("backend = %v, want nil", b)
	}
}

// TestChefBackendWiring checks the configured case: the backend runs knife on
// the configured workstation, with the configured command and without sudo by
// default.
func TestChefBackendWiring(t *testing.T) {
	cfg := config.Default()
	cfg.Chef.Workstation = "chef.lab.example.internal"
	cfg.Chef.Knife = "cinc"
	fake := transporttest.New().On("cinc node list", transporttest.Reply{Stdout: `["web-01"]`})
	red, err := redact.New(nil)
	if err != nil {
		t.Fatalf("redactor: %v", err)
	}
	b, err := ChefBackend(cfg, fake, red, nil)
	if err != nil {
		t.Fatalf("ChefBackend: %v", err)
	}
	if b == nil {
		t.Fatal("backend = nil, want a chef backend")
	}
	if b.Name() != "chef" {
		t.Errorf("name = %q, want chef", b.Name())
	}
	ch, err := b.List(context.Background(), resource.Query{Kind: "chef-role"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for range ch {
	}
	calls := fake.Calls()
	if len(calls) == 0 {
		t.Fatal("no knife call reached the transport")
	}
	if calls[0].Host != "chef.lab.example.internal" {
		t.Errorf("host = %q, want the configured workstation", calls[0].Host)
	}
	if calls[0].Command.Sudo {
		t.Error("sudo = true, want false by default")
	}
}

// TestChefBackendNeedsATransport checks a configured workstation without an ssh
// transport is a config error, not a nil dereference.
func TestChefBackendNeedsATransport(t *testing.T) {
	cfg := config.Default()
	cfg.Chef.Workstation = "chef.lab.example.internal"
	b, err := ChefBackend(cfg, nil, nil, nil)
	if err == nil {
		t.Fatalf("backend = %v, want a config error", b)
	}
}
