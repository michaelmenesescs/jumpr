// Command auth-helper-cookie is an example jumper auth helper for an SSO proxy
// that issues a cookie: it prints the cookie as helper JSON on stdout.
// The value comes from JUMPER_AUTH_COOKIE, or from the macOS Keychain.
// jumper runs this directly, without a shell; diagnostics go to stderr.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// securityBin is the macOS Keychain CLI.
const securityBin = "/usr/bin/security"

// document is the helper protocol of SPEC §7.2: headers, cookies and an
// optional expiry. jumper keeps these values in memory only.
type document struct {
	Headers   map[string]string `json:"headers,omitempty"`
	Cookies   []cookie          `json:"cookies"`
	ExpiresAt string            `json:"expires_at,omitempty"`
}

type cookie struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Domain string `json:"domain,omitempty"`
}

func main() {
	name := flag.String("name", "_oauth2_proxy", "cookie name the proxy expects")
	domain := flag.String("domain", "", "cookie domain, e.g. .example.internal (empty sends it to every host)")
	service := flag.String("service", "jumper-auth", "Keychain service name")
	account := flag.String("account", "", "Keychain account name; empty uses the cookie name")
	valid := flag.Duration("expires", 50*time.Minute, "how long the cookie stays valid")
	envVar := flag.String("env", "JUMPER_AUTH_COOKIE", "environment variable holding the cookie value")
	flag.Parse()

	accountName := *account
	if accountName == "" {
		accountName = *name
	}

	value := strings.TrimSpace(os.Getenv(*envVar))
	source := "the " + *envVar + " environment variable"
	if value == "" {
		v, err := keychain(*service, accountName)
		if err != nil {
			fmt.Fprintf(os.Stderr, "auth-helper-cookie: no cookie in %s and the Keychain lookup failed: %v\n", *envVar, err)
			os.Exit(1)
		}
		value = v
		source = "the Keychain (" + *service + "/" + accountName + ")"
	}
	if value == "" {
		fmt.Fprintf(os.Stderr, "auth-helper-cookie: the cookie found in %s is empty\n", source)
		os.Exit(1)
	}

	doc := document{
		Cookies:   []cookie{{Name: *name, Value: value, Domain: *domain}},
		ExpiresAt: time.Now().Add(*valid).UTC().Format(time.RFC3339),
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(doc); err != nil {
		fmt.Fprintf(os.Stderr, "auth-helper-cookie: cannot write the helper JSON: %v\n", err)
		os.Exit(1)
	}
}

// keychain reads a generic password from the login Keychain. The value is
// returned to stdout only, never logged.
func keychain(service, account string) (string, error) {
	out, err := exec.Command(securityBin, "find-generic-password", "-s", service, "-a", account, "-w").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
