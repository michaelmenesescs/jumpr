package devtools

import (
	"fmt"
	"hash/fnv"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Replacement values the sanitizer writes. They are all in documentation
// ranges, so a captured file can live in the repo without carrying anything
// that identifies a real machine.
const (
	redactedFingerprint = "SHA256:REDACTED"
	macPrefix           = "02:00:00:00:00:"
	ipv4Prefix          = "192.0.2."
	localUserName       = "sre"
)

// Patterns Sanitize rewrites, in order. Each one is deliberately narrow: a
// parser input that happens to contain something that looks like a version
// number must survive.
var (
	// A full SHA-256 fingerprint as `ssh-keygen -l` and ssh print it.
	fingerprintRE = regexp.MustCompile(`SHA256:[A-Za-z0-9+/]+={0,2}`)
	// A UUID or a systemd boot ID.
	uuidRE = regexp.MustCompile(`\b[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}\b`)
	// A 32-hex machine ID.
	machineIDRE = regexp.MustCompile(`\b[0-9a-fA-F]{32}\b`)
	// A colon-separated MAC address.
	macRE = regexp.MustCompile(`\b(?:[0-9a-fA-F]{2}:){5}[0-9a-fA-F]{2}\b`)
	// A dotted quad. Octets are validated in the replacement function, so a
	// "1.2.3.999" stays as it is.
	ipv4RE = regexp.MustCompile(`\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}\b`)
)

// Sanitize replaces the lab's real identities in captured output with stable
// documentation values so the result can be committed.
//
// hostMap maps every real name onto its replacement: the lab machine names
// (`sr-web-01`) and their OrbStack domains map onto the `*.lab.example.internal`
// aliases, and the local user name onto `sre`. Keys are replaced longest first,
// so a fully qualified name wins over its short form. HostMap builds the table
// a capture run uses.
//
// Everything else is derived from the input itself: SSH key fingerprints become
// SHA256:REDACTED, UUIDs, boot IDs and machine IDs become zeros, MAC addresses
// become 02:00:00:00:00:NN and IPv4 addresses outside the documentation ranges
// become 192.0.2.N, where N comes from a hash of the address. The same input
// therefore yields the same output on every call, and running Sanitize twice
// changes nothing.
func Sanitize(b []byte, hostMap map[string]string) []byte {
	s := string(b)
	for _, name := range sortedKeys(hostMap) {
		s = strings.ReplaceAll(s, name, hostMap[name])
	}
	s = fingerprintRE.ReplaceAllString(s, redactedFingerprint)
	s = uuidRE.ReplaceAllStringFunc(s, zeroUUID)
	s = machineIDRE.ReplaceAllString(s, strings.Repeat("0", 32))
	s = macRE.ReplaceAllStringFunc(s, mapMAC)
	s = ipv4RE.ReplaceAllStringFunc(s, mapIPv4)
	return []byte(s)
}

// sortedKeys returns the map's keys longest first, then alphabetically, so
// replacement is deterministic.
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if len(keys[i]) != len(keys[j]) {
			return len(keys[i]) > len(keys[j])
		}
		return keys[i] < keys[j]
	})
	return keys
}

// zeroUUID keeps the UUID's shape and zeroes every hex digit.
func zeroUUID(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r == '-' {
			b.WriteRune('-')
			continue
		}
		b.WriteByte('0')
	}
	return b.String()
}

// mapMAC turns a MAC address into 02:00:00:00:00:NN. An address that is
// already in the sanitized form is left alone, which keeps Sanitize idempotent.
func mapMAC(s string) string {
	lower := strings.ToLower(s)
	if strings.HasPrefix(lower, macPrefix) {
		return s
	}
	return macPrefix + fmt.Sprintf("%02x", 1+hashString(lower)%254)
}

// mapIPv4 turns an address outside the documentation ranges into 192.0.2.N and
// leaves everything else alone: documentation addresses are already safe,
// loopback and the wildcard/broadcast addresses are meaningful to the parsers.
func mapIPv4(s string) string {
	parts := strings.Split(s, ".")
	if len(parts) != 4 {
		return s
	}
	octets := make([]int, 4)
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || n > 255 || (len(p) > 1 && p[0] == '0') {
			return s
		}
		octets[i] = n
	}
	if documentationIP(octets) || loopbackIP(octets) || wildcardIP(octets) {
		return s
	}
	return ipv4Prefix + strconv.Itoa(1+int(hashString(s)%254))
}

// documentationIP reports whether the address is in one of the three IPv4
// documentation ranges (RFC 5737).
func documentationIP(o []int) bool {
	switch {
	case o[0] == 192 && o[1] == 0 && o[2] == 2:
		return true
	case o[0] == 198 && o[1] == 51 && o[2] == 100:
		return true
	case o[0] == 203 && o[1] == 0 && o[2] == 113:
		return true
	}
	return false
}

// loopbackIP reports whether the address is in 127.0.0.0/8.
func loopbackIP(o []int) bool { return o[0] == 127 }

// wildcardIP reports whether the address is 0.0.0.0 or 255.255.255.255.
func wildcardIP(o []int) bool {
	all := func(v int) bool { return o[0] == v && o[1] == v && o[2] == v && o[3] == v }
	return all(0) || all(255)
}

// hashString is the stable hash every derived replacement uses. It is fnv-1a,
// so it never changes between runs or Go releases.
func hashString(s string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(s))
	return h.Sum32()
}
