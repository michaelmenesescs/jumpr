//go:build deps

// Package deps holds blank imports of every third-party dependency pinned for
// M0-M7 so that `go mod tidy` keeps them in go.mod even before the packages
// that use them exist (BUILD-PLAN D11). It is compiled only with the `deps`
// build tag and contains no code.
package deps

import (
	_ "github.com/charmbracelet/bubbles/textinput"
	_ "github.com/charmbracelet/bubbletea"
	_ "github.com/charmbracelet/lipgloss"
	_ "github.com/charmbracelet/x/ansi"
	_ "github.com/charmbracelet/x/exp/teatest"
	_ "github.com/charmbracelet/x/vt"
	_ "github.com/creack/pty"
	_ "github.com/gliderlabs/ssh"
	_ "github.com/kevinburke/ssh_config"
	_ "github.com/mattn/go-runewidth"
	_ "github.com/pmezard/go-difflib/difflib"
	_ "github.com/skeema/knownhosts"
	_ "golang.org/x/crypto/ssh"
	_ "golang.org/x/crypto/ssh/agent"
	_ "golang.org/x/sync/errgroup"
	_ "golang.org/x/sys/unix"
	_ "golang.org/x/term"
	_ "gopkg.in/yaml.v3"
)
