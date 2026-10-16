// Package mux is luxmux's multiplexer: windows that are shells on lux
// Runs, one shown at a time, the raw passthrough of its output, and the
// keys, prompts and repaints around it.
package mux

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/marcioapm/lux/api"
	"github.com/marcioapm/lux/vterm"

	"github.com/marcioapm/lux-mux/internal/config"
	"github.com/marcioapm/lux-mux/internal/keys"
	"github.com/marcioapm/lux-mux/internal/luxapi"
	"github.com/marcioapm/lux-mux/internal/screen"
	"github.com/marcioapm/lux-mux/internal/state"
)

// UI runs the full-screen dialogs (the chooser, the new-window dialog and
// the key help). They own the terminal while open. Nil: those keys show a
// notice instead.
type UI interface {
	Choose(m *Mux) (ChooseResult, error)
	NewDialog(m *Mux) (*NewWindowRequest, error)
	Help(m *Mux) error
	// ChooseSession runs the session chooser. Its renames and kills
	// happen through the Mux as they are made.
	ChooseSession(m *Mux) (SessionChoice, error)
}

// Options configure a Mux.
type Options struct {
	Session string
	// Label is the luxmux-session label of the session's Runs: its name,
	// unless it was renamed (sessions.Label). Empty: Session.
	Label string
	// Unlock releases the session's lock (nil: none is held).
	Unlock func()
	// Console is the terminal and keyboard, shared by the sessions one
	// process shows in turn. Nil: New makes one of In and Out, closed
	// when the session ends.
	Console *Console
	// Notice is shown once the session is on the terminal.
	Notice string
	Config *config.Config
	// Endpoints makes the client of each window's lux.
	Endpoints *luxapi.Endpoints
	StateDir  string
	// In is the keyboard (raw mode is the caller's), Out the terminal.
	In  io.Reader
	Out io.Writer
	// Size is the terminal's rows and columns.
	Size func() (rows, cols int)
	UI   UI
	// First is the profile of the first window of a session that has
	// none (nil: its default profile), already checked by lux's dry run.
	First *config.Profile
	// Lookup finds secret values in the environment (os.LookupEnv).
	Lookup func(string) (string, bool)
	// Backoff is the first retry delay (default 250ms); retries double up
	// to MaxBackoff (default 10s).
	Backoff, MaxBackoff time.Duration
	// NoticeFor is how long a notice stays (default 2s).
	NoticeFor time.Duration
	// RestoreWait is how long an attach waits for a restore snapshot
	// before taking the host for one without restore (default 1s).
	RestoreWait time.Duration
	// Poll is how often a supervisor re-reads its Run while it waits
	// for events (default 5s).
	Poll time.Duration
	// Crash reports a panic after the terminal was restored and does not
	// return (default: print it and exit with status 2).
	Crash func(msg string)
	// streamStable: how long a stream must stay up for the backoff after
	// lux or the network ends one to start again from Backoff (default
	// 10s). Unexported: only tests change it.
	streamStable time.Duration
	// discoverEvery: how often the session's Runs are listed for windows
	// new-window made while the feed was down (default 30s). Unexported:
	// only tests change it.
	discoverEvery time.Duration
	// switchOpenWait bounds opening a switch's target (default
	// switchOpenWait). Unexported: only tests change it.
	switchOpenWait time.Duration
	// afterSwitchUnlock, when set, runs once a switch released this
	// session's lock and before it takes the target's. Only tests set it.
	afterSwitchUnlock func()
	// spareDetachWait: how long a detach waits for spare submits in
	// flight (default spareDetachWait). Unexported: only tests change it.
	spareDetachWait time.Duration
	// writeKeys writes one message of keystrokes to a stream (default
	// wsjson.Write). Unexported: only tests change it.
	writeKeys func(ctx context.Context, c *websocket.Conn, d api.StreamData) error
}

// Mux is one attached session.
type Mux struct {
	opt  Options
	eps  *luxapi.Endpoints
	lux  *luxapi.Client
	keys *keys.Machine
	in   *input
	// tw is the only writer of the terminal (opt.Out).
	tw      *termWriter
	console *Console
	// label is the luxmux-session label of the session's Runs, and of
	// those it makes: a rename keeps it.
	label string

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	// spareCtx bounds spare submits: a detach does not cancel them (lux
	// may make the Run, and its id must be recorded) until it has waited
	// spareDetachWait for them; a switch cancels them at once.
	spareCtx   context.Context
	stopSpares context.CancelFunc
	// inputDone closes when the input loop returned: the next session's
	// loop must not start while this one can still take a key.
	inputDone chan struct{}

	// mu guards the windows, the order of what is queued for the terminal
	// and the session state. Nothing waits on the terminal, a stream or an
	// emulator while holding it.
	mu sync.Mutex
	// name is the session's name (a rename changes it); unlock releases
	// the lock of that name.
	name      string
	unlock    func()
	next      *switchTarget
	windows   []*Window
	active    int
	last      int
	rows      int
	cols      int
	suspended bool
	onResize  func(rows, cols int)
	sess      *state.Session
	notice    string
	noticeGen int
	// considered are Run ids no window has that were looked at for
	// adoption (considerRun), and those of windows closed or relaunched,
	// which are never adopted back.
	considered map[string]bool
	// consideredOn is the endpoint each considered id was seen on, so a
	// listing of one endpoint prunes only its own.
	consideredOn map[string]config.Endpoint
	byRun        map[string]*Window
	// feeds are the endpoints whose event stream is followed: one each.
	feeds map[config.Endpoint]bool
	// listed is closed for an endpoint once its first listing of the
	// session was tried; a spare is submitted there only after it, so a
	// spare an earlier attach left unrecorded is found first.
	listed map[config.Endpoint]chan struct{}
	// promptState is the open prompt.
	promptState *prompt
	// shown is the window whose screen the terminal shows, painted and
	// followed by its live output since (nil: luxmux's own screen, or a
	// window with none). Dialogs drawn over it leave it set: closing them
	// redraws it.
	shown *Window
	// ending: the last window closed; spares submitted from now on are
	// terminated, not recorded.
	ending    bool
	done      chan string // the reason Run returns
	crashOnce sync.Once
	// preview follows another session's shown window for the session
	// chooser.
	preview sessionPreview
	// stepping: a C-b ( or ) is listing the sessions.
	stepping atomic.Bool
}

// ErrNoSession is returned by Open for attach to a session lux has no
// Runs of and no file for.
var ErrNoSession = errors.New("no such session")

func (o *Options) defaults() {
	if o.Backoff == 0 {
		o.Backoff = 250 * time.Millisecond
	}
	if o.MaxBackoff == 0 {
		o.MaxBackoff = 10 * time.Second
	}
	if o.NoticeFor == 0 {
		o.NoticeFor = 2 * time.Second
	}
	if o.RestoreWait == 0 {
		o.RestoreWait = time.Second
	}
	if o.Poll == 0 {
		o.Poll = 5 * time.Second
	}
	if o.Lookup == nil {
		o.Lookup = os.LookupEnv
	}
	if o.Crash == nil {
		o.Crash = defaultCrash
	}
	if o.streamStable == 0 {
		o.streamStable = 10 * time.Second
	}
	if o.discoverEvery == 0 {
		o.discoverEvery = 30 * time.Second
	}
	if o.switchOpenWait == 0 {
		o.switchOpenWait = switchOpenWait
	}
	if o.spareDetachWait == 0 {
		o.spareDetachWait = spareDetachWait
	}
	if o.writeKeys == nil {
		o.writeKeys = func(ctx context.Context, c *websocket.Conn, d api.StreamData) error {
			return wsjson.Write(ctx, c, d)
		}
	}
}

// New makes a Mux for the session in sess (loaded or rebuilt by the
// caller); Run starts it.
func New(opt Options, sess *state.Session) (*Mux, error) {
	opt.defaults()
	pfx, err := config.ParsePrefix(opt.Config.Prefix)
	if err != nil {
		return nil, err
	}
	km, err := keys.New(pfx, opt.Config.Keys.Direct)
	if err != nil {
		return nil, err
	}
	con := opt.Console
	if con == nil {
		con = NewConsole(opt.In, opt.Out)
	}
	if opt.Label == "" {
		opt.Label = opt.Session
	}
	eps := opt.Endpoints
	m := &Mux{opt: opt, eps: eps, keys: km, tw: con.tw, in: con.in, console: con, label: opt.Label,
		name: opt.Session, unlock: opt.Unlock, inputDone: make(chan struct{}),
		sess: sess, byRun: map[string]*Window{}, considered: map[string]bool{}, consideredOn: map[string]config.Endpoint{},
		feeds: map[config.Endpoint]bool{}, listed: map[config.Endpoint]chan struct{}{}, done: make(chan string, 1)}
	m.lux, _ = eps.Client(eps.Default())
	m.rows, m.cols = clampSize(opt.Size())
	m.ctx, m.cancel = context.WithCancel(context.Background())
	m.spareCtx, m.stopSpares = context.WithCancel(context.Background())
	def := eps.Default()
	// A slot reserved with no id is a submit a detach stopped waiting for:
	// it is free, and the Run lux may have made is found by its labels
	// (adoptSpares).
	for slot, id := range sess.Spares {
		if id == "" {
			delete(sess.Spares, slot)
			delete(sess.SpareEndpoints, slot)
		}
	}
	for _, sw := range sess.Windows {
		spec := opt.Config.Profiles[sw.Profile]
		if sw.Spec != nil {
			spec = *sw.Spec
		}
		w := m.newWindow(sw.Name, sw.Profile, spec, sw.EndpointOr(def))
		w.RunID = sw.RunID
		m.windows = append(m.windows, w)
		m.byRun[w.RunID] = w
	}
	m.active = clamp(sess.Active, len(m.windows))
	m.last = clamp(sess.Last, len(m.windows))
	// A slot reserved with no id is a submit a switch cancelled: it is
	// free (sessions.Open records the Run lux may have made in it).
	for key, id := range sess.Spares {
		if id == "" {
			delete(sess.Spares, key)
		}
	}
	return m, nil
}

// clampSize bounds a terminal size to what lux attaches with and its
// emulators keep: at most MaxRows×MaxCols (luxd refuses larger). A larger
// terminal shows windows in its top-left corner.
func clampSize(rows, cols int) (int, int) {
	return min(max(rows, 1), vterm.MaxRows), min(max(cols, 1), vterm.MaxCols)
}

func clamp(i, n int) int {
	if i < 0 || i >= n {
		return 0
	}
	return i
}

// Session is the session's name.
func (m *Mux) Session() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.name
}

// Config is the configuration.
func (m *Mux) Config() *config.Config { return m.config() }

// DefaultProfile is the profile C-b c uses: the session's (luxmux new -p,
// or the dialog's checkbox) while the configuration still has it, else the
// configuration's default_profile.
func (m *Mux) DefaultProfile() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.opt.Config.Profiles[m.sess.DefaultProfile]; ok {
		return m.sess.DefaultProfile
	}
	return m.opt.Config.DefaultProfile
}

// Endpoint is the lux a window of profile p is made on.
func (m *Mux) Endpoint(p config.Profile) config.Endpoint { return m.eps.Of(p) }

// DefaultEndpoint is the lux CLI configuration's lux.
func (m *Mux) DefaultEndpoint() config.Endpoint { return m.eps.Default() }

// Pools are the pools a window of profile p may use, on p's lux.
func (m *Mux) Pools(ctx context.Context, p config.Profile) ([]api.Pool, error) {
	c, err := m.eps.Client(m.eps.Of(p))
	if err != nil {
		return nil, err
	}
	return c.Pools(ctx)
}

// clientOf is ep's client.
func (m *Mux) clientOf(ep config.Endpoint) (*luxapi.Client, error) { return m.eps.Client(ep) }

// Terminal is where dialogs draw, in order with everything else written
// to the terminal, and its size.
func (m *Mux) Terminal() (io.Writer, int, int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.tw, m.rows, m.cols
}

// OnResize registers the open dialog's resize handler; nil clears it.
func (m *Mux) OnResize(fn func(rows, cols int)) {
	m.mu.Lock()
	m.onResize = fn
	m.mu.Unlock()
}

// Context ends when the session does.
func (m *Mux) Context() context.Context { return m.ctx }

// Run shows the session until it is detached, its last window closes,
// another session is switched to, or ctx ends. It returns why:
// "detached", "exited", "switch" or ctx's error.
func (m *Mux) Run(ctx context.Context) (string, error) {
	m.console.cur.Store(m)
	m.tw.setCaughtUp(m.terminalCaughtUp)
	m.goSafe(func() {
		select {
		case <-ctx.Done():
			m.finish("")
		case <-m.ctx.Done():
		}
	})
	m.mu.Lock()
	if len(m.windows) == 0 {
		cfg := m.opt.Config
		m.mu.Unlock()
		name := m.DefaultProfile()
		p := cfg.Profiles[name]
		if m.opt.First != nil {
			p = *m.opt.First
		}
		m.newWindowFrom(name, p, "", false)
		m.mu.Lock()
	}
	for _, w := range m.windows {
		if w.RunID != "" && w.cancel == nil {
			m.startSupervisor(w)
		}
	}
	// Every lux the configured profiles name, the session's windows live
	// on, or the rebuild could not list: one event stream and listing each.
	eps := m.opt.Config.Endpoints(m.eps.Base())
	for _, w := range m.windows {
		eps = append(eps, w.Endpoint)
	}
	for _, ep := range m.sess.Unlisted {
		eps = append(eps, ep)
		m.setNoticeLocked("lux " + ep.Label() + ": endpoint unreachable; its windows are added when it answers")
	}
	for _, ep := range eps {
		m.followEndpointLocked(ep)
	}
	m.repaintLocked()
	m.saveLocked()
	if m.opt.Notice != "" {
		m.setNoticeLocked(m.opt.Notice)
	}
	m.mu.Unlock()
	m.wg.Add(1)
	m.goSafe(m.keepSpares)
	m.goSafe(func() {
		defer close(m.inputDone)
		m.inputLoop()
	})
	<-m.ctx.Done()
	reason := <-m.done
	m.shutdown(reason == reasonSwitch)
	if reason == "" && ctx.Err() != nil {
		return "", ctx.Err()
	}
	return reason, nil
}

// finish ends Run with reason (first caller wins).
func (m *Mux) finish(reason string) {
	select {
	case m.done <- reason:
	default:
	}
	m.cancel()
}

// spareDetachWait is how long a detach waits for spare submits in
// flight; a lux that does not answer would otherwise hold it for the
// submit's 30 s.
const spareDetachWait = 5 * time.Second

// shutdown closes every stream, the event feeds and the session preview,
// and waits for the session's goroutines, the input loop's included; the
// Runs keep running and the state is saved. Spare submits in flight are
// waited for up to spareDetachWait; for a switch they are cancelled
// rather than waited for. A Console the session made is closed; a shared
// one is left to the next session, its terminal reset.
func (m *Mux) shutdown(switching bool) {
	m.mu.Lock()
	ws := append([]*Window{}, m.windows...)
	m.saveLocked()
	m.suspended = true
	m.tw.enqueue(screen.Reset(nil))
	m.mu.Unlock()
	if switching {
		m.stopSpares()
	}
	for _, w := range ws {
		if c := w.getConn(); c != nil {
			c.CloseNow()
		}
	}
	// The input loop first: an open session chooser may still start a
	// preview until it returns.
	<-m.inputDone
	m.stopSessionPreview()
	capWait := time.AfterFunc(m.opt.spareDetachWait, m.stopSpares)
	m.wg.Wait()
	capWait.Stop()
	m.stopSpares()
	m.mu.Lock()
	for _, w := range ws {
		m.closeFeedLocked(w)
	}
	m.mu.Unlock()
	if m.opt.Console == nil {
		m.tw.close(2 * time.Second)
	}
}

func (m *Mux) closeFeedLocked(w *Window) {
	if !w.closed {
		w.closed = true
		w.q.close()
		close(w.inStop)
	}
}

// output takes a chunk of w's stream: queued for the terminal at once if
// w is shown, and for its emulator. Neither waits: both queues are bounded
// by dropping (liveLimit, feedLimit), so the stream is always read. The
// key-mode tracker reads the chunk first, under w's own lock; what it
// found is published with the chunk, so a paint queued after it sees it.
func (m *Mux) output(w *Window, data []byte) {
	changed := w.trackKeys(data)
	m.mu.Lock()
	defer m.mu.Unlock()
	if w.closed {
		return
	}
	if m.showingLocked(w) {
		m.tw.enqueueLive(data)
	}
	w.tail.Add(data)
	if changed {
		w.publishKeysLocked()
	}
	w.q.push(feedItem{data: data})
}

// terminalCaughtUp repaints the shown window once a terminal that fell
// behind took everything queued: the output it missed is in the window's
// emulator.
func (m *Mux) terminalCaughtUp() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tw.caughtUpDone()
	m.repaintLocked()
}

// showingLocked reports whether w's output goes to the terminal now: it is
// the active window and no dialog owns the terminal.
func (m *Mux) showingLocked(w *Window) bool {
	return !m.suspended && m.activeLocked() == w
}

func (m *Mux) activeLocked() *Window {
	if m.active >= 0 && m.active < len(m.windows) {
		return m.windows[m.active]
	}
	return nil
}

func (m *Mux) indexLocked(w *Window) int {
	for i, x := range m.windows {
		if x == w {
			return i
		}
	}
	return -1
}

// titleLocked is the terminal title naming w: luxmux:<session> [i] name.
func (m *Mux) titleLocked(w *Window) string {
	return fmt.Sprintf("luxmux:%s [%d] %s", m.name, m.indexLocked(w), w.Name)
}

// paintKind is why a window is painted; it decides what is drawn instead
// when its emulator is stale (see feedLimit).
type paintKind int

const (
	// paintShow: the window is switched to: the terminal has none of its
	// screen. A stale window shows a placeholder and is attached again
	// with restore (refreshLocked).
	paintShow paintKind = iota
	// paintRedraw: the shown window is drawn again after the terminal
	// missed some of its output (a dialog drew over it, the
	// terminal fell behind) or resized. A stale window is cleared and
	// attached again with restore, which repaints it from the snapshot.
	paintRedraw
	// paintOverlay: the shown window's bottom row (a notice, a prompt,
	// its own line) or title changed. A stale window's terminal holds all
	// of its output already: only the bottom row is drawn or erased.
	paintOverlay
)

// repaintLocked draws the active window again (paintRedraw).
func (m *Mux) repaintLocked() { m.paintActiveLocked(paintRedraw) }

// overlayGoneLocked draws the active window after its bottom-row overlay
// or title changed (paintOverlay).
func (m *Mux) overlayGoneLocked() { m.paintActiveLocked(paintOverlay) }

// paintActiveLocked shows the active window as it is: its emulator's
// screen and modes when it has one, its progress or its end otherwise,
// and the bottom line it needs. A window the terminal is not showing is
// painted as paintShow whatever kind says.
func (m *Mux) paintActiveLocked(kind paintKind) {
	w := m.activeLocked()
	if w == nil || m.suspended {
		return
	}
	line := m.overlayLineLocked(w)
	if !w.hasScreen {
		m.shown = nil
		b := append([]byte{0x18}, m.statusScreen(nil, w)...)
		if line != "" {
			b = m.plainOverlay(b, line)
		}
		m.tw.enqueue(b)
		return
	}
	if m.shown != w {
		kind = paintShow
	}
	m.shown = w
	title := m.titleLocked(w)
	pend := w.tail.Pending()
	km := m.keyModesLocked(w)
	m.paintLocked(w, kind, line, func(b []byte, t *vtermT) []byte {
		// CAN ends whatever sequence the previous output stopped inside.
		b = append(b, 0x18)
		b = screen.Paint(b, t, title, km)
		if line != "" {
			rows, _ := t.Size()
			b = screen.Overlay(b, t, rows-1, line)
		}
		return append(b, pend...)
	})
}

// keyModesLocked are the keyboard modes w's program set, as of the output
// queued so far (the place a paint queued now takes in the terminal's
// order). An ended window's program reads no keys: r and & are read plain.
func (m *Mux) keyModesLocked(w *Window) screen.KeyModes {
	if w.phase == phaseDead {
		return screen.KeyModes{}
	}
	return w.keys
}

// paintLocked queues for the terminal what draw makes from w's emulator,
// once the emulator has taken every output queued before: in the
// terminal's order, without waiting. A stale emulator is not painted;
// what is drawn instead depends on kind (see paintKind). line is the
// bottom row's overlay.
func (m *Mux) paintLocked(w *Window, kind paintKind, line string, draw func([]byte, *vtermT) []byte) {
	fallback := m.staleScreenLocked(w, kind, line)
	if w.q.isStale() {
		m.staleLocked(w, kind)
		m.tw.enqueue(fallback)
		return
	}
	s := m.tw.slot()
	w.q.push(feedItem{
		paint: func(t *vtermT) { s.fill(draw(nil, t)) },
		skip: func(stale bool) {
			if !stale {
				// The window closed: the next repaint replaces it.
				s.fill(nil)
				return
			}
			s.fill(fallback)
			m.goSafe(func() {
				m.mu.Lock()
				defer m.mu.Unlock()
				if !w.closed && w.q.isStale() {
					m.staleLocked(w, kind)
				}
			})
		},
	})
}

// staleScreenLocked is what a stale w shows in place of a paint of kind.
func (m *Mux) staleScreenLocked(w *Window, kind paintKind, line string) []byte {
	b := []byte{0x18}
	switch kind {
	case paintShow:
		b = screen.Text(b, []string{
			fmt.Sprintf(" luxmux: window %d (%s): refreshing its screen from lux…", m.indexLocked(w), w.Name)}, m.rows, m.cols)
		if line != "" {
			b = m.plainOverlay(b, line)
		}
	case paintRedraw:
		b = screen.Blank(b, m.keyModesLocked(w), w.altScreen)
		if line != "" {
			b = m.bottomRow(b, line)
		}
		b = append(b, w.tail.Pending()...)
	case paintOverlay:
		b = m.bottomRow(b, line)
		if km := m.keyModesLocked(w); m.promptState == nil && !km.Zero() {
			// A prompt that closed turned the modes off (askLocked).
			b = screen.KeysHere(b, km, w.altScreen)
		}
		b = append(b, w.tail.Pending()...)
	}
	if kind == paintShow {
		b = append(b, "\x1b]2;"...)
		b = append(b, m.titleLocked(w)...)
		b = append(b, 0x07)
	}
	return b
}

// staleLocked acts on a stale w painted as kind: a window switched to, or
// the shown one drawn again in full, is attached again with restore (at
// most once per refreshEvery, see refreshLocked); an overlay change never
// is.
func (m *Mux) staleLocked(w *Window, kind paintKind) {
	if kind == paintShow || kind == paintRedraw {
		m.refreshLocked(w)
	}
}

// refreshEvery is the least time from any attach of a window to a refresh
// of it: a window whose output outpaces its emulator goes stale again
// soon after each, and each attach costs lux a restore snapshot of up to
// 4 MiB.
const refreshEvery = 2 * time.Second

// refreshLocked closes w's stream so its supervisor attaches again with
// restore, rebuilding the emulator from the snapshot; within refreshEvery
// of w's last attach it is put off until then. The limit runs from the
// attach, not from the close: the stream can take a while to end.
func (m *Mux) refreshLocked(w *Window) {
	if w.refreshing || w.closed || w.refreshDue {
		return
	}
	c := w.getConn()
	if c == nil {
		return
	}
	if wait := refreshEvery - time.Since(w.lastAttach); wait > 0 {
		w.refreshDue = true
		time.AfterFunc(wait, func() {
			defer m.recoverPanic()
			m.mu.Lock()
			defer m.mu.Unlock()
			w.refreshDue = false
			// Only for the window still shown: a chooser preview asks
			// again on its next tick.
			if !w.closed && w.q.isStale() && m.activeLocked() == w {
				m.refreshLocked(w)
			}
		})
		return
	}
	w.refreshing = true
	m.refreshStream(c, w.closingStream(c))
}

// closeGrace bounds refreshStream's handshake.
const closeGrace = time.Second

// refreshStream ends a stream that is still up with the websocket close
// handshake, not CloseNow: CloseNow resets the connection, and lux then
// loses keystrokes it received but had not read yet. With the handshake
// lux reads every message written before the close frame, and a key write
// after it fails, so the keys stay queued for the next stream. A peer
// that does not answer within closeGrace is cut off (cut closes the
// network connection; the library's own wait for the answer is 5 s, and
// its CloseNow only waits for Close). It does not wait.
func (m *Mux) refreshStream(c *websocket.Conn, cut func()) {
	t := time.AfterFunc(closeGrace, func() {
		if cut != nil {
			cut()
		}
	})
	m.goSafe(func() {
		_ = c.Close(websocket.StatusNormalClosure, "")
		t.Stop()
	})
}

// selectLocked makes window i the active one and repaints.
func (m *Mux) selectLocked(i int) {
	if i < 0 || i >= len(m.windows) {
		return
	}
	if i != m.active {
		m.last = m.active
		m.active = i
	}
	m.repaintLocked()
	m.saveLocked()
}

// saveLocked writes the session's state file.
func (m *Mux) saveLocked() {
	s := m.sess
	s.Windows = s.Windows[:0]
	for _, w := range m.windows {
		if w.RunID == "" {
			continue
		}
		spec := w.Spec
		ep := w.Endpoint
		s.Windows = append(s.Windows, state.Window{RunID: w.RunID, Name: w.Name, Profile: w.Profile, Spec: &spec, Endpoint: &ep})
	}
	s.Active, s.Last = m.active, m.last
	if m.opt.StateDir != "" {
		_ = state.Save(m.opt.StateDir, s)
	}
}

// Windows is a snapshot of the windows for the chooser.
func (m *Mux) Windows() []WindowInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]WindowInfo, 0, len(m.windows))
	def := m.eps.Default()
	for i, w := range m.windows {
		out = append(out, WindowInfo{Index: i, Name: w.Name, RunID: w.RunID, State: m.stateWordLocked(w),
			Image: w.Spec.Image, Size: sizeWord(w.Spec), Repo: repoWord(w.Spec), Created: w.created, Active: i == m.active,
			Endpoint: w.Endpoint.Label(), OtherEndpoint: w.Endpoint != def})
	}
	return out
}

// WindowInfo is one row of the chooser.
type WindowInfo struct {
	Index   int
	Name    string
	RunID   string
	State   string
	Image   string
	Size    string
	Repo    string
	Created time.Time
	Active  bool
	// Endpoint is the window's lux and tenant; OtherEndpoint: not the
	// lux CLI configuration's.
	Endpoint      string
	OtherEndpoint bool
}

func sizeWord(p config.Profile) string {
	switch {
	case p.Size != "":
		return p.Size
	case p.CPUs != 0 || p.Memory != "":
		s := ""
		if p.CPUs != 0 {
			s = fmt.Sprintf("%gcpu", p.CPUs)
		}
		if p.Memory != "" {
			if s != "" {
				s += "/"
			}
			s += p.Memory
		}
		return s
	}
	return "-"
}

func repoWord(p config.Profile) string {
	repos := p.RepositoryList()
	if len(repos) == 0 {
		return "-"
	}
	r := repos[0].RepoName()
	if ref := repos[0].RepoRef(); ref != "" {
		r += "@" + ref
	}
	if len(repos) > 1 {
		r += fmt.Sprintf(" +%d", len(repos)-1)
	}
	return r
}

func (m *Mux) stateWordLocked(w *Window) string {
	switch w.phase {
	case phaseLive:
		return "running"
	case phaseDead:
		return "ended"
	case phaseReconnecting:
		return "reconnecting"
	}
	if w.status != "" {
		return w.status
	}
	return "starting"
}

// previewFor is how long a rendered preview is reused: the chooser
// renders on every key and every tick.
const previewFor = 250 * time.Millisecond

// Preview renders rows×cols of window i's screen for the chooser: the
// part of it that ends at its cursor's row, so a shell's prompt shows. It
// reads the emulator as it stands, without waiting for its queue.
func (m *Mux) Preview(i, rows, cols int) []string {
	m.mu.Lock()
	if i < 0 || i >= len(m.windows) || rows <= 0 || cols <= 0 {
		m.mu.Unlock()
		return nil
	}
	w := m.windows[i]
	if !w.hasScreen {
		s := []string{"  " + m.stateWordLocked(w) + ": " + w.status}
		m.mu.Unlock()
		return s
	}
	if w.q.isStale() {
		// The window the terminal shows is attached again when the
		// chooser closes (a full redraw); any other is attached now.
		if w != m.shown {
			m.refreshLocked(w)
			m.mu.Unlock()
			return []string{"  refreshing its screen from lux…"}
		}
		m.mu.Unlock()
		return []string{"  (out of date here; refreshed from lux when the chooser closes)"}
	}
	m.mu.Unlock()
	w.prevMu.Lock()
	defer w.prevMu.Unlock()
	if w.prev != nil && w.prevSize == [2]int{rows, cols} && time.Since(w.prevAt) < previewFor {
		return w.prev
	}
	w.emuMu.Lock()
	out := screen.Preview(w.emu, rows, cols)
	w.emuMu.Unlock()
	w.prev, w.prevSize, w.prevAt = out, [2]int{rows, cols}, time.Now()
	return out
}

// Select shows window i (the chooser's choice).
func (m *Mux) Select(i int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.selectLocked(i)
}

// Rename names window i.
func (m *Mux) Rename(i int, name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if i >= 0 && i < len(m.windows) && name != "" {
		m.windows[i].Name = name
		m.saveLocked()
	}
}

// Kill terminates window i's Run and closes it.
func (m *Mux) Kill(i int) {
	m.mu.Lock()
	if i < 0 || i >= len(m.windows) {
		m.mu.Unlock()
		return
	}
	w := m.windows[i]
	m.mu.Unlock()
	m.closeWindow(w, true, "")
}

// closeWindow removes w; with terminate, its Run is terminated first.
// notice, when set, is shown afterwards. A Run whose lux has no client
// now (its key unreadable) is not terminated, and its window is kept:
// dropping it would leave the Run running with nothing recording it.
func (m *Mux) closeWindow(w *Window, terminate bool, notice string) {
	if terminate && w.RunID != "" {
		if err := m.terminate(w.Endpoint, w.RunID); err != nil {
			m.Notice(cannotTerminate(w.Endpoint, err))
			return
		}
	}
	m.mu.Lock()
	i := m.indexLocked(w)
	if i < 0 {
		m.mu.Unlock()
		return
	}
	if w.cancel != nil {
		w.cancel()
	}
	if c := w.getConn(); c != nil {
		c.CloseNow()
	}
	m.windows = append(m.windows[:i], m.windows[i+1:]...)
	delete(m.byRun, w.RunID)
	m.considerLocked(w.Endpoint, w.RunID)
	m.dropClaimedLocked(w.RunID)
	m.closeFeedLocked(w)
	if len(m.windows) == 0 {
		m.saveLocked()
		m.mu.Unlock()
		m.endSession()
		return
	}
	// The window shown next: the last one if it was the active one, as
	// tmux does, else the active one stays.
	activeWin := m.activeLocked()
	if i == m.active || activeWin == nil {
		next := m.last
		if next > i {
			next--
		}
		if next == i || next < 0 || next >= len(m.windows) {
			next = min(i, len(m.windows)-1)
		}
		m.active = next
	} else if i < m.active {
		m.active--
	}
	if m.last >= len(m.windows) || m.last == m.active {
		m.last = max(0, m.active-1)
	} else if i < m.last {
		m.last--
	}
	if notice != "" {
		m.setNoticeLocked(notice)
	}
	m.repaintLocked()
	m.saveLocked()
	m.mu.Unlock()
}

// endSession ends the session when its last window closed: its spares are
// terminated (nothing would claim them) and its file removed. A spare on
// a lux with no client now (its key unreadable) stays recorded in the
// file, which then holds only spares; luxmux spares lists them.
func (m *Mux) endSession() {
	m.mu.Lock()
	m.ending = true
	type spare struct {
		slot string
		ep   config.Endpoint
		id   string
	}
	spares := make([]spare, 0, len(m.sess.Spares))
	for slot, id := range m.sess.Spares {
		if id != "" {
			spares = append(spares, spare{slot, m.spareEndpointLocked(slot), id})
		}
	}
	m.sess.Spares, m.sess.SpareEndpoints = nil, nil
	m.mu.Unlock()
	sort.Slice(spares, func(i, j int) bool { return spares[i].id < spares[j].id })
	var kept []spare
	for _, s := range spares {
		if err := m.terminate(s.ep, s.id); err != nil {
			kept = append(kept, s)
		}
	}
	if m.opt.StateDir != "" {
		m.mu.Lock()
		name := m.name
		m.mu.Unlock()
		if len(kept) == 0 {
			_ = state.Remove(m.opt.StateDir, name)
			_ = state.DropRenamed(m.opt.StateDir, name)
		} else {
			m.mu.Lock()
			for _, s := range kept {
				m.sess.SetSpare(s.slot, s.id, s.ep)
			}
			m.saveLocked()
			m.mu.Unlock()
		}
	}
	m.finish("exited")
}

// cannotTerminate is the notice for a Run left running because its lux
// has no client now.
func cannotTerminate(ep config.Endpoint, err error) string {
	return fmt.Sprintf("cannot terminate on %s: %v", ep.Label(), err)
}

// terminate terminates a Run on ep, retrying a few times on errors. Its
// error is only that ep has no client (the Run was not asked to end);
// lux's own refusals after the retries are not reported.
func (m *Mux) terminate(ep config.Endpoint, id string) error {
	c, err := m.clientOf(ep)
	if err != nil {
		return err
	}
	d := m.opt.Backoff
	for range 5 {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		err := c.Terminate(ctx, id)
		cancel()
		if err == nil {
			return nil
		}
		time.Sleep(d)
		d = min(2*d, m.opt.MaxBackoff)
	}
	return nil
}
