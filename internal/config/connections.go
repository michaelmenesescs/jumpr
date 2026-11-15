package config

import (
	"path/filepath"

	"jumpr/internal/transport/httpx"
)

// ConnectionsFile is the file name of the connection profiles (SPEC §7.2).
const ConnectionsFile = "connections.yaml"

// ConnectionsPath returns the connection-profile file inside the config
// directory.
func (p Paths) ConnectionsPath() string {
	return filepath.Join(p.ConfigDir, ConnectionsFile)
}

// LoadConnections reads the connection profiles for p. Profiles are strict
// YAML (unknown keys are errors), a missing file yields an empty set and every
// profile is validated by httpx.LoadProfiles, which owns the schema.
func LoadConnections(path, home string) (map[string]*httpx.Profile, error) {
	return httpx.LoadProfiles(path, home)
}
