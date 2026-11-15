package app

import (
	"testing"

	"jumpr/internal/config"
	"jumpr/internal/transport"
	"jumpr/internal/transport/ssh"
)

// TestBuildExecSetWithoutRules checks the default composition: no ssh.transports
// means the native transport for every host, and the pool stays visible to
// `:health`.
func TestBuildExecSetWithoutRules(t *testing.T) {
	cfg := config.Default()
	stack := &sshStack{Transport: ssh.New(nil, ssh.Options{})}
	set, err := buildExecSet(cfg, Options{Home: "/home/tester"}, stack, config.Paths{}, nil)
	if err != nil {
		t.Fatalf("buildExecSet: %v", err)
	}
	if _, ok := set.Executor.(*transport.Router); ok {
		t.Fatal("a router was built without ssh.transports")
	}
	if _, ok := set.Executor.(*runtimeTransport); !ok {
		t.Fatalf("executor = %T, want *runtimeTransport", set.Executor)
	}
	if set.PoolStats().Bastions != 0 {
		t.Fatalf("pool stats = %+v", set.PoolStats())
	}
}

// TestBuildExecSetWithRules checks that ssh.transports puts the router in
// front, and that the rules reach it.
func TestBuildExecSetWithRules(t *testing.T) {
	cfg := config.Default()
	cfg.SSH.Transports = []config.SSHTransportRule{
		{Match: "*.legacy.example.internal", Transport: "system-ssh"},
	}
	stack := &sshStack{Transport: ssh.New(nil, ssh.Options{})}
	set, err := buildExecSet(cfg, Options{Home: "/home/tester"}, stack, config.Paths{CacheDir: t.TempDir()}, nil)
	if err != nil {
		t.Fatalf("buildExecSet: %v", err)
	}
	router, ok := set.Executor.(*transport.Router)
	if !ok {
		t.Fatalf("executor = %T, want *transport.Router", set.Executor)
	}
	if got := router.Via("db.legacy.example.internal"); got != transport.SystemSSH {
		t.Fatalf("legacy host goes via %q, want system-ssh", got)
	}
	if got := router.Via("web.example.internal"); got != transport.Native {
		t.Fatalf("other host goes via %q, want native", got)
	}
}

// TestBuildExecSetRejectsBadRule checks the config error path: Validate
// catches these, and the builder does too.
func TestBuildExecSetRejectsBadRule(t *testing.T) {
	cfg := config.Default()
	cfg.SSH.Transports = []config.SSHTransportRule{{Match: "h", Transport: "telnet"}}
	stack := &sshStack{Transport: ssh.New(nil, ssh.Options{})}
	if _, err := buildExecSet(cfg, Options{Home: "/home/tester"}, stack, config.Paths{}, nil); err == nil {
		t.Fatal("a bad transport rule was accepted")
	}
}
