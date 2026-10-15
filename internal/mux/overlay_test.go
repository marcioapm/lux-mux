package mux

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/marcioapm/lux-mux/internal/fakelux"
)

// emuScreen is window i's emulator screen as text, as clientTerm.screen.
func (h *harness) emuScreen(i int) string {
	h.m.mu.Lock()
	w := h.m.windows[i]
	h.m.mu.Unlock()
	var out string
	w.withEmu(func(t *vtermT) {
		rows, cols := t.Size()
		var lines []string
		for y := 0; y < rows; y++ {
			var b strings.Builder
			for x := 0; x < cols; x++ {
				b.WriteString(t.Text(t.Cell(x, y)))
			}
			lines = append(lines, strings.TrimRight(b.String(), " "))
		}
		out = strings.Join(lines, "\n")
	})
	return out
}

// TestNoticeClearAfterScrollLeavesNoGhost: output that scrolls while a
// notice is up does not leave the notice's text behind once it goes.
func TestNoticeClearAfterScrollLeavesNoGhost(t *testing.T) {
	h := newHarness(t, cfgDefault, nil)
	a := h.live(0)
	for i := range 30 {
		h.lux.Term(a).Output(fmt.Appendf(nil, "line %d\r\n", i))
	}
	h.waitScreen("lines", h.contains("line 29"))
	// Long enough for the output below to scroll while the notice is up.
	h.m.mu.Lock()
	h.m.opt.NoticeFor = 400 * time.Millisecond
	h.m.mu.Unlock()
	h.type_("\x029")
	h.waitScreen("notice", h.contains("no window 9"))
	for i := range 3 {
		h.lux.Term(a).Output(fmt.Appendf(nil, "more %d\r\n", i))
	}
	h.waitScreen("more", h.contains("more 2"))
	// The first line overwrote the start of the notice's row; the rest of
	// the notice scrolled up with it.
	if !strings.Contains(h.term.screen(), "ndow 9") {
		t.Fatalf("the notice went before the output scrolled it:\n%s", h.term.screen())
	}
	h.waitFor("notice gone, screen as the emulator", func() bool {
		s := h.term.screen()
		return !strings.Contains(s, "ndow 9") && s == h.emuScreen(0)
	})
	h.detach()
}

// TestHelpSuspendsOutput: the active window's output is not drawn over
// the help, and is on screen once the help closes.
func TestHelpSuspendsOutput(t *testing.T) {
	h := newHarness(t, cfgDefault, nil)
	a := h.live(0)
	h.type_("\x02?")
	h.waitScreen("help", h.contains("luxmux keys"))
	before := len(h.term.rawBytes())
	h.lux.Term(a).Output([]byte("OVERHELP"))
	h.waitFor("emulator has it", func() bool { return strings.Contains(h.emuScreen(0), "OVERHELP") })
	time.Sleep(50 * time.Millisecond)
	if bytes.Contains(h.term.rawBytes()[before:], []byte("OVERHELP")) {
		t.Fatal("output drawn over the help")
	}
	h.type_("x")
	h.waitScreen("output after help", func(s string) bool {
		return strings.Contains(s, "OVERHELP") && !strings.Contains(s, "luxmux keys")
	})
	h.detach()
}

// TestOutputWhileDialogOpen: the active window's output while a dialog
// owns the terminal is not drawn, and is shown when it closes.
func TestOutputWhileDialogOpen(t *testing.T) {
	h := newHarness(t, cfgDefault, nil)
	ui := &fakeUI{choose: 0, hold: make(chan struct{}), opened: make(chan struct{})}
	h.m.opt.UI = ui
	a := h.live(0)
	h.type_("\x02w")
	<-ui.opened
	h.waitScreen("chooser", h.contains("CHOOSER-OPEN"))
	before := len(h.term.rawBytes())
	h.lux.Term(a).Output([]byte("WHILE-DIALOG"))
	h.waitFor("emulator has it", func() bool { return strings.Contains(h.emuScreen(0), "WHILE-DIALOG") })
	time.Sleep(50 * time.Millisecond)
	if bytes.Contains(h.term.rawBytes()[before:], []byte("WHILE-DIALOG")) {
		t.Fatal("output drawn over the dialog")
	}
	close(ui.hold)
	h.waitScreen("output after the dialog", func(s string) bool {
		return strings.Contains(s, "WHILE-DIALOG") && !strings.Contains(s, "CHOOSER-OPEN")
	})
	h.detach()
}

// TestKeysAfterDialogKeyGoToDialog: bytes typed in the same read after
// C-b w are the dialog's, not the shell's.
func TestKeysAfterDialogKeyGoToDialog(t *testing.T) {
	var keys inputLog
	h := newHarness(t, cfgDefault, func(l *fakelux.Server) { l.InputHook = keys.hook })
	ui := &fakeUI{choose: 0, read: 3}
	h.m.opt.UI = ui
	a := h.live(0)
	h.type_("\x02wjj\r")
	h.waitScreen("chooser closed", func(s string) bool { return !strings.Contains(s, "CHOOSER-OPEN") && strings.Contains(h.title(), "[0]") })
	h.type_("after\r")
	h.waitFor("after reached the shell", func() bool { return bytes.Contains(keys.of(a), []byte("after")) })
	if string(ui.got) != "jj\r" {
		t.Fatalf("dialog read %q", ui.got)
	}
	if got := keys.of(a); bytes.Contains(got, []byte("jj")) {
		t.Fatalf("shell got %q", got)
	}
	h.detach()
}

// TestLiveOutputExact: the shown window's output reaches the terminal as
// the exact bytes of its frames, nothing added before, between or after.
func TestLiveOutputExact(t *testing.T) {
	h := newHarness(t, cfgDefault, nil)
	a := h.live(0)
	h.type_("settle\r\n")
	h.waitScreen("settled", h.contains("settle"))
	time.Sleep(100 * time.Millisecond)
	before := len(h.term.rawBytes())
	m1 := []byte("\x1b[1;35mpassthrough\x1b[0m\x07odd\x1b]0;t\x07")
	m2 := []byte("é second\x1b[3")
	h.lux.Term(a).Output(m1)
	h.lux.Term(a).Output(m2)
	want := append(append([]byte{}, m1...), m2...)
	h.waitFor("bytes", func() bool { return len(h.term.rawBytes())-before >= len(want) })
	time.Sleep(50 * time.Millisecond)
	if got := h.term.rawBytes()[before:]; !bytes.Equal(got, want) {
		t.Fatalf("terminal got %q, want %q", got, want)
	}
	h.detach()
}

// TestSwitchMidSequence: a window whose output stopped inside an escape
// sequence or a UTF-8 character is shown, then its output finishes it:
// the terminal takes it whole, not as text.
func TestSwitchMidSequence(t *testing.T) {
	for _, tc := range []struct{ name, first, rest, want, bad string }{
		{"csi", "start\x1b[3", "1mRED", "startRED", "1mRED"},
		{"utf8", "abc\xc3", "\xa9def", "abcédef", "abc"},
		{"osc", "osc\x1b]2;my ti", "tle\x07after", "oscafter", "tle"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, cfgDefault, nil)
			a := h.live(0)
			h.type_("\x02c")
			h.live(1)
			h.waitFor("window 1 shown", func() bool { return h.title() == "luxmux:s [1] default" })
			h.lux.Term(a).Output([]byte(tc.first))
			h.waitFor("emulator", func() bool { return strings.Contains(h.emuScreen(0), tc.first[:3]) })
			h.type_("\x020")
			h.waitFor("window 0 shown", func() bool { return strings.HasPrefix(h.title(), "luxmux:s [0]") })
			time.Sleep(20 * time.Millisecond)
			h.lux.Term(a).Output([]byte(tc.rest))
			h.waitScreen(tc.want, h.contains(tc.want))
			if s := h.term.screen(); strings.Contains(strings.ReplaceAll(s, tc.want, ""), tc.bad) {
				t.Fatalf("printed as text:\n%s", s)
			}
			h.detach()
		})
	}
}

// TestSwitchAwayMidSequence: the shown window's output stops inside an
// escape sequence and another window is shown: the repaint is not taken
// as part of that sequence.
func TestSwitchAwayMidSequence(t *testing.T) {
	for _, cut := range []string{"\x1b]2;unterminated title", "\x1bP1$r", "\x1b[38;2;1"} {
		t.Run(fmt.Sprintf("%q", cut), func(t *testing.T) {
			h := newHarness(t, cfgDefault, nil)
			h.live(0)
			h.type_("\x02c")
			b := h.live(1)
			h.lux.Term(b).Output([]byte("ONE-TEXT"))
			h.type_("\x020")
			h.waitFor("window 0", func() bool { return h.title() == "luxmux:s [0] default" })
			h.lux.Term(h.window(0)).Output([]byte("zero" + cut))
			time.Sleep(30 * time.Millisecond)
			h.type_("\x021")
			h.waitScreen("window 1 repainted", h.contains("ONE-TEXT"))
			h.waitFor("title", func() bool { return h.title() == "luxmux:s [1] default" })
			h.detach()
		})
	}
}
