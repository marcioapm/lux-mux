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
	"time"

	"github.com/marcioapm/lux-mux/internal/config"
	"github.com/marcioapm/lux-mux/internal/keys"
	"github.com/marcioapm/lux-mux/internal/luxapi"
	"github.com/marcioapm/lux-mux/internal/screen"
	"github.com/marcioapm/lux-mux/internal/state"
)

// UI runs the full-screen dialogs (the chooser and the new-window
// dialog). They own the terminal while open. Nil: those keys show a
// notice instead.
type UI interface {
	Choose(m *Mux) (ChooseResult, error)
	NewDialog(m *Mux) (*NewWindowRequest, error)
}

// Options configure a Mux.
type Options struct {
	Session  string
	Config   *config.Config
	Lux      *luxapi.Client
	StateDir string
	// In is the keyboard (raw mode is the caller's), Out the terminal.
	In  io.Reader
	Out io.Writer
	// Size is the terminal's rows and columns.
	Size func() (rows, cols int)
	UI   UI
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
}

// Mux is one attached session.
type Mux struct {
	opt  Options
	lux  *luxapi.Client
	keys *keys.Machine
	in   *input
	out  io.Writer

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	// mu guards the windows, the terminal (every write to out) and the
	// session state.
	mu        sync.Mutex
	windows   []*Window
	active    int
	last      int
	rows      int
	cols      int
	suspended bool
	sess      *state.Session
	notice    string
	noticeGen int
	byRun     map[string]*Window
	// promptState is the open prompt; help: the key help is shown.
	promptState *prompt
	help        bool
	done        chan string // the reason Run returns
	buf         []byte
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
	m := &Mux{opt: opt, lux: opt.Lux, keys: km, in: &input{r: opt.In}, out: opt.Out,
		sess: sess, byRun: map[string]*Window{}, done: make(chan string, 1)}
	m.rows, m.cols = opt.Size()
	m.ctx, m.cancel = context.WithCancel(context.Background())
	for _, sw := range sess.Windows {
		spec := opt.Config.Profiles[sw.Profile]
		if sw.Spec != nil {
			spec = *sw.Spec
		}
		w := m.newWindow(sw.Name, sw.Profile, spec)
		w.RunID = sw.RunID
		m.windows = append(m.windows, w)
		m.byRun[w.RunID] = w
	}
	m.active = clamp(sess.Active, len(m.windows))
	m.last = clamp(sess.Last, len(m.windows))
	return m, nil
}

func clamp(i, n int) int {
	if i < 0 || i >= n {
		return 0
	}
	return i
}

// Session is the session's name.
func (m *Mux) Session() string { return m.opt.Session }

// Config is the configuration.
func (m *Mux) Config() *config.Config { return m.opt.Config }

// Lux is the lux client.
func (m *Mux) Lux() *luxapi.Client { return m.lux }

// Terminal is where dialogs draw, and its size.
func (m *Mux) Terminal() (io.Writer, int, int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.out, m.rows, m.cols
}

// Context ends when the session does.
func (m *Mux) Context() context.Context { return m.ctx }

// Run shows the session until it is detached, its last window closes, or
// ctx ends. It returns why: "detached", "exited" or ctx's error.
func (m *Mux) Run(ctx context.Context) (string, error) {
	go func() {
		select {
		case <-ctx.Done():
			m.finish("")
		case <-m.ctx.Done():
		}
	}()
	m.mu.Lock()
	if len(m.windows) == 0 {
		m.mu.Unlock()
		m.newWindowFrom(m.opt.Config.DefaultProfile, m.opt.Config.Profiles[m.opt.Config.DefaultProfile], "", false)
		m.mu.Lock()
	}
	for _, w := range m.windows {
		if w.RunID != "" && w.cancel == nil {
			m.startSupervisor(w)
		}
	}
	m.repaintLocked()
	m.saveLocked()
	m.mu.Unlock()
	m.wg.Add(2)
	go m.followEvents()
	go m.keepSpares()
	go m.inputLoop()
	<-m.ctx.Done()
	reason := <-m.done
	m.shutdown()
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

// shutdown closes every stream and waits for the goroutines; the Runs
// keep running.
func (m *Mux) shutdown() {
	m.mu.Lock()
	ws := append([]*Window{}, m.windows...)
	m.saveLocked()
	m.suspended = true
	b := screen.Reset(nil)
	_, _ = m.out.Write(b)
	m.mu.Unlock()
	for _, w := range ws {
		if c := w.getConn(); c != nil {
			c.CloseNow()
		}
	}
	m.wg.Wait()
	m.mu.Lock()
	for _, w := range ws {
		m.closeFeedLocked(w)
	}
	m.mu.Unlock()
}

func (m *Mux) closeFeedLocked(w *Window) {
	if !w.closed {
		w.closed = true
		close(w.feed)
	}
}

// write writes to the terminal; called with m.mu held.
func (m *Mux) write(b []byte) {
	if len(b) > 0 {
		_, _ = m.out.Write(b)
	}
}

// output takes a chunk of w's stream: written to the terminal at once if
// w is shown, and queued for its emulator. The emulator is fed apart, so
// the terminal never waits on it.
func (m *Mux) output(w *Window, data []byte) {
	m.mu.Lock()
	if w.closed {
		m.mu.Unlock()
		return
	}
	if !m.suspended && m.activeLocked() == w {
		_, _ = m.out.Write(data)
	}
	w.tail.Add(data)
	w.feed <- feedItem{data: data}
	m.mu.Unlock()
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
	return fmt.Sprintf("luxmux:%s [%d] %s", m.opt.Session, m.indexLocked(w), w.Name)
}

// repaintLocked shows the active window as it is: its emulator's screen
// and modes when it has one, its progress or its end otherwise, and the
// bottom line it needs.
func (m *Mux) repaintLocked() {
	w := m.activeLocked()
	if w == nil || m.suspended {
		return
	}
	b := m.buf[:0]
	// CAN ends whatever sequence the previous output stopped inside.
	b = append(b, 0x18)
	if w.hasScreen {
		w.withEmu(func(t *vtermT) { b = screen.Paint(b, t, m.titleLocked(w)) })
		if line := m.bottomLineLocked(w); line != "" {
			b = m.overlayBytes(b, line)
		}
		b = append(b, w.tail.Pending()...)
	} else {
		b = m.statusScreen(b, w)
	}
	m.write(b)
	m.buf = b
	if m.notice != "" {
		m.drawNoticeLocked()
	}
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
		s.Windows = append(s.Windows, state.Window{RunID: w.RunID, Name: w.Name, Profile: w.Profile, Spec: &spec})
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
	for i, w := range m.windows {
		out = append(out, WindowInfo{Index: i, Name: w.Name, RunID: w.RunID, State: m.stateWordLocked(w),
			Image: w.Spec.Image, Size: sizeWord(w.Spec), Repo: repoWord(w.Spec), Created: w.created, Active: i == m.active})
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
	if p.Repository == "" {
		return "-"
	}
	r := config.RepoName(p.Repository)
	if ref := p.GitRef(); ref != "" {
		r += "@" + ref
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

// Preview renders rows×cols of window i's screen for the chooser: the
// part of it that ends at its cursor's row, so a shell's prompt shows.
func (m *Mux) Preview(i, rows, cols int) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if i < 0 || i >= len(m.windows) || rows <= 0 || cols <= 0 {
		return nil
	}
	w := m.windows[i]
	if !w.hasScreen {
		return []string{"  " + m.stateWordLocked(w) + ": " + w.status}
	}
	var out []string
	w.withEmu(func(t *vtermT) { out = screen.Preview(t, rows, cols) })
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
// notice, when set, is shown afterwards.
func (m *Mux) closeWindow(w *Window, terminate bool, notice string) {
	if terminate && w.RunID != "" {
		m.terminate(w.RunID)
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
// terminated (nothing would claim them) and its file removed.
func (m *Mux) endSession() {
	m.mu.Lock()
	spares := make([]string, 0, len(m.sess.Spares))
	for _, id := range m.sess.Spares {
		spares = append(spares, id)
	}
	m.sess.Spares = nil
	m.mu.Unlock()
	sort.Strings(spares)
	for _, id := range spares {
		m.terminate(id)
	}
	if m.opt.StateDir != "" {
		_ = state.Remove(m.opt.StateDir, m.opt.Session)
	}
	m.finish("exited")
}

// terminate terminates a Run, retrying a few times on errors.
func (m *Mux) terminate(id string) {
	d := m.opt.Backoff
	for range 5 {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		err := m.lux.Terminate(ctx, id)
		cancel()
		if err == nil {
			return
		}
		time.Sleep(d)
		d = min(2*d, m.opt.MaxBackoff)
	}
}
