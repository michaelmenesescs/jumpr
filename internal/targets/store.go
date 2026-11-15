package targets

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"jumpr/internal/errs"
	"jumpr/internal/resource"
)

// setsDir is the directory under the store root that holds saved sets.
const setsDir = "sets"

// Store reads and writes saved sets as <root>/sets/<name>.txt, one host per
// line, "#" comments.
type Store struct {
	root string
	now  func() time.Time
}

// NewStore builds a store rooted at root (normally the config directory). A
// list or load from a root that has no sets yet is empty, not an error.
func NewStore(root string) *Store {
	return &Store{root: root, now: time.Now}
}

// dir is the sets directory of the store.
func (s *Store) dir() string {
	return filepath.Join(s.root, setsDir)
}

// path is the file of one set.
func (s *Store) path(name string) string {
	return filepath.Join(s.dir(), name+".txt")
}

// List returns the saved set names, sorted. A missing sets directory is empty.
func (s *Store) List() []string {
	if s == nil || s.root == "" {
		return nil
	}
	entries, err := os.ReadDir(s.dir())
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name, ok := strings.CutSuffix(e.Name(), ".txt")
		if !ok || !validName(name) {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Load reads one saved set: one host per line, blank lines and "#" comments
// skipped. An unknown or invalid name is an errs.Config error.
func (s *Store) Load(name string) ([]resource.HostID, error) {
	if s == nil || s.root == "" {
		return nil, errs.New(errs.Config, "targets.Store.Load", "no target set store configured")
	}
	if !validName(name) {
		return nil, badName("targets.Store.Load", name)
	}
	f, err := os.Open(s.path(name))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, unknownSet(name)
		}
		return nil, errs.Wrap(errs.Config, "targets.Store.Load", err)
	}
	defer f.Close()

	var hosts []resource.HostID
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if id := resource.NormalizeHostID(line); id != "" {
			hosts = append(hosts, id)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, errs.Wrap(errs.Config, "targets.Store.Load", err)
	}
	return normalizeSorted(hosts), nil
}

// Save writes one set atomically as <root>/sets/<name>.txt, with a header
// comment carrying the save date. The name must match ^[a-z0-9][a-z0-9_-]*$.
func (s *Store) Save(name string, hosts []resource.HostID) error {
	if s == nil || s.root == "" {
		return errs.New(errs.Config, "targets.Store.Save", "no target set store configured")
	}
	if !validName(name) {
		return badName("targets.Store.Save", name)
	}
	if err := os.MkdirAll(s.dir(), 0o755); err != nil {
		return errs.Wrap(errs.Config, "targets.Store.Save", err)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# jumpr target set: %s\n", name)
	fmt.Fprintf(&b, "# saved: %s\n", s.now().UTC().Format(time.RFC3339))
	for _, h := range normalizeSorted(hosts) {
		b.WriteString(string(h))
		b.WriteByte('\n')
	}

	tmp, err := os.CreateTemp(s.dir(), ".set-*")
	if err != nil {
		return errs.Wrap(errs.Config, "targets.Store.Save", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeded

	if _, err := tmp.WriteString(b.String()); err != nil {
		tmp.Close()
		return errs.Wrap(errs.Config, "targets.Store.Save", err)
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return errs.Wrap(errs.Config, "targets.Store.Save", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return errs.Wrap(errs.Config, "targets.Store.Save", err)
	}
	if err := tmp.Close(); err != nil {
		return errs.Wrap(errs.Config, "targets.Store.Save", err)
	}
	if err := os.Rename(tmpName, s.path(name)); err != nil {
		return errs.Wrap(errs.Config, "targets.Store.Save", err)
	}
	return nil
}

// Delete removes one saved set. An unknown or invalid name is an errs.Config
// error.
func (s *Store) Delete(name string) error {
	if s == nil || s.root == "" {
		return errs.New(errs.Config, "targets.Store.Delete", "no target set store configured")
	}
	if !validName(name) {
		return badName("targets.Store.Delete", name)
	}
	if err := os.Remove(s.path(name)); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return unknownSet(name)
		}
		return errs.Wrap(errs.Config, "targets.Store.Delete", err)
	}
	return nil
}

// validName reports whether a set name matches ^[a-z0-9][a-z0-9_-]*$. It is
// checked by hand so no expression is compiled per call.
func validName(name string) bool {
	if name == "" || len(name) > 128 {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case (c == '_' || c == '-') && i > 0:
		default:
			return false
		}
	}
	return true
}

// badName builds the error for a name outside the allowed alphabet.
func badName(op, name string) *errs.Error {
	return errs.New(errs.Config, op,
		fmt.Sprintf("bad set name %q (want ^[a-z0-9][a-z0-9_-]*$)", name))
}
