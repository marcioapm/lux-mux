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

// feedLimit bounds the output a window's emulator may lag behind its
// stream. The backpressure policy: the stream reader never waits for the
// emulator. When a window's queued output passes feedLimit (the emulator
// parses slower than the Run writes, as with long runs of CJK, combining
// marks or truecolour), the queued output is dropped, the emulator reset
// and the window marked stale; a stale emulator takes no more output.
// When a stale window is next switched to or previewed, or the shown one
// is drawn again in full (a dialog closed, the terminal caught
// up), it is attached again with restore (at most once per refreshEvery)
// and lux's snapshot rebuilds the emulator; a notice or prompt over the
// shown window only redraws its bottom row, since the terminal has its
// output already (see paintKind). The shown window's output goes to the
// terminal unchanged throughout.
//
// The limit trades switch latency against refreshes: a switch to a
// window waits for its emulator to parse what is queued, at most
// feedLimit. At 8 MiB that is up to 0.48 s for `yes` at 200×500 (vterm
// parses it at about 17.6 MB/s), 0.16 s for CJK (53 MB/s), a few ms for
// a 50×200 build log; a smaller limit makes windows go stale, and pay a
// restore snapshot of up to 4 MiB, more often.
const feedLimit = 8 << 20

// feedItem is one thing for a window's emulator, in stream order: output,
// a resize, a reset to a blank emulator, or a paint run with the
// emulator caught up to everything queued before it.
type feedItem struct {
	data       []byte
	rows, cols int
	reset      bool
	paint      func(*vterm.Terminal)
	// skip runs instead of paint when the paint is dropped: the queue
	// overflowed (stale true) or closed.
	skip func(stale bool)
}

// feedQueue is a window's emulator input. push never blocks.
type feedQueue struct {
	mu     sync.Mutex
	items  []feedItem
	bytes  int
	stale  bool
	closed bool
	wake   chan struct{}
}

// push queues it. Output over feedLimit drops the queued output and
// marks the queue stale (see feedLimit); it reports that.
func (q *feedQueue) push(it feedItem) (overflowed bool) {
	var skips []func(bool)
	q.mu.Lock()
	switch {
	case q.closed:
		if it.skip != nil {
			it.skip(false)
		}
	case it.data != nil && q.stale:
	case it.data != nil && q.bytes+len(it.data) > feedLimit:
		kept := q.items[:0]
		for _, x := range q.items {
			switch {
			case x.paint != nil:
				if x.skip != nil {
					skips = append(skips, x.skip)
				}
			case x.rows > 0:
				kept = append(kept, x)
			}
		}
		clear(q.items[len(kept):])
		q.items = append(kept, feedItem{reset: true})
		q.bytes = 0
		q.stale = true
		overflowed = true
	default:
		q.items = append(q.items, it)
		q.bytes += len(it.data)
	}
	q.mu.Unlock()
	q.signal()
	for _, f := range skips {
		f(true)
	}
	return overflowed
}

// restart clears the stale mark and queues a reset: a new stream starts.
func (q *feedQueue) restart() {
	q.mu.Lock()
	q.stale = false
	q.mu.Unlock()
	q.push(feedItem{reset: true})
}

func (q *feedQueue) isStale() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.stale
}

func (q *feedQueue) signal() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// close drops what is queued (its paints skipped) and ends the feeder.
func (q *feedQueue) close() {
	q.mu.Lock()
	items := q.items
	q.items, q.bytes, q.closed = nil, 0, true
	q.mu.Unlock()
	q.signal()
	for _, x := range items {
		if x.skip != nil {
			x.skip(false)
		}
	}
}

// pop waits for the next item; ok is false once the queue is closed.
func (q *feedQueue) pop() (feedItem, bool) {
	for {
		q.mu.Lock()
		if q.closed {
			q.mu.Unlock()
			return feedItem{}, false
		}
		if len(q.items) > 0 {
			it := q.items[0]
			q.items[0] = feedItem{}
			q.items = q.items[1:]
			q.bytes -= len(it.data)
			q.mu.Unlock()
			return it, true
		}
		q.mu.Unlock()
		<-q.wake
	}
}

// Window is one window: a Run, its emulator and its supervisor.
type Window struct {
	m *Mux
	// Endpoint is the lux the window's Runs live on; set when the window
	// is made and never changed, so it is read without a lock.
	Endpoint config.Endpoint

	// Guarded by m.mu.
	RunID   string
	Name    string
	Profile string
	Spec    config.Profile
	// SpareKey is the warm-spare slot this window's Run may come from
	// (empty: none).
	SpareKey string
	phase    phase
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
	// refreshing: the stream was closed so a new attach with restore
	// rebuilds a stale emulator. lastAttach: when the stream was last
	// opened; refreshDue: a refresh is put off until refreshEvery after it.
	refreshing bool
	refreshDue bool
	lastAttach time.Time
	// resumable: the Run is stopped and r resumes it; resumeAsked: r
	// was pressed, for the supervisor.
	resumable   bool
	resumeAsked bool
	tail        screen.Tail
	// keyModes follows the keyboard modes the program set (vterm keeps
	// none of them), fed the same output as tail by the stream's reader
	// under kmMu, off m.mu. keys and altScreen are its state as of the
	// output queued so far, published under m.mu with that output.
	kmMu      sync.Mutex
	keyModes  screen.KeyTracker
	keys      screen.KeyModes
	altScreen bool
	epoch     int
	cancel    context.CancelFunc
	closed    bool

	// connMu guards the stream and the window's input queue. in holds the
	// keystrokes typed for the window and not yet written, in order: the
	// input loop appends (send); the window's key writer alone writes
	// them and removes bytes only once their write succeeded. inFlight is
	// how many bytes at its head are being written (trimming spares
	// them); inGen changes whenever the queue is discarded, so a write in
	// flight then removes nothing. inEpoch is the Run epoch of the last
	// stream made live, the shell every queued key was typed for (0:
	// before any stream in this process). connUp: conn delivered its
	// first frame and no write to it failed; keys are written only then.
	// closing: the stream luxmux is closing for a refresh. inHold: that
	// stream ended and the queue is held whole (bounded as if live) for
	// the next one. connCut ends conn at once: its reader and its network
	// connection.
	connMu   sync.Mutex
	conn     *websocket.Conn
	connUp   bool
	closing  *websocket.Conn
	inHold   bool
	connCut  func()
	in       []byte
	inFlight int
	inGen    int
	inEpoch  int
	inWake   chan struct{}
	inStop   chan struct{}
	// inRoom wakes an input loop waiting in send for the queue to drain
	// or the stream to change.
	inRoom chan struct{}

	// The emulator is written by its feeder goroutine and read by
	// previews, both under emuMu.
	emuMu sync.Mutex
	emu   *vterm.Terminal
	q     *feedQueue

	// preview caches the chooser's rendering between its ticks.
	prevMu   sync.Mutex
	prevAt   time.Time
	prevSize [2]int
	prev     []string

	notify chan struct{}
}

func (m *Mux) newWindow(name, profile string, spec config.Profile, ep config.Endpoint) *Window {
	w := &Window{m: m, Endpoint: ep, Name: name, Profile: profile, Spec: spec, created: time.Now(),
		emu:    newEmulator(m.rows, m.cols),
		q:      &feedQueue{wake: make(chan struct{}, 1)},
		notify: make(chan struct{}, 1),
		inWake: make(chan struct{}, 1),
		inRoom: make(chan struct{}, 1),
		inStop: make(chan struct{})}
	m.goSafe(w.feeder)
	m.goSafe(func() { m.keyWriter(w) })
	return w
}

// newEmulator keeps no history: luxmux reads only the screen.
func newEmulator(rows, cols int) *vterm.Terminal { return vterm.New(rows, cols, 0) }

// feeder writes the window's output into its emulator, apart from the
// path that writes it to the terminal.
func (w *Window) feeder() {
	for {
		it, ok := w.q.pop()
		if !ok {
			return
		}
		w.emuMu.Lock()
		switch {
		case it.reset:
			r, c := w.emu.Size()
			w.emu = newEmulator(r, c)
		case it.rows > 0:
			w.emu.Resize(it.rows, it.cols)
		case it.paint != nil:
			it.paint(w.emu)
		case len(it.data) > 0:
			w.emu.Write(it.data)
		}
		w.emuMu.Unlock()
	}
}

// withEmu runs fn with the emulator once it has taken everything queued
// before; false if the paint was dropped. It waits: never call it with
// m.mu held.
func (w *Window) withEmu(fn func(*vterm.Terminal)) bool {
	done := make(chan bool, 1)
	w.q.push(feedItem{paint: func(t *vterm.Terminal) { fn(t); done <- true }, skip: func(bool) { done <- false }})
	return <-done
}

// poke wakes the supervisor.
func (w *Window) poke() {
	select {
	case w.notify <- struct{}{}:
	default:
	}
}

// trackKeys feeds output to the key-mode tracker; it reports whether the
// modes or the screen may have changed (publishKeysLocked is then due).
// Only the window's stream reader calls it, in stream order.
func (w *Window) trackKeys(data []byte) bool {
	w.kmMu.Lock()
	defer w.kmMu.Unlock()
	return w.keyModes.Write(data)
}

// publishKeysLocked makes the tracker's state the one paints use.
func (w *Window) publishKeysLocked() {
	w.kmMu.Lock()
	defer w.kmMu.Unlock()
	w.keys = w.keyModes.Modes()
	w.altScreen = w.keyModes.AltScreen()
}

// resetKeysLocked forgets every key mode: a new program.
func (w *Window) resetKeysLocked() {
	w.kmMu.Lock()
	w.keyModes.Reset()
	w.kmMu.Unlock()
	w.keys, w.altScreen = screen.KeyModes{}, false
}

func (w *Window) getConn() *websocket.Conn {
	w.connMu.Lock()
	defer w.connMu.Unlock()
	return w.conn
}
