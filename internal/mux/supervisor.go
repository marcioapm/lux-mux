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
	// A key that cannot be read now (its variable unset in this shell, its
	// file unreadable) says nothing of the Run: the window waits, as for a
	// lux that does not answer, and never ends on it.
	lux, err := m.eps.Client(w.Endpoint)
	for err != nil {
		m.setWaiting(w, err.Error())
		wait(bo)
		grow()
		if ctx.Err() != nil {
			return
		}
		lux, err = m.eps.Client(w.Endpoint)
	}
	bo = m.opt.Backoff
	// exitedEpoch is the epoch whose last stream ended with the shim's
	// "the workload exited" (0: none since the last attach). Only that,
	// and lux's exit-code reason for the same epoch, is the shell's exit:
	// the shim sends it for any stop of the workload (a move, a timeout).
	exitedEpoch := 0
	// dropWait: the wait before attaching again after lux or the network
	// ended a stream.
	dropWait := m.opt.Backoff
	for ctx.Err() == nil {
		gctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		run, err := lux.Get(gctx, w.RunID)
		cancel()
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			m.inputDropped(w, w.releaseHold())
			if luxapi.IsStatus(err, http.StatusNotFound) {
				m.markDead(w, "lux has no such Run")
				m.waitClosed(ctx, w)
				return
			}
			m.setWaiting(w, luxapi.FailedWord(err)+" ("+w.Endpoint.Label()+"): "+luxapi.Describe(err))
			wait(bo)
			grow()
			continue
		}
		if run.Epoch != exitedEpoch {
			exitedEpoch = 0
		}
		m.noteRun(w, run)
		if run.State != "running" || exitedEpoch != 0 {
			m.inputDropped(w, w.releaseHold())
		}
		switch run.State {
		case "running":
			if exitedEpoch != 0 {
				// The shell exited; lux is ending the Run.
				m.setWaiting(w, "shell exited")
				wait(bo)
				grow()
				continue
			}
			start := time.Now()
			end, why := m.attachOnce(ctx, lux, w, run)
			refreshed := end == endDropped && m.takeRefreshing(w)
			if !refreshed {
				// Keys a refresh held wait for no stream now.
				m.inputDropped(w, w.releaseHold())
			}
			switch end {
			case endStopped:
				return
			case endExited:
				exitedEpoch = run.Epoch
				bo = m.opt.Backoff
				m.setWaiting(w, "shell exited")
				wait(bo)
			case endDropped:
				bo = m.opt.Backoff
				if refreshed {
					// Closed by refreshLocked, which keeps to refreshEvery.
					continue
				}
				// Ended by lux or the network: attach again after Backoff,
				// doubling up to refreshEvery (or MaxBackoff if less)
				// while streams keep ending soon after attaching: a host
				// that ends every restore attach would otherwise cost it
				// a snapshot each Backoff.
				if time.Since(start) >= m.opt.streamStable {
					dropWait = m.opt.Backoff
				}
				m.setWaiting(w, why)
				wait(dropWait)
				dropWait = min(2*dropWait, refreshEvery, m.opt.MaxBackoff)
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
			if err := m.resumeRun(ctx, lux, w, run); err != nil {
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

// noteAttach records that w's stream was just opened, for refreshLocked's
// limit.
func (m *Mux) noteAttach(w *Window) {
	m.mu.Lock()
	w.lastAttach = time.Now()
	m.mu.Unlock()
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
func (m *Mux) resumeRun(ctx context.Context, lux *luxapi.Client, w *Window, run *api.Run) error {
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
	err = lux.Resume(rctx, run.ID, secrets)
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
func (m *Mux) attachOnce(ctx context.Context, lux *luxapi.Client, w *Window, run *api.Run) (attachEnd, string) {
	m.mu.Lock()
	rows, cols := m.rows, m.cols
	m.mu.Unlock()
	actx, cancel := context.WithTimeout(ctx, 30*time.Second)
	conn, cut, err := lux.Attach(actx, run.ID, true, rows, cols)
	cancel()
	if err != nil {
		if ctx.Err() != nil {
			return endStopped, ""
		}
		return endNotAttached, "attach: " + luxapi.Describe(err)
	}
	m.noteAttach(w)
	conn.SetReadLimit(8 << 20)
	defer func() {
		dropped := w.endStream(conn)
		conn.CloseNow()
		m.inputDropped(w, dropped)
	}()
	discarded := w.setStream(conn, cut, run.Epoch)
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
	// Keys go out only now: lux ends an attach it cannot stream with an
	// error as its first frame, before it reads any input.
	w.streamUp(conn)
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
			switch {
			case quiet:
				quiet = false
				m.restored(w)
			case raw:
				m.rawRestored(w)
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
	m.sendNameLocked(w)
	// A Run named as luxmux named every Run before window names reached
	// lux, or a claimed spare still named as one, is renamed; a name set
	// on lux by hand is left as it is.
	if (run.Name == "luxmux-"+m.label || run.Name == m.spareRunNameLocked()) && run.Name != m.runNameLocked(w) {
		w.runNamePending = true
	}
	m.sendRunNameLocked(w)
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

// rawRestored draws the bottom-row overlay again once a snapshot written
// straight to the terminal is in: the snapshot cleared whatever notice
// or prompt was drawn before or during it.
func (m *Mux) rawRestored(w *Window) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !w.closed && m.showingLocked(w) && m.shown == w && m.overlayLineLocked(w) != "" {
		m.drawOverlayLocked()
	}
}

// pendingInLimit bounds the keys a window keeps while it has no stream:
// past it the oldest are dropped (those of a write in flight aside), with
// a notice.
const pendingInLimit = 64 << 10

// liveInLimit bounds a window's input queue while its stream is up, or
// held across a refresh: past it send waits for the key writer to drain
// it, so nothing typed is dropped and the queue holds at most liveInLimit
// plus one read. Keys typed after a paste (the prefix, a switch) act once
// the paste's tail is queued, while it is still being delivered.
const liveInLimit = 256 << 10

// send queues keystrokes for w's key writer. It never writes to a stream:
// the writer alone does, so keys reach the shell in the order typed.
func (m *Mux) send(w *Window, b []byte) {
	w.connMu.Lock()
	for (w.connUp || w.inHold) && len(w.in) >= liveInLimit {
		w.connMu.Unlock()
		select {
		case <-w.inRoom:
		case <-w.inStop:
			return
		case <-m.ctx.Done():
			return
		}
		w.connMu.Lock()
	}
	w.in = append(w.in, b...)
	dropped := 0
	if !w.connUp && !w.inHold {
		dropped = w.trimInputLocked()
	}
	w.connMu.Unlock()
	w.wakeWriter()
	m.inputDropped(w, dropped)
}

// roomLocked wakes a send waiting for w's queue to drain or its stream
// to go.
func (w *Window) roomLocked() {
	select {
	case w.inRoom <- struct{}{}:
	default:
	}
}

// trimInputLocked drops the oldest queued bytes past pendingInLimit,
// sparing those being written; it returns how many it dropped.
func (w *Window) trimInputLocked() int {
	n := len(w.in) - w.inFlight - pendingInLimit
	if n <= 0 {
		return 0
	}
	w.in = append(w.in[:w.inFlight], w.in[w.inFlight+n:]...)
	return n
}

// lostStreamLocked handles w's stream c no longer taking keys. If luxmux
// closed it for a refresh, the queue is held as if live until the next
// stream is up (or fails: releaseHold); otherwise it is bounded to
// pendingInLimit. It returns how many bytes that dropped.
func (w *Window) lostStreamLocked(c *websocket.Conn) int {
	if w.conn == c {
		w.connUp = false
	}
	w.roomLocked()
	if w.closing == c {
		w.inHold = true
		return 0
	}
	w.inHold = false
	return w.trimInputLocked()
}

// releaseHold ends the hold a refresh put on w's queue: no stream came
// back. It returns how many bytes bounding the queue dropped.
func (w *Window) releaseHold() int {
	w.connMu.Lock()
	defer w.connMu.Unlock()
	if !w.inHold {
		return 0
	}
	w.inHold = false
	w.roomLocked()
	return w.trimInputLocked()
}

func (w *Window) wakeWriter() {
	select {
	case w.inWake <- struct{}{}:
	default:
	}
}

// keyWriter is w's only writer of keystrokes. It writes the head of the
// queue (up to pendingInLimit bytes) as one message to the current stream
// once that delivered its first frame (streamUp), and removes those bytes
// only once the write succeeded. A failed write leaves them at the head;
// it waits for the next stream and writes them again, whole, before
// anything typed after them.
//
// Delivery: the websocket library reports a write as failed when the
// frame was not completely handed to the socket, so lux cannot have
// decoded that message; resending it whole adds no duplicate. A
// duplicate is possible only if the socket took the whole frame and the
// close raced the write's return so that it reported an error anyway.
// Streams luxmux closes (refreshStream) end with the close handshake, so
// lux reads every message written before it: keys survive a refresh
// exactly once. A write that succeeded is final: when lux or the network
// ends a stream, bytes the socket took but lux never read are lost, not
// resent (the protocol has no acknowledgement), and what follows them
// still arrives in order.
func (m *Mux) keyWriter(w *Window) {
	for {
		w.connMu.Lock()
		c := w.conn
		if c == nil || !w.connUp || len(w.in) == 0 {
			w.connMu.Unlock()
			select {
			case <-w.inWake:
			case <-w.inStop:
				return
			case <-m.ctx.Done():
				return
			}
			continue
		}
		// send only appends past len(w.in) and trimming keeps the
		// in-flight head, so b is not changed while written.
		n := min(len(w.in), pendingInLimit)
		b := w.in[:n:n]
		w.inFlight = n
		gen := w.inGen
		w.connMu.Unlock()
		ctx, cancel := context.WithTimeout(m.ctx, 10*time.Second)
		err := m.opt.writeKeys(ctx, c, api.StreamData{Data: b})
		cancel()
		w.connMu.Lock()
		w.inFlight = 0
		dropped := 0
		switch {
		case err != nil:
			// That stream is never written to again.
			dropped = w.lostStreamLocked(c)
		case gen == w.inGen:
			w.in = w.in[len(b):]
			if len(w.in) == 0 {
				w.in = nil
			}
		}
		w.roomLocked()
		w.connMu.Unlock()
		m.inputDropped(w, dropped)
	}
}

// setStream makes c, a stream of the Run's epoch just opened, w's stream;
// cut ends it at once. Keys wait until streamUp. Keys queued for another
// epoch's shell (the Run moved: a new placement, a fresh shell) are
// discarded; it returns how many.
func (w *Window) setStream(c *websocket.Conn, cut func(), epoch int) (discarded int) {
	w.connMu.Lock()
	defer w.connMu.Unlock()
	if w.inEpoch != 0 && w.inEpoch != epoch {
		discarded = w.discardInputLocked()
	}
	w.inEpoch = epoch
	w.conn = c
	w.connUp = false
	w.connCut = cut
	return discarded
}

// streamUp lets w's key writer write to c: its first frame came.
func (w *Window) streamUp(c *websocket.Conn) {
	w.connMu.Lock()
	if w.conn == c {
		w.connUp = true
		w.inHold = false
	}
	w.connMu.Unlock()
	w.wakeWriter()
}

// endStream forgets c as w's stream; keys queued and typed from now on
// wait for the next one (see lostStreamLocked). It returns how many
// queued bytes that dropped.
func (w *Window) endStream(c *websocket.Conn) (dropped int) {
	w.connMu.Lock()
	defer w.connMu.Unlock()
	if w.conn != c {
		return 0
	}
	dropped = w.lostStreamLocked(c)
	w.conn, w.connCut = nil, nil
	return dropped
}

// closingStream marks w's stream c as closed by luxmux for a refresh, and
// returns what cuts it off.
func (w *Window) closingStream(c *websocket.Conn) func() {
	w.connMu.Lock()
	defer w.connMu.Unlock()
	if w.conn != c {
		return nil
	}
	w.closing = c
	return w.connCut
}

// discardInputLocked empties w's input queue; a write in flight then
// removes nothing when it completes. It returns how many bytes it
// discarded.
func (w *Window) discardInputLocked() int {
	n := len(w.in)
	w.in = nil
	w.inFlight = 0
	w.inGen++
	w.roomLocked()
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

// dropInput forgets keystrokes queued for w's Run: it ended, or is
// replaced.
func (w *Window) dropInput() {
	w.connMu.Lock()
	w.discardInputLocked()
	w.inEpoch = 0
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

// shellExited closes a window whose shell exited, terminating its Run. A
// Run terminate could not end keeps the window, ended: r relaunches it
// and & closes it, each terminating the Run first.
func (m *Mux) shellExited(w *Window) {
	m.mu.Lock()
	idx := m.indexLocked(w)
	m.mu.Unlock()
	if err := m.closeWindow(w, true, fmt.Sprintf("window %d exited", idx)); err != nil {
		m.markDead(w, "shell exited; "+cannotTerminate(w.Endpoint, err))
	}
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
	resize := m.onResize
	suspended := m.suspended
	m.mu.Unlock()
	if suspended && resize != nil {
		resize(rows, cols)
	}
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
