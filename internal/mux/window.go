package mux

import (
	"context"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/marcioapm/lux/vterm"

	"github.com/marcioapm/lux-mux/internal/config"
	"github.com/marcioapm/lux-mux/internal/screen"
)

// HistoryLines is how much scrollback each window's emulator keeps, as
// lux-shim's restore does.
const HistoryLines = 2000

// phase is where a window is in following its Run.
type phase int

const (
	// phaseStarting: never attached in this process; the window shows
	// the Run's progress.
	phaseStarting phase = iota
	// phaseLive: attached; output flows.
	phaseLive
	// phaseReconnecting: was attached; the stream is gone for now.
	phaseReconnecting
	// phaseDead: the Run ended for good (or could not be made); the
	// window offers r (relaunch) and & (close).
	phaseDead
)

// feedItem is one thing for a window's emulator, in stream order: output,
// a resize, a reset to a blank emulator, or a barrier the painter waits on.
type feedItem struct {
	data       []byte
	rows, cols int
	reset      bool
	done       chan struct{}
}

// Window is one window: a Run, its emulator and its supervisor.
type Window struct {
	m *Mux

	// Guarded by m.mu.
	RunID   string
	Name    string
	Profile string
	Spec    config.Profile
	phase   phase
	// status: the Run's latest state and stage in words; reason: why it
	// is dead or what the reconnect waits on.
	status    string
	reason    string
	steps     progress
	created   time.Time
	runState  string
	hasScreen bool // the emulator holds a screen of this Run
	// attachedOnce: attached in this process (its restore went to the
	// terminal, or it was not shown then).
	attachedOnce bool
	tail         screen.Tail
	epoch        int
	cancel       context.CancelFunc
	closed       bool

	connMu sync.Mutex
	conn   *websocket.Conn

	// The emulator is written only by its feeder goroutine, and read by
	// painters after a barrier, both under emuMu.
	emuMu sync.Mutex
	emu   *vterm.Terminal
	feed  chan feedItem

	notify chan struct{}
}

func (m *Mux) newWindow(name, profile string, spec config.Profile) *Window {
	w := &Window{m: m, Name: name, Profile: profile, Spec: spec, created: time.Now(),
		emu:    vterm.New(m.rows, m.cols, HistoryLines),
		feed:   make(chan feedItem, 256),
		notify: make(chan struct{}, 1)}
	go w.feeder()
	return w
}

// feeder writes the window's output into its emulator, apart from the
// path that writes it to the terminal.
func (w *Window) feeder() {
	for it := range w.feed {
		w.emuMu.Lock()
		switch {
		case it.reset:
			r, c := w.emu.Size()
			w.emu = vterm.New(r, c, HistoryLines)
		case it.rows > 0:
			w.emu.Resize(it.rows, it.cols)
		case len(it.data) > 0:
			w.emu.Write(it.data)
		}
		w.emuMu.Unlock()
		if it.done != nil {
			close(it.done)
		}
	}
}

// sync waits until the emulator has taken everything queued before it.
// Callers hold m.mu, so nothing is queued meanwhile.
func (w *Window) sync() {
	done := make(chan struct{})
	w.feed <- feedItem{done: done}
	<-done
}

// withEmu runs fn with the emulator after a barrier. Called with m.mu held.
func (w *Window) withEmu(fn func(*vterm.Terminal)) {
	w.sync()
	w.emuMu.Lock()
	defer w.emuMu.Unlock()
	fn(w.emu)
}

// poke wakes the supervisor.
func (w *Window) poke() {
	select {
	case w.notify <- struct{}{}:
	default:
	}
}

func (w *Window) setConn(c *websocket.Conn) {
	w.connMu.Lock()
	w.conn = c
	w.connMu.Unlock()
}

func (w *Window) getConn() *websocket.Conn {
	w.connMu.Lock()
	defer w.connMu.Unlock()
	return w.conn
}
