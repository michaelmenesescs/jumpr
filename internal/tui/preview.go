package tui

import (
	"io"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"jumpr/internal/render"
	"jumpr/internal/resource"
	"jumpr/internal/tui/theme"
)

// The preview is the right-hand half of a list: the view `enter` would open
// for the row under the cursor, live, without leaving the list. It is the
// list + detail layout of isd applied to every kind that has a drill-in.
const (
	// previewMinWidth is the narrowest main area that is still split in two.
	previewMinWidth = 110
	// previewDebounce is how long the cursor must rest before a preview loads.
	previewDebounce = 180 * time.Millisecond
	// previewSeparator divides the list from the preview.
	previewSeparator = "│ "
)

// Previewer is implemented by a view that can say what `enter` would open for
// its current selection. key identifies the selection, so the pane reloads the
// preview only when it changes.
type Previewer interface {
	PreviewQuery() (q resource.Query, key string, ok bool)
}

// ViewPreviewer is implemented by a view whose selection opens a view that is
// not reached through a query (a Kubernetes object's describe screen). It
// wins over Previewer when both are implemented and it reports ok.
type ViewPreviewer interface {
	PreviewView() (v View, key string, ok bool)
}

// previewTickMsg fires when the selection has rested for previewDebounce.
type previewTickMsg struct{ gen int }

// previewMsg carries a message of the preview's own view back to it.
type previewMsg struct {
	gen int
	msg tea.Msg
}

// forPreview wraps every message cmd produces so it reaches the preview view
// of generation gen and no other view.
func forPreview(gen int, cmd tea.Cmd) tea.Cmd {
	if cmd == nil {
		return nil
	}
	return func() tea.Msg {
		switch msg := cmd().(type) {
		case nil:
			return nil
		case tea.BatchMsg:
			batch := make(tea.BatchMsg, 0, len(msg))
			for _, c := range msg {
				if c != nil {
					batch = append(batch, forPreview(gen, c))
				}
			}
			return batch
		default:
			return previewMsg{gen: gen, msg: msg}
		}
	}
}

// preview is the pane's preview state.
type preview struct {
	on   bool
	gen  int
	key  string
	q    resource.Query
	view View
	// byView is true when the selection is previewed through ViewPreviewer.
	byView bool
}

func (pv *preview) close() {
	if pv.view == nil {
		return
	}
	if c, ok := pv.view.(io.Closer); ok {
		_ = c.Close()
	}
	pv.view = nil
}

// track compares the top view's selection with the one on show and, when it
// changed, drops the stale preview and starts the debounce.
func (p *ViewPane) trackPreview() tea.Cmd {
	pv := &p.preview
	top, ok := p.stack.Top().(Previewer)
	if vp, isView := p.stack.Top().(ViewPreviewer); isView && pv.on {
		// Only the key is read here; the view is built on the tick.
		if _, key, has := vp.PreviewView(); has {
			if key == pv.key {
				return nil
			}
			pv.key, pv.byView = key, true
			pv.gen++
			gen := pv.gen
			return tea.Tick(previewDebounce, func(time.Time) tea.Msg { return previewTickMsg{gen: gen} })
		}
	}
	pv.byView = false
	if !ok || !pv.on {
		if pv.key != "" || pv.view != nil {
			pv.close()
			pv.key = ""
			pv.gen++
		}
		return nil
	}
	q, key, has := top.PreviewQuery()
	if !has {
		key = ""
	}
	if key == pv.key {
		return nil
	}
	pv.key = key
	pv.q = q
	pv.gen++
	if key == "" {
		pv.close()
		return nil
	}
	gen := pv.gen
	return tea.Tick(previewDebounce, func(time.Time) tea.Msg { return previewTickMsg{gen: gen} })
}

// handlePreviewMsg loads the preview on its tick and routes the preview view's
// own messages to it. handled is false for every other message.
func (p *ViewPane) handlePreviewMsg(msg tea.Msg) (tea.Cmd, bool) {
	pv := &p.preview
	switch m := msg.(type) {
	case previewTickMsg:
		if m.gen != pv.gen || pv.key == "" {
			return nil, true
		}
		var v View
		var err error
		if vp, isView := p.stack.Top().(ViewPreviewer); pv.byView && isView {
			built, _, has := vp.PreviewView()
			if !has {
				pv.close()
				return nil, true
			}
			v = built
		} else {
			v, err = p.viewFor(pv.q)
		}
		pv.close()
		if err != nil {
			return nil, true
		}
		p.attach(v)
		pv.view = v
		return forPreview(pv.gen, v.Init()), true
	case previewMsg:
		if m.gen != pv.gen || pv.view == nil {
			return nil, true
		}
		switch m.msg.(type) {
		case PushViewMsg, PopViewMsg, ReplaceViewMsg, OpenQueryMsg, FlashMsg:
			// A preview never navigates and never talks on the flash line.
			return nil, true
		}
		v, cmd := pv.view.Update(m.msg)
		if v != nil {
			pv.view = v
		}
		return forPreview(pv.gen, cmd), true
	}
	return nil, false
}

// previewVisible reports whether the main area is split for a preview.
func (p *ViewPane) previewVisible(mainW int) bool {
	if !p.preview.on || mainW < previewMinWidth {
		return false
	}
	switch p.stack.Top().(type) {
	case Previewer, ViewPreviewer:
		return true
	}
	return false
}

// renderPreview draws the preview column, separator included.
func (p *ViewPane) renderPreview(w, h int) string {
	content := w - render.Width(previewSeparator)
	if content < 1 {
		content = 1
	}
	var t *theme.Theme
	if p.env != nil {
		t = p.env.Theme
	}
	var body string
	switch {
	case p.preview.view != nil:
		title := p.preview.view.Crumb()
		if t != nil {
			title = t.Header.Render(render.Truncate(title, content))
		}
		body = title + "\n" + render.Fit(p.preview.view.Render(content, max(h-1, 0)), content, max(h-1, 0))
	case p.preview.key != "":
		body = dimStyle(t, "loading"+string(glyphsFor(p.env).Ellipsis))
	default:
		body = dimStyle(t, "nothing selected")
	}
	rows := strings.Split(render.Fit(body, content, h), "\n")
	bar, _ := shellGlyphs(p.env)
	sep := bar + " "
	if t != nil {
		sep = t.Border.Render(sep)
	}
	for i, row := range rows {
		rows[i] = sep + row
	}
	return strings.Join(rows, "\n")
}
