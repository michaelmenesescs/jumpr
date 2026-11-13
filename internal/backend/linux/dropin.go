package linux

import (
	"fmt"
	"path"
	"strings"

	"jumper/internal/group"
	"jumper/internal/shq"
)

// Where `systemctl edit <unit>` (without --full) puts a local override: the
// unit's drop-in directory under /etc. jumper only ever names this path; it
// never writes it (SPEC §8.1.1, zero footprint).
const (
	dropInRoot = "/etc/systemd/system"
	dropInName = "override.conf"
)

// dropInSections are the section names a systemd unit file may carry. Any other
// [header] is a typo: systemd logs it and ignores the file.
var dropInSections = map[string]bool{
	"Unit":    true,
	"Service": true,
	"Socket":  true,
	"Timer":   true,
	"Install": true,
	"Mount":   true,
	"Path":    true,
	"Slice":   true,
}

// DropInPath is the drop-in file a draft override would become:
// /etc/systemd/system/<unit>.d/override.conf.
func DropInPath(unit string) string {
	unit = strings.TrimSpace(unit)
	return path.Join(dropInRoot, unit+".d", dropInName)
}

// DropInBody returns the body of one file in `systemctl cat` output: the lines
// below its `# <path>` header, up to the next header. It reports false when the
// file is not in the output. The body has no trailing newline.
func DropInBody(cat, file string) (string, bool) {
	file = strings.TrimSpace(file)
	if file == "" {
		return "", false
	}
	lines := strings.Split(strings.ReplaceAll(cat, "\r\n", "\n"), "\n")
	for i, line := range lines {
		p, ok := catHeaderPath(line)
		if !ok || p != file {
			continue
		}
		body := make([]string, 0, 8)
		for j := i + 1; j < len(lines); j++ {
			if _, ok := catHeaderPath(lines[j]); ok {
				break
			}
			body = append(body, lines[j])
		}
		return strings.TrimRight(strings.Join(body, "\n"), "\n"), true
	}
	return "", false
}

// ValidateDropIn checks a drop-in draft against the rules systemd applies and
// returns one warning per problem, in file order. It never rejects anything:
// the draft is the user's. The warnings cover a missing section header, an
// unknown section, a line that is not key=value, and an `ExecStart=` that
// appends to the unit's own because no empty `ExecStart=` reset precedes it.
func ValidateDropIn(text string) []string {
	var warnings []string
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	headerSeen := false
	resetSeen := false
	for i, raw := range lines {
		n := i + 1
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if name, ok := dropInHeader(line); ok {
			headerSeen = true
			resetSeen = false
			if !dropInSections[name] {
				warnings = append(warnings, fmt.Sprintf("line %d: unknown section [%s]", n, name))
			}
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" {
			warnings = append(warnings, fmt.Sprintf("line %d: %q is not a key=value line", n, line))
			continue
		}
		if key != "ExecStart" {
			continue
		}
		if strings.TrimSpace(value) == "" {
			resetSeen = true
			continue
		}
		if !resetSeen {
			warnings = append(warnings, fmt.Sprintf(
				"line %d: ExecStart= needs an empty ExecStart= before it, or systemd appends to the unit's own", n))
		}
	}
	if !headerSeen {
		warnings = append([]string{"no section header: systemd ignores every key outside a [Section]"}, warnings...)
	}
	return warnings
}

// dropInHeader is the section name of a `[Section]` line.
func dropInHeader(line string) (string, bool) {
	if len(line) < 3 || line[0] != '[' || line[len(line)-1] != ']' {
		return "", false
	}
	name := strings.TrimSpace(line[1 : len(line)-1])
	if name == "" {
		return "", false
	}
	return name, true
}

// DropInPreview is the read-only result of a draft override: what `systemctl
// cat` would show with it, the warnings, and the commands that apply it by
// hand. jumper never runs those commands and never writes on the node.
type DropInPreview struct {
	Unit     string
	Path     string   // the drop-in file the draft would become
	Text     string   // the draft, newline-normalised, no trailing blank lines
	Diff     string   // unified diff: `systemctl cat` now → with the drop-in
	Warnings []string // ValidateDropIn(text)
	Apply    string   // the "to apply by hand" block, ready to paste
}

// PreviewDropIn builds the preview of text as the drop-in of unit, given the
// `systemctl cat` output already loaded for it. It is pure: no transport, no
// filesystem, nothing reaches the node.
func PreviewDropIn(unit, cat, text string) DropInPreview {
	unit = strings.TrimSpace(unit)
	text = normaliseDropIn(text)
	p := DropInPreview{Unit: unit, Path: DropInPath(unit), Text: text}
	p.Warnings = ValidateDropIn(text)
	p.Diff = group.UnifiedDiff(
		shq.Join("systemctl", "cat", unit), withFinalNewline(normaliseDropIn(cat)),
		shq.Join("systemctl", "cat", unit)+" (with the drop-in)",
		withFinalNewline(catWithDropIn(cat, p.Path, text)),
	)
	p.Apply = applyByHand(unit, p.Path, text)
	return p
}

// normaliseDropIn turns CRLF into LF and drops trailing blank lines, so a diff
// of the same file does not show a spurious last line.
func normaliseDropIn(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	return strings.TrimRight(text, "\n")
}

// withFinalNewline gives a normalised text exactly one trailing newline, which
// is what difflib needs to treat the last line as a complete line.
func withFinalNewline(text string) string {
	text = strings.TrimRight(text, "\n")
	if text == "" {
		return ""
	}
	return text + "\n"
}

// catWithDropIn is the `systemctl cat` output with the drop-in in place: the
// existing block for file is replaced where it already sits (systemctl prints
// drop-ins after the unit file), otherwise the block is appended.
func catWithDropIn(cat, file, text string) string {
	lines := strings.Split(normaliseDropIn(cat), "\n")
	if cat == "" {
		lines = nil
	}
	block := "# " + file
	if text != "" {
		block += "\n" + text
	}
	out := make([]string, 0, len(lines)+4)
	replaced := false
	for i := 0; i < len(lines); i++ {
		if p, ok := catHeaderPath(lines[i]); ok && p == file {
			out = append(out, strings.Split(block, "\n")...)
			replaced = true
			for i+1 < len(lines) {
				if _, next := catHeaderPath(lines[i+1]); next {
					break
				}
				i++
			}
			continue
		}
		out = append(out, lines[i])
	}
	if !replaced {
		out = append(out, strings.Split(block, "\n")...)
	}
	return strings.Join(out, "\n")
}

// applyByHand is the block the user runs themselves: the editor route with the
// content, and the file route (`tee`, `daemon-reload`, `restart`). The commands
// are display text, assembled with shq so the unit name and the path are quoted
// exactly as they would be if jumper ever ran them — which it does not.
func applyByHand(unit, file, text string) string {
	var b strings.Builder
	b.WriteString("to apply by hand\n")
	b.WriteString("\n")
	b.WriteString("# editor route: opens a temporary copy of the drop-in\n")
	b.WriteString("sudo " + shq.Join("systemctl", "edit", unit) + "\n")
	b.WriteString("#   paste the content below, save and exit\n")
	b.WriteString("\n")
	b.WriteString("# file route, no editor\n")
	b.WriteString("sudo " + shq.Join("mkdir", "-p", path.Dir(file)) + "\n")
	b.WriteString("sudo tee " + shq.Quote(file) + " >/dev/null <<'EOF'\n")
	if text != "" {
		b.WriteString(text + "\n")
	}
	b.WriteString("EOF\n")
	b.WriteString("sudo " + shq.Join("systemctl", "daemon-reload") + "\n")
	b.WriteString("sudo " + shq.Join("systemctl", "restart", unit) + "\n")
	return b.String()
}
