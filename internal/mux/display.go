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
	switch w.phase {
	case phaseDead:
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
	switch w.phase {
	case phaseReconnecting:
		return fmt.Sprintf(" window %d reconnecting… (%s)", idx, w.reason)
	case phaseDead:
		return fmt.Sprintf(" window %d ended: %s — r relaunch, & close", idx, w.reason)
	}
	return ""
}

// overlayBytes appends the active window's bottom row overwritten by line.
func (m *Mux) overlayBytes(b []byte, line string) []byte {
	w := m.activeLocked()
	w.withEmu(func(t *vtermT) {
		rows, _ := t.Size()
		b = screen.Overlay(b, t, rows-1, line)
	})
	return b
}

// setNoticeLocked shows a transient notice on the bottom row; it goes
// after NoticeFor, the row repainted from the emulator.
func (m *Mux) setNoticeLocked(text string) {
	m.notice = text
	m.noticeGen++
	gen := m.noticeGen
	m.drawNoticeLocked()
	time.AfterFunc(m.opt.NoticeFor, func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.noticeGen == gen {
			m.clearNoticeLocked()
		}
	})
}

// Notice shows a transient notice.
func (m *Mux) Notice(text string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.setNoticeLocked(text)
}

func (m *Mux) drawNoticeLocked() {
	w := m.activeLocked()
	if w == nil || m.suspended || m.notice == "" {
		return
	}
	b := m.buf[:0]
	if w.hasScreen {
		b = m.overlayBytes(b, " "+m.notice)
		b = append(b, w.tail.Pending()...)
	} else {
		b = screen.CUP(b, m.rows-1, 0)
		b = append(b, "\x1b[0;7m"...)
		b = append(b, screen.Fit(" "+m.notice, m.cols-1)...)
		b = append(b, "\x1b[m"...)
	}
	m.write(b)
	m.buf = b
}

// clearNoticeLocked takes the notice down: the bottom row is repainted
// from the emulator (or the status screen redrawn).
func (m *Mux) clearNoticeLocked() {
	m.notice = ""
	w := m.activeLocked()
	if w == nil || m.suspended {
		return
	}
	if !w.hasScreen {
		m.repaintLocked()
		return
	}
	b := m.buf[:0]
	w.withEmu(func(t *vtermT) {
		rows, _ := t.Size()
		if line := m.bottomLineLocked(w); line != "" {
			b = screen.Overlay(b, t, rows-1, line)
		} else {
			b = screen.Restore(b, t, rows-1)
		}
	})
	b = append(b, w.tail.Pending()...)
	m.write(b)
	m.buf = b
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
		"  r      relaunch an ended window",
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

func (m *Mux) drawPromptLocked() {
	p := m.promptState
	w := m.activeLocked()
	if p == nil || w == nil || m.suspended {
		return
	}
	line := " " + p.label + string(p.text)
	b := m.buf[:0]
	if w.hasScreen {
		w.withEmu(func(t *vtermT) {
			rows, _ := t.Size()
			b = screen.Overlay(b, t, rows-1, line)
		})
	} else {
		b = screen.CUP(b, m.rows-1, 0)
		b = append(b, "\x1b[0;7m"...)
		b = append(b, screen.Fit(line, m.cols-1)...)
		b = append(b, "\x1b[m"...)
	}
	m.write(b)
	m.buf = b
}

// ask opens a prompt; done runs (with m.mu held) when it is answered or
// cancelled.
func (m *Mux) askLocked(label string, initial string, single bool, done func(string, bool)) {
	m.promptState = &prompt{label: label, text: []rune(initial), single: single, done: done}
	m.drawPromptLocked()
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
	m.drawPromptLocked()
	return nil
}

func (m *Mux) clearPromptLocked() {
	if m.notice != "" {
		m.drawNoticeLocked()
		return
	}
	m.clearNoticeLocked()
}

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
