package mux

import (
	"fmt"
	"strings"
	"time"

	"github.com/marcioapm/lux/vterm"

	"github.com/marcioapm/lux-mux/internal/screen"
)

type vtermT = vterm.Terminal

// progress is a starting Run's way to running, as its events tell it.
type progress struct {
	steps []string
}

// note records a step, once.
func (p *progress) note(s string) {
	if s == "" || (len(p.steps) > 0 && p.steps[len(p.steps)-1] == s) {
		return
	}
	p.steps = append(p.steps, s)
	if len(p.steps) > 12 {
		p.steps = p.steps[len(p.steps)-12:]
	}
}

// stageWords says a lux stage as luxmux shows it.
var stageWords = map[string]string{
	"waiting":      "queued",
	"image":        "pulling image",
	"volumes":      "restoring volumes",
	"repositories": "cloning",
	"container":    "starting container",
	"running":      "running",
	"stopping":     "stopping",
}

// statusScreen draws a window that has no screen yet: what its Run is
// doing, or why it ended and what the user can do.
func (m *Mux) statusScreen(b []byte, w *Window) []byte {
	idx := m.indexLocked(w)
	lines := []string{fmt.Sprintf(" luxmux: window %d (%s), profile %s", idx, w.Name, w.Profile)}
	if w.RunID != "" {
		lines = append(lines, " Run "+w.RunID)
	}
	lines = append(lines, "")
	switch {
	case w.phase == phaseDead && w.resumable:
		lines = append(lines, " "+w.reason, "",
			" r  resume the Run",
			" &  close this window")
	case w.phase == phaseDead:
		lines = append(lines, " ended: "+w.reason, "",
			" r  relaunch a fresh Run of this profile in this window",
			" &  close this window")
	default:
		for _, s := range w.steps.steps {
			lines = append(lines, "  - "+s)
		}
		if w.reason != "" {
			lines = append(lines, "", " "+w.reason)
		}
	}
	b = screen.Text(b, lines, m.rows, m.cols)
	b = append(b, "\x1b]2;"...)
	b = append(b, m.titleLocked(w)...)
	return append(b, 0x07)
}

// bottomLineLocked is the persistent bottom-row line a window with a
// screen needs: while it reconnects, or once it ended.
func (m *Mux) bottomLineLocked(w *Window) string {
	idx := m.indexLocked(w)
	switch {
	case w.phase == phaseReconnecting:
		return fmt.Sprintf(" window %d reconnecting… (%s)", idx, w.reason)
	case w.phase == phaseDead && w.resumable:
		return fmt.Sprintf(" window %d %s — r resume, & close", idx, w.reason)
	case w.phase == phaseDead:
		return fmt.Sprintf(" window %d ended: %s — r relaunch, & close", idx, w.reason)
	}
	return ""
}

// overlayLineLocked is what the active window's bottom row shows over its
// screen: the open prompt, else the notice, else the window's own line.
func (m *Mux) overlayLineLocked(w *Window) string {
	switch {
	case m.promptState != nil:
		return " " + m.promptState.label + string(m.promptState.text)
	case m.notice != "":
		return " " + m.notice
	}
	return m.bottomLineLocked(w)
}

// plainOverlay draws line on the bottom row of a screen luxmux drew itself.
func (m *Mux) plainOverlay(b []byte, line string) []byte {
	b = screen.CUP(b, m.rows-1, 0)
	b = append(b, "\x1b[0;7m"...)
	b = append(b, screen.Fit(line, m.cols-1)...)
	return append(b, "\x1b[m"...)
}

// bottomRow draws line on the bottom row over a screen luxmux has no copy
// of (a stale window's), or erases that row when line is empty, putting
// the cursor, pen and origin mode back as the program had them (DECSC and
// DECRC save and restore all three).
func (m *Mux) bottomRow(b []byte, line string) []byte {
	b = append(b, "\x1b7\x1b[?6l"...)
	if line == "" {
		b = screen.CUP(b, m.rows-1, 0)
		b = append(b, "\x1b[m\x1b[2K"...)
	} else {
		b = m.plainOverlay(b, line)
	}
	return append(b, "\x1b8"...)
}

// setNoticeLocked shows a transient notice on the bottom row; after
// NoticeFor the window is repainted without it.
func (m *Mux) setNoticeLocked(text string) {
	m.notice = text
	m.noticeGen++
	gen := m.noticeGen
	m.drawOverlayLocked()
	time.AfterFunc(m.opt.NoticeFor, func() {
		defer m.recoverPanic()
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.noticeGen == gen {
			m.notice = ""
			m.overlayGoneLocked()
		}
	})
}

// Notice shows a transient notice.
func (m *Mux) Notice(text string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.setNoticeLocked(text)
}

// drawOverlayLocked draws the active window's bottom-row line (the
// prompt, the notice, or its own) over its screen. Taking an overlay down
// repaints the whole window: live output may have scrolled the overlay up
// on the terminal since it was drawn. A stale window (see feedLimit) has
// no screen to repaint from: there the bottom row is drawn or erased in
// place, and the rest of the terminal, which has all of the window's
// output, is left as it is; a ghost of an overlay that output scrolled up
// stays until the window is next switched to.
func (m *Mux) drawOverlayLocked() {
	w := m.activeLocked()
	if w == nil || m.suspended || m.help {
		return
	}
	line := m.overlayLineLocked(w)
	if line == "" {
		m.overlayGoneLocked()
		return
	}
	if !w.hasScreen {
		m.tw.enqueue(m.plainOverlay(nil, line))
		return
	}
	pend := w.tail.Pending()
	m.paintLocked(w, paintOverlay, line, func(b []byte, t *vtermT) []byte {
		rows, _ := t.Size()
		b = screen.Overlay(b, t, rows-1, line)
		return append(b, pend...)
	})
}

// helpLines is the key help overlay.
func (m *Mux) helpLines() []string {
	p := m.opt.Config.Prefix
	l := []string{
		" luxmux keys (prefix " + p + ", then):", "",
		"  c      new window (default profile)",
		"  C      new window (dialog)",
		"  n p    next / previous window",
		"  0-9    select window",
		"  l      last window",
		"  '      select window by index",
		"  w      window chooser",
		"  ,      rename window",
		"  &      kill window",
		"  r      resume a stopped Run / relaunch an ended one",
		"  d      detach (the Runs keep running)",
		"  ?      this help",
		"  " + p + "    send " + p + " to the shell",
	}
	if len(m.opt.Config.Keys.Direct) > 0 {
		l = append(l, "", " without the prefix:")
		for k, v := range m.opt.Config.Keys.Direct {
			l = append(l, fmt.Sprintf("  %-6s %s", k, v))
		}
	}
	return append(l, "", " press any key to return")
}

// prompt is a one-line question on the bottom row, answered by typing.
type prompt struct {
	label  string
	text   []rune
	single bool // answered by one key (y/N)
	done   func(answer string, ok bool)
}

// ask opens a prompt; done runs (with m.mu held) when it is answered or
// cancelled.
func (m *Mux) askLocked(label string, initial string, single bool, done func(string, bool)) {
	m.promptState = &prompt{label: label, text: []rune(initial), single: single, done: done}
	if w := m.activeLocked(); w != nil && !m.suspended && !m.help && !w.keyModes.Modes().Zero() {
		// The prompt reads plain keys; closing it repaints the window,
		// which sets its keyboard modes again.
		m.tw.enqueue(screen.ResetKeysHere(nil))
	}
	m.drawOverlayLocked()
}

// promptKeys feeds typed bytes to the open prompt; it returns what is
// left after the prompt closed.
func (m *Mux) promptKeysLocked(b []byte) []byte {
	p := m.promptState
	for i := 0; i < len(b); i++ {
		c := b[i]
		if p.single {
			m.promptState = nil
			m.clearPromptLocked()
			p.done(string(rune(c)), c == 'y' || c == 'Y')
			return b[i+1:]
		}
		switch {
		case c == '\r' || c == '\n':
			m.promptState = nil
			m.clearPromptLocked()
			p.done(string(p.text), true)
			return b[i+1:]
		case c == 0x1b || c == 0x03 || c == 0x07:
			// Escape (and any sequence it starts in this read), C-c, C-g.
			m.promptState = nil
			m.clearPromptLocked()
			p.done("", false)
			if c == 0x1b {
				return nil
			}
			return b[i+1:]
		case c == 0x7f || c == 0x08:
			if len(p.text) > 0 {
				p.text = p.text[:len(p.text)-1]
			}
		case c == 0x15: // C-u
			p.text = p.text[:0]
		case c >= 0x20:
			r, n := decodeRune(b[i:])
			p.text = append(p.text, r)
			i += n - 1
		}
	}
	m.drawOverlayLocked()
	return nil
}

func (m *Mux) clearPromptLocked() { m.overlayGoneLocked() }

func decodeRune(b []byte) (rune, int) {
	s := string(b)
	for _, r := range s {
		n := len(string(r))
		return r, n
	}
	return 0, 1
}

// stateLine is a Run's state and stage in words.
func stateLine(state, stage, reason string) string {
	s := state
	if w, ok := stageWords[stage]; ok && stage != state {
		s += ", " + w
	}
	if reason != "" {
		s += ": " + strings.TrimSpace(reason)
	}
	return s
}
