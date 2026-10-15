package mux

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/marcioapm/lux/api"

	"github.com/marcioapm/lux-mux/internal/config"
	"github.com/marcioapm/lux-mux/internal/luxapi"
	"github.com/marcioapm/lux-mux/internal/screen"
)

// snapshotPrefix is how a restore snapshot starts (vterm.Snapshot resets
// the cursor's visibility and leaves the alternate screen first).
var snapshotPrefix = []byte("\x1b[?25l\x1b[?1049l")

// Stream end messages from luxd and the shim.
const (
	errWorkloadExited = "the workload exited"
	// A restore attach whose reader fell behind is ended by newer hosts
	// with this; it is a dropped stream like any other: attach again,
	// with restore.
	errFellBehind = "attach fell behind; attach again"
)

// attachEnd is how one attach ended.
type attachEnd int

const (
	endNotAttached attachEnd = iota // the attach itself failed
	endDropped                      // attached, then the stream went away
	endExited                       // the workload exited
	endStopped                      // the window was closed or the session detached
)

func (m *Mux) startSupervisor(w *Window) {
	ctx, cancel := context.WithCancel(m.ctx)
	w.cancel = cancel
	m.wg.Add(1)
	m.goSafe(func() {
		defer m.wg.Done()
		m.supervise(ctx, w)
	})
}

// supervise follows w's Run until the window closes: it attaches while
// the Run runs, shows its progress while it starts or moves, resumes it
// when lux stopped it for its secrets or lost it, and otherwise shows how
// it ended: a stopped Run waits for r to resume it, an ended one for r to
// relaunch.
func (m *Mux) supervise(ctx context.Context, w *Window) {
	bo := m.opt.Backoff
	wait := func(d time.Duration) {
		select {
		case <-ctx.Done():
		case <-time.After(d):
		}
	}
	grow := func() { bo = min(2*bo, m.opt.MaxBackoff) }
	// exitedEpoch is the epoch whose last stream ended with the shim's
	// "the workload exited" (0: none since the last attach). Only that,
	// and lux's exit-code reason for the same epoch, is the shell's exit:
	// the shim sends it for any stop of the workload (a move, a timeout).
	exitedEpoch := 0
	for ctx.Err() == nil {
		gctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		run, err := m.lux.Get(gctx, w.RunID)
		cancel()
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			if luxapi.IsStatus(err, http.StatusNotFound) {
				m.markDead(w, "lux has no such Run")
				m.waitClosed(ctx, w)
				return
			}
			m.setWaiting(w, "luxd unreachable: "+luxapi.Describe(err))
			wait(bo)
			grow()
			continue
		}
		if run.Epoch != exitedEpoch {
			exitedEpoch = 0
		}
		m.noteRun(w, run)
		switch run.State {
		case "running":
			if exitedEpoch != 0 {
				// The shell exited; lux is ending the Run.
				m.setWaiting(w, "shell exited")
				wait(bo)
				grow()
				continue
			}
			end, why := m.attachOnce(ctx, w, run)
			switch end {
			case endStopped:
				return
			case endExited:
				exitedEpoch = run.Epoch
				bo = m.opt.Backoff
				m.setWaiting(w, "shell exited")
				wait(bo)
			case endDropped:
				// Attached and then lost: try again at once, then back off.
				bo = m.opt.Backoff
				if m.takeRefreshing(w) {
					continue
				}
				m.setWaiting(w, why)
				wait(bo)
			default:
				m.setWaiting(w, why)
				wait(bo)
				grow()
			}
		case "stopped", "lost":
			if !run.Resumable {
				m.markDead(w, stateLine(run.State, "", run.StateReason))
				m.waitClosed(ctx, w)
				return
			}
			if !autoResume(run) && !m.takeResumeAsked(w) {
				// Stopped by a person (lux stop, the console): r resumes.
				m.markStopped(w, stoppedLine(run))
				select {
				case <-ctx.Done():
				case <-w.notify:
				case <-time.After(m.opt.Poll):
				}
				continue
			}
			if err := m.resumeRun(ctx, w, run); err != nil {
				m.markDead(w, "cannot resume: "+err.Error())
				m.waitClosed(ctx, w)
				return
			}
			bo = m.opt.Backoff
		case "succeeded", "failed":
			if run.State == "succeeded" || (exitedEpoch == run.Epoch && strings.HasPrefix(run.StateReason, "exit code")) {
				m.shellExited(w)
				return
			}
			m.markDead(w, stateLine(run.State, "", run.StateReason))
			m.waitClosed(ctx, w)
			return
		case "terminated":
			reason := run.StateReason
			if reason == "" {
				reason = "terminated"
			}
			m.markDead(w, reason)
			m.waitClosed(ctx, w)
			return
		default:
			// submitted, provisioning, scheduled, starting, resuming,
			// stopping: wait for the next event, or poll, often at first
			// (the feed misses what happened before it connected).
			select {
			case <-ctx.Done():
			case <-w.notify:
			case <-time.After(min(bo, m.opt.Poll)):
			}
			grow()
		}
	}
}

// secretsLostReason is luxd's stateReason for a Run it stopped because no
// luxd held its secret values any more.
const secretsLostReason = "secrets must be supplied again: resume with them"

// autoResume reports whether lux, not a person, left run stopped or lost
// in a way luxmux resumes without asking: its secrets must be supplied
// again, or its host was lost.
func autoResume(run *api.Run) bool {
	return run.State == "lost" || strings.HasPrefix(run.StateReason, secretsLostReason)
}

// stoppedLine says who stopped a Run.
func stoppedLine(run *api.Run) string {
	switch r := strings.TrimSpace(run.StateReason); {
	case r == "" || r == "stop":
		return "stopped by lux stop"
	default:
		return "stopped by " + r
	}
}

// takeResumeAsked reports (and forgets) that r asked to resume w's Run.
func (m *Mux) takeResumeAsked(w *Window) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	asked := w.resumeAsked
	w.resumeAsked = false
	return asked
}

// markStopped shows that w's Run is stopped and r resumes it.
func (m *Mux) markStopped(w *Window, why string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if w.closed || (w.phase == phaseDead && w.resumable && w.reason == why) {
		return
	}
	w.phase = phaseDead
	w.resumable = true
	w.reason = why
	m.forgetStaleScreenLocked(w)
	if m.activeLocked() == w {
		m.repaintLocked()
	} else {
		m.setNoticeLocked(fmt.Sprintf("window %d %s", m.indexLocked(w), why))
	}
}

// resume asks w's supervisor to resume its stopped Run (the r key).
func (m *Mux) resume(w *Window) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if w.closed || !w.resumable {
		return
	}
	w.resumable = false
	w.resumeAsked = true
	w.phase = phaseReconnecting
	if !w.hasScreen {
		w.phase = phaseStarting
	}
	w.reason = "resuming"
	m.repaintLocked()
	w.poke()
}

// takeRefreshing reports (and forgets) that w's stream was closed to
// rebuild its screen: that stream's end is no news, a failure to attach
// again is.
func (m *Mux) takeRefreshing(w *Window) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := w.refreshing
	w.refreshing = false
	return r
}

// waitClosed parks a dead window's supervisor until the window closes or
// is relaunched (which starts a new supervisor).
func (m *Mux) waitClosed(ctx context.Context, w *Window) { <-ctx.Done() }

// resumeRun resumes a stopped or lost Run with the secrets lux lists for
// it, their values found as the window's profile says (lux keeps none).
func (m *Mux) resumeRun(ctx context.Context, w *Window, run *api.Run) error {
	m.setWaiting(w, "resuming "+run.State+" Run")
	names := make([]string, 0, len(run.Secrets))
	for _, s := range run.Secrets {
		names = append(names, s.Name)
	}
	m.mu.Lock()
	spec := w.Spec
	m.mu.Unlock()
	vals, err := spec.ResolveNamed(names, m.opt.Lookup)
	if err != nil {
		return err
	}
	secrets := config.SecretValues(vals)
	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	err = m.lux.Resume(rctx, run.ID, secrets)
	if err != nil && luxapi.IsStatus(err, http.StatusConflict) {
		// Resuming already, or its state moved on: the loop reads it again.
		return nil
	}
	if err != nil {
		return fmt.Errorf("%s", luxapi.Describe(err))
	}
	return nil
}

// attachOnce attaches to w's running Run with restore and pumps its
// output until the stream ends.
func (m *Mux) attachOnce(ctx context.Context, w *Window, run *api.Run) (attachEnd, string) {
	m.mu.Lock()
	rows, cols := m.rows, m.cols
	m.mu.Unlock()
	actx, cancel := context.WithTimeout(ctx, 30*time.Second)
	conn, err := m.lux.Attach(actx, run.ID, true, rows, cols)
	cancel()
	if err != nil {
		if ctx.Err() != nil {
			return endStopped, ""
		}
		return endNotAttached, "attach: " + luxapi.Describe(err)
	}
	conn.SetReadLimit(8 << 20)
	defer func() {
		w.setConn(nil)
		conn.CloseNow()
	}()
	discarded, err := m.goLive(ctx, w, conn, run.Epoch)
	if err != nil {
		m.inputDiscarded(w, discarded)
		if ctx.Err() != nil {
			return endStopped, ""
		}
		return endDropped, "stream lost"
	}
	frames := make(chan api.StreamData, 64)
	readErr := make(chan error, 1)
	m.goSafe(func() {
		defer close(frames)
		for {
			var d api.StreamData
			if err := wsjson.Read(ctx, conn, &d); err != nil {
				readErr <- err
				return
			}
			select {
			case frames <- d:
			case <-ctx.Done():
				return
			}
		}
	})

	// The first frame says what the host does. A host with restore sends
	// the snapshot (starting with snapshotPrefix), then one frame marked
	// Restored where live output starts; with no snapshot to send (the
	// screen over its limit, or its emulator gone) the marker comes
	// first. A host from before restore sends neither: live output, or
	// nothing within RestoreWait.
	var first *api.StreamData
	select {
	case <-ctx.Done():
		return endStopped, ""
	case d, ok := <-frames:
		if ok {
			first = &d
		}
	case <-time.After(m.opt.RestoreWait):
	}
	if first != nil && first.Data == nil && !first.Restored {
		m.inputDiscarded(w, discarded)
		return m.streamEnd(ctx, first, readErr)
	}
	restoring := first != nil && bytes.HasPrefix(first.Data, snapshotPrefix)
	raw := m.beginAttach(w, run, restoring)
	// After beginAttach's notice of the move, which this one replaces.
	m.inputDiscarded(w, discarded)
	if !restoring {
		// Nothing to redraw from: ask the shell to redraw itself by
		// changing the size and back.
		_ = wsjson.Write(ctx, conn, api.StreamData{Rows: max(rows-1, 1), Cols: cols})
		_ = wsjson.Write(ctx, conn, api.StreamData{Rows: rows, Cols: cols})
	}
	// quiet: the snapshot goes to the emulator only, until the marker.
	quiet := restoring && !raw
	handle := func(d api.StreamData) (attachEnd, string, bool) {
		if d.Error != "" || d.ExitCode != nil {
			end, why := m.streamEnd(ctx, &d, readErr)
			return end, why, true
		}
		if d.Restored {
			if quiet {
				quiet = false
				m.restored(w)
			}
			return 0, "", false
		}
		if len(d.Data) == 0 {
			return 0, "", false
		}
		if quiet {
			m.outputQuiet(w, d.Data)
			return 0, "", false
		}
		m.output(w, d.Data)
		return 0, "", false
	}
	if first != nil {
		if end, why, done := handle(*first); done {
			return end, why
		}
	}
	for d := range frames {
		if end, why, done := handle(d); done {
			return end, why
		}
	}
	if quiet {
		m.restored(w)
	}
	return m.streamEnd(ctx, nil, readErr)
}

// streamEnd classifies a stream's end: its last message, or its read error.
func (m *Mux) streamEnd(ctx context.Context, last *api.StreamData, readErr chan error) (attachEnd, string) {
	if ctx.Err() != nil {
		return endStopped, ""
	}
	if last != nil {
		switch {
		case last.ExitCode != nil, last.Error == errWorkloadExited:
			return endExited, ""
		case last.Error == errFellBehind:
			return endDropped, "stream fell behind"
		case last.Error != "":
			return endDropped, last.Error
		}
	}
	select {
	case err := <-readErr:
		if websocket.CloseStatus(err) == websocket.StatusNormalClosure {
			return endDropped, "stream closed"
		}
		return endDropped, "stream lost"
	default:
	}
	return endDropped, "stream closed"
}

// beginAttach readies w for a new stream: a blank emulator for the
// snapshot (or for whatever a host without restore sends), and the window
// live. It reports whether the snapshot goes to the terminal as it comes
// (raw): only on the first attach of the shown window in this process,
// so the restored history lands in the terminal's scrollback once.
func (m *Mux) beginAttach(w *Window, run *api.Run, restoring bool) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if w.closed {
		return false
	}
	moved := w.hasScreen && w.epoch != 0 && run.Epoch != w.epoch
	if run.Epoch != w.epoch {
		// A new placement runs a new shell, which set no keyboard modes.
		// On the same one the program keeps those it set: the snapshot
		// does not carry them.
		w.resetKeysLocked()
	}
	firstHere := !w.attachedOnce
	w.attachedOnce = true
	w.epoch = run.Epoch
	w.phase = phaseLive
	w.reason = ""
	w.refreshing = false
	w.q.restart()
	w.q.push(feedItem{rows: m.rows, cols: m.cols})
	w.tail.Reset()
	w.hasScreen = true
	active := m.showingLocked(w)
	raw := restoring && firstHere && active
	if raw {
		m.shown = w
	}
	if !restoring && active {
		m.repaintLocked()
	}
	if moved {
		m.setNoticeLocked(fmt.Sprintf("window %d: new shell (the Run moved)", m.indexLocked(w)))
	} else if !firstHere && !restoring && active {
		m.setNoticeLocked(fmt.Sprintf("window %d reconnected", m.indexLocked(w)))
	}
	return raw
}

// outputQuiet feeds w's emulator only: a snapshot being restored off screen.
func (m *Mux) outputQuiet(w *Window, data []byte) {
	changed := w.trackKeys(data)
	m.mu.Lock()
	defer m.mu.Unlock()
	if w.closed {
		return
	}
	w.tail.Add(data)
	if changed {
		w.publishKeysLocked()
	}
	w.q.push(feedItem{data: data})
}

// restored repaints w from its emulator once its snapshot is in, if shown.
func (m *Mux) restored(w *Window) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !w.closed && m.activeLocked() == w {
		m.repaintLocked()
	}
}

// pendingInLimit bounds the keystrokes kept for a window without a
// stream; past it the oldest are dropped, with a notice.
const pendingInLimit = 64 << 10

// send writes keystrokes to w's stream. While w has none (it starts,
// reconnects or is being refreshed) they are kept and sent, in order,
// once its next stream is up (goLive); a write that fails because the
// stream just closed is kept the same way.
func (m *Mux) send(w *Window, b []byte) {
	w.connMu.Lock()
	c, ep := w.conn, w.inEpoch
	if c == nil || len(w.pendingIn) > 0 {
		dropped := w.keepInputLocked(b)
		w.connMu.Unlock()
		m.inputDropped(w, dropped)
		return
	}
	w.connMu.Unlock()
	ctx, cancel := context.WithTimeout(m.ctx, 10*time.Second)
	err := wsjson.Write(ctx, c, api.StreamData{Data: b})
	cancel()
	if err != nil && m.ctx.Err() == nil {
		w.connMu.Lock()
		// Ahead of anything kept meanwhile: typed before it, for c's shell.
		w.pendingIn = append(append([]byte{}, b...), w.pendingIn...)
		w.pendingEpoch = ep
		dropped := w.trimInputLocked()
		// A stream that came up meanwhile has sent what was kept; it
		// sends this too before it takes more (goLive).
		next, nextEp := w.conn, w.inEpoch
		if next == c {
			next = nil
		}
		w.conn = nil
		w.connMu.Unlock()
		m.inputDropped(w, dropped)
		if next != nil {
			n, _ := m.goLive(m.ctx, w, next, nextEp)
			m.inputDiscarded(w, n)
		}
	}
}

// keepInputLocked keeps b for the next stream; it returns how many of
// the oldest bytes kept were dropped for room.
func (w *Window) keepInputLocked(b []byte) int {
	if len(w.pendingIn) == 0 {
		w.pendingEpoch = w.inEpoch
	}
	w.pendingIn = append(w.pendingIn, b...)
	return w.trimInputLocked()
}

func (w *Window) trimInputLocked() int {
	n := len(w.pendingIn) - pendingInLimit
	if n <= 0 {
		return 0
	}
	w.pendingIn = append(w.pendingIn[:0], w.pendingIn[n:]...)
	return n
}

func (m *Mux) inputDropped(w *Window, n int) {
	if n == 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.setNoticeLocked(fmt.Sprintf("window %d: %d typed bytes dropped while it had no stream", m.indexLocked(w), n))
}

// inputDiscarded tells that n kept keystrokes were for a shell the Run no
// longer has.
func (m *Mux) inputDiscarded(w *Window, n int) {
	if n == 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.setNoticeLocked(fmt.Sprintf("window %d: %d typed bytes discarded — it moved to a new shell", m.indexLocked(w), n))
}

// goLive makes c, a stream of the Run's epoch, w's stream: the keystrokes
// kept for it are sent first, in order, and c takes new ones only once
// none are left. Keystrokes typed for another epoch's shell (the Run
// moved: a new placement, a fresh shell) are discarded instead; it
// returns how many.
func (m *Mux) goLive(ctx context.Context, w *Window, c *websocket.Conn, epoch int) (discarded int, err error) {
	w.connMu.Lock()
	w.inEpoch = epoch
	if len(w.pendingIn) > 0 && w.pendingEpoch != 0 && w.pendingEpoch != epoch {
		discarded = len(w.pendingIn)
		w.pendingIn = nil
	}
	w.connMu.Unlock()
	for {
		w.connMu.Lock()
		b := w.pendingIn
		if len(b) == 0 {
			w.conn = c
			w.connMu.Unlock()
			return discarded, nil
		}
		w.pendingIn = nil
		w.connMu.Unlock()
		wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err = wsjson.Write(wctx, c, api.StreamData{Data: b})
		cancel()
		if err != nil {
			w.connMu.Lock()
			w.pendingIn = append(b, w.pendingIn...)
			w.pendingEpoch = epoch
			dropped := w.trimInputLocked()
			w.connMu.Unlock()
			m.inputDropped(w, dropped)
			return discarded, err
		}
	}
}

// dropInput forgets keystrokes kept for w's Run: it is replaced.
func (w *Window) dropInput() {
	w.connMu.Lock()
	w.pendingIn = nil
	w.inEpoch, w.pendingEpoch = 0, 0
	w.connMu.Unlock()
}

// noteRun records what a Run read says about it.
func (m *Mux) noteRun(w *Window, run *api.Run) {
	m.mu.Lock()
	defer m.mu.Unlock()
	w.runState = run.State
	if w.phase == phaseStarting {
		w.status = stateLine(run.State, run.Stage, "")
		w.steps.note(stageOrState(run.State, run.Stage))
		if m.activeLocked() == w {
			m.repaintLocked()
		}
	}
}

func stageOrState(state, stage string) string {
	if s, ok := stageWords[stage]; ok {
		return s
	}
	return state
}

// setWaiting shows why w has no live stream: the progress screen of a
// window never attached, or the reconnecting line of one that was.
func (m *Mux) setWaiting(w *Window, why string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if w.closed {
		return
	}
	if w.phase == phaseLive {
		w.phase = phaseReconnecting
	}
	if w.reason == why {
		return
	}
	w.reason = why
	if m.activeLocked() == w {
		m.repaintLocked()
	}
}

// markDead shows that w's Run ended for good.
func (m *Mux) markDead(w *Window, why string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if w.closed {
		return
	}
	w.phase = phaseDead
	w.resumable = false
	w.reason = why
	w.dropInput()
	m.forgetStaleScreenLocked(w)
	if m.activeLocked() == w {
		m.repaintLocked()
	} else {
		m.setNoticeLocked(fmt.Sprintf("window %d ended: %s", m.indexLocked(w), why))
	}
}

// forgetStaleScreenLocked drops the screen of a window that will not be
// attached again until r: a stale emulator (see feedLimit) could only be
// rebuilt by an attach, so the window shows its status screen instead,
// with the r/& offer, prompts and notices. The attach after r restores it.
func (m *Mux) forgetStaleScreenLocked(w *Window) {
	if w.q.isStale() {
		w.hasScreen = false
	}
}

// shellExited closes a window whose shell exited, terminating its Run.
func (m *Mux) shellExited(w *Window) {
	m.mu.Lock()
	idx := m.indexLocked(w)
	m.mu.Unlock()
	m.closeWindow(w, true, fmt.Sprintf("window %d exited", idx))
}

// followEvents follows the tenant's event feed and routes each event of
// a window's Run to it: progress while it starts, a wake-up for its
// supervisor whenever its state or stage changes.
func (m *Mux) followEvents() {
	defer m.wg.Done()
	var after int64
	bo := m.opt.Backoff
	for m.ctx.Err() == nil {
		start := time.Now()
		_ = m.lux.Feed(m.ctx, after, func(e api.FeedEvent) {
			after = e.ID
			if e.RunID == nil {
				return
			}
			m.event(*e.RunID, e.Event)
		})
		if m.ctx.Err() != nil {
			return
		}
		if time.Since(start) > time.Minute {
			bo = m.opt.Backoff
		}
		select {
		case <-m.ctx.Done():
		case <-time.After(bo):
		}
		bo = min(2*bo, m.opt.MaxBackoff)
	}
}

func (m *Mux) event(runID string, e api.Event) {
	if e.Type != "state" && e.Type != "stage" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	w := m.byRun[runID]
	if w == nil {
		return
	}
	str := func(k string) string { s, _ := e.Data[k].(string); return s }
	if w.phase == phaseStarting {
		if e.Type == "stage" {
			w.steps.note(stageOrState("", str("stage")))
		} else {
			w.steps.note(str("state"))
			w.status = str("state")
		}
		if m.activeLocked() == w {
			m.repaintLocked()
		}
	} else if w.phase == phaseReconnecting && e.Type == "stage" {
		w.reason = stageOrState("", str("stage"))
		if m.activeLocked() == w {
			m.repaintLocked()
		}
	}
	w.poke()
}

// Resize takes the terminal's new size: every emulator and every stream.
func (m *Mux) Resize(rows, cols int) {
	rows, cols = clampSize(rows, cols)
	m.mu.Lock()
	if rows == m.rows && cols == m.cols {
		m.mu.Unlock()
		return
	}
	m.rows, m.cols = rows, cols
	var conns []*websocket.Conn
	for _, w := range m.windows {
		if !w.closed {
			w.q.push(feedItem{rows: rows, cols: cols})
		}
		if c := w.getConn(); c != nil {
			conns = append(conns, c)
		}
	}
	m.repaintLocked()
	m.mu.Unlock()
	for _, c := range conns {
		ctx, cancel := context.WithTimeout(m.ctx, 5*time.Second)
		_ = wsjson.Write(ctx, c, api.StreamData{Rows: rows, Cols: cols})
		cancel()
	}
}

// Suspend stops writing to the terminal while a dialog owns it.
func (m *Mux) suspend(on bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.suspended = on
	if on {
		// The dialog reads keys as a terminal sends them without the
		// shown window's keyboard modes.
		m.tw.enqueue(screen.ResetKeys(nil))
	} else {
		m.repaintLocked()
	}
}
