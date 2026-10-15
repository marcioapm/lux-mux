package mux

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/marcioapm/lux-mux/internal/fakelux"
)

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
// not hold up the keyboard: keystrokes still reach the active window's
// stream and C-b d still detaches.
func TestKeysFlowWhileTerminalStalled(t *testing.T) {
	var keys inputLog
	h := newHarness(t, cfgDefault, func(l *fakelux.Server) { l.InputHook = keys.hook })
	a := h.live(0)
	h.type_("ready\r\n")
	h.waitScreen("echo", h.contains("ready"))
	h.term.setStall(true)
	t.Cleanup(func() { h.term.setStall(false) })
	// More output than the terminal queue holds: the stream's reader now
	// waits for the terminal.
	big := bytes.Repeat([]byte("flood-line-0123456789abcdef\r\n"), (3<<20)/29)
	h.lux.Term(a).Output(big)
	h.lux.Term(a).Output(big)
	time.Sleep(100 * time.Millisecond)
	h.type_("typed-while-stalled")
	h.waitFor("keys reached the stream", func() bool {
		return bytes.Contains(keys.of(a), []byte("typed-while-stalled"))
	})
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

// overflow fills window i's emulator queue past feedLimit while its
// emulator is held, then lets it go.
func (h *harness) overflow(i int, id string) {
	h.t.Helper()
	h.m.mu.Lock()
	w := h.m.windows[i]
	h.m.mu.Unlock()
	w.emuMu.Lock()
	chunk := bytes.Repeat([]byte("OVERFLOW-FILL-0123456789\r\n"), 1<<14)
	for sent := 0; sent <= feedLimit+len(chunk); sent += len(chunk) {
		h.lux.Term(id).Output(chunk)
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
