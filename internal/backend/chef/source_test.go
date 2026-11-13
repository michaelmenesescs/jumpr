package chef

import (
	"context"
	"errors"
	"testing"
	"time"

	"jumper/internal/errs"
	"jumper/internal/inventory"
	"jumper/internal/transport/transporttest"
)

// TestInventorySource checks requirement 4: the inventory source lists every
// registered node with the chef.environment, chef.roles and chef.platform
// labels, sorted by host id.
func TestInventorySource(t *testing.T) {
	fake := transporttest.New().OnHost(ws, "knife status", transporttest.Reply{Stdout: string(readFixture(t, "status.json"))})
	src := &Source{Runner: NewRunner(fake, ws, RunnerOptions{}), Now: func() time.Time { return testTime }}
	if src.Name() != "chef" {
		t.Errorf("Name = %q, want chef", src.Name())
	}
	hosts, err := src.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(hosts) != 4 {
		t.Fatalf("hosts = %d, want 4", len(hosts))
	}
	if hosts[0].ID != "db-01" || hosts[3].ID != "web-03" {
		t.Errorf("order = %q … %q, want sorted by id", hosts[0].ID, hosts[3].ID)
	}
	var web inventory.Host
	for _, h := range hosts {
		if h.ID == "web-01" {
			web = h
		}
	}
	if web.Labels["chef.environment"] != "production" {
		t.Errorf("chef.environment = %q", web.Labels["chef.environment"])
	}
	if web.Labels["chef.roles"] != "base,web" {
		t.Errorf("chef.roles = %q, want comma-joined", web.Labels["chef.roles"])
	}
	if web.Labels["chef.platform"] != "ubuntu" {
		t.Errorf("chef.platform = %q", web.Labels["chef.platform"])
	}
	if !web.SeenAt.Equal(testTime) {
		t.Errorf("SeenAt = %s, want %s", web.SeenAt, testTime)
	}
	wantAliases := []string{"10.0.0.11", "web-01.lab.example.internal"}
	if len(web.Aliases) != len(wantAliases) || web.Aliases[0] != wantAliases[0] || web.Aliases[1] != wantAliases[1] {
		t.Errorf("aliases = %v, want %v", web.Aliases, wantAliases)
	}
}

// TestInventorySourceNeedsARunner checks an unwired source fails with Config
// rather than panicking.
func TestInventorySourceNeedsARunner(t *testing.T) {
	if _, err := (&Source{}).Load(context.Background()); errs.KindOf(err) != errs.Config {
		t.Fatalf("kind = %q, want config", errs.KindOf(err))
	}
}

// TestInventorySourceFailure checks a knife failure is returned, not swallowed.
func TestInventorySourceFailure(t *testing.T) {
	fake := transporttest.New().OnHost(ws, "knife status", transporttest.Reply{
		RC: 1, Stderr: "ERROR: Failed to authenticate to https://chef.lab.example.internal/ as sre",
	})
	src := NewSource(NewRunner(fake, ws, RunnerOptions{}))
	_, err := src.Load(context.Background())
	if errs.KindOf(err) != errs.Auth {
		t.Fatalf("kind = %q, want auth (%v)", errs.KindOf(err), err)
	}
	var e *errs.Error
	if !errors.As(err, &e) || e.Host != string(ws) {
		t.Errorf("error does not name the workstation: %v", err)
	}
}
