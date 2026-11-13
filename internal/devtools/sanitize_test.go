package devtools

import (
	"regexp"
	"strings"
	"testing"
)

// testHostMap is the table a capture run builds, with a made-up local user.
func testHostMap() map[string]string {
	hosts := []CaptureHost{
		{Host: "web-01.lab.example.internal", Short: "web-01", Distro: "rocky9"},
	}
	return HostMap(hosts, "developer")
}

// TestSanitize covers the replacement table: lab names and their OrbStack
// domains, the local user, MAC addresses, UUIDs, machine IDs, SSH fingerprints
// and IPv4 addresses.
func TestSanitize(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    []string // substrings the output must contain
		notWant []string // substrings the output must not contain
	}{
		{
			name:    "machine name",
			in:      "hostname: sr-web-01\n",
			want:    []string{"hostname: web-01.lab.example.internal\n"},
			notWant: []string{"sr-web-01"},
		},
		{
			name:    "orbstack domain",
			in:      "sr-web-01.orb.local sr-web-01.orbstack.dev sr-web-01.local\n",
			want:    []string{"web-01.lab.example.internal web-01.lab.example.internal web-01.lab.example.internal"},
			notWant: []string{"orb.local", "orbstack.dev", "sr-web-01"},
		},
		{
			name: "fqdn alias is unchanged",
			in:   "Connected to web-01.lab.example.internal\n",
			want: []string{"web-01.lab.example.internal"},
		},
		{
			name:    "local user",
			in:      "drwxr-xr-x developer staff /Users/developer/.ssh\n",
			want:    []string{"sre", "/Users/sre/.ssh"},
			notWant: []string{"developer"},
		},
		{
			name:    "mac address",
			in:      "link/ether 52:54:00:12:34:56 brd ff:ff:ff:ff:ff:ff\n",
			want:    []string{"02:00:00:00:00:"},
			notWant: []string{"52:54:00:12:34:56"},
		},
		{
			name:    "uuid and boot id",
			in:      "BOOT_ID=3f2a1b4c-5d6e-7f80-91a2-b3c4d5e6f708\n",
			want:    []string{"BOOT_ID=00000000-0000-0000-0000-000000000000\n"},
			notWant: []string{"3f2a1b4c"},
		},
		{
			name:    "machine id",
			in:      "machine-id: 0123456789abcdef0123456789abcdef\n",
			want:    []string{"machine-id: 00000000000000000000000000000000"},
			notWant: []string{"0123456789abcdef"},
		},
		{
			name:    "ssh fingerprint",
			in:      "ED25519 key fingerprint is SHA256:k3Jd0pQ7rXyZabCdEfGhIjKlMnOpQrStUvWxYz0123.\n",
			want:    []string{"fingerprint is SHA256:REDACTED."},
			notWant: []string{"k3Jd0pQ7rXyZabCdEfGhIjKlMnOpQrStUvWxYz0123"},
		},
		{
			name: "private and public ipv4",
			in:   "addr 192.168.139.5 peer 10.20.30.40 gateway 203.0.113.7\n",
			want: []string{"addr 192.0.2.", "peer 192.0.2.", "gateway 203.0.113.7"},
			notWant: []string{
				"192.168.139.5", "10.20.30.40",
			},
		},
		{
			name: "documentation, loopback and wildcard stay",
			in:   "a 192.0.2.9 b 198.51.100.9 c 127.0.0.1 d 0.0.0.0 e 255.255.255.255\n",
			want: []string{"a 192.0.2.9 b 198.51.100.9 c 127.0.0.1 d 0.0.0.0 e 255.255.255.255"},
		},
		{
			name:    "invalid octets untouched",
			in:      "not an address 1.2.3.999 version 5.14.0\n",
			want:    []string{"1.2.3.999", "5.14.0"},
			notWant: []string{"192.0.2."},
		},
		{
			name:    "systemd unit file",
			in:      "FragmentPath=/etc/systemd/system/ssh.service\n",
			want:    []string{"FragmentPath=/etc/systemd/system/ssh.service"},
			notWant: []string{"REDACTED"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := string(Sanitize([]byte(tt.in), testHostMap()))
			for _, want := range tt.want {
				if !strings.Contains(got, want) {
					t.Errorf("Sanitize(%q) = %q; want it to contain %q", tt.in, got, want)
				}
			}
			for _, notWant := range tt.notWant {
				if strings.Contains(got, notWant) {
					t.Errorf("Sanitize(%q) = %q; want it without %q", tt.in, got, notWant)
				}
			}
		})
	}
}

// TestSanitizeStableIPMapping checks the one property a pure function can
// promise: the same address always maps to the same output, within a run and
// across runs, so a capture is reproducible.
func TestSanitizeStableIPMapping(t *testing.T) {
	const in = "192.168.139.5 192.168.139.5\n"
	first := string(Sanitize([]byte(in), testHostMap()))
	second := string(Sanitize([]byte(in), testHostMap()))
	if first != second {
		t.Fatalf("Sanitize is not deterministic: %q then %q", first, second)
	}
	fields := strings.Fields(first)
	if len(fields) != 2 || fields[0] != fields[1] {
		t.Fatalf("same address mapped differently within one run: %q", first)
	}
	if !regexp.MustCompile(`^192\.0\.2\.\d+$`).MatchString(fields[0]) {
		t.Fatalf("mapped address %q is not in 192.0.2.0/24", fields[0])
	}
}

// TestSanitizeIdempotent runs the sanitizer twice over a realistic blob and
// requires the second pass to change nothing, because the command rewrites
// already-captured files in place.
func TestSanitizeIdempotent(t *testing.T) {
	in := strings.Join([]string{
		"host=sr-web-01 fqdn=sr-web-01.orb.local",
		"link/ether 52:54:00:12:34:56",
		"BOOT_ID=3f2a1b4c-5d6e-7f80-91a2-b3c4d5e6f708",
		"machine-id 0123456789abcdef0123456789abcdef",
		"SHA256:k3Jd0pQ7rXyZabCdEfGhIjKlMnOpQrStUvWxYz0123",
		"addr 192.168.139.5 user developer",
		"",
	}, "\n")
	m := testHostMap()
	once := Sanitize([]byte(in), m)
	twice := Sanitize(once, m)
	if string(once) != string(twice) {
		t.Fatalf("Sanitize is not idempotent:\n once: %q\ntwice: %q", once, twice)
	}
}

// TestHostMap checks the table the capture command builds: every lab machine
// name and OrbStack domain points at the alias, and the local user maps to sre.
func TestHostMap(t *testing.T) {
	m := testHostMap()
	for _, name := range []string{"sr-web-01", "sr-web-01.orb.local", "sr-web-01.orbstack.dev", "sr-web-01.local"} {
		if got := m[name]; got != "web-01.lab.example.internal" {
			t.Errorf("HostMap()[%q] = %q; want web-01.lab.example.internal", name, got)
		}
	}
	if got := m["developer"]; got != "sre" {
		t.Errorf("HostMap()[developer] = %q; want sre", got)
	}
	if got := HostMap(nil, ""); len(got) != 0 {
		t.Errorf("HostMap(nil, \"\") = %v; want empty", got)
	}
}
