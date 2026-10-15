package mux

import (
	"bytes"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/marcioapm/lux-mux/internal/fakelux"
)

// attachesOf are the attach requests for one Run.
func attachesOf(l *fakelux.Server, id string) []fakelux.AttachReq {
	var out []fakelux.AttachReq
	for _, a := range l.Attaches() {
		if a.RunID == id {
			out = append(out, a)
		}
	}
	return out
}

// TestBlipReattachesWithRestore: a dropped stream on the same placement
// is reattached with restore; the shell (its screen) is the same.
func TestBlipReattachesWithRestore(t *testing.T) {
	h := newHarness(t, cfgDefault, nil)
	id := h.live(0)
	h.type_("before-blip\r\n")
	h.waitScreen("echo", h.contains("before-blip"))
	term := h.lux.Term(id)
	h.lux.Blip(id)
	h.waitFor("reattached", func() bool { return len(attachesOf(h.lux, id)) >= 2 && h.lux.Streams(id) > 0 })
	h.live(0)
	if h.lux.Term(id) != term {
		t.Fatal("a blip must keep the same terminal")
	}
	for _, a := range attachesOf(h.lux, id) {
		if !a.Restore || a.Rows != 24 || a.Cols != 80 {
			t.Fatalf("attach without restore at the terminal's size: %+v", a)
		}
	}
	h.type_("after-blip\r\n")
	h.waitScreen("both lines", func(s string) bool {
		return strings.Contains(s, "before-blip") && strings.Contains(s, "after-blip")
	})
	h.detach()
}

// TestFellBehindReattaches: a newer host ends a restore attach that fell
// behind with an error; luxmux attaches again with restore.
func TestFellBehindReattaches(t *testing.T) {
	h := newHarness(t, cfgDefault, nil)
	id := h.live(0)
	h.type_("kept-screen\r\n")
	h.waitScreen("echo", h.contains("kept-screen"))
	h.lux.FellBehind(id)
	h.waitFor("reattached", func() bool { return len(attachesOf(h.lux, id)) >= 2 && h.lux.Streams(id) > 0 })
	h.live(0)
	if a := attachesOf(h.lux, id); !a[len(a)-1].Restore {
		t.Fatal("reattach without restore")
	}
	if h.phase(0) == phaseDead {
		t.Fatal("fell-behind must not end the window")
	}
	h.type_("more\r\n")
	h.waitScreen("restored and live", func(s string) bool { return strings.Contains(s, "kept-screen") && strings.Contains(s, "more") })
	// It was a dropped stream, not the shell's end: a failure of the Run
	// afterwards shows its reason instead of closing the window as an exit.
	h.lux.End(id, "failed", "host lost: missed heartbeats")
	h.waitFor("dead with the reason", func() bool { return h.phase(0) == phaseDead })
	if !strings.Contains(h.reason(0), "host lost") || h.nwindows() != 1 {
		t.Fatalf("reason %q, %d windows", h.reason(0), h.nwindows())
	}
	h.detach()
}

// TestMoveGivesFreshShell: a moved Run comes back on a new placement with
// a new terminal; the window shows reconnecting, then the new shell.
func TestMoveGivesFreshShell(t *testing.T) {
	h := newHarness(t, cfgDefault, nil)
	id := h.live(0)
	h.type_("old-shell\r\n")
	h.waitScreen("echo", h.contains("old-shell"))
	old := h.lux.Term(id)
	h.lux.Move(id, true)
	h.waitScreen("reconnecting line", h.contains("reconnecting"))
	if h.phase(0) != phaseReconnecting {
		t.Fatalf("phase %v", h.phase(0))
	}
	h.lux.Start(id)
	h.waitFor("new terminal attached", func() bool { return h.lux.Term(id) != old && h.lux.Streams(id) > 0 })
	h.live(0)
	h.waitScreen("old screen gone", func(s string) bool { return !strings.Contains(s, "old-shell") })
	h.waitScreen("moved notice", h.contains("new shell"))
	h.type_("new-shell\r\n")
	h.waitScreen("new shell echo", h.contains("new-shell"))
	if r := h.lux.Run(id); r.Epoch != 2 {
		t.Fatalf("epoch %d", r.Epoch)
	}
	h.detach()
}

// TestStoppedNeedsSecretsResumes: a Run stopped because luxd no longer
// holds its secrets is resumed with the profile's secrets, then attached.
func TestStoppedNeedsSecretsResumes(t *testing.T) {
	h := newHarnessEnv(t, "[profiles.default]\nimage = \"alpine\"\nsecrets = [\"API_TOKEN\"]\n", map[string]string{"API_TOKEN": "v1"}, nil)
	id := h.live(0)
	if got := h.lux.Secrets(id); !reflect.DeepEqual(got, map[string]string{"API_TOKEN": "v1"}) {
		t.Fatalf("submitted secrets %v", got)
	}
	h.setEnv("API_TOKEN", "v2", true) // rotated meanwhile
	h.lux.LoseSecrets(id)
	h.waitFor("resumed", func() bool { return len(h.lux.Resumes()) == 1 })
	h.waitFor("running again", func() bool { return h.lux.Run(id).State == "running" && h.lux.Streams(id) > 0 })
	h.live(0)
	if r := h.lux.Resumes()[0]; r.RunID != id || !reflect.DeepEqual(r.Secrets, map[string]string{"API_TOKEN": "v2"}) {
		t.Fatalf("resume %+v", r)
	}
	h.detach()
}

// TestStoppedSecretsMissing: without the values, the window says so and
// offers relaunch instead of resuming without them.
func TestStoppedSecretsMissing(t *testing.T) {
	h := newHarnessEnv(t, "[profiles.default]\nimage = \"alpine\"\nsecrets = [\"API_TOKEN\"]\n", map[string]string{"API_TOKEN": "v1"}, nil)
	id := h.live(0)
	h.setEnv("API_TOKEN", "", false)
	h.lux.LoseSecrets(id)
	h.waitFor("dead", func() bool { return h.phase(0) == phaseDead })
	if !strings.Contains(h.reason(0), "API_TOKEN") {
		t.Fatalf("reason %q", h.reason(0))
	}
	if len(h.lux.Resumes()) != 0 {
		t.Fatal("resumed without the secrets")
	}
	h.detach()
}

// TestTerminatedOffersRelaunch: a Run terminated elsewhere shows its
// reason; r relaunches a fresh Run of the same profile in the same slot.
func TestTerminatedOffersRelaunch(t *testing.T) {
	h := newHarness(t, cfgDefault, nil)
	id := h.live(0)
	h.type_("\x02c")
	h.live(1)
	h.type_("\x020")
	h.waitFor("window 0 shown", func() bool { return h.title() == "luxmux:s [0] default" })
	h.lux.End(id, "terminated", "expired: stopped for 90 days")
	h.waitScreen("reason and offer", func(s string) bool {
		return strings.Contains(s, "expired: stopped for 90 days") && strings.Contains(s, "r relaunch")
	})
	if h.nwindows() != 2 {
		t.Fatal("an ended window closes only on &")
	}
	h.type_("r")
	h.waitFor("relaunched", func() bool { return h.window(0) != "" && h.window(0) != id })
	nid := h.live(0)
	if h.nwindows() != 2 {
		t.Fatalf("relaunch added a window: %d", h.nwindows())
	}
	if l := h.lux.Run(nid).Labels; l["luxmux-session"] != "s" || l["luxmux-profile"] != "default" {
		t.Fatalf("labels %v", l)
	}
	h.type_("echo-new\r\n")
	h.waitScreen("new run echo", h.contains("echo-new"))
	// & closes a dead window.
	h.lux.End(nid, "failed", "host lost")
	h.waitFor("dead again", func() bool { return h.phase(0) == phaseDead })
	h.type_("&")
	h.waitScreen("confirm", h.contains("(y/N)"))
	h.type_("y")
	h.waitFor("closed", func() bool { return h.nwindows() == 1 })
	h.detach()
}

// TestNoRestoreFallback: a host without restore sends no snapshot; luxmux
// nudges the shell to redraw (resize rows-1, then rows) and goes live.
func TestNoRestoreFallback(t *testing.T) {
	h := newHarness(t, cfgDefault, func(l *fakelux.Server) { l.NoRestore = true })
	id := h.live(0)
	term := h.lux.Term(id)
	h.waitFor("redraw nudge", func() bool {
		return reflect.DeepEqual(term.Resizes(), [][2]int{{23, 80}, {24, 80}})
	})
	h.type_("typed-live\r\n")
	h.waitScreen("live", h.contains("typed-live"))
	// A move on such a host: no snapshot clears the old shell's screen, so
	// luxmux shows the new shell on a blank one.
	h.lux.Move(id, false)
	h.waitFor("new terminal", func() bool { return h.lux.Term(id) != term && h.lux.Streams(id) > 0 })
	h.live(0)
	h.waitScreen("old screen gone", func(s string) bool { return !strings.Contains(s, "typed-live") })
	h.type_("\x02c")
	h.live(1)
	h.type_("\x020")
	h.waitFor("window 0 shown", func() bool { return h.title() == "luxmux:s [0] default" })
	h.waitScreen("repainted blank", func(s string) bool { return !strings.Contains(s, "typed-live") })
	h.detach()
}

// TestReattachNewProcessRestores: a new luxmux process restores each
// window's screen from the host's snapshot.
func TestReattachNewProcessRestores(t *testing.T) {
	h := newHarness(t, cfgDefault, nil)
	h.live(0)
	h.type_("screen-zero\r\n")
	h.waitScreen("echo 0", h.contains("screen-zero"))
	h.type_("\x02c")
	h.live(1)
	h.type_("screen-one\r\n")
	h.waitScreen("echo 1", h.contains("screen-one"))
	h.detach()
	ids := h.lux.RunIDs()
	for _, id := range ids {
		if h.lux.Run(id).State != "running" {
			t.Fatalf("detach must leave Runs running: %s %s", id, h.lux.Run(id).State)
		}
	}
	h.start(nil)
	h.waitScreen("window 1 restored", h.contains("screen-one"))
	h.live(0)
	h.live(1)
	h.type_("\x020")
	h.waitScreen("window 0 restored", h.contains("screen-zero"))
	h.detach()
}

// TestResizeReachesEveryStream: SIGWINCH resizes every window's emulator
// and stream.
func TestResizeReachesEveryStream(t *testing.T) {
	h := newHarness(t, cfgDefault, nil)
	a := h.live(0)
	h.type_("\x02c")
	b := h.live(1)
	h.m.Resize(30, 100)
	for _, id := range []string{a, b} {
		term := h.lux.Term(id)
		h.waitFor("resize "+id, func() bool {
			r := term.Resizes()
			return len(r) > 0 && r[len(r)-1] == [2]int{30, 100}
		})
	}
	h.m.mu.Lock()
	ws := append([]*Window{}, h.m.windows...)
	h.m.mu.Unlock()
	for _, w := range ws {
		w.withEmu(func(t2 *vtermT) {
			if r, c := t2.Size(); r != 30 || c != 100 {
				t.Errorf("emulator %dx%d", r, c)
			}
		})
	}
	h.detach()
}

// TestLuxFailureAfterWorkloadExitedIsNotAnExit: lux stops a Run's
// workload for a timeout (the stream ends with "the workload exited"),
// then fails it with reason timeout: the window is dead with the reason,
// and the Run is not terminated.
func TestLuxFailureAfterWorkloadExitedIsNotAnExit(t *testing.T) {
	h := newHarness(t, cfgDefault, nil)
	id := h.live(0)
	h.lux.End(id, "failed", "timeout")
	h.waitFor("dead", func() bool { return h.phase(0) == phaseDead })
	if h.nwindows() != 1 || !strings.Contains(h.reason(0), "timeout") {
		t.Fatalf("%d windows, reason %q", h.nwindows(), h.reason(0))
	}
	h.waitScreen("offer", h.contains("r relaunch"))
	if len(h.lux.Terminated()) != 0 || h.lux.Run(id).State != "failed" {
		t.Fatalf("terminated %v, state %s", h.lux.Terminated(), h.lux.Run(id).State)
	}
	h.detach()
}

// TestFailureAfterMoveIsNotAnExit: a move ends the stream with "the
// workload exited" too; a later failure of the moved Run is lux's, not
// the shell's.
func TestFailureAfterMoveIsNotAnExit(t *testing.T) {
	h := newHarness(t, cfgDefault, nil)
	id := h.live(0)
	old := h.lux.Term(id)
	h.lux.Move(id, false)
	h.waitFor("new terminal attached", func() bool { return h.lux.Term(id) != old && h.lux.Streams(id) > 0 })
	h.live(0)
	h.lux.End(id, "failed", "disk limit exceeded")
	h.waitFor("dead", func() bool { return h.phase(0) == phaseDead })
	if h.nwindows() != 1 || len(h.lux.Terminated()) != 0 {
		t.Fatalf("%d windows, terminated %v", h.nwindows(), h.lux.Terminated())
	}
	h.detach()
}

// TestShellExitCodes: the shell exiting with a non-zero code (failed:
// exit code 3) closes the window and terminates the Run, as exit 0
// (succeeded) does.
func TestShellExitCodes(t *testing.T) {
	h := newHarness(t, cfgDefault, nil)
	a := h.live(0)
	h.type_("\x02c")
	b := h.live(1)
	h.lux.Term(b).ExitWith(3)
	h.waitFor("window 1 closed", func() bool { return h.nwindows() == 1 })
	h.waitFor("terminated", func() bool { return h.lux.Run(b).State == "terminated" })
	if r := h.lux.Run(b); r.ExitCode == nil || *r.ExitCode != 3 {
		t.Fatalf("exit code %v", r.ExitCode)
	}
	h.lux.Term(a).ExitWith(0)
	select {
	case r := <-h.done:
		if r != "exited" {
			t.Fatalf("Run returned %q", r)
		}
	case <-timeout():
		t.Fatal("succeeded did not close the last window")
	}
}

// TestSucceededWithoutStreamEndCloses: a Run lux reports succeeded closes
// its window even when no stream saw the exit (it was not attached).
func TestSucceededWithoutStreamEndCloses(t *testing.T) {
	h := newHarness(t, cfgDefault, nil)
	h.live(0)
	h.type_("\x02c")
	b := h.live(1)
	h.lux.End(b, "succeeded", "")
	h.waitFor("window 1 closed", func() bool { return h.nwindows() == 1 })
	h.detach()
}

// TestExitCodeReasonWithoutWorkloadExitIsDead: lux's "exit code" reason
// for an epoch whose stream did not end with "the workload exited" (the
// stream was dropped first) leaves the window dead, not closed.
func TestExitCodeReasonOnlyWithWorkloadExit(t *testing.T) {
	h := newHarness(t, cfgDefault, nil)
	id := h.live(0)
	h.lux.Blip(id)
	h.lux.Blip(id)
	h.lux.SetState(id, "failed", "exit code 1")
	h.waitFor("dead", func() bool { return h.phase(0) == phaseDead })
	if h.nwindows() != 1 {
		t.Fatal("closed without the shim's exit")
	}
	h.detach()
}

// TestLuxdStreamErrorsReattach: each error luxd or the shim ends a stream
// with while the Run runs is a dropped stream: attach again with restore.
func TestLuxdStreamErrorsReattach(t *testing.T) {
	for _, msg := range []string{fakelux.ErrStreamDropped, fakelux.ErrHostDisconnected, fakelux.ErrAttachFellBehind} {
		t.Run(msg, func(t *testing.T) {
			h := newHarness(t, cfgDefault, nil)
			id := h.live(0)
			h.type_("kept\r\n")
			h.waitScreen("echo", h.contains("kept"))
			n := len(attachesOf(h.lux, id))
			h.lux.DropStreams(id, msg)
			h.waitFor("reattached", func() bool { return len(attachesOf(h.lux, id)) > n && h.lux.Streams(id) > 0 })
			h.live(0)
			if a := attachesOf(h.lux, id); !a[len(a)-1].Restore {
				t.Fatal("reattach without restore")
			}
			h.type_("after\r\n")
			h.waitScreen("live", func(s string) bool { return strings.Contains(s, "kept") && strings.Contains(s, "after") })
			if h.nwindows() != 1 || len(h.lux.Terminated()) != 0 {
				t.Fatal("a stream error closed the window")
			}
			h.detach()
		})
	}
}

// TestStoppedByPersonWaitsForResume: a Run someone stopped (lux stop) is
// not resumed unasked; the window says who stopped it, and r resumes it.
func TestStoppedByPersonWaitsForResume(t *testing.T) {
	h := newHarnessEnv(t, "[profiles.default]\nimage = \"alpine\"\nsecrets = [\"API_TOKEN\"]\n", map[string]string{"API_TOKEN": "v1"}, nil)
	id := h.live(0)
	h.type_("before-stop\r\n")
	h.waitScreen("echo", h.contains("before-stop"))
	h.lux.End(id, "stopped", "stop")
	h.waitScreen("offer", h.contains("stopped by lux stop — r resume, & close"))
	time.Sleep(150 * time.Millisecond)
	if len(h.lux.Resumes()) != 0 || h.lux.Run(id).State != "stopped" {
		t.Fatalf("resumed unasked: %v %s", h.lux.Resumes(), h.lux.Run(id).State)
	}
	h.type_("r")
	h.waitFor("resumed", func() bool { return len(h.lux.Resumes()) == 1 })
	h.waitFor("running", func() bool { return h.lux.Run(id).State == "running" && h.lux.Streams(id) > 0 })
	h.live(0)
	if r := h.lux.Resumes()[0]; !reflect.DeepEqual(r.Secrets, map[string]string{"API_TOKEN": "v1"}) {
		t.Fatalf("resume %+v", r)
	}
	if h.lux.Run(id).Epoch != 2 {
		t.Fatal("r relaunched instead of resuming")
	}
	h.detach()
}

// TestLostResumes: a Run whose host was lost is resumed by luxmux.
func TestLostResumes(t *testing.T) {
	h := newHarness(t, cfgDefault, nil)
	id := h.live(0)
	h.lux.End(id, "lost", "host lost: missed heartbeats")
	h.waitFor("resumed", func() bool { return len(h.lux.Resumes()) == 1 })
	h.waitFor("running", func() bool { return h.lux.Run(id).State == "running" && h.lux.Streams(id) > 0 })
	h.detach()
}

// TestResumeResolvesOnlyTheRunsSecrets: a resume needs values only for
// the secrets lux lists for the Run, not every one the profile names now.
func TestResumeResolvesOnlyTheRunsSecrets(t *testing.T) {
	h := newHarnessEnv(t, "[profiles.default]\nimage = \"alpine\"\nsecrets = [\"API_TOKEN\"]\n", map[string]string{"API_TOKEN": "v1"}, nil)
	id := h.live(0)
	// The window's profile gained a secret whose value is nowhere.
	h.m.mu.Lock()
	h.m.windows[0].Spec.Secrets = []string{"API_TOKEN", "ADDED_LATER"}
	h.m.mu.Unlock()
	h.lux.LoseSecrets(id)
	h.waitFor("resumed", func() bool { return len(h.lux.Resumes()) == 1 })
	if r := h.lux.Resumes()[0]; !reflect.DeepEqual(r.Secrets, map[string]string{"API_TOKEN": "v1"}) {
		t.Fatalf("resume %+v", r)
	}
	h.waitFor("running", func() bool { return h.lux.Run(id).State == "running" })
	h.detach()
}

// TestPanicRestoresTerminal: a panic in a supervisor goroutine gives the
// terminal back (main screen, the program's modes off, the cursor shown
// in its default shape) before the panic is reported.
func TestPanicRestoresTerminal(t *testing.T) {
	h := newHarnessEnv(t, "[profiles.default]\nimage = \"alpine\"\nsecrets = [\"API_TOKEN\"]\n", map[string]string{"API_TOKEN": "v1"}, nil)
	id := h.live(0)
	h.lux.Term(id).Output([]byte("\x1b[?1049h\x1b[?1000h\x1b[?2004h\x1b[?25l\x1b=\x1b[5 q"))
	h.waitFor("program's modes on", func() bool { return h.term.altScreen() && h.term.mode(1000) && h.term.mode(2004) })
	h.envMu.Lock()
	h.panicOn = "API_TOKEN"
	h.envMu.Unlock()
	h.lux.LoseSecrets(id)
	var msg string
	select {
	case msg = <-h.crashed:
	case <-timeout():
		t.Fatal("no crash reported")
	}
	if !strings.Contains(msg, "panic: injected: lookup API_TOKEN") || !strings.Contains(msg, "resumeRun") {
		t.Fatalf("crash message %q", msg)
	}
	if h.term.altScreen() || h.term.mode(1000) || h.term.mode(2004) || !h.term.mode(25) {
		t.Fatalf("terminal not restored: alt %v mouse %v paste %v cursor %v",
			h.term.altScreen(), h.term.mode(1000), h.term.mode(2004), h.term.mode(25))
	}
	if kp, sh := h.term.keypadShape(); kp || sh != 0 {
		t.Fatalf("keypad %v shape %d", kp, sh)
	}
}

// TestRestoredMarkerEndsSnapshot: a window not shown when attached takes
// its snapshot quietly, however it is chunked, until the Restored marker;
// what follows is live.
func TestRestoredMarkerEndsSnapshot(t *testing.T) {
	h := newHarness(t, cfgDefault, nil)
	a := h.live(0)
	// A screen whose snapshot takes several 32 KiB frames.
	var b strings.Builder
	for y := range 24 {
		fmt.Fprintf(&b, "\x1b[%d;1H", y+1)
		for x := range 80 {
			fmt.Fprintf(&b, "\x1b[38;2;%d;%d;%dm#", x, y, x+y)
		}
	}
	h.lux.Term(a).Output([]byte(b.String() + "\x1b[mMARK-A"))
	h.type_("\x02c")
	h.live(1)
	h.detach()
	h.start(nil)
	h.live(0)
	h.live(1)
	h.type_("\x020")
	h.waitScreen("window 0 restored", h.contains("MARK-A"))
	h.lux.Term(a).Output([]byte("\r\nLIVE-AFTER"))
	h.waitScreen("live after the marker", h.contains("LIVE-AFTER"))
	h.detach()
}

// TestRestoredMarkerWithoutSnapshot: a host with restore that has no
// snapshot to send sends the marker first: the window goes live at once
// (not after RestoreWait, as for a host from before restore), from a
// blank screen the shell is asked to redraw.
func TestRestoredMarkerWithoutSnapshot(t *testing.T) {
	h := newHarness(t, cfgDefault, func(l *fakelux.Server) { l.NoSnapshot = true; l.Hold = true })
	h.waitFor("submitted", func() bool { return len(h.lux.RunIDs()) == 1 })
	id := h.lux.RunIDs()[0]
	h.lux.Start(id)
	h.waitFor("attached", func() bool { return len(attachesOf(h.lux, id)) == 1 })
	start := time.Now()
	h.live(0)
	if d := time.Since(start); d >= h.m.opt.RestoreWait {
		t.Fatalf("live after %v: waited as for a host without restore", d)
	}
	term := h.lux.Term(id)
	h.waitFor("redraw nudge", func() bool {
		return reflect.DeepEqual(term.Resizes(), [][2]int{{24, 80}, {23, 80}, {24, 80}})
	})
	h.type_("typed-live\r\n")
	h.waitScreen("live", h.contains("typed-live"))
	h.detach()
}

// TestHugeTerminalClamped: a terminal larger than lux allows attaches at
// lux's largest size, 200×500, and resizes to it.
func TestHugeTerminalClamped(t *testing.T) {
	h := newHarness(t, cfgDefault, func(l *fakelux.Server) {})
	id := h.live(0)
	h.m.Resize(300, 900)
	term := h.lux.Term(id)
	h.waitFor("resize", func() bool {
		r := term.Resizes()
		return len(r) > 0 && r[len(r)-1] == [2]int{200, 500}
	})
	h.lux.Blip(id)
	h.waitFor("reattached", func() bool { return len(attachesOf(h.lux, id)) >= 2 && h.lux.Streams(id) > 0 })
	a := attachesOf(h.lux, id)
	if last := a[len(a)-1]; last.Rows != 200 || last.Cols != 500 {
		t.Fatalf("attach %+v", last)
	}
	h.m.mu.Lock()
	w := h.m.windows[0]
	h.m.mu.Unlock()
	w.withEmu(func(t2 *vtermT) {
		if r, c := t2.Size(); r != 200 || c != 500 {
			t.Errorf("emulator %dx%d", r, c)
		}
	})
	h.detach()
}

// TestRestoreBudgetReattaches: a restore attach whose stream falls more
// than the host's budget behind (as lux-shim ends it) is attached again
// with restore, and the screen is right afterwards.
func TestRestoreBudgetReattaches(t *testing.T) {
	h := newHarness(t, cfgDefault, nil)
	id := h.live(0)
	n := len(attachesOf(h.lux, id))
	// m.mu held: the stream's reader stops at its next frame, and the
	// host queues output for it past its budget.
	h.m.mu.Lock()
	term := h.lux.Term(id)
	chunk := bytes.Repeat([]byte("x"), 64<<10)
	for range fakelux.RestoreBudget/len(chunk) + 64 {
		term.Output(chunk)
	}
	h.m.mu.Unlock()
	h.waitFor("reattached", func() bool { return len(attachesOf(h.lux, id)) > n && h.lux.Streams(id) > 0 })
	if a := attachesOf(h.lux, id); !a[len(a)-1].Restore {
		t.Fatal("reattach without restore")
	}
	term.Output([]byte("\x1b[2J\x1b[HAFTER-BUDGET"))
	h.waitScreen("screen right", h.contains("AFTER-BUDGET"))
	h.detach()
}
