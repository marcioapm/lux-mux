package mux

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/marcioapm/lux/api"
	"github.com/marcioapm/lux/vterm"

	"github.com/marcioapm/lux-mux/internal/config"
	"github.com/marcioapm/lux-mux/internal/screen"
	"github.com/marcioapm/lux-mux/internal/sessions"
	"github.com/marcioapm/lux-mux/internal/state"
)

// reasonSwitch is Run's reason when another session is switched to.
const reasonSwitch = "switch"

// NewSessionRequest is what the new-session dialog asks for: a session
// and its first window's profile, already checked by lux's dry run.
type NewSessionRequest struct {
	Name        string
	ProfileName string
	Profile     config.Profile
}

// SessionChoice is what the session chooser did: a session to switch to,
// or one to create ("" and nil: neither).
type SessionChoice struct {
	Switch string
	New    *NewSessionRequest
}

// SessionInfo is one row of the session chooser.
type SessionInfo struct {
	sessions.Info
	Current bool
}

// switchTarget is the session a switch goes to.
type switchTarget struct {
	name string
	// create: a new session whose first window is first, with
	// defaultProfile as its default.
	create         bool
	defaultProfile string
	first          *config.Profile
	notice         string
}

// Attach shows opt's session, then each session switched to from inside
// it, one at a time on one terminal, until one is detached or exits or
// ctx ends. It returns why the last one ended and that session's name.
// opt.Unlock is the first session's lock; every lock taken is released
// by the time it returns.
func Attach(ctx context.Context, opt Options, sess *state.Session) (reason, name string, err error) {
	if opt.Console == nil {
		opt.Console = NewConsole(opt.In, opt.Out)
		defer opt.Console.Close(2 * time.Second)
	}
	for {
		m, err := New(opt, sess)
		if err != nil {
			if opt.Unlock != nil {
				opt.Unlock()
			}
			return "", opt.Session, err
		}
		reason, err := m.Run(ctx)
		if reason != reasonSwitch {
			m.releaseLock()
			return reason, m.Session(), err
		}
		opt, sess, err = m.switchOver(ctx)
		if err != nil {
			return "detached", m.Session(), err
		}
	}
}

// releaseLock releases the session's lock, once.
func (m *Mux) releaseLock() {
	m.mu.Lock()
	unlock := m.unlock
	m.unlock = nil
	m.mu.Unlock()
	if unlock != nil {
		unlock()
	}
}

const (
	// switchOpenWait bounds the lux calls that open a switch's target.
	switchOpenWait = 10 * time.Second
	// refreshWait bounds the re-read of the session a failed switch
	// returns to; its saved state is used when lux does not answer.
	refreshWait = 10 * time.Second
	// relockTries×relockEvery is how long a failed switch retries this
	// session's lock: luxmux new-window -t takes it for a moment.
	relockTries = 5
	relockEvery = 50 * time.Millisecond
)

// switchOver runs once the session was detached for a switch (its streams
// closed, its goroutines ended, its state saved): it releases this
// session's lock, takes the target's and opens it as luxmux attach -t
// does. When the target cannot be locked or opened, this session is
// locked again and reattached from the state it saved, with a notice
// saying why: lux is not needed for that. err is set only when this
// session's lock was taken by another process meanwhile.
func (m *Mux) switchOver(ctx context.Context) (Options, *state.Session, error) {
	m.mu.Lock()
	t, old, oldLabel, oldSess := m.next, m.name, m.label, m.sess
	rows, cols := m.rows, m.cols
	afterUnlock := m.opt.afterSwitchUnlock
	m.mu.Unlock()
	m.releaseLock()
	if afterUnlock != nil {
		afterUnlock()
	}
	m.tw.enqueue(screen.Text(nil, []string{" luxmux: switching to session " + t.name + "…"}, rows, cols))
	opt := m.opt
	opt.Config = m.config()
	opt.First, opt.Notice, opt.Unlock, opt.afterSwitchUnlock = nil, "", nil, nil
	timedOut := false
	unlock, err := m.lockSession(t.name)
	if err == nil {
		octx, cancel := context.WithTimeout(ctx, m.opt.switchOpenWait)
		sess, label, oerr := m.openTarget(octx, opt.Config, t)
		timedOut = octx.Err() != nil
		cancel()
		if oerr == nil {
			opt.Session, opt.Label, opt.Unlock, opt.Notice, opt.First = t.name, label, unlock, t.notice, t.first
			return opt, sess, nil
		}
		unlock()
		err = oerr
	}
	unlock, lerr := m.relock(old)
	if lerr != nil {
		return opt, nil, fmt.Errorf("session %s: %w", old, lerr)
	}
	sess, label := oldSess, oldLabel
	// A lux that let the target's open time out would let this wait as
	// long again: the saved state is reconciled by the supervisors.
	if !timedOut {
		rctx, cancel := context.WithTimeout(ctx, refreshWait)
		if s, l, ok, rerr := sessions.Open(rctx, m.lux, m.opt.StateDir, opt.Config, old); rerr == nil && ok {
			sess, label = s, l
		}
		cancel()
	}
	opt.Session, opt.Label, opt.Unlock = old, label, unlock
	opt.Notice = fmt.Sprintf("session %s: %v", t.name, err)
	return opt, sess, nil
}

// lockSession takes session name's lock.
func (m *Mux) lockSession(name string) (func(), error) {
	unlock, err := state.Lock(m.opt.StateDir, name)
	if errors.Is(err, state.ErrLocked) {
		return nil, fmt.Errorf("%w; stayed here", err)
	}
	return unlock, err
}

// relock takes back session name's lock after a failed switch, retrying
// for relockTries×relockEvery while another process holds it.
func (m *Mux) relock(name string) (func(), error) {
	for i := 1; ; i++ {
		unlock, err := state.Lock(m.opt.StateDir, name)
		if err == nil || !errors.Is(err, state.ErrLocked) || i == relockTries {
			return unlock, err
		}
		time.Sleep(relockEvery)
	}
}

// openTarget reads the session t names, its lock held: a new one is
// checked free and starts empty; an existing one is opened as attach does.
func (m *Mux) openTarget(ctx context.Context, cfg *config.Config, t *switchTarget) (*state.Session, string, error) {
	if t.create {
		if err := sessions.CheckFree(ctx, m.lux, m.opt.StateDir, t.name, ""); err != nil {
			return nil, "", err
		}
		return &state.Session{Name: t.name, DefaultProfile: t.defaultProfile}, t.name, nil
	}
	sess, label, existed, err := sessions.Open(ctx, m.lux, m.opt.StateDir, cfg, t.name)
	if err != nil {
		return nil, "", err
	}
	if !existed {
		return nil, "", fmt.Errorf("no session %s", t.name)
	}
	return sess, label, nil
}

// SwitchTo detaches this session and attaches name in its place, unless
// name is this session or another process has it, which a notice says.
// notice is shown once name is on the terminal. It reports whether the
// switch started.
func (m *Mux) SwitchTo(name, notice string) bool {
	if name == m.Session() {
		m.Notice("session " + name + " is this one")
		return false
	}
	if _, err := sessions.Label(m.opt.StateDir, name); err != nil {
		m.Notice(err.Error())
		return false
	}
	if pid, held := state.Held(m.opt.StateDir, name); held {
		m.Notice(fmt.Sprintf("session %s is attached elsewhere (pid %s)", name, pid))
		return false
	}
	return m.startSwitch(&switchTarget{name: name, notice: notice})
}

// CheckSessionName says whether a new session may be named name.
func (m *Mux) CheckSessionName(ctx context.Context, name string) error {
	return sessions.CheckFree(ctx, m.lux, m.opt.StateDir, name, "")
}

// NewSession detaches this session and creates req's in its place,
// unless its name is taken or its first window's secrets are missing.
func (m *Mux) NewSession(req *NewSessionRequest) bool {
	ctx, cancel := context.WithTimeout(m.ctx, 15*time.Second)
	defer cancel()
	if err := sessions.CheckFree(ctx, m.lux, m.opt.StateDir, req.Name, ""); err != nil {
		m.Notice("new session: " + err.Error())
		return false
	}
	if _, err := req.Profile.ResolveSecrets(m.opt.Lookup); err != nil {
		m.Notice("new session: " + err.Error())
		return false
	}
	p := req.Profile
	return m.startSwitch(&switchTarget{name: req.Name, create: true, defaultProfile: req.ProfileName, first: &p,
		notice: "new session " + req.Name})
}

func (m *Mux) startSwitch(t *switchTarget) bool {
	m.mu.Lock()
	if m.ending || m.ctx.Err() != nil {
		m.mu.Unlock()
		return false
	}
	m.next = t
	m.mu.Unlock()
	m.finish(reasonSwitch)
	return true
}

// Sessions lists every session for the chooser and ( ), sorted by name,
// this one marked; its window count is the live one.
func (m *Mux) Sessions() ([]SessionInfo, error) {
	ctx, cancel := context.WithTimeout(m.ctx, 15*time.Second)
	defer cancel()
	list, err := sessions.List(ctx, m.lux, m.opt.StateDir)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]SessionInfo, 0, len(list)+1)
	found := false
	for _, in := range list {
		si := SessionInfo{Info: in}
		if in.Name == m.name {
			found = true
			si.Current, si.Elsewhere, si.Pid = true, false, ""
			si.Windows = len(m.windows)
			si.DefaultProfile = m.sess.DefaultProfile
		}
		out = append(out, si)
	}
	if !found {
		cur := SessionInfo{Info: sessions.Info{Name: m.name, Label: m.label, Windows: len(m.windows),
			DefaultProfile: m.sess.DefaultProfile, HasFile: true}, Current: true}
		i := 0
		for i < len(out) && out[i].Name < m.name {
			i++
		}
		out = append(out[:i], append([]SessionInfo{cur}, out[i:]...)...)
	}
	return out, nil
}

// stepSession switches to the next session by name (dir 1) or the
// previous one (-1), wrapping, past those attached elsewhere. Listing
// the sessions asks lux: it runs off the input loop, which keeps taking
// keys meanwhile; a step already listing makes this one a no-op.
func (m *Mux) stepSession(dir int) {
	if !m.stepping.CompareAndSwap(false, true) {
		return
	}
	// In m.wg: added from the input loop, which shutdown waits for first.
	m.wg.Add(1)
	m.goSafe(func() {
		defer m.wg.Done()
		defer m.stepping.Store(false)
		list, err := m.Sessions()
		if err != nil {
			if m.ctx.Err() == nil {
				m.Notice("sessions: " + err.Error())
			}
			return
		}
		m.stepIn(list, dir)
	})
}

// stepIn switches from the current session of list to the next one in
// direction dir that is not attached elsewhere.
func (m *Mux) stepIn(list []SessionInfo, dir int) {
	n := len(list)
	cur := 0
	for i, s := range list {
		if s.Current {
			cur = i
		}
	}
	var skipped []string
	for k := 1; k < n; k++ {
		// Go's % keeps the dividend's sign: add n to wrap below 0.
		s := list[((cur+dir*k)%n+n)%n]
		if s.Elsewhere {
			skipped = append(skipped, s.Name)
			continue
		}
		notice := "session " + s.Name
		if len(skipped) > 0 {
			notice += fmt.Sprintf(" (skipped %s: attached elsewhere)", strings.Join(skipped, ", "))
		}
		m.SwitchTo(s.Name, notice)
		return
	}
	if len(skipped) > 0 {
		m.Notice(fmt.Sprintf("no other session to switch to (%s attached elsewhere)", strings.Join(skipped, ", ")))
		return
	}
	m.Notice("no other session")
}

// ErrKillCurrent is KillSession's refusal of the session attached here.
var ErrKillCurrent = errors.New("this is the session attached here: switch away first, or exit its windows")

// KillSession terminates session name's Runs and forgets it, as
// luxmux kill-session does. The session attached here, and one attached
// elsewhere, are refused.
func (m *Mux) KillSession(name string) error {
	if name == m.Session() {
		return ErrKillCurrent
	}
	unlock, err := state.Lock(m.opt.StateDir, name)
	if err != nil {
		return fmt.Errorf("session %s: %w", name, err)
	}
	defer unlock()
	m.stopSessionPreview()
	ctx, cancel := context.WithTimeout(m.ctx, 2*time.Minute)
	defer cancel()
	return sessions.Kill(ctx, m.lux, m.opt.StateDir, name, nil)
}

// RenameSession names session old new: its file and the rename map (its
// Runs keep their label: lux cannot change it). Both names are locked
// for it; this session's lock moves to the new name.
func (m *Mux) RenameSession(old, new string) error {
	if old == new {
		return nil
	}
	if err := sessions.CheckName(new); err != nil {
		return err
	}
	current := old == m.Session()
	var unlockOld func()
	if !current {
		u, err := state.Lock(m.opt.StateDir, old)
		if err != nil {
			return fmt.Errorf("session %s: %w", old, err)
		}
		unlockOld = u
	}
	unlockNew, err := state.Lock(m.opt.StateDir, new)
	if err != nil {
		if unlockOld != nil {
			unlockOld()
		}
		return fmt.Errorf("session %s: %w", new, err)
	}
	ctx, cancel := context.WithTimeout(m.ctx, 15*time.Second)
	defer cancel()
	label := m.label
	if !current {
		label, err = sessions.Label(m.opt.StateDir, old)
	}
	if err == nil {
		err = sessions.CheckFree(ctx, m.lux, m.opt.StateDir, new, label)
	}
	if err != nil {
		unlockNew()
		if unlockOld != nil {
			unlockOld()
		}
		return err
	}
	if !current {
		m.stopSessionPreview()
		err = sessions.Move(m.opt.StateDir, old, new, label)
		unlockNew()
		unlockOld()
		return err
	}
	m.mu.Lock()
	if m.ctx.Err() != nil {
		// Detaching or switching: its lock is being released.
		m.mu.Unlock()
		unlockNew()
		return errors.New("the session is detaching")
	}
	m.saveLocked()
	if err = sessions.Move(m.opt.StateDir, old, new, label); err != nil {
		m.mu.Unlock()
		unlockNew()
		return err
	}
	m.name, m.sess.Name = new, new
	unlockOld, m.unlock = m.unlock, unlockNew
	m.saveLocked()
	m.overlayGoneLocked()
	m.mu.Unlock()
	if unlockOld != nil {
		unlockOld()
	}
	return nil
}

// askRenameSessionLocked is C-b $: a prompt for the session's new name.
func (m *Mux) askRenameSessionLocked() {
	m.askLocked("rename session: ", m.name, false, func(s string, ok bool) {
		if !ok || s == "" || s == m.name {
			return
		}
		old := m.name
		// Renaming reads lux and the disk: off the lock.
		m.goSafe(func() {
			if err := m.RenameSession(old, s); err != nil {
				m.Notice("rename session: " + err.Error())
				return
			}
			m.Notice("session renamed to " + s)
		})
	})
}

// openSessionChooser runs the session chooser, then acts on its choice.
func (m *Mux) openSessionChooser() {
	if m.opt.UI == nil {
		m.Notice("no session chooser in this build")
		return
	}
	m.suspend(true)
	choice, err := m.opt.UI.ChooseSession(m)
	m.stopSessionPreview()
	switched := false
	switch {
	case err != nil:
	case choice.New != nil:
		switched = m.NewSession(choice.New)
	case choice.Switch != "" && choice.Switch != m.Session():
		switched = m.SwitchTo(choice.Switch, "session "+choice.Switch)
	}
	if !switched {
		m.suspend(false)
	}
	if err != nil {
		m.Notice("sessions: " + err.Error())
	}
}

// sessionPreview follows another session's shown window, for the
// session chooser: a restore attach that sends nothing, read into an
// emulator of its own. It takes no lock: a session attached elsewhere is
// not previewed at all.
type sessionPreview struct {
	mu     sync.Mutex
	name   string
	msg    string
	emu    *vterm.Terminal
	cancel context.CancelFunc
	done   chan struct{}
}

// SessionPreview renders rows×cols of session name's shown window: this
// session's from its emulator, another's from a restore attach to its
// Run, kept open while name stays the one previewed.
func (m *Mux) SessionPreview(name string, rows, cols int) []string {
	if rows <= 0 || cols <= 0 {
		return nil
	}
	m.mu.Lock()
	cur, active := m.name, m.active
	m.mu.Unlock()
	if name == cur {
		return m.Preview(active, rows, cols)
	}
	if _, held := state.Held(m.opt.StateDir, name); held {
		m.stopSessionPreview()
		return []string{"  attached elsewhere — preview unavailable"}
	}
	p := &m.preview
	p.mu.Lock()
	if p.name != name {
		p.mu.Unlock()
		m.stopSessionPreview()
		m.startSessionPreview(name)
		p.mu.Lock()
	}
	defer p.mu.Unlock()
	if p.emu == nil {
		return []string{"  " + p.msg}
	}
	return screen.Preview(p.emu, rows, cols)
}

// startSessionPreview attaches to session name's shown window; after the
// session ended it only says so.
func (m *Mux) startSessionPreview(name string) {
	ctx, cancel := context.WithCancel(m.ctx)
	done := make(chan struct{})
	p := &m.preview
	p.mu.Lock()
	p.name, p.msg, p.emu, p.cancel, p.done = name, "reading its screen from lux…", nil, cancel, done
	p.mu.Unlock()
	if ctx.Err() != nil {
		close(done)
		return
	}
	m.mu.Lock()
	rows, cols := m.rows, m.cols
	m.mu.Unlock()
	// Not in m.wg: shutdown waits for it through stopSessionPreview,
	// once the input loop that starts it returned.
	m.goSafe(func() {
		defer close(done)
		msg := m.followPreview(ctx, name, rows, cols)
		p.mu.Lock()
		if p.done == done && msg != "" {
			p.msg, p.emu = msg, nil
		}
		p.mu.Unlock()
	})
}

// followPreview reads session name's shown window into the preview until
// ctx ends; it returns what to show instead when it cannot.
func (m *Mux) followPreview(ctx context.Context, name string, rows, cols int) string {
	lctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	runID, err := m.shownRun(lctx, name)
	cancel()
	if err != nil || runID == "" {
		if ctx.Err() != nil {
			return ""
		}
		if err != nil {
			return "no preview: " + err.Error()
		}
		return "no window to preview"
	}
	actx, cancel := context.WithTimeout(ctx, 15*time.Second)
	// No size: an attach with one would resize the other session's
	// shell. The snapshot comes at the size it has.
	conn, cut, err := m.lux.Attach(actx, runID, true, 0, 0)
	cancel()
	if err != nil {
		if ctx.Err() != nil {
			return ""
		}
		return "no preview: " + err.Error()
	}
	stop := context.AfterFunc(ctx, cut)
	defer stop()
	defer conn.CloseNow()
	conn.SetReadLimit(8 << 20)
	emu := newEmulator(rows, cols)
	p := &m.preview
	for {
		var d api.StreamData
		if err := wsjson.Read(ctx, conn, &d); err != nil {
			if ctx.Err() != nil || websocket.CloseStatus(err) == websocket.StatusNormalClosure {
				return ""
			}
			return "preview stream ended"
		}
		if d.Error != "" {
			return "no preview: " + d.Error
		}
		if len(d.Data) == 0 {
			continue
		}
		p.mu.Lock()
		emu.Write(d.Data)
		if p.emu == nil && p.name == name {
			p.emu = emu
		}
		p.mu.Unlock()
	}
}

// shownRun is the Run of session name's shown window: its file's active
// window, else the first of its Runs.
func (m *Mux) shownRun(ctx context.Context, name string) (string, error) {
	if s, ok, err := state.Load(m.opt.StateDir, name); err != nil {
		return "", err
	} else if ok && len(s.Windows) > 0 {
		return s.Windows[clamp(s.Active, len(s.Windows))].RunID, nil
	}
	runs, err := sessions.LiveRuns(ctx, m.lux, m.opt.StateDir, name)
	if err != nil {
		return "", err
	}
	label, _ := sessions.Label(m.opt.StateDir, name)
	s := state.RebuildLabelled(name, label, runs, nil)
	if len(s.Windows) == 0 {
		return "", nil
	}
	return s.Windows[0].RunID, nil
}

// stopSessionPreview ends the preview's stream and waits for its reader.
func (m *Mux) stopSessionPreview() {
	p := &m.preview
	p.mu.Lock()
	cancel, done := p.cancel, p.done
	p.name, p.msg, p.emu, p.cancel, p.done = "", "", nil, nil, nil
	p.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
}
