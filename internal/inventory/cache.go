package inventory

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"jumper/internal/errs"
)

// cacheFile is the on-disk form of the per-source host lists.
type cacheFile struct {
	Sources map[string]cacheSource `json:"sources"`
}

type cacheSource struct {
	Hosts   []Host    `json:"hosts"`
	Updated time.Time `json:"updated"`
}

// LoadCache fills the store from cachePath for an instant first frame. A
// missing file is not an error.
func (s *Store) LoadCache() error {
	if s.cachePath == "" {
		return nil
	}
	data, err := os.ReadFile(s.cachePath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return errs.Wrap(errs.Config, "inventory.LoadCache", err)
	}
	var cf cacheFile
	if err := json.Unmarshal(data, &cf); err != nil {
		return errs.Wrap(errs.Parse, "inventory.LoadCache", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for name, cs := range cf.Sources {
		s.bySource[name] = sourceState{hosts: cloneHosts(cs.Hosts), updated: cs.Updated}
		if cs.Updated.After(s.lastRefresh) {
			s.lastRefresh = cs.Updated
		}
	}
	s.rebuildLocked()
	return nil
}

// cacheSnapshotLocked copies the per-source states for writing outside the lock.
func (s *Store) cacheSnapshotLocked() cacheFile {
	cf := cacheFile{Sources: make(map[string]cacheSource, len(s.bySource))}
	for name, st := range s.bySource {
		cf.Sources[name] = cacheSource{Hosts: cloneHosts(st.hosts), Updated: st.updated}
	}
	return cf
}

// writeCache writes cf atomically: temp file in the same directory, then
// rename. An empty cache path disables caching.
func (s *Store) writeCache(cf cacheFile) error {
	if s.cachePath == "" {
		return nil
	}
	dir := filepath.Dir(s.cachePath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return errs.Wrap(errs.Config, "inventory.writeCache", err)
	}
	data, err := json.Marshal(cf)
	if err != nil {
		return errs.Wrap(errs.Internal, "inventory.writeCache", err)
	}
	tmp, err := os.CreateTemp(dir, ".inventory-*.json")
	if err != nil {
		return errs.Wrap(errs.Config, "inventory.writeCache", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return errs.Wrap(errs.Config, "inventory.writeCache", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return errs.Wrap(errs.Config, "inventory.writeCache", err)
	}
	if err := os.Rename(tmpName, s.cachePath); err != nil {
		os.Remove(tmpName)
		return errs.Wrap(errs.Config, "inventory.writeCache", err)
	}
	return nil
}
