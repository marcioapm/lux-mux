package mux

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/marcioapm/lux-mux/internal/fakelux"
)

// closedConn is a websocket connection already closed: every write on it
// fails.
func closedConn(t *testing.T) *websocket.Conn {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		<-r.Context().Done()
		c.CloseNow()
	}))
	t.Cleanup(srv.Close)
	c, _, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	c.CloseNow()
	return c
}

// handled waits until the input loop handled everything typed before: the
// notice C-b 9 shows comes after it.
func (h *harness) handled() {
	h.t.Helper()
	h.type_("\x029")
	h.noticeShown("no window 9")
}

// TestKeysToAnEndedWindowAreNotKept: an ended window's Run takes no keys:
// what is typed to it (other than r and &) is neither sent nor kept for
// the resumed shell.
func TestKeysToAnEndedWindowAreNotKept(t *testing.T) {
	var keys inputLog
	h := newHarness(t, cfgDefault, func(l *fakelux.Server) { l.InputHook = keys.hook })
	a := h.live(0)
	h.lux.End(a, "stopped", "stop")
	h.waitScreen("stopped", h.contains("r resume"))
	h.type_("xyz")
	h.handled()
	if p := h.pending(0); len(p) != 0 {
		t.Fatalf("kept for the ended window: %q", p)
	}
	h.type_("r")
	h.live(0)
	h.type_("after")
	h.waitFor("typed after", func() bool { return strings.Contains(string(keys.of(a)), "after") })
	if got := string(keys.of(a)); got != "after" {
		t.Fatalf("the resumed shell got %q", got)
	}
	h.detach()
}

// TestKeysKeptForARunThatEndsAreDropped: keys kept while a Run moved are
// forgotten when it ends instead: nothing would take them.
func TestKeysKeptForARunThatEndsAreDropped(t *testing.T) {
	h := newHarness(t, cfgDefault, nil)
	a := h.live(0)
	h.lux.Move(a, true)
	h.waitFor("reconnecting", func() bool { return h.phase(0) == phaseReconnecting })
	h.type_("q")
	h.waitFor("kept", func() bool { return string(h.pending(0)) == "q" })
	h.lux.End(a, "failed", "timeout")
	h.waitFor("ended", func() bool { return h.phase(0) == phaseDead })
	if p := h.pending(0); len(p) != 0 {
		t.Fatalf("still kept: %q", p)
	}
	h.detach()
}

// TestRelaunchSendsNoKeysOfTheOldRun: keys still kept for an ended Run
// (a write that failed as it ended puts them back) do not reach the Run r
// relaunches.
func TestRelaunchSendsNoKeysOfTheOldRun(t *testing.T) {
	var keys inputLog
	h := newHarness(t, cfgDefault, func(l *fakelux.Server) { l.InputHook = keys.hook })
	a := h.live(0)
	h.lux.End(a, "failed", "timeout")
	h.waitScreen("ended", h.contains("r relaunch"))
	w := h.win(0)
	w.connMu.Lock()
	w.pendingIn = []byte("old-run-keys")
	w.connMu.Unlock()
	h.type_("r")
	h.waitFor("relaunched", func() bool { return h.window(0) != "" && h.window(0) != a })
	b := h.live(0)
	h.type_("new")
	h.waitFor("typed", func() bool { return strings.Contains(string(keys.of(b)), "new") })
	if got := string(keys.of(b)); got != "new" {
		t.Fatalf("the relaunched Run got %q", got)
	}
	h.detach()
}

// TestKeysOnAFailedWriteAreKept: keys whose write fails because the
// stream just closed are kept, ahead of those typed after, and sent in
// order on the next stream.
func TestKeysOnAFailedWriteAreKept(t *testing.T) {
	var keys inputLog
	var gate attachGate
	h := newHarness(t, cfgDefault, func(l *fakelux.Server) { l.InputHook = keys.hook; l.AttachHook = gate.hook })
	a := h.live(0)
	w := h.win(0)
	w.connMu.Lock()
	w.conn = closedConn(t)
	w.connMu.Unlock()
	gate.hold(a)
	t.Cleanup(func() { gate.release(a) })
	h.type_("k1,")
	h.waitFor("k1 kept", func() bool { return string(h.pending(0)) == "k1," })
	h.type_("k2")
	h.waitFor("k2 kept", func() bool { return string(h.pending(0)) == "k1,k2" })
	h.lux.Blip(a)
	h.waitFor("attach held", func() bool { return gate.held_() == 1 })
	gate.release(a)
	h.waitFor("sent", func() bool { return string(keys.of(a)) == "k1,k2" })
	h.detach()
}

// TestKeysKeptWhenGoLiveFails: a new stream whose first write fails keeps
// the keys it was to send, in order, for the stream after it.
func TestKeysKeptWhenGoLiveFails(t *testing.T) {
	var keys inputLog
	var gate attachGate
	h := newHarness(t, cfgDefault, func(l *fakelux.Server) { l.InputHook = keys.hook; l.AttachHook = gate.hook })
	a := h.live(0)
	gate.hold(a)
	t.Cleanup(func() { gate.release(a) })
	h.lux.Blip(a)
	h.waitFor("attach held", func() bool { return gate.held_() == 1 })
	h.type_("k1,k2")
	h.waitFor("kept", func() bool { return string(h.pending(0)) == "k1,k2" })
	w := h.win(0)
	h.m.mu.Lock()
	ep := w.epoch
	h.m.mu.Unlock()
	if _, err := h.m.goLive(h.m.ctx, w, closedConn(t), ep); err == nil {
		t.Fatal("a write on a closed stream succeeded")
	}
	h.type_(",k3")
	h.waitFor("all kept", func() bool { return string(h.pending(0)) == "k1,k2,k3" })
	gate.release(a)
	h.waitFor("sent", func() bool { return string(keys.of(a)) == "k1,k2,k3" })
	h.detach()
}

// TestNoSuchRunEnds: a Run lux no longer has is shown ended.
func TestNoSuchRunEnds(t *testing.T) {
	h := newHarness(t, cfgDefault, nil)
	a := h.live(0)
	h.lux.Forget(a)
	h.waitScreen("ended", h.contains("lux has no such Run"))
	if h.phase(0) != phaseDead {
		t.Fatalf("phase %v", h.phase(0))
	}
	h.detach()
}

// TestResumeConflictRereads: a resume lux answers 409 (the Run is being
// resumed already) is not an error: the window follows the Run to
// running.
func TestResumeConflictRereads(t *testing.T) {
	var once sync.Once
	h := newHarness(t, cfgDefault, func(l *fakelux.Server) {
		l.ResumeHook = func(id string) {
			once.Do(func() { l.SetState(id, "resuming", "auto-resume") })
		}
	})
	a := h.live(0)
	h.lux.End(a, "lost", "host lost: missed heartbeats")
	h.waitFor("resume asked", func() bool { return len(h.lux.Resumes()) == 1 })
	h.lux.Start(a)
	h.live(0)
	if h.phase(0) != phaseLive || strings.Contains(h.term.screen(), "cannot resume") {
		t.Fatalf("screen:\n%s", h.term.screen())
	}
	h.detach()
}

// TestRefreshThatFailsToAttachSaysWhy: when the attach after a refresh
// fails, the window says why and backs off, as for any failed attach.
func TestRefreshThatFailsToAttachSaysWhy(t *testing.T) {
	h := newHarness(t, cfgDefault, nil)
	a := h.live(0)
	h.type_("\x02c")
	h.live(1)
	h.overflow(0, a)
	h.lux.FailAttaches(a, fakelux.ErrHostDisconnected)
	h.type_("\x020")
	h.waitFor("the reason shown", func() bool { return h.reason(0) == fakelux.ErrHostDisconnected })
	h.lux.FailAttaches(a, "")
	h.live(0)
	h.detach()
}

// TestEndedWindowReadsKeysPlain: an ended window's screen is shown with
// its program's keyboard modes off, so r and & arrive plain.
func TestEndedWindowReadsKeysPlain(t *testing.T) {
	h := newHarness(t, cfgDefault, nil)
	a := h.live(0)
	h.lux.Term(a).Output([]byte(kittyNvim))
	h.waitFor("modes on the terminal", func() bool { return wantKeys(h.term.keyModes(), 1, 2) })
	h.lux.End(a, "failed", "timeout")
	h.waitScreen("ended", h.contains("r relaunch"))
	h.waitFor("modes off", func() bool { return h.term.keyModes().Zero() })
	h.detach()
}

// TestReleasesDoNotCloseHelp: the releases of C-b and ? (kitty's report
// event types), read after help opened, go to the help and do not close
// it.
func TestReleasesDoNotCloseHelp(t *testing.T) {
	h := newHarness(t, cfgDefault, nil)
	h.live(0)
	helpOn := h.helpOn
	h.type_("\x1b[98;5:1u\x1b[63;2:1u")
	h.waitFor("help", helpOn)
	h.type_("\x1b[63;2:3u")
	h.type_("\x1b[98;5:3u")
	time.Sleep(100 * time.Millisecond)
	if !helpOn() {
		t.Fatal("a release closed help")
	}
	h.type_("x")
	h.waitFor("help closed", func() bool { return !helpOn() })
	h.detach()
}

// TestStaleWindowSwitchedBackFromStatusScreen: a stale window switched to
// from another window's status screen shows the placeholder while it is
// attached again: the terminal has none of its screen.
func TestStaleWindowSwitchedBackFromStatusScreen(t *testing.T) {
	var gate attachGate
	h := newHarness(t, cfgDefault, func(l *fakelux.Server) { l.Hold = true; l.AttachHook = gate.hook })
	h.waitFor("submitted", func() bool { return h.window(0) != "" })
	a := h.window(0)
	h.lux.Start(a)
	h.live(0)
	h.type_("\x02c")
	h.waitScreen("window 1's status screen", h.contains("luxmux: window 1"))
	h.overflow(0, a)
	gate.hold(a)
	t.Cleanup(func() { gate.release(a) })
	h.type_("\x020")
	h.waitScreen("placeholder", h.contains("refreshing its screen"))
	gate.release(a)
	h.live(0)
	h.detach()
}

// TestDeferredRefreshOnlyForTheShownWindow: a refresh put off by the 2 s
// limit is not done if the window is no longer shown by then.
func TestDeferredRefreshOnlyForTheShownWindow(t *testing.T) {
	h := newHarness(t, cfgDefault, nil)
	a := h.live(0)
	h.type_("\x02c")
	h.live(1)
	h.overflow(0, a)
	n := len(attachesOf(h.lux, a))
	due := func() bool { h.m.mu.Lock(); defer h.m.mu.Unlock(); return h.m.windows[0].refreshDue }
	h.m.mu.Lock()
	h.m.windows[0].lastRefresh = time.Now()
	h.m.mu.Unlock()
	h.type_("\x020")
	h.waitScreen("placeholder", h.contains("refreshing its screen"))
	h.waitFor("refresh put off", due)
	h.type_("\x021")
	h.waitFor("window 1 shown", func() bool { return h.title() == "luxmux:s [1] default" })
	h.waitFor("the put-off refresh ran", func() bool { return !due() })
	time.Sleep(100 * time.Millisecond)
	if got := len(attachesOf(h.lux, a)); got != n {
		t.Fatalf("window 0 attached again while not shown (%d attaches)", got-n)
	}
	h.detach()
}

// TestStaleOverlayKeepsPendingSequence: output that ended inside an escape
// sequence is finished as the program meant, at the cursor it left, after
// a notice is drawn and taken down over a stale shown window; and after
// help closed over it while its refresh is put off.
func TestStaleOverlayKeepsPendingSequence(t *testing.T) {
	for _, by := range []string{"notice", "help"} {
		t.Run(by, func(t *testing.T) {
			h := newHarness(t, cfgDefault, nil)
			a := h.live(0)
			h.overflow(0, a)
			term := h.lux.Term(a)
			term.Output([]byte("\x1b[2J\x1b[Hab\x1b[3"))
			h.waitFor("sent", func() bool { return strings.HasSuffix(string(h.term.rawBytes()), "ab\x1b[3") })
			want := "abRED"
			if by == "notice" {
				h.m.Notice("a notice")
				h.noticeShown("a notice")
			} else {
				// Help closed: the screen is cleared until the refresh.
				want = "RED"
				h.m.mu.Lock()
				h.m.windows[0].lastRefresh = time.Now()
				h.m.mu.Unlock()
				h.type_("\x02?")
				h.waitScreen("help", h.contains("luxmux keys"))
				h.type_("x")
				h.waitFor("help gone", func() bool { return !strings.Contains(h.term.screen(), "luxmux keys") })
			}
			term.Output([]byte("1mRED"))
			h.waitScreen("RED", h.contains("RED"))
			if first, _, _ := strings.Cut(h.term.screen(), "\n"); first != want {
				t.Fatalf("first row %q, want %q; screen:\n%s", first, want, h.term.screen())
			}
			h.detach()
		})
	}
}

// TestSpareNotSubmittedWhileSessionEnds: a spare slot freed while the
// session ends (a claim that finishes as it terminates the other spares)
// is not refilled: nothing is submitted.
func TestSpareNotSubmittedWhileSessionEnds(t *testing.T) {
	const cfg2 = cfgSpare + `
[profiles.other]
image = "busybox"
warm_spare = true
`
	var mu sync.Mutex
	var otherSpare string
	termHeld, termRelease := make(chan struct{}), make(chan struct{})
	var termOnce, releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(termRelease) }) }
	h := newHarness(t, cfg2, func(l *fakelux.Server) {
		l.TerminateHook = func(id string) {
			mu.Lock()
			o := otherSpare
			mu.Unlock()
			if id == o {
				termOnce.Do(func() { close(termHeld); <-termRelease })
			}
		}
	})
	// After the harness's: runs before the server is closed.
	t.Cleanup(release)
	first := h.live(0)
	h.waitFor("both spares", func() bool {
		s := h.spares()
		return len(s) == 2 && !slices.Contains(mapValues(s), "")
	})
	devSpare := spareOf(h)
	mu.Lock()
	for _, id := range h.spares() {
		if id != devSpare {
			otherSpare = id
		}
	}
	mu.Unlock()
	claimHeld, claimRelease := make(chan struct{}), make(chan struct{})
	var claimOnce, claimReleaseOnce sync.Once
	releaseClaim := func() { claimReleaseOnce.Do(func() { close(claimRelease) }) }
	t.Cleanup(releaseClaim)
	h.lux.SetGetHook(func(id string) {
		if id == devSpare {
			claimOnce.Do(func() { close(claimHeld); <-claimRelease })
		}
	})
	h.type_("\x02c")
	waitChan(t, "claim checking the spare", claimHeld)
	h.m.Kill(1)
	h.waitFor("one window", func() bool { return h.nwindows() == 1 })
	h.lux.Term(first).Exit()
	waitChan(t, "the session terminating its spares", termHeld)
	submits := len(h.lux.Submits())
	releaseClaim()
	h.waitFor("claimed spare terminated", func() bool { return h.lux.Run(devSpare).State == "terminated" })
	time.Sleep(200 * time.Millisecond)
	if n := len(h.lux.Submits()); n != submits {
		t.Fatalf("%d spares submitted while the session ended", n-submits)
	}
	release()
	select {
	case <-h.done:
	case <-timeout():
		t.Fatal("session did not end")
	}
}

func waitChan(t *testing.T, what string, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-timeout():
		t.Fatalf("waiting for %s", what)
	}
}

func mapValues(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}
