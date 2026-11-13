package plugin

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"jumper/internal/errs"
)

// Saving a `:run` result as a plugin (SPEC §5 W5: "saved as a YAML plugin in
// one keystroke"). The file is a normal plugin the loader validates like any
// other: target host, the command verbatim, parse raw, and the grouped view, so
// the next run of it looks exactly like the run it came from.

const (
	// saveOp is the errs.Error op of every failure in this file.
	saveOp = "plugin.Save"
	// SaveExt is the extension of a plugin file.
	SaveExt = ".yaml"
	// DirName is the directory plugins live in, under the config directory
	// (SPEC §12: ~/.config/jumper/plugins/*.yaml).
	DirName = "plugins"
	// saveDescription is the description written into a saved plugin.
	saveDescription = "saved from a :run result"
	// saveNameMax is the longest generated name; a plugin name doubles as a
	// file name and a kind name.
	saveNameMax = 40
)

// Save writes a new plugin file for a command that was run with `:run`. dir is
// the plugins directory; the file is <dir>/<name>.yaml and it is never
// overwritten: an existing file of that name is an error. It returns the path
// it wrote so the caller can flash it.
func Save(dir, name, command string, sudo bool) (string, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return "", errs.New(errs.Config, saveOp, "no plugin directory configured")
	}
	name = strings.TrimSpace(name)
	if !nameRe.MatchString(name) {
		return "", errs.New(errs.Config, saveOp,
			fmt.Sprintf("plugin name %q must match %s", name, nameRe.String()))
	}
	command = strings.TrimRight(command, " \t\r\n")
	if strings.TrimSpace(command) == "" {
		return "", errs.New(errs.Config, saveOp, "the command is empty")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", errs.Wrap(errs.Config, saveOp, err)
	}
	path := filepath.Join(dir, name+SaveExt)

	// O_EXCL is the "never overwrites" rule, enforced by the kernel rather than
	// by a stat that another writer could race.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return "", errs.New(errs.Config, saveOp,
				fmt.Sprintf("plugin %q already exists at %s; pick another name", name, path))
		}
		return "", errs.Wrap(errs.Config, saveOp, err)
	}
	if _, err := f.WriteString(renderSaved(name, command, sudo)); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return "", errs.Wrap(errs.Config, saveOp, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return "", errs.Wrap(errs.Config, saveOp, err)
	}
	return path, nil
}

// renderSaved is the YAML text of a saved plugin. The command is written as a
// block scalar, so a multi-line shell line keeps its shape and its indentation.
func renderSaved(name, command string, sudo bool) string {
	var b strings.Builder
	b.WriteString("apiVersion: " + APIVersion + "\n")
	b.WriteString("kind: " + KindPlugin + "\n")
	b.WriteString("name: " + name + "\n")
	b.WriteString("description: " + scalar(saveDescription) + "\n")
	b.WriteString("target: " + TargetHost + "\n")
	if sudo {
		b.WriteString("privileged: true\n")
	}
	b.WriteString("command: " + scalar(command) + "\n")
	b.WriteString("parse:\n  type: " + ParseRaw + "\n")
	b.WriteString("view:\n  group_by_output: true\n")
	return b.String()
}

// scalar renders s as a YAML scalar: a block literal when it spans lines, and a
// single-quoted scalar (quoting every rune YAML reserves) when it does not.
func scalar(s string) string {
	if strings.ContainsAny(s, "\r\n") {
		lines := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
		var b strings.Builder
		// |- keeps the line breaks between the lines and drops the final one,
		// so the command reads back byte for byte.
		b.WriteString("|-")
		for _, line := range lines {
			if line == "" {
				b.WriteString("\n")
				continue
			}
			b.WriteString("\n  " + line)
		}
		return b.String()
	}
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// SuggestName turns a view title or a command into a plugin name: the shape a
// plugin name may have (lower case, digits and dashes, starting with a letter).
// The editor shows it as the starting value of the name prompt.
func SuggestName(text string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(text) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		default:
			if !dash && b.Len() > 0 {
				b.WriteByte('-')
				dash = true
			}
		}
	}
	name := strings.Trim(b.String(), "-")
	if len(name) > saveNameMax {
		name = strings.Trim(name[:saveNameMax], "-")
	}
	if name == "" {
		return "run"
	}
	if c := name[0]; c < 'a' || c > 'z' {
		name = "run-" + name
	}
	return name
}
