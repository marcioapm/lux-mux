package mux

import (
	"strings"
	"testing"

	"github.com/marcioapm/lux-mux/internal/fakelux"
	"github.com/marcioapm/lux-mux/internal/screen"
)

// kittyNvim is what a program like nvim writes on the alternate screen:
// it pushes kitty keyboard flags and sets modifyOtherKeys.
const kittyNvim = "\x1b[?1049h\x1b[>1u\x1b[>4;2m\x1b[H\x1b[2JNVIM-SCREEN"

func wantKeys(km screen.KeyModes, kitty, mok int) bool {
	return km.Alt.Flags == kitty && km.Main.Flags == 0 && km.ModifyOtherKeys == mok
}

// TestKeyModesFollowTheWindow: the keyboard modes a window's program set
// reach the terminal while it is shown, are reset for another window and
// on detach, and are set again when the window is repainted.
func TestKeyModesFollowTheWindow(t *testing.T) {
	h := newHarness(t, cfgDefault, nil)
	a := h.live(0)
	h.lux.Term(a).Output([]byte(kittyNvim))
	h.waitScreen("nvim", h.contains("NVIM-SCREEN"))
	h.waitFor("modes on the terminal", func() bool { return wantKeys(h.term.keyModes(), 1, 2) })

	// The prefix as the terminal now sends it: kitty, then modifyOtherKeys.
	h.type_("\x1b[98;5uc")
	h.live(1)
	h.waitFor("window 1 shown", func() bool { return h.title() == "luxmux:s [1] default" })
	if km := h.term.keyModes(); !km.Zero() {
		t.Fatalf("window 0's keyboard modes left on for window 1: %+v", km)
	}
	h.type_("\x1b[27;5;98~0")
	h.waitScreen("window 0 back", h.contains("NVIM-SCREEN"))
	h.waitFor("modes set again", func() bool { return wantKeys(h.term.keyModes(), 1, 2) })

	// A repaint of the shown window (a notice taken down) sets them again
	// after its reset.
	h.type_("\x1b[98;5u9")
	h.waitScreen("notice", h.contains("no window 9"))
	h.waitFor("notice gone", func() bool { return !strings.Contains(h.term.screen(), "no window 9") })
	if km := h.term.keyModes(); !wantKeys(km, 1, 2) {
		t.Fatalf("after the repaint: %+v", km)
	}

	h.type_("\x1b[98;5ud")
	select {
	case r := <-h.done:
		if r != "detached" {
			t.Fatalf("Run returned %q", r)
		}
	case <-timeout():
		t.Fatal("the encoded prefix did not detach")
	}
	if km := h.term.keyModes(); !km.Zero() {
		t.Fatalf("keyboard modes left after detach: %+v", km)
	}
}

// TestEncodedPrefixTwiceReachesTheWindow: the prefix pressed twice sends
// the window one prefix, in the encoding the terminal used.
func TestEncodedPrefixTwiceReachesTheWindow(t *testing.T) {
	var keys inputLog
	h := newHarness(t, cfgDefault, func(l *fakelux.Server) { l.InputHook = keys.hook })
	a := h.live(0)
	h.type_("\x1b[98;5u\x1b[98;5u|\x1b[27;5;98~\x1b[27;5;98~|\x02\x02.")
	want := "\x1b[98;5u|\x1b[27;5;98~|\x02."
	h.waitFor("keys", func() bool { return string(keys.of(a)) == want })
	h.detach()
}

// TestKeyModesResetOnNewShell: a shell on a new placement starts without
// the old program's keyboard modes.
func TestKeyModesResetOnNewShell(t *testing.T) {
	h := newHarness(t, cfgDefault, nil)
	a := h.live(0)
	h.lux.Term(a).Output([]byte(kittyNvim))
	h.waitFor("modes on the terminal", func() bool { return wantKeys(h.term.keyModes(), 1, 2) })
	h.lux.Move(a, false)
	h.waitScreen("new shell notice", h.contains("new shell"))
	h.waitFor("modes gone", func() bool { return h.term.keyModes().Zero() })
	h.detach()
}

// TestKeyModesBackAfterPromptOverStaleWindow: a prompt closed over the
// shown window whose emulator is stale (only the bottom row is redrawn)
// turns the window's keyboard modes back on, once: a notice after it
// leaves them as they are.
func TestKeyModesBackAfterPromptOverStaleWindow(t *testing.T) {
	h := newHarness(t, cfgDefault, nil)
	a := h.live(0)
	h.lux.Term(a).Output([]byte(kittyNvim))
	h.waitFor("modes on the terminal", func() bool { return wantKeys(h.term.keyModes(), 1, 2) })
	h.overflow(0, a)
	n := len(attachesOf(h.lux, a))
	h.type_("\x1b[98;5u,")
	h.waitScreen("rename prompt", h.contains("rename window:"))
	h.waitFor("modes off in the prompt", func() bool { return h.term.keyModes().Zero() })
	h.type_("\x1b")
	h.waitFor("prompt closed", func() bool { return !strings.Contains(h.term.screen(), "rename window:") })
	h.waitFor("modes back", func() bool { return wantKeys(h.term.keyModes(), 1, 2) })
	h.m.Notice("a notice")
	h.noticeShown("a notice")
	if km := h.term.keyModes(); !wantKeys(km, 1, 2) || len(km.Alt.Saved) != 1 {
		t.Fatalf("after a notice: %+v", km)
	}
	if !h.win(0).q.isStale() || len(attachesOf(h.lux, a)) != n {
		t.Fatal("the window was refreshed: the stale path was not tested")
	}
	h.detach()
}

// TestKeyModesOffInPrompts: a prompt, help and a dialog read keys as plain
// bytes, so the window's keyboard modes are off while one is open, and
// back after.
func TestKeyModesOffInPrompts(t *testing.T) {
	h := newHarness(t, cfgDefault, nil)
	ui := &fakeUI{choose: 0, hold: make(chan struct{}), opened: make(chan struct{})}
	h.m.opt.UI = ui
	a := h.live(0)
	h.lux.Term(a).Output([]byte(kittyNvim))
	h.waitFor("modes on the terminal", func() bool { return wantKeys(h.term.keyModes(), 1, 2) })
	h.type_("\x1b[98;5u,")
	h.waitScreen("rename prompt", h.contains("rename window:"))
	h.waitFor("modes off in the prompt", func() bool { return h.term.keyModes().Zero() })
	h.type_("\x1b")
	h.waitFor("prompt closed", func() bool { return !strings.Contains(h.term.screen(), "rename window:") })
	h.waitFor("modes back", func() bool { return wantKeys(h.term.keyModes(), 1, 2) })

	h.type_("\x1b[98;5u?")
	h.waitScreen("help", h.contains("luxmux keys"))
	if km := h.term.keyModes(); !km.Zero() {
		t.Fatalf("modes on under help: %+v", km)
	}
	h.type_("x")
	h.waitFor("help closed, modes back", func() bool {
		return !strings.Contains(h.term.screen(), "luxmux keys") && wantKeys(h.term.keyModes(), 1, 2)
	})

	h.type_("\x1b[98;5uw")
	<-ui.opened
	h.waitScreen("chooser", h.contains("CHOOSER-OPEN"))
	if km := h.term.keyModes(); !km.Zero() {
		t.Fatalf("modes on under the chooser: %+v", km)
	}
	close(ui.hold)
	h.waitFor("chooser closed, modes back", func() bool {
		return !strings.Contains(h.term.screen(), "CHOOSER-OPEN") && wantKeys(h.term.keyModes(), 1, 2)
	})
	h.detach()
}
