// Package mux holds the pane multiplexer: layout tree, windows, panes.
package mux

// PaneID identifies a pane within a session.
type PaneID int

// WindowID identifies a window within a session.
type WindowID int

// Rect is a pane rectangle in screen cells, borders included.
type Rect struct{ X, Y, W, H int }

// Direction is a screen direction.
type Direction int

const (
	Left Direction = iota
	Right
	Up
	Down
)
