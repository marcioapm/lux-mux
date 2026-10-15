package mux

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/marcioapm/lux-mux/internal/fakelux"
)

// isBehind reports whether live output was dropped and no repaint has
// been queued since.
func (t *termWriter) isBehind() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.behind
}

// inputLog records the keystrokes each Run's streams received.
type inputLog struct {
	mu  sync.Mutex
	got map[string][]byte
}

func (l *inputLog) hook(id string, d []byte) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.got == nil {
		l.got = map[string][]byte{}
	}
	l.got[id] = append(l.got[id], d...)
}

func (l *inputLog) of(id string) []byte {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]byte{}, l.got[id]...)
}

// TestKeysFlowWhileTerminalStalled: a terminal that takes no output does
// not hold up the keyboard or the stream: keystrokes still reach the
// active window's stream, its output is still read (lux does not drop the
// stream), and C-b d still detaches.
func TestKeysFlowWhileTerminalStalled(t *testing.T) {
	var keys inputLog
	h := newHarness(t, cfgDefault, func(l *fakelux.Server) { l.InputHook = keys.hook })
	a := h.live(0)
	h.type_("ready\r\n")
	h.waitScreen("echo", h.contains("ready"))
	h.term.setStall(true)
	t.Cleanup(func() { h.term.setStall(false) })
	// More output than the terminal's queue holds, paced within the
	// host's budget for the stream.
	big := bytes.Repeat([]byte("flood-line-0123456789abcdef\r\n"), (1<<20)/29)
	term := h.lux.Term(a)
	for range 8 {
		term.Output(big)
		h.waitFor("stream read", func() bool { return term.Queued() < 2<<20 })
	}
	h.type_("typed-while-stalled")
	h.waitFor("keys reached the stream", func() bool {
		return bytes.Contains(keys.of(a), []byte("typed-while-stalled"))
	})
	if n := len(attachesOf(h.lux, a)); n != 1 {
		t.Fatalf("%d attaches: the stream was dropped", n)
	}
	h.type_("\x02d")
	select {
	case r := <-h.done:
		if r != "detached" {
			t.Fatalf("Run returned %q", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("C-b d did not detach while the terminal was stalled")
	}
}

// TestSlowTerminalCatchesUp: a terminal that fell more than liveLimit
// behind misses the output in between, then shows the window's screen as
// it is.
func TestSlowTerminalCatchesUp(t *testing.T) {
	h := newHarness(t, cfgDefault, nil)
	a := h.live(0)
	h.type_("ready\r\n")
	h.waitScreen("echo", h.contains("ready"))
	h.term.setStall(true)
	big := bytes.Repeat([]byte("flood-line-0123456789abcdef\r\n"), (1<<20)/29)
	term := h.lux.Term(a)
	for range 6 {
		term.Output(big)
		h.waitFor("stream read", func() bool { return term.Queued() < 2<<20 })
	}
	h.waitFor("behind", h.m.tw.isBehind)
	// Live output while behind is not queued for the terminal: the
	// repaint after it catches up shows it.
	term.Output([]byte("AFTER-DROP"))
	h.waitFor("emulator has it", func() bool { return strings.Contains(h.emuScreen(0), "AFTER-DROP") })
	term.Output([]byte("\x1b[2J\x1b[HFINAL-SCREEN"))
	h.waitFor("emulator", func() bool { return strings.Contains(h.emuScreen(0), "FINAL-SCREEN") })
	before := len(h.term.rawBytes())
	h.term.setStall(false)
	h.waitScreen("final screen", func(s string) bool { return s == h.emuScreen(0) })
	if !strings.Contains(h.term.screen(), "FINAL-SCREEN") {
		t.Fatal("final screen missing")
	}
	got := h.term.rawBytes()[before:]
	rp := bytes.Index(got, []byte("\x18\x1b[?1049l"))
	if rp < 0 {
		t.Fatal("no repaint")
	}
	if bytes.Contains(got[:rp], []byte("AFTER-DROP")) || bytes.Contains(got[:rp], []byte("FINAL-SCREEN")) {
		t.Fatal("live output queued while behind reached the terminal before the repaint")
	}
	h.type_("after\r\n")
	h.waitScreen("live again", h.contains("after"))
	h.detach()
}

// overflow fills window i's emulator queue past feedLimit while its
// emulator is held, then lets it go. The output is paced so the host's
// own budget for the stream is never passed: luxmux reads it all.
func (h *harness) overflow(i int, id string) {
	h.t.Helper()
	h.m.mu.Lock()
	w := h.m.windows[i]
	h.m.mu.Unlock()
	w.emuMu.Lock()
	chunk := bytes.Repeat([]byte("OVERFLOW-FILL-0123456789\r\n"), 1<<14)
	term := h.lux.Term(id)
	for sent := 0; sent <= feedLimit+len(chunk); sent += len(chunk) {
		term.Output(chunk)
		h.waitFor("stream drained", func() bool { return term.Queued() < 2<<20 && h.termQueued() < liveLimit/2 })
	}
	h.waitFor("stale", w.q.isStale)
	w.emuMu.Unlock()
}

// TestOverflowedWindowReattaches: a background window whose emulator falls
// more than feedLimit behind is marked stale without slowing the shown
// window; shown again, it is attached again with restore and painted from
// lux's snapshot.
func TestOverflowedWindowReattaches(t *testing.T) {
	var keys inputLog
	h := newHarness(t, cfgDefault, func(l *fakelux.Server) { l.InputHook = keys.hook })
	a := h.live(0)
	h.type_("\x02c")
	b := h.live(1)
	h.overflow(0, a)
	h.lux.Term(a).Output([]byte("\x1b[2J\x1b[HAFTER-OVERFLOW"))
	// The shown window is untouched.
	h.type_("still-typing\r\n")
	h.waitScreen("window 1 echo", h.contains("still-typing"))
	if !bytes.Contains(keys.of(b), []byte("still-typing")) {
		t.Fatal("keys did not reach window 1")
	}
	before := len(attachesOf(h.lux, a))
	h.type_("\x020")
	h.waitFor("reattached with restore", func() bool {
		as := attachesOf(h.lux, a)
		return len(as) > before && as[len(as)-1].Restore
	})
	h.waitScreen("window 0 from the snapshot", h.contains("AFTER-OVERFLOW"))
	if strings.Contains(h.term.screen(), "still-typing") {
		t.Fatalf("window 1 left on screen:\n%s", h.term.screen())
	}
	h.m.mu.Lock()
	stale := h.m.windows[0].q.isStale()
	h.m.mu.Unlock()
	if stale {
		t.Fatal("still stale after the restore")
	}
	h.detach()
}

// TestOverflowShownWindowKeepsRawOutput: the shown window's own output
// reaches the terminal whole even when its emulator overflows.
func TestOverflowShownWindowKeepsRawOutput(t *testing.T) {
	h := newHarness(t, cfgDefault, nil)
	a := h.live(0)
	h.waitFor("settled", func() bool { return len(h.term.rawBytes()) > 0 })
	time.Sleep(100 * time.Millisecond)
	before := len(h.term.rawBytes())
	h.overflow(0, a)
	h.lux.Term(a).Output([]byte("TAIL-MARK"))
	h.waitFor("all output", func() bool { return bytes.Contains(h.term.rawBytes()[before:], []byte("TAIL-MARK")) })
	got := h.term.rawBytes()[before:]
	if n := bytes.Count(got, []byte("OVERFLOW-FILL-0123456789\r\n")); n < feedLimit/26 {
		t.Fatalf("only %d lines reached the terminal", n)
	}
	h.detach()
}

// TestOverflowWhilePainting: a window switched to whose emulator overflows
// before the paint was drawn shows the placeholder, and is attached again
// so lux's snapshot replaces it.
func TestOverflowWhilePainting(t *testing.T) {
	h := newHarness(t, cfgDefault, nil)
	a := h.live(0)
	h.lux.Term(a).Output([]byte("BEFORE-OVERFLOW"))
	h.type_("\x02c")
	h.live(1)
	h.waitFor("window 1 shown", func() bool { return h.title() == "luxmux:s [1] default" })
	w := h.win(0)
	w.emuMu.Lock()
	// The feeder takes this and waits for the emulator, so what follows
	// stays queued.
	w.q.push(feedItem{data: []byte(" ")})
	h.waitFor("feeder busy", func() bool { w.q.mu.Lock(); defer w.q.mu.Unlock(); return len(w.q.items) == 0 })
	before := len(attachesOf(h.lux, a))
	h.type_("\x020")
	// The paint is queued behind the held emulator.
	h.waitFor("paint queued", func() bool {
		w.q.mu.Lock()
		defer w.q.mu.Unlock()
		for _, it := range w.q.items {
			if it.paint != nil {
				return true
			}
		}
		return false
	})
	chunk := bytes.Repeat([]byte("OVERFLOW-FILL-0123456789\r\n"), 1<<14)
	term := h.lux.Term(a)
	for !w.q.isStale() {
		w.q.mu.Lock()
		want := w.q.bytes + len(chunk)
		w.q.mu.Unlock()
		term.Output(chunk)
		// Paced on what reached the emulator's queue: this attach may be
		// a plain one, whose host queue misses output silently.
		ok := fakelux.WaitFor(context.Background(), 5*time.Second, func() bool {
			w.q.mu.Lock()
			defer w.q.mu.Unlock()
			return w.q.stale || w.q.bytes >= want
		})
		if !ok {
			w.q.mu.Lock()
			defer w.q.mu.Unlock()
			t.Fatalf("chunk not queued for the emulator: want %d bytes, %d queued", want, w.q.bytes)
		}
	}
	w.emuMu.Unlock()
	h.waitFor("reattached with restore", func() bool {
		as := attachesOf(h.lux, a)
		return len(as) > before && as[len(as)-1].Restore
	})
	term.Output([]byte("\x1b[2J\x1b[HFROM-THE-SNAPSHOT"))
	h.waitScreen("window 0 from lux", h.contains("FROM-THE-SNAPSHOT"))
	if strings.Contains(h.term.screen(), "refreshing") {
		t.Fatalf("placeholder left:\n%s", h.term.screen())
	}
	h.detach()
}

// TestStaleFeedQueueDropsData: once overflowed, a feed queue takes no
// output until restart (the stream that replaces it starts from lux's
// snapshot); resizes and paints are still queued.
func TestStaleFeedQueueDropsData(t *testing.T) {
	q := &feedQueue{wake: make(chan struct{}, 1)}
	big := make([]byte, feedLimit)
	q.push(feedItem{data: big})
	if !q.push(feedItem{data: []byte("x")}) || !q.isStale() {
		t.Fatal("no overflow")
	}
	n := len(q.items)
	q.push(feedItem{data: []byte("while stale")})
	q.push(feedItem{rows: 10, cols: 20})
	if q.bytes != 0 || len(q.items) != n+1 || q.items[n].rows != 10 {
		t.Fatalf("stale queue: %d bytes, %d items (had %d)", q.bytes, len(q.items), n)
	}
	q.restart()
	q.push(feedItem{data: []byte("after")})
	if q.isStale() || q.bytes != len("after") {
		t.Fatalf("after restart: stale %v, %d bytes", q.isStale(), q.bytes)
	}
}

// TestPreviewOfStaleWindowRefreshes: the chooser's preview of a stale
// window that is not shown attaches it again.
func TestPreviewOfStaleWindowRefreshes(t *testing.T) {
	h := newHarness(t, cfgDefault, nil)
	a := h.live(0)
	h.type_("\x02c")
	h.live(1)
	h.overflow(0, a)
	before := len(attachesOf(h.lux, a))
	if p := h.m.Preview(0, 5, 40); !strings.Contains(strings.Join(p, ""), "refreshing") {
		t.Fatalf("preview %q", p)
	}
	h.waitFor("reattached with restore", func() bool {
		as := attachesOf(h.lux, a)
		return len(as) > before && as[len(as)-1].Restore
	})
	h.detach()
}

// TestPreviewHoldsNoGlobalLock: rendering a window's preview waits only on
// that window's emulator: while it waits, keys and other output flow; a
// second render within previewFor reuses the first.
func TestPreviewHoldsNoGlobalLock(t *testing.T) {
	var keys inputLog
	h := newHarness(t, cfgDefault, func(l *fakelux.Server) { l.InputHook = keys.hook })
	a := h.live(0)
	h.lux.Term(a).Output([]byte("preview-me"))
	h.type_("\x02c")
	b := h.live(1)
	h.waitFor("emulator 0", func() bool { return strings.Contains(h.emuScreen(0), "preview-me") })
	h.m.mu.Lock()
	w := h.m.windows[0]
	h.m.mu.Unlock()
	w.emuMu.Lock()
	got := make(chan []string, 1)
	go func() { got <- h.m.Preview(0, 5, 40) }()
	time.Sleep(20 * time.Millisecond)
	h.type_("typed-during-preview")
	h.waitFor("keys flow", func() bool { return bytes.Contains(keys.of(b), []byte("typed-during-preview")) })
	h.lux.Term(b).Output([]byte("OUT-DURING-PREVIEW"))
	h.waitScreen("output flows", h.contains("OUT-DURING-PREVIEW"))
	w.emuMu.Unlock()
	first := <-got
	if !strings.Contains(strings.Join(first, "\n"), "preview-me") {
		t.Fatalf("preview %q", first)
	}
	// Cached: rendered again with the emulator busy, at once.
	w.emuMu.Lock()
	again := make(chan []string, 1)
	go func() { again <- h.m.Preview(0, 5, 40) }()
	select {
	case p := <-again:
		w.emuMu.Unlock()
		if strings.Join(p, "\n") != strings.Join(first, "\n") {
			t.Fatalf("cached preview %q", p)
		}
	case <-time.After(time.Second):
		w.emuMu.Unlock()
		t.Fatal("the second render waited on the emulator")
	}
	h.detach()
}

// TestEmulatorKeepsNoHistory: a window's emulator holds its screen only
// (luxmux reads nothing else), so lines scrolled off are not retained.
func TestEmulatorKeepsNoHistory(t *testing.T) {
	h := newHarness(t, cfgDefault, nil)
	a := h.live(0)
	h.lux.Term(a).Output(bytes.Repeat([]byte("scrolled line\r\n"), 200))
	h.lux.Term(a).Output([]byte("LAST"))
	h.waitFor("emulator", func() bool { return strings.Contains(h.emuScreen(0), "LAST") })
	h.m.mu.Lock()
	w := h.m.windows[0]
	h.m.mu.Unlock()
	w.withEmu(func(t2 *vtermT) {
		if n := t2.HistoryLen(); n != 0 {
			t.Errorf("history of %d lines", n)
		}
	})
	h.detach()
}

// termQueued is how much live output waits for the terminal.
func (h *harness) termQueued() int {
	h.m.tw.mu.Lock()
	defer h.m.tw.mu.Unlock()
	return h.m.tw.live
}
