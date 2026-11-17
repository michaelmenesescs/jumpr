package tui

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"jumpr/internal/resource"
	"jumpr/internal/shq"
	"jumpr/internal/transport"
)

// The network tool's two active checks. Both are read-only: they resolve a
// name, ask the kernel for a route, open one TCP connection and close it.
//
//	:reach <target>:<port> [--tls] [--http]   from every target host
//	:trace <from-host> <to-host> <port>       one path, both ends, a verdict
const (
	reachUsage = "usage: :reach <name-or-ip>:<port> [--tls] [--http]"
	traceUsage = "usage: :trace <from-host> <to-host> <port>"
	// traceTimeout bounds the whole trace: two remote commands.
	traceTimeout = 25 * time.Second
)

// netNameRe is what a DNS name or an IP literal may contain. Anything else is
// refused before it gets near a shell line; the value is quoted as well.
var netNameRe = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9._:-]{0,252})$`)

// RegisterNetCommands registers `:reach` and `:trace` on c.
func RegisterNetCommands(c *Commands, open FanoutOpener) error {
	if c == nil {
		return nil
	}
	if err := c.Register(Command{
		Name:  "reach",
		Usage: ":reach <name-or-ip>:<port> [--tls] [--http]",
		Help:  "from the targets: resolve, route, TCP connect, optional TLS dates and HTTP status",
		Run:   func(env *Env, args string) tea.Cmd { return runReach(env, args, open) },
	}); err != nil {
		return err
	}
	return c.Register(Command{
		Name:  "trace",
		Usage: ":trace <from-host> <to-host> <port>",
		Help:  "one path hop by hop: name, route, connect, far-end listener and firewall",
		Run:   runTrace,
	})
}

// parseEndpoint splits "name:port" and validates both halves.
func parseEndpoint(s string) (name string, port int, err error) {
	i := strings.LastIndex(s, ":")
	if i <= 0 || i == len(s)-1 {
		return "", 0, fmt.Errorf("need <name-or-ip>:<port>")
	}
	name = strings.Trim(s[:i], "[]")
	port, err = parsePort(s[i+1:])
	if err != nil {
		return "", 0, err
	}
	if !netNameRe.MatchString(name) {
		return "", 0, fmt.Errorf("%q is not a host name or address", name)
	}
	return name, port, nil
}

func parsePort(s string) (int, error) {
	p, err := strconv.Atoi(s)
	if err != nil || p < 1 || p > 65535 {
		return 0, fmt.Errorf("%q is not a port (1-65535)", s)
	}
	return p, nil
}

// reachScript is the POSIX sh text of one reachability check. name and port
// are validated by the caller and quoted here; every line it prints starts
// with the step, then ok or FAIL, so results group across hosts.
func reachScript(name string, port int, tls, http bool) string {
	p := strconv.Itoa(port)
	var b strings.Builder
	b.WriteString("T=" + shq.Quote(name) + "; P=" + shq.Quote(p) + "; ")
	b.WriteString(`A=$(getent ahostsv4 "$T" 2>/dev/null | awk 'NR==1{print $1}'); `)
	b.WriteString(`[ -n "$A" ] || A=$(getent hosts "$T" 2>/dev/null | awk 'NR==1{print $1}'); `)
	b.WriteString(`if [ -z "$A" ]; then echo "dns    FAIL  $T does not resolve"; exit 0; fi; `)
	b.WriteString(`echo "dns    ok    $T is $A"; `)
	b.WriteString(`R=$(ip route get "$A" 2>/dev/null | head -n 1); `)
	b.WriteString(`if [ -n "$R" ]; then echo "route  ok    $R"; else echo "route  FAIL  no route to $A"; fi; `)
	b.WriteString(`if command -v bash >/dev/null 2>&1; then `)
	b.WriteString(`if timeout 3 bash -c 'exec 3<>"/dev/tcp/$0/$1"' "$A" "$P" 2>/dev/null; then echo "tcp    ok    $A:$P accepts connections"; `)
	b.WriteString(`else echo "tcp    FAIL  $A:$P refused, filtered or silent for 3s"; exit 0; fi; `)
	b.WriteString(`elif command -v nc >/dev/null 2>&1; then `)
	b.WriteString(`if nc -z -w 3 "$A" "$P" 2>/dev/null; then echo "tcp    ok    $A:$P accepts connections"; `)
	b.WriteString(`else echo "tcp    FAIL  $A:$P refused, filtered or silent for 3s"; exit 0; fi; `)
	b.WriteString(`else echo "tcp    skip  neither bash nor nc on this host"; fi; `)
	if tls {
		b.WriteString(`if command -v openssl >/dev/null 2>&1; then `)
		b.WriteString(`C=$(echo | timeout 5 openssl s_client -connect "$A:$P" -servername "$T" 2>/dev/null | openssl x509 -noout -subject -enddate 2>/dev/null | tr '\n' ' '); `)
		b.WriteString(`if [ -n "$C" ]; then echo "tls    ok    $C"; else echo "tls    FAIL  no certificate from $A:$P"; fi; `)
		b.WriteString(`else echo "tls    skip  no openssl on this host"; fi; `)
	}
	if http {
		scheme := "http"
		if tls {
			scheme = "https"
		}
		b.WriteString(`if command -v curl >/dev/null 2>&1; then `)
		b.WriteString(`H=$(curl -sS -k -o /dev/null -m 5 -w '%{http_code}' ` + shq.Quote(scheme+"://"+name+":"+p+"/") + ` 2>/dev/null); `)
		b.WriteString(`if [ -n "$H" ] && [ "$H" != 000 ]; then echo "http   ok    status $H"; else echo "http   FAIL  no HTTP answer"; fi; `)
		b.WriteString(`else echo "http   skip  no curl on this host"; fi; `)
	}
	return strings.TrimSuffix(b.String(), "; ")
}

// probeHosts is where a network check runs: the session targets, or every
// inventory host when no targets are set. The checks are read-only, so the
// wide default is safe, and a tool that answers beats one that asks for setup.
func probeHosts(env *Env) []resource.HostID {
	if hosts := sessionHosts(env); len(hosts) > 0 {
		return hosts
	}
	if env == nil || env.Inventory == nil {
		return nil
	}
	all := env.Inventory.Hosts()
	out := make([]resource.HostID, 0, len(all))
	for _, h := range all {
		out = append(out, h.ID)
	}
	return out
}

func runReach(env *Env, args string, open FanoutOpener) tea.Cmd {
	var endpoint string
	var tls, http bool
	for _, tok := range strings.Fields(args) {
		switch tok {
		case "--tls":
			tls = true
		case "--http":
			http = true
		default:
			if endpoint != "" || strings.HasPrefix(tok, "-") {
				return Flash(FlashError, "%s", reachUsage)
			}
			endpoint = tok
		}
	}
	if endpoint == "" {
		return Flash(FlashError, "%s", reachUsage)
	}
	name, port, err := parseEndpoint(endpoint)
	if err != nil {
		return Flash(FlashError, "%v; %s", err, reachUsage)
	}
	hosts := probeHosts(env)
	if len(hosts) == 0 {
		return Flash(FlashError, "no hosts to run from: the inventory is empty")
	}
	if open == nil {
		return Flash(FlashError, "no fan-out view for network checks")
	}
	cmd := transport.Command{Script: reachScript(name, port, tls, http), Timeout: 20 * time.Second}
	title := fmt.Sprintf("reach %s:%d", name, port)
	return func() tea.Msg { return PushViewMsg{View: open(env, title, cmd, hosts, nil)} }
}

// farEndScript reports what listens on the port and which firewall rules name
// it. sudo is tried without a password and skipped when it is not allowed.
func farEndScript(port int) string {
	p := shq.Quote(strconv.Itoa(port))
	return `P=` + p + `; S=""; if sudo -n true 2>/dev/null; then S="sudo -n"; fi; ` +
		`ip -o -4 addr show scope global 2>/dev/null | awk '{split($4,a,"/"); print "addr   " a[1]}'; ` +
		`L=$($S ss -H -ltnp "sport = :$P" 2>/dev/null); ` +
		`if [ -n "$L" ]; then echo "$L" | awk '{print "listen ok    " $4 " " $6}'; else echo "listen FAIL  nothing listens on tcp/$P"; fi; ` +
		`if [ -z "$S" ]; then echo "fw     skip  firewall rules need sudo -n"; ` +
		`elif command -v nft >/dev/null 2>&1; then F=$($S nft list ruleset 2>/dev/null | grep -n -w -- "$P" | head -n 8); ` +
		`if [ -n "$F" ]; then echo "$F" | sed 's/^/fw     rule  nft /'; else echo "fw     ok    no nft rule names port $P"; fi; ` +
		`elif command -v iptables-save >/dev/null 2>&1; then F=$($S iptables-save 2>/dev/null | grep -n -w -- "--dport $P" | head -n 8); ` +
		`if [ -n "$F" ]; then echo "$F" | sed 's/^/fw     rule  iptables /'; else echo "fw     ok    no iptables rule names port $P"; fi; ` +
		`else echo "fw     skip  no nft or iptables on this host"; fi`
}

// traceVerdict reads the step lines and says where the path breaks.
func traceVerdict(from, to string, port int, near, far []string, farErr error) string {
	failed := func(lines []string, step string) bool {
		for _, l := range lines {
			f := strings.Fields(l)
			if len(f) >= 2 && f[0] == step && f[1] == "FAIL" {
				return true
			}
		}
		return false
	}
	// A listener bound to loopback only is the classic "it works on the box".
	loopbackOnly := false
	for _, l := range far {
		f := strings.Fields(l)
		if len(f) >= 3 && f[0] == "listen" && f[1] == "ok" {
			if strings.HasPrefix(f[2], "127.") || strings.HasPrefix(f[2], "[::1]") {
				loopbackOnly = true
				continue
			}
			loopbackOnly = false
			break
		}
	}
	switch {
	case failed(near, "tcp") && farErr == nil && loopbackOnly:
		return fmt.Sprintf("breaks at the far end: tcp/%d on %s is bound to loopback only, so nothing outside the host can connect", port, to)
	case failed(near, "dns"):
		return fmt.Sprintf("breaks at name resolution: %s cannot resolve %s", from, to)
	case failed(near, "route"):
		return fmt.Sprintf("breaks at routing: %s has no route to %s", from, to)
	case failed(near, "tcp") && farErr == nil && failed(far, "listen"):
		return fmt.Sprintf("breaks at the far end: nothing listens on %s tcp/%d", to, port)
	case failed(near, "tcp") && farErr == nil:
		return fmt.Sprintf("breaks in between: %s listens on tcp/%d but %s cannot connect — check the firewall rules above and anything on the path", to, port, from)
	case failed(near, "tcp"):
		return fmt.Sprintf("breaks at connect: %s cannot open %s tcp/%d, and the far end could not be inspected", from, to, port)
	default:
		return fmt.Sprintf("path is open: %s reaches %s tcp/%d", from, to, port)
	}
}

func runTrace(env *Env, args string) tea.Cmd {
	f := strings.Fields(args)
	if len(f) != 3 {
		return Flash(FlashError, "%s", traceUsage)
	}
	port, err := parsePort(f[2])
	if err != nil {
		return Flash(FlashError, "%v; %s", err, traceUsage)
	}
	if !netNameRe.MatchString(f[0]) || !netNameRe.MatchString(f[1]) {
		return Flash(FlashError, "%s", traceUsage)
	}
	if env == nil || env.Exec == nil {
		return Flash(FlashError, "no SSH transport in this session")
	}
	from, to := resource.HostID(f[0]), f[1]
	target := to
	var farHost resource.HostID
	if env.Inventory != nil {
		if h, ok := env.Inventory.Resolve(f[0]); ok {
			from = h.ID
		}
		if h, ok := env.Inventory.Resolve(to); ok {
			farHost = h.ID
		}
	}
	exec := env.Exec
	red := env.Redact
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), traceTimeout)
		defer cancel()
		lines := []string{
			fmt.Sprintf("trace  %s  →  %s  tcp/%d", from, to, port),
			"",
			fmt.Sprintf("from %s", from),
		}
		split := func(b []byte) []string {
			text := strings.TrimRight(string(b), "\n")
			if red != nil {
				text = red.String(text)
			}
			if text == "" {
				return nil
			}
			return strings.Split(text, "\n")
		}
		// The far end is asked first: it says what listens, which firewall
		// rules name the port, and which address it really has — an inventory
		// name is often an SSH alias that DNS has never heard of.
		var near, far []string
		var farErr error
		farLines := []string{"", fmt.Sprintf("at %s", to)}
		if farHost == "" {
			farErr = fmt.Errorf("not in the inventory")
			farLines = append(farLines, "  "+to+" is not a host jumpr can log in to; the far end was not inspected")
		} else {
			fres, ferr := exec.Exec(ctx, farHost, transport.Command{Script: farEndScript(port), Timeout: 12 * time.Second})
			farErr = ferr
			if ferr != nil {
				farLines = append(farLines, "  could not run on "+string(farHost)+": "+ferr.Error())
			} else {
				for _, l := range split(fres.Stdout) {
					if f := strings.Fields(l); len(f) == 2 && f[0] == "addr" {
						if target == to && netNameRe.MatchString(f[1]) {
							target = f[1]
						}
						continue
					}
					far = append(far, l)
					farLines = append(farLines, "  "+l)
				}
			}
		}
		if target != to {
			lines = append(lines, fmt.Sprintf("  using %s, the address %s reports for itself", target, to))
		}
		res, err := exec.Exec(ctx, from, transport.Command{Script: reachScript(target, port, false, false), Timeout: 12 * time.Second})
		if err != nil {
			lines = append(lines, "  could not run on "+string(from)+": "+err.Error())
			return PushViewMsg{View: newTextPage(env, "trace", lines)}
		}
		near = split(res.Stdout)
		for _, l := range near {
			lines = append(lines, "  "+l)
		}
		lines = append(lines, farLines...)
		lines = append(lines, "", "verdict  "+traceVerdict(string(from), to, port, near, far, farErr))
		return PushViewMsg{View: newTextPage(env, "trace", lines)}
	}
}
