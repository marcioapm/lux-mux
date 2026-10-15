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
// the cursor's visibility and leaves the alternate screen first): the
// first frame of an attach tells a host with restore from one without.
var snapshotPrefix = []byte("\x1b[?25l\x1b[?1049l")

// streamChunk is the largest output frame lux-shim sends: a snapshot
// arrives as full chunks and a shorter last one.
const streamChunk = 32 << 10

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
	go func() {
		defer m.wg.Done()
		m.supervise(ctx, w)
	}()
}

// supervise follows w's Run until the window closes: it attaches while
// the Run runs, shows its progress while it starts or moves, resumes it
// with its secrets when it is stopped or lost, and marks the window dead
// when the Run ended for good.
func (m *Mux) supervise(ctx context.Context, w *Window) {
	bo := m.opt.Backoff
	wait := func(d time.Duration) {
		select {
		case <-ctx.Done():
		case <-time.After(d):
		}
	}
	grow := func() { bo = min(2*bo, m.opt.MaxBackoff) }
	exiting := false
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
		m.noteRun(w, run)
		switch run.State {
		case "running":
			end, why := m.attachOnce(ctx, w, run)
			switch end {
			case endStopped:
				return
			case endExited:
				exiting = true
				m.setWaiting(w, "shell exited")
				wait(m.opt.Backoff)
			case endDropped:
				// Attached and then lost: try again at once, then back off.
				bo = m.opt.Backoff
				m.setWaiting(w, why)
				wait(bo)
			default:
				if exiting {
					m.setWaiting(w, "shell exited")
				} else {
					m.setWaiting(w, why)
				}
				wait(bo)
				grow()
			}
		case "stopped", "lost":
			if !run.Resumable {
				m.markDead(w, stateLine(run.State, "", run.StateReason))
				m.waitClosed(ctx, w)
				return
			}
			if err := m.resumeRun(ctx, w, run); err != nil {
				m.markDead(w, "cannot resume: "+err.Error())
				m.waitClosed(ctx, w)
				return
			}
			bo = m.opt.Backoff
		case "succeeded", "failed":
			if run.State == "succeeded" || strings.HasPrefix(run.StateReason, "exit code") || exiting {
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

// waitClosed parks a dead window's supervisor until the window closes or
// is relaunched (which starts a new supervisor).
func (m *Mux) waitClosed(ctx context.Context, w *Window) { <-ctx.Done() }

// resumeRun resumes a stopped or lost Run with every one of its secrets,
// their values found as the profile says (lux keeps none).
func (m *Mux) resumeRun(ctx context.Context, w *Window, run *api.Run) error {
	m.setWaiting(w, "resuming "+run.State+" Run")
	var secrets []api.Secret
	if len(run.Secrets) > 0 {
		vals, err := w.Spec.ResolveSecrets(m.opt.Lookup)
		if err != nil {
			return err
		}
		have := map[string]string{}
		var missing []string
		for _, s := range run.Secrets {
			v, ok := vals[s.Name]
			if !ok {
				if v, ok = m.opt.Lookup(s.Name); !ok {
					missing = append(missing, s.Name)
					continue
				}
			}
			have[s.Name] = v
		}
		if len(missing) > 0 {
			return fmt.Errorf("no value for its secrets %s", strings.Join(missing, ", "))
		}
		secrets = config.SecretValues(have)
	}
	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	err := m.lux.Resume(rctx, run.ID, secrets)
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
	w.setConn(conn)
	defer func() {
		w.setConn(nil)
		conn.CloseNow()
	}()
	frames := make(chan api.StreamData, 64)
	readErr := make(chan error, 1)
	go func() {
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
	}()

	// The first frame says whether the host restores: a snapshot starts
	// with snapshotPrefix. None within RestoreWait, or live output first:
	// a host from before restore.
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
	if first != nil && first.Data == nil {
		return m.streamEnd(ctx, first, readErr)
	}
	restoring := first != nil && bytes.HasPrefix(first.Data, snapshotPrefix)
	raw := m.beginAttach(w, run, restoring)
	if !restoring {
		// Nothing to redraw from: ask the shell to redraw itself by
		// changing the size and back.
		_ = wsjson.Write(ctx, conn, api.StreamData{Rows: max(rows-1, 1), Cols: cols})
		_ = wsjson.Write(ctx, conn, api.StreamData{Rows: rows, Cols: cols})
	}
	quiet := restoring && !raw
	handle := func(d api.StreamData) (attachEnd, string, bool) {
		if d.Error != "" || d.ExitCode != nil {
			end, why := m.streamEnd(ctx, &d, readErr)
			return end, why, true
		}
		if len(d.Data) == 0 {
			return 0, "", false
		}
		if quiet {
			m.outputQuiet(w, d.Data)
			if len(d.Data) < streamChunk {
				// The snapshot's last chunk: show the window as restored.
				quiet = false
				m.restored(w)
			}
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
	firstHere := !w.attachedOnce
	w.attachedOnce = true
	w.epoch = run.Epoch
	w.phase = phaseLive
	w.reason = ""
	w.feed <- feedItem{reset: true}
	w.feed <- feedItem{rows: m.rows, cols: m.cols}
	w.tail.Reset()
	w.hasScreen = true
	active := m.activeLocked() == w && !m.suspended
	raw := restoring && firstHere && active
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
	m.mu.Lock()
	defer m.mu.Unlock()
	if w.closed {
		return
	}
	w.tail.Add(data)
	w.feed <- feedItem{data: data}
}

// restored repaints w from its emulator once its snapshot is in, if shown.
func (m *Mux) restored(w *Window) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !w.closed && m.activeLocked() == w {
		m.repaintLocked()
	}
}

// send writes keystrokes to w's stream.
func (m *Mux) send(w *Window, b []byte) {
	c := w.getConn()
	if c == nil {
		return
	}
	ctx, cancel := context.WithTimeout(m.ctx, 10*time.Second)
	_ = wsjson.Write(ctx, c, api.StreamData{Data: b})
	cancel()
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
	w.reason = why
	if m.activeLocked() == w {
		m.repaintLocked()
	} else {
		m.setNoticeLocked(fmt.Sprintf("window %d ended: %s", m.indexLocked(w), why))
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
	m.mu.Lock()
	if rows == m.rows && cols == m.cols {
		m.mu.Unlock()
		return
	}
	m.rows, m.cols = rows, cols
	var conns []*websocket.Conn
	for _, w := range m.windows {
		if !w.closed {
			w.feed <- feedItem{rows: rows, cols: cols}
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
	if !on {
		m.repaintLocked()
	}
}

var screenText = screen.Text
