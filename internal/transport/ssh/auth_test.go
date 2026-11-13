package ssh

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"

	"jumper/internal/errs"
	"jumper/internal/interact"
)

// newEd25519 returns a fresh key pair. Test keys are generated at run time and
// never committed.
func newEd25519(t *testing.T) (ed25519.PrivateKey, ssh.PublicKey) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return priv, signer.PublicKey()
}

// writePrivateKey writes one OpenSSH private key file and returns its path.
func writePrivateKey(t *testing.T, priv ed25519.PrivateKey, passphrase string) string {
	t.Helper()
	var (
		block *pem.Block
		err   error
	)
	if passphrase == "" {
		block, err = ssh.MarshalPrivateKey(priv, "")
	} else {
		block, err = ssh.MarshalPrivateKeyWithPassphrase(priv, "", []byte(passphrase))
	}
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func writePublicKey(t *testing.T, pub ssh.PublicKey) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "id_ed25519.pub")
	if err := os.WriteFile(path, ssh.MarshalAuthorizedKey(pub), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// hostSigner is the server key for the in-memory handshake.
func hostSigner(t *testing.T) ssh.Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// countingPrompter records every prompt and delegates the answers.
type countingPrompter struct {
	mu    sync.Mutex
	asks  []interact.Prompt
	inner interact.Prompter
}

func newCountingPrompter(answers ...interact.Answer) *countingPrompter {
	return &countingPrompter{inner: interact.Static(answers...)}
}

func (c *countingPrompter) Ask(ctx context.Context, p interact.Prompt) (interact.Answer, error) {
	c.mu.Lock()
	c.asks = append(c.asks, p)
	c.mu.Unlock()
	if c.inner == nil {
		return interact.Answer{}, errs.New(errs.Interaction, "test", "nobody to ask")
	}
	return c.inner.Ask(ctx, p)
}

func (c *countingPrompter) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.asks)
}

func (c *countingPrompter) last() interact.Prompt {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.asks) == 0 {
		return interact.Prompt{}
	}
	return c.asks[len(c.asks)-1]
}

// dialAuth runs one full SSH handshake over an in-memory pipe. It returns the
// public key the client authenticated with.
func dialAuth(t *testing.T, methods []ssh.AuthMethod, accept func(ssh.PublicKey) bool) (ssh.PublicKey, error) {
	t.Helper()
	type result struct {
		key ssh.PublicKey
		err error
	}
	done := make(chan result, 1)
	serverKey := hostSigner(t)

	go func() {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			done <- result{err: err}
			return
		}
		defer ln.Close()

		var (
			once sync.Once
			mu   sync.Mutex
			used ssh.PublicKey
		)
		cfg := &ssh.ServerConfig{
			PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
				if accept != nil && !accept(key) {
					return nil, errors.New("key not accepted")
				}
				mu.Lock()
				once.Do(func() { used = key })
				mu.Unlock()
				return nil, nil
			},
		}
		cfg.AddHostKey(serverKey)

		serverDone := make(chan struct{})
		go func() {
			defer close(serverDone)
			serverConn, err := ln.Accept()
			if err != nil {
				return
			}
			defer serverConn.Close()
			conn, chans, reqs, err := ssh.NewServerConn(serverConn, cfg)
			if err != nil {
				return
			}
			go ssh.DiscardRequests(reqs)
			for ch := range chans {
				ch.Reject(ssh.Prohibited, "no channels")
			}
			conn.Close()
		}()

		clientConn, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			<-serverDone
			done <- result{err: err}
			return
		}
		cc, chans, reqs, err := ssh.NewClientConn(clientConn, ln.Addr().String(), &ssh.ClientConfig{
			User:            "tester",
			Auth:            methods,
			HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		})
		if err != nil {
			clientConn.Close()
			<-serverDone
			done <- result{err: err}
			return
		}
		go ssh.DiscardRequests(reqs)
		go func() {
			for ch := range chans {
				ch.Reject(ssh.Prohibited, "no channels")
			}
		}()
		cc.Close()
		<-serverDone
		mu.Lock()
		k := used
		mu.Unlock()
		done <- result{key: k}
	}()

	select {
	case r := <-done:
		return r.key, r.err
	case <-time.After(20 * time.Second):
		t.Fatal("ssh handshake timed out")
		return nil, nil
	}
}

func keyEqual(a, b ssh.PublicKey) bool {
	if a == nil || b == nil {
		return false
	}
	return string(a.Marshal()) == string(b.Marshal())
}

func TestAuthKeyFile(t *testing.T) {
	priv, pub := newEd25519(t)
	keyFile := writePrivateKey(t, priv, "")
	missing := filepath.Join(t.TempDir(), "nope")

	spec := HostSpec{
		Alias:         "example",
		HostName:      "example.internal",
		User:          "tester",
		Port:          22,
		IdentityFiles: []string{missing, keyFile},
	}
	methods, cleanup := AuthMethods(spec, AuthOptions{})
	defer cleanup()

	used, err := dialAuth(t, methods, func(k ssh.PublicKey) bool { return keyEqual(k, pub) })
	if err != nil {
		t.Fatalf("handshake failed: %v", err)
	}
	if !keyEqual(used, pub) {
		t.Fatalf("server saw key %v, want the identity file key", used)
	}
}

func TestAuthPassphrasePromptedAndCached(t *testing.T) {
	priv, pub := newEd25519(t)
	keyFile := writePrivateKey(t, priv, "s3cret")
	pr := newCountingPrompter(interact.Answer{Text: "s3cret"}, interact.Answer{Text: "s3cret"})

	spec := HostSpec{HostName: "example.internal", User: "tester", Port: 22, IdentityFiles: []string{keyFile}}
	methods, cleanup := AuthMethods(spec, AuthOptions{Prompter: pr})
	defer cleanup()

	accept := func(k ssh.PublicKey) bool { return keyEqual(k, pub) }
	if _, err := dialAuth(t, methods, accept); err != nil {
		t.Fatalf("first handshake failed: %v", err)
	}
	if _, err := dialAuth(t, methods, accept); err != nil {
		t.Fatalf("second handshake failed: %v", err)
	}
	if got := pr.count(); got != 1 {
		t.Fatalf("passphrase prompts = %d, want 1 (signer must be cached)", got)
	}
	p := pr.last()
	if p.Kind != interact.Passphrase || !p.Secret {
		t.Fatalf("prompt = %+v, want a secret Passphrase prompt", p)
	}
	if want := "Passphrase for " + keyFile; p.Title != want {
		t.Fatalf("prompt title = %q, want %q", p.Title, want)
	}
}

func TestAuthWrongPassphraseThreeTimes(t *testing.T) {
	priv, pub := newEd25519(t)
	keyFile := writePrivateKey(t, priv, "right")
	pr := newCountingPrompter(
		interact.Answer{Text: "wrong1"},
		interact.Answer{Text: "wrong2"},
		interact.Answer{Text: "wrong3"},
	)

	spec := HostSpec{HostName: "example.internal", User: "tester", Port: 22, IdentityFiles: []string{keyFile}}
	methods, cleanup := AuthMethods(spec, AuthOptions{Prompter: pr})
	defer cleanup()

	if _, err := dialAuth(t, methods, func(k ssh.PublicKey) bool { return keyEqual(k, pub) }); err == nil {
		t.Fatal("handshake succeeded with a wrong passphrase")
	}
	if got := pr.count(); got != 3 {
		t.Fatalf("passphrase prompts = %d, want 3", got)
	}
}

// startAgent serves an in-process keyring on a unix socket. The socket lives in
// /tmp because t.TempDir() paths can pass the 103-byte socket limit on macOS.
func startAgent(t *testing.T, keys ...ed25519.PrivateKey) string {
	t.Helper()
	keyring := agent.NewKeyring()
	for _, k := range keys {
		if err := keyring.Add(agent.AddedKey{PrivateKey: k}); err != nil {
			t.Fatal(err)
		}
	}
	dir, err := os.MkdirTemp("/tmp", "jumper-agent-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "agent.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				agent.ServeAgent(keyring, conn)
			}()
		}
	}()
	return sock
}

func TestAuthAgentFirst(t *testing.T) {
	filePriv, filePub := newEd25519(t)
	agentPriv, agentPub := newEd25519(t)
	keyFile := writePrivateKey(t, filePriv, "")
	sock := startAgent(t, agentPriv)

	spec := HostSpec{HostName: "example.internal", User: "tester", Port: 22, IdentityFiles: []string{keyFile}}
	methods, cleanup := AuthMethods(spec, AuthOptions{AgentSocket: func() string { return sock }})
	defer cleanup()

	used, err := dialAuth(t, methods, func(k ssh.PublicKey) bool {
		return keyEqual(k, filePub) || keyEqual(k, agentPub)
	})
	if err != nil {
		t.Fatalf("handshake failed: %v", err)
	}
	if !keyEqual(used, agentPub) {
		t.Fatalf("authenticated with %v, want the agent key %v", used, agentPub)
	}
}

func TestAuthIdentitiesOnlyFiltersAgentKeys(t *testing.T) {
	// The identity file's public key is also in the agent; the other agent key
	// must never be offered.
	filePriv, filePub := newEd25519(t)
	otherPriv, otherPub := newEd25519(t)
	keyFile := writePrivateKey(t, filePriv, "")
	if err := os.WriteFile(keyFile+".pub", ssh.MarshalAuthorizedKey(filePub), 0o600); err != nil {
		t.Fatal(err)
	}
	sock := startAgent(t, otherPriv, filePriv)

	spec := HostSpec{
		HostName:       "example.internal",
		User:           "tester",
		Port:           22,
		IdentityFiles:  []string{keyFile},
		IdentitiesOnly: true,
	}
	methods, cleanup := AuthMethods(spec, AuthOptions{AgentSocket: func() string { return sock }})
	defer cleanup()

	used, err := dialAuth(t, methods, func(k ssh.PublicKey) bool {
		return keyEqual(k, filePub) || keyEqual(k, otherPub)
	})
	if err != nil {
		t.Fatalf("handshake failed: %v", err)
	}
	if !keyEqual(used, filePub) {
		t.Fatalf("authenticated with %v, want only the listed key %v", used, filePub)
	}
}

func TestAuthIdentitiesOnlyWithoutAgentUsesFiles(t *testing.T) {
	priv, pub := newEd25519(t)
	keyFile := writePrivateKey(t, priv, "")

	spec := HostSpec{
		HostName:       "example.internal",
		User:           "tester",
		Port:           22,
		IdentityFiles:  []string{keyFile},
		IdentitiesOnly: true,
	}
	methods, cleanup := AuthMethods(spec, AuthOptions{
		AgentSocket: func() string { return filepath.Join(t.TempDir(), "gone.sock") },
	})
	defer cleanup()

	if _, err := dialAuth(t, methods, func(k ssh.PublicKey) bool { return keyEqual(k, pub) }); err != nil {
		t.Fatalf("handshake failed: %v", err)
	}
}

func TestAuthNoMethods(t *testing.T) {
	methods, cleanup := AuthMethods(HostSpec{HostName: "example.internal", Port: 22}, AuthOptions{})
	defer cleanup()
	if len(methods) != 0 {
		t.Fatalf("methods = %d, want 0 without an agent or identity files", len(methods))
	}
}
