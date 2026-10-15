package mux

import (
	"bytes"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/marcioapm/lux-mux/internal/fakelux"
)

// attachGate holds attaches to the Runs it is closed for until opened.
type attachGate struct {
	mu   sync.Mutex
	held map[string]chan struct{}
	// waiting counts attaches held now.
	waiting int
}

func (g *attachGate) hook(id string) {
	g.mu.Lock()
	ch := g.held[id]
	if ch != nil {
		g.waiting++
	}
	g.mu.Unlock()
	if ch != nil {
		<-ch
		g.mu.Lock()
		g.waiting--
		g.mu.Unlock()
	}
}

func (g *attachGate) hold(id string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.held == nil {
		g.held = map[string]chan struct{}{}
	}
	g.held[id] = make(chan struct{})
}

func (g *attachGate) release(id string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if ch := g.held[id]; ch != nil {
		close(ch)
		delete(g.held, id)
	}
}

func (g *attachGate) held_() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.waiting
}

func (h *harness) win(i int) *Window {
	h.m.mu.Lock()
	defer h.m.mu.Unlock()
	return h.m.windows[i]
}

func (h *harness) noticeShown(s string) {
	h.t.Helper()
	h.waitScreen("notice "+s, h.contains(s))
	h.waitFor("notice "+s+" gone", func() bool { return !strings.Contains(h.term.screen(), s) })
}

// TestStaleShownWindowIsNotReattached: the shown window's terminal has all
// of its output, so taking a notice or a prompt down while its emulator is
// stale draws only the bottom row: no reattach, no placeholder, and the
// screen the output drew stays.
func TestStaleShownWindowIsNotReattached(t *testing.T) {
	h := newHarness(t, cfgDefault, nil)
	a := h.live(0)
	h.overflow(0, a)
	h.lux.Term(a).Output([]byte("\x1b[2J\x1b[HSHOWN-SCREEN"))
	h.waitScreen("output", h.contains("SHOWN-SCREEN"))
	n := len(attachesOf(h.lux, a))
	h.type_("\x029")
	h.noticeShown("no window 9")
	h.type_("\x02,")
	h.waitScreen("prompt", h.contains("rename window:"))
	h.type_("\x1b")
	h.waitFor("prompt gone", func() bool { return !strings.Contains(h.term.screen(), "rename window:") })
	h.m.Notice("from elsewhere")
	h.noticeShown("from elsewhere")
	time.Sleep(100 * time.Millisecond)
	if got := len(attachesOf(h.lux, a)); got != n {
		t.Fatalf("%d reattaches of the shown window for overlays", got-n)
	}
	scr := h.term.screen()
	if strings.Contains(scr, "refreshing") || !strings.Contains(scr, "SHOWN-SCREEN") {
		t.Fatalf("screen:\n%s", scr)
	}
	if !h.win(0).q.isStale() {
		t.Fatal("not stale: the test did not test it")
	}
	h.detach()
}

// TestStaleShownWindowRestoredAfterHelp: closing help, the chooser or the
// new-window dialog over a stale shown window draws it in full again, so
// it is attached again with restore and its screen comes back from lux's
// snapshot, not left blank (a shell at its prompt does not redraw itself).
func TestStaleShownWindowRestoredAfterHelp(t *testing.T) {
	for _, closeBy := range []string{"help", "chooser", "dialog"} {
		t.Run(closeBy, func(t *testing.T) {
			h := newHarness(t, cfgDefault, nil)
			h.m.opt.UI = &fakeUI{choose: -1}
			a := h.live(0)
			h.overflow(0, a)
			h.lux.Term(a).Output([]byte("\x1b[2J\x1b[HQUIET-FINAL\r\n$ "))
			h.waitScreen("output", h.contains("QUIET-FINAL"))
			n := len(attachesOf(h.lux, a))
			switch closeBy {
			case "help":
				h.type_("\x02?")
				h.waitScreen("help", h.contains("luxmux keys"))
				h.type_("x")
			case "chooser":
				h.type_("\x02w")
			case "dialog":
				h.type_("\x02C")
			}
			h.waitFor("reattached with restore", func() bool {
				as := attachesOf(h.lux, a)
				return len(as) == n+1 && as[n].Restore
			})
			h.waitScreen("screen restored", func(s string) bool {
				return strings.Contains(s, "QUIET-FINAL") && !strings.Contains(s, "luxmux keys") && !strings.Contains(s, "CHOOSER-OPEN")
			})
			h.waitFor("not stale", func() bool { return !h.win(0).q.isStale() })
			h.detach()
		})
	}
}

// TestStaleShownWindowRestoredAfterSlowTerminal: a terminal that fell
// behind while the shown window was stale gets its screen back from lux
// once it caught up.
func TestStaleShownWindowRestoredAfterSlowTerminal(t *testing.T) {
	h := newHarness(t, cfgDefault, nil)
	a := h.live(0)
	h.overflow(0, a)
	h.term.setStall(true)
	t.Cleanup(func() { h.term.setStall(false) })
	term := h.lux.Term(a)
	big := bytes.Repeat([]byte("flood-line-0123456789abcdef\r\n"), (1<<20)/29)
	for range 6 {
		term.Output(big)
		h.waitFor("stream read", func() bool { return term.Queued() < 2<<20 })
	}
	h.waitFor("behind", h.m.tw.isBehind)
	term.Output([]byte("\x1b[2J\x1b[HSLOW-FINAL\r\n$ "))
	h.term.setStall(false)
	h.waitScreen("screen restored", h.contains("SLOW-FINAL"))
	h.waitFor("not stale", func() bool { return !h.win(0).q.isStale() })
	h.detach()
}

// TestStalePreviewOfShownWindowDoesNotReattach: the chooser's preview of
// the stale window under it says so and leaves it attached: closing the
// chooser restores it.
func TestStalePreviewOfShownWindowDoesNotReattach(t *testing.T) {
	h := newHarness(t, cfgDefault, nil)
	a := h.live(0)
	h.overflow(0, a)
	n := len(attachesOf(h.lux, a))
	if p := strings.Join(h.m.Preview(0, 5, 40), ""); !strings.Contains(p, "out of date here") {
		t.Fatalf("preview %q", p)
	}
	time.Sleep(100 * time.Millisecond)
	if got := len(attachesOf(h.lux, a)); got != n {
		t.Fatalf("%d reattaches for a preview", got-n)
	}
	h.detach()
}

// TestStaleShownWindowUnderFloodRefreshBounded: a shown window kept stale
// by a flood, with notices coming and going and help opened and closed
// over and over, is attached again at most once per refreshEvery, and
// every key typed meanwhile reaches its shell, in order.
func TestStaleShownWindowUnderFloodRefreshBounded(t *testing.T) {
	var keys inputLog
	var mu sync.Mutex
	var at []time.Time
	h := newHarness(t, cfgDefault, func(l *fakelux.Server) {
		l.InputHook = keys.hook
		l.AttachHook = func(string) { mu.Lock(); at = append(at, time.Now()); mu.Unlock() }
	})
	a := h.live(0)
	w := h.win(0)
	// The emulator is held for the whole test: every stream after a
	// restore overflows it again.
	h.overflow(0, a)
	w.emuMu.Lock()
	released := false
	release := func() {
		if !released {
			released = true
			w.emuMu.Unlock()
		}
	}
	t.Cleanup(release)
	mu.Lock()
	at = nil
	mu.Unlock()
	stop := make(chan struct{})
	var bg sync.WaitGroup
	bg.Add(2)
	go func() {
		defer bg.Done()
		chunk := bytes.Repeat([]byte("FLOOD-0123456789abcdef\r\n"), 1<<14)
		for {
			select {
			case <-stop:
				return
			default:
			}
			// Paced on lux's budget only: the terminal may fall behind
			// (and catch up, another full redraw) as it will.
			if term := h.lux.Term(a); term != nil && term.Queued() < 2<<20 {
				term.Output(chunk)
			} else {
				time.Sleep(time.Millisecond)
			}
		}
	}()
	go func() {
		defer bg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			case <-time.After(100 * time.Millisecond):
				h.m.Notice(fmt.Sprintf("notice %d", i))
			}
		}
	}()
	start := time.Now()
	var want bytes.Buffer
	for i := 0; time.Since(start) < 5*time.Second; i++ {
		if i%10 == 0 {
			h.type_("\x02?")
			h.waitFor("help", h.helpOn)
			h.type_("x")
			h.waitFor("help closed", func() bool { return !h.helpOn() })
		}
		s := fmt.Sprintf("k%03d,", i)
		want.WriteString(s)
		h.type_(s)
		time.Sleep(20 * time.Millisecond)
	}
	close(stop)
	bg.Wait()
	release()
	mu.Lock()
	got := append([]time.Time{}, at...)
	elapsed := time.Since(start)
	mu.Unlock()
	if len(got) == 0 {
		t.Fatal("never attached again: the test did not test the limit")
	}
	if limit := int(elapsed/refreshEvery) + 1; len(got) > limit {
		t.Fatalf("%d reattaches in %v (at most %d)", len(got), elapsed, limit)
	}
	for i := 1; i < len(got); i++ {
		if d := got[i].Sub(got[i-1]); d < refreshEvery-100*time.Millisecond {
			t.Fatalf("reattach %d came %v after the one before", i, d)
		}
	}
	h.waitFor("all keys", func() bool { return len(keys.of(a)) >= want.Len() })
	if k := keys.of(a); !bytes.Equal(k, want.Bytes()) {
		t.Fatalf("keys:\n got %q\nwant %q", k, want.Bytes())
	}
	t.Logf("%d reattaches in %v", len(got), elapsed.Round(time.Millisecond))
	h.detach()
}

// TestStaleRefreshAtMostEvery2s: a window that goes stale again right
// after a refresh is not attached again within refreshEvery of the last,
// however often it is switched to; once that passed, it is.
func TestStaleRefreshAtMostEvery2s(t *testing.T) {
	h := newHarness(t, cfgDefault, nil)
	a := h.live(0)
	h.type_("\x02c")
	h.live(1)
	h.overflow(0, a)
	n := len(attachesOf(h.lux, a))
	h.type_("\x020")
	h.waitFor("refreshed", func() bool { return len(attachesOf(h.lux, a)) == n+1 && !h.win(0).q.isStale() })
	h.live(0)
	first := time.Now()
	h.type_("\x021")
	h.waitFor("window 1 shown", func() bool { return h.title() == "luxmux:s [1] default" })
	h.overflow(0, a)
	for range 5 {
		h.type_("\x020")
		h.waitScreen("placeholder", h.contains("refreshing its screen"))
		h.type_("\x021")
		h.waitFor("window 1 shown", func() bool { return h.title() == "luxmux:s [1] default" })
	}
	h.type_("\x020")
	if time.Since(first) < refreshEvery {
		if got := len(attachesOf(h.lux, a)); got != n+1 {
			t.Fatalf("%d attaches within %v of the last refresh", got-n-1, time.Since(first))
		}
	} else {
		t.Log("the overflow took longer than refreshEvery: the limit is not tested")
	}
	h.waitFor("refreshed once more", func() bool { return len(attachesOf(h.lux, a)) == n+2 })
	if d := time.Since(first); d < refreshEvery-100*time.Millisecond {
		t.Fatalf("refreshed again after %v", d)
	}
	h.lux.Term(a).Output([]byte("\x1b[2J\x1b[HAFTER-SECOND"))
	h.waitScreen("restored", h.contains("AFTER-SECOND"))
	if got := len(attachesOf(h.lux, a)); got != n+2 {
		t.Fatalf("%d attaches", got-n)
	}
	h.detach()
}

// TestStaleWindowThatEndedShowsItsEnd: a stale window whose Run then
// failed, or was stopped by a person, shows that with its r/& offer, and
// its prompts and notices, not the "refreshing" placeholder; r on the
// stopped one resumes it and its screen comes back from lux.
func TestStaleWindowThatEndedShowsItsEnd(t *testing.T) {
	for _, tc := range []struct {
		name, state, reason, want, offer string
	}{
		{"failed", "failed", "timeout", "ended: failed: timeout", "r  relaunch a fresh Run"},
		{"stopped", "stopped", "stop", "stopped by lux stop", "r  resume the Run"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, cfgDefault, nil)
			a := h.live(0)
			h.type_("\x02c")
			h.live(1)
			h.overflow(0, a)
			h.lux.End(a, tc.state, tc.reason)
			h.waitFor("window 0 ended", func() bool { return h.phase(0) == phaseDead })
			h.type_("\x020")
			// The offer is on the status screen only (the notice of the
			// end, on window 1, says less).
			h.waitScreen("its end", func(s string) bool { return strings.Contains(s, tc.want) && strings.Contains(s, tc.offer) })
			if strings.Contains(h.term.screen(), "refreshing") {
				t.Fatalf("placeholder:\n%s", h.term.screen())
			}
			h.type_("\x029")
			h.waitScreen("notice", h.contains("no window 9"))
			h.type_("\x02&")
			h.waitScreen("kill prompt", h.contains("kill window 0 (default)? (y/N)"))
			h.type_("n")
			h.waitFor("prompt closed", func() bool { return !strings.Contains(h.term.screen(), "(y/N)") })
			if tc.state == "stopped" {
				h.type_("r")
				h.waitFor("resumed", func() bool { return len(h.lux.Resumes()) == 1 })
				h.live(0)
				h.lux.Term(a).Output([]byte("\x1b[2J\x1b[HRESUMED-SCREEN"))
				h.waitScreen("screen back", h.contains("RESUMED-SCREEN"))
			}
			h.detach()
		})
	}
}

// TestKeysTypedDuringRefreshArrive: keys typed while a window's stream is
// down for a refresh are all sent, in order, once the new stream is up.
func TestKeysTypedDuringRefreshArrive(t *testing.T) {
	var keys inputLog
	var gate attachGate
	h := newHarness(t, cfgDefault, func(l *fakelux.Server) { l.InputHook = keys.hook; l.AttachHook = gate.hook })
	a := h.live(0)
	h.type_("\x02c")
	h.live(1)
	h.overflow(0, a)
	gate.hold(a)
	t.Cleanup(func() { gate.release(a) })
	h.type_("\x020")
	h.waitFor("attach held", func() bool { return gate.held_() == 1 })
	var want bytes.Buffer
	for i := range 200 {
		s := fmt.Sprintf("k%03d,", i)
		want.WriteString(s)
		h.type_(s)
	}
	time.Sleep(50 * time.Millisecond)
	if got := keys.of(a); len(got) != 0 {
		t.Fatalf("keys reached a stream that is down: %q", got)
	}
	gate.release(a)
	h.waitFor("all keys", func() bool { return len(keys.of(a)) >= want.Len() })
	if got := keys.of(a); !bytes.Equal(got, want.Bytes()) {
		t.Fatalf("got %q\nwant %q", got, want.Bytes())
	}
	h.detach()
}

// TestKeysTypedDuringReconnectBounded: past pendingInLimit typed while a
// stream is down, the oldest keys are dropped, with a notice, and the
// newest arrive in order.
func TestKeysTypedDuringReconnectBounded(t *testing.T) {
	var keys inputLog
	var gate attachGate
	h := newHarness(t, cfgDefault, func(l *fakelux.Server) { l.InputHook = keys.hook; l.AttachHook = gate.hook })
	a := h.live(0)
	gate.hold(a)
	t.Cleanup(func() { gate.release(a) })
	h.lux.Blip(a)
	h.waitFor("attach held", func() bool { return gate.held_() == 1 })
	chunk := bytes.Repeat([]byte("0123456789abcdef"), 256) // 4 KiB
	var all []byte
	for i := range 20 {
		c := append([]byte{}, chunk...)
		c[0] = 'A' + byte(i)
		all = append(all, c...)
		h.type_(string(c))
	}
	want := all[len(all)-pendingInLimit:]
	// Every chunk typed has reached the window: the notice alone can come
	// before the last chunks are kept, and those would then go out live.
	h.waitFor("all keys kept", func() bool {
		h.m.mu.Lock()
		w := h.m.windows[0]
		h.m.mu.Unlock()
		w.connMu.Lock()
		defer w.connMu.Unlock()
		return bytes.Equal(w.pendingIn, want)
	})
	h.waitScreen("dropped notice", h.contains("typed bytes dropped"))
	gate.release(a)
	h.waitFor("kept keys", func() bool { return len(keys.of(a)) >= len(want) })
	if got := keys.of(a); !bytes.Equal(got, want) {
		t.Fatalf("got %d bytes starting %q, want the last %d", len(got), got[:16], len(want))
	}
	h.detach()
}

// pending is what is kept for window i's next stream.
func (h *harness) pending(i int) []byte {
	w := h.win(i)
	w.connMu.Lock()
	defer w.connMu.Unlock()
	return append([]byte{}, w.pendingIn...)
}

// TestKeysTypedBeforeAMoveAreDiscarded: keys typed while the Run moves
// were meant for the old shell; the fresh shell on the new placement gets
// none of them, the user is told, and keys typed after reach it.
func TestKeysTypedBeforeAMoveAreDiscarded(t *testing.T) {
	var keys inputLog
	h := newHarness(t, cfgDefault, func(l *fakelux.Server) { l.InputHook = keys.hook })
	a := h.live(0)
	h.type_("before\r")
	h.waitFor("typed before", func() bool { return bytes.Contains(keys.of(a), []byte("before\r")) })
	h.lux.Move(a, true)
	h.waitFor("reconnecting", func() bool { return h.phase(0) == phaseReconnecting })
	h.type_(":wq\r")
	h.waitFor("kept", func() bool { return string(h.pending(0)) == ":wq\r" })
	n := len(keys.of(a))
	h.lux.Start(a)
	h.live(0)
	h.waitScreen("discarded notice", h.contains("window 0: 4 typed bytes discarded — it moved to a new shell"))
	h.type_("after\r")
	h.waitFor("typed after", func() bool { return bytes.Contains(keys.of(a), []byte("after\r")) })
	if got := keys.of(a)[n:]; string(got) != "after\r" {
		t.Fatalf("the new shell got %q", got)
	}
	h.detach()
}

// TestKeysTypedDuringABlipAreSent: on the same placement (a blip, the
// stream fell behind) the shell is the same one: kept keys reach it, in
// order, and nothing is said about discarding.
func TestKeysTypedDuringABlipAreSent(t *testing.T) {
	for _, drop := range []string{"blip", "fell behind"} {
		t.Run(drop, func(t *testing.T) {
			var keys inputLog
			var gate attachGate
			h := newHarness(t, cfgDefault, func(l *fakelux.Server) { l.InputHook = keys.hook; l.AttachHook = gate.hook })
			a := h.live(0)
			gate.hold(a)
			t.Cleanup(func() { gate.release(a) })
			if drop == "blip" {
				h.lux.Blip(a)
			} else {
				h.lux.FellBehind(a)
			}
			h.waitFor("attach held", func() bool { return gate.held_() == 1 })
			h.type_("kept-1,")
			h.type_("kept-2")
			h.waitFor("kept", func() bool { return string(h.pending(0)) == "kept-1,kept-2" })
			gate.release(a)
			h.waitFor("sent", func() bool { return string(keys.of(a)) == "kept-1,kept-2" })
			h.live(0)
			if strings.Contains(string(h.term.rawBytes()), "discarded") {
				t.Fatal("discard notice on the same placement")
			}
			h.detach()
		})
	}
}
