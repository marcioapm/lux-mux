package mux

import (
	"bytes"
	"strings"
	"testing"
)

const cfgDefault = ""

// TestSwitchRepaintsAndRestoresModes: a window switched to is painted
// from its emulator, with the modes its program set, into the terminal
// that showed another window.
func TestSwitchRepaintsAndRestoresModes(t *testing.T) {
	h := newHarness(t, cfgDefault, nil)
	a := h.live(0)
	h.type_("first-window-text\r\n")
	h.waitScreen("window 0 echo", h.contains("first-window-text"))
	// The program in window 0 sets bracketed paste, application cursor
	// keys and mouse tracking, and a coloured word.
	h.lux.Term(a).Output([]byte("\x1b[?2004h\x1b[?1h\x1b[?1000h\x1b[31mRED\x1b[m"))
	h.waitFor("modes on the terminal", func() bool { return h.term.mode(2004) && h.term.mode(1) && h.term.mode(1000) })

	h.type_("\x02c")
	b := h.live(1)
	h.waitFor("window 1 shown", func() bool { return h.title() == "luxmux:s [1] default" })
	h.waitFor("modes reset for window 1", func() bool { return !h.term.mode(2004) && !h.term.mode(1) && !h.term.mode(1000) })
	if strings.Contains(h.term.screen(), "first-window-text") {
		t.Fatalf("window 0's text on window 1's screen:\n%s", h.term.screen())
	}
	// Window 1 runs a full-screen program.
	h.lux.Term(b).Output([]byte("\x1b[?1049h\x1b[H\x1b[2JFULLSCREEN-B"))
	h.waitScreen("window 1 alt screen", h.contains("FULLSCREEN-B"))

	before := len(h.term.rawBytes())
	h.type_("\x020")
	h.waitScreen("window 0 back", h.contains("first-window-text"))
	h.waitFor("window 0 modes back", func() bool { return h.term.mode(2004) && h.term.mode(1) && h.term.mode(1000) })
	if h.term.altScreen() {
		t.Fatal("window 0 is on the main screen")
	}
	if strings.Contains(h.term.screen(), "FULLSCREEN-B") {
		t.Fatalf("window 1's screen left over:\n%s", h.term.screen())
	}
	if h.title() != "luxmux:s [0] default" {
		t.Fatalf("title %q", h.title())
	}
	// The colour came from the emulator, as SGR 31.
	paint := h.term.rawBytes()[before:]
	if !bytes.Contains(paint, []byte("\x1b[0;31mRED")) {
		t.Fatalf("repaint lacks the red word: %q", paint)
	}

	h.type_("\x021")
	h.waitScreen("window 1 back", h.contains("FULLSCREEN-B"))
	h.waitFor("alt screen restored", h.term.altScreen)
	h.detach()
}

func (h *harness) title() string { return h.term.title() }

// TestLiveOutputPassesThroughRaw: the active window's output reaches the
// terminal byte for byte; another window's does not reach it at all.
func TestLiveOutputPassesThroughRaw(t *testing.T) {
	h := newHarness(t, cfgDefault, nil)
	a := h.live(0)
	h.type_("\x02c")
	b := h.live(1)
	h.waitFor("window 1 shown", func() bool { return h.title() == "luxmux:s [1] default" })
	marker := []byte("\x1b[1;35mpassthrough\x1b[0m\x07odd\x1b]0;t\x07")
	before := len(h.term.rawBytes())
	h.lux.Term(a).Output([]byte("HIDDEN-OUTPUT"))
	h.lux.Term(b).Output(marker)
	h.waitFor("marker", func() bool { return bytes.Contains(h.term.rawBytes()[before:], marker) })
	if bytes.Contains(h.term.rawBytes(), []byte("HIDDEN-OUTPUT")) {
		t.Fatal("a background window's output reached the terminal")
	}
	h.type_("\x020")
	h.waitScreen("background output in window 0's repaint", h.contains("HIDDEN-OUTPUT"))
	h.detach()
}

func TestNavigationKeys(t *testing.T) {
	h := newHarness(t, "[keys.direct]\n\"alt-1\" = \"select 1\"\n\"alt-n\" = \"next\"\n", nil)
	h.live(0)
	h.type_("\x02c")
	h.live(1)
	h.type_("\x02c")
	h.live(2)
	at := func(i int) func() bool {
		return func() bool { h.m.mu.Lock(); defer h.m.mu.Unlock(); return h.m.active == i }
	}
	h.waitFor("2", at(2))
	h.type_("\x02n")
	h.waitFor("next wraps", at(0))
	h.type_("\x02p")
	h.waitFor("prev wraps", at(2))
	h.type_("\x02l")
	h.waitFor("last", at(0))
	h.type_("\x1b1")
	h.waitFor("alt-1", at(1))
	h.type_("\x1bn")
	h.waitFor("alt-n", at(2))
	h.type_("\x02'1\r")
	h.waitFor("prompted index", at(1))
	h.type_("\x02,")
	h.waitScreen("rename prompt", h.contains("rename window: default"))
	h.type_("\x7f\x7f\x7f\x7f\x7f\x7f\x7fedit\r")
	h.waitFor("renamed", func() bool { return h.title() == "luxmux:s [1] edit" })
	h.type_("\x029")
	h.waitScreen("no window 9", h.contains("no window 9"))
	h.waitFor("notice gone", func() bool { return !strings.Contains(h.term.screen(), "no window 9") })
	h.type_("\x02?")
	h.waitScreen("help", h.contains("luxmux keys"))
	h.type_("x")
	h.waitFor("help gone", func() bool { return !strings.Contains(h.term.screen(), "luxmux keys") })
	h.detach()
}

func TestKillWindowConfirms(t *testing.T) {
	h := newHarness(t, cfgDefault, nil)
	h.live(0)
	h.type_("\x02c")
	b := h.live(1)
	h.type_("\x02&")
	h.waitScreen("confirm", h.contains("kill window 1 (default)? (y/N)"))
	h.type_("n")
	h.waitFor("prompt closed", func() bool { return !strings.Contains(h.term.screen(), "(y/N)") })
	if h.nwindows() != 2 {
		t.Fatal("n killed the window")
	}
	h.type_("\x02&y")
	h.waitFor("window gone", func() bool { return h.nwindows() == 1 })
	h.waitFor("Run terminated", func() bool { return h.lux.Run(b).State == "terminated" })
	h.detach()
}

// TestShellExitClosesWindow: the shell's exit terminates the Run and
// closes the window; the last one ends the session.
func TestShellExitClosesWindow(t *testing.T) {
	h := newHarness(t, cfgDefault, nil)
	a := h.live(0)
	h.type_("\x02c")
	b := h.live(1)
	h.lux.Term(b).Exit()
	h.waitFor("window 1 closed", func() bool { return h.nwindows() == 1 })
	h.waitFor("Run 1 terminated", func() bool { return h.lux.Run(b).State == "terminated" })
	h.waitScreen("exit notice", h.contains("window 1 exited"))
	h.lux.Term(a).Exit()
	select {
	case r := <-h.done:
		if r != "exited" {
			t.Fatalf("Run returned %q", r)
		}
	case <-timeout():
		t.Fatal("the last window's exit did not end the session")
	}
	if h.lux.Run(a).State != "terminated" {
		t.Fatal("Run 0 not terminated")
	}
}
