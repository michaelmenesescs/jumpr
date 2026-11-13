package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"jumper/internal/errs"
)

// Paths are the directories jumper uses on the local machine.
type Paths struct {
	ConfigDir     string // ~/.config/jumper by default
	CacheDir      string // ~/.cache/jumper
	KnowledgeRoot string // knowledge.root, default ~/jumper
}

// ResolvePaths returns the local paths for the given home directory. An empty
// configDirOverride selects the default config directory; a non-empty one wins.
// It reads no environment variables (BUILD-PLAN §6.1).
func ResolvePaths(home, configDirOverride string) Paths {
	configDir := ExpandHome(configDirOverride, home)
	if configDir == "" {
		configDir = filepath.Join(home, ".config", "jumper")
	}
	return Paths{
		ConfigDir:     configDir,
		CacheDir:      filepath.Join(home, ".cache", "jumper"),
		KnowledgeRoot: filepath.Join(home, "jumper"),
	}
}

// ExpandHome replaces a leading "~/" (or a bare "~") in p with home. Any other
// path is returned unchanged.
func ExpandHome(p, home string) string {
	if p == "~" {
		return home
	}
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(home, p[2:])
	}
	return p
}

// Load reads path into a copy of Default(), so omitted keys keep their default
// value. A missing file is not an error: it yields Default(). Unknown keys are
// errors because the decoder is strict. All errors are *errs.Error with kind
// Config and name the file (and line, when YAML reports one).
//
// Load expands "~/" in the path fields (ssh.config_files, ssh.known_hosts,
// inventory.sources[].path, knowledge.root) using home, then validates the
// result.
func Load(path, home string) (Config, error) {
	cfg := Default()

	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return cfg, nil
	case err != nil:
		return cfg, &errs.Error{
			Kind: errs.Config,
			Op:   "config.load",
			Msg:  fmt.Sprintf("cannot read %s", path),
			Err:  err,
		}
	}

	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
		return cfg, &errs.Error{
			Kind: errs.Config,
			Op:   "config.load",
			Msg:  fmt.Sprintf("invalid config %s", path),
			Err:  err,
		}
	}

	expandPaths(&cfg, home)

	if err := cfg.Validate(); err != nil {
		return cfg, &errs.Error{
			Kind: errs.Config,
			Op:   "config.load",
			Msg:  path,
			Err:  err,
		}
	}
	return cfg, nil
}

// expandPaths applies ExpandHome to every path field of c.
func expandPaths(c *Config, home string) {
	for i := range c.SSH.ConfigFiles {
		c.SSH.ConfigFiles[i] = ExpandHome(c.SSH.ConfigFiles[i], home)
	}
	for i := range c.SSH.KnownHosts {
		c.SSH.KnownHosts[i] = ExpandHome(c.SSH.KnownHosts[i], home)
	}
	for i := range c.Inventory.Sources {
		c.Inventory.Sources[i].Path = ExpandHome(c.Inventory.Sources[i].Path, home)
	}
	c.Knowledge.Root = ExpandHome(c.Knowledge.Root, home)
}
