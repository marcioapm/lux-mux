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

// TestStaleShownWindowRedrawsAfterHelp: closing help over a stale shown
// window clears the screen and asks its program to redraw (a resize
// nudge), without a reattach.
func TestStaleShownWindowRedrawsAfterHelp(t *testing.T) {
	h := newHarness(t, cfgDefault, nil)
	a := h.live(0)
	h.overflow(0, a)
	n := len(attachesOf(h.lux, a))
	term := h.lux.Term(a)
	r0 := len(term.Resizes())
	h.type_("\x02?")
	h.waitScreen("help", h.contains("luxmux keys"))
	h.type_("x")
	h.waitFor("help gone", func() bool { return !strings.Contains(h.term.screen(), "luxmux keys") })
	h.waitFor("redraw nudge", func() bool { return len(term.Resizes()) >= r0+2 })
	if got := len(attachesOf(h.lux, a)); got != n {
		t.Fatalf("%d reattaches", got-n)
	}
	if strings.Contains(h.term.screen(), "OVERFLOW-FILL") || strings.Contains(h.term.screen(), "refreshing") {
		t.Fatalf("screen not cleared for the redraw:\n%s", h.term.screen())
	}
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
		name, state, reason, want string
	}{
		{"failed", "failed", "timeout", "ended: failed: timeout"},
		{"stopped", "stopped", "stop", "stopped by lux stop"},
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
			h.waitScreen("its end", h.contains(tc.want))
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
	h.waitScreen("dropped notice", h.contains("typed bytes dropped"))
	gate.release(a)
	want := all[len(all)-pendingInLimit:]
	h.waitFor("kept keys", func() bool { return len(keys.of(a)) >= len(want) })
	if got := keys.of(a); !bytes.Equal(got, want) {
		t.Fatalf("got %d bytes starting %q, want the last %d", len(got), got[:16], len(want))
	}
	h.detach()
}
