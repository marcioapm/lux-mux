package screen

import (
	"fmt"
	"strings"
	"testing"

	"github.com/marcioapm/lux/vterm"
)

// src is a 24×80 window's emulator fed what its program wrote.
func src(out string) *vterm.Terminal {
	t := vterm.New(24, 80, 0)
	t.Write([]byte(out))
	return t
}

// client is the user's terminal after it showed before, then got b.
func client(before string, b []byte) *vterm.Terminal {
	t := vterm.New(24, 80, 0)
	t.Write([]byte(before))
	t.Write(b)
	return t
}

func rowText(t *vterm.Terminal, y int, main bool) string {
	_, cols := t.Size()
	var sb strings.Builder
	for x := 0; x < cols; x++ {
		c := t.Cell(x, y)
		if main {
			c = t.MainCell(x, y)
		}
		sb.WriteString(t.Text(c))
	}
	return strings.TrimRight(sb.String(), " ")
}

// dirty is a terminal state with every replayed mode and setting on that
// Paint resets: what the previous window left.
const dirty = "\x1b[?1h\x1b[?5h\x1b[?12h\x1b[?66h\x1b[?1004h\x1b[?1005h\x1b[?1006h\x1b[?1015h" +
	"\x1b[?9h\x1b[?1000h\x1b[?1001h\x1b[?1002h\x1b[?1003h\x1b[?2004h\x1b[?7l\x1b[3;10r\x1b[?6h\x1b[?25l\x1b=\x1b[5 q" +
	"\x1b[4h\x1b[1;31m\x1b(0"

// TestPaintModes: every mode Paint replays ends as the window has it,
// whichever way the terminal had it before.
func TestPaintModes(t *testing.T) {
	for _, n := range replayModes {
		for _, on := range []bool{true, false} {
			t.Run(fmt.Sprintf("%d=%v", n, on), func(t *testing.T) {
				set := fmt.Sprintf("\x1b[?%dh", n)
				w := src("")
				if on {
					w.Write([]byte(set))
				}
				before := set
				if on {
					before = fmt.Sprintf("\x1b[?%dl", n)
				}
				c := client(before, Paint(nil, w, ""))
				if c.Mode(n) != on {
					t.Fatalf("mode %d is %v, the window has %v", n, c.Mode(n), on)
				}
			})
		}
	}
}

// TestPaintResetsWhatThePreviousWindowLeft: from a terminal with every
// setting changed, the paint of a plain window leaves all of them at
// their defaults and the text where it belongs.
func TestPaintResetsWhatThePreviousWindowLeft(t *testing.T) {
	w := src("line one\r\nline two")
	c := client(dirty, Paint(nil, w, ""))
	for _, n := range replayModes {
		if c.Mode(n) {
			t.Errorf("mode %d left on", n)
		}
	}
	if !c.Mode(7) || c.Mode(6) {
		t.Errorf("autowrap %v origin %v", c.Mode(7), c.Mode(6))
	}
	if top, bot := c.ScrollRegion(); top != 0 || bot != 23 {
		t.Errorf("scroll region %d-%d", top, bot)
	}
	if x, y, vis := c.Cursor(); x != 8 || y != 1 || !vis {
		t.Errorf("cursor %d,%d visible %v", x, y, vis)
	}
	if kp, sh := KeypadAndShape(c); kp || sh != 0 {
		t.Errorf("keypad %v shape %d", kp, sh)
	}
	if rowText(c, 0, false) != "line one" || rowText(c, 1, false) != "line two" {
		t.Errorf("rows %q %q", rowText(c, 0, false), rowText(c, 1, false))
	}
	// The pen, insert mode and character set were reset: text written
	// next is plain, not inserted, not line-drawing.
	c.Write([]byte("q"))
	if cell := c.Cell(8, 1); c.Text(cell) != "q" || cell.Style != (vterm.Style{}) {
		t.Errorf("next character %q style %+v", c.Text(cell), cell.Style)
	}
}

// TestPaintWindowSettings: autowrap off, origin mode with a scroll region,
// a hidden cursor, keypad mode and a cursor shape set by the window are
// all replayed.
func TestPaintWindowSettings(t *testing.T) {
	w := src("\x1b[5;20r\x1b[?6h\x1b[3;7H\x1b[?7l\x1b[?25l\x1b=\x1b[4 q")
	c := client("", Paint(nil, w, "title-x"))
	if c.Mode(7) {
		t.Error("autowrap on")
	}
	if !c.Mode(6) {
		t.Error("origin mode off")
	}
	if top, bot := c.ScrollRegion(); top != 4 || bot != 19 {
		t.Errorf("scroll region %d-%d", top, bot)
	}
	wx, wy, _ := w.Cursor()
	if x, y, vis := c.Cursor(); x != wx || y != wy || vis {
		t.Errorf("cursor %d,%d visible %v; window %d,%d", x, y, vis, wx, wy)
	}
	if kp, sh := KeypadAndShape(c); !kp || sh != 4 {
		t.Errorf("keypad %v shape %d", kp, sh)
	}
	if c.Title() != "title-x" {
		t.Errorf("title %q", c.Title())
	}
}

// TestPaintAltScreenKeepsMainScreen: a window in a full-screen program is
// painted over its own main screen, so when the program exits the
// terminal shows that window's main screen, not the previous window's.
func TestPaintAltScreenKeepsMainScreen(t *testing.T) {
	w := src("W0-L1\r\nW0-L2\r\n$ vim\x1b[?1049h\x1b[H\x1b[2JVIM-SCREEN")
	other := strings.Repeat("OTHER-WINDOW-TEXT\r\n", 8)
	c := client(other, Paint(nil, w, ""))
	if !c.AltScreen() || rowText(c, 0, false) != "VIM-SCREEN" {
		t.Fatalf("alt %v row 0 %q", c.AltScreen(), rowText(c, 0, false))
	}
	exit := "\x1b[?1049l\r\n$ PROMPT"
	w.Write([]byte(exit))
	c.Write([]byte(exit))
	for y := 0; y < 24; y++ {
		if got, want := rowText(c, y, false), rowText(w, y, false); got != want {
			t.Fatalf("row %d after the program exited: %q, the window has %q", y, got, want)
		}
	}
}

// TestPaintSGR: cell styles survive the paint: attributes, underline
// styles, the 16 colours, 256 colours and RGB, foreground and background.
func TestPaintSGR(t *testing.T) {
	styles := []string{
		"1", "2", "3", "5", "7", "8", "9", "4", "4:3",
		"31", "42", "93", "104", "38;5;123", "48;5;200",
		"38;2;10;20;30", "48;2;200;100;50", "1;3;38;2;1;2;3;48;5;17",
	}
	var out strings.Builder
	for i, s := range styles {
		fmt.Fprintf(&out, "\x1b[%d;1H\x1b[%smX%d\x1b[m", i+1, s, i)
	}
	w := src(out.String())
	c := client("", Paint(nil, w, ""))
	for i, s := range styles {
		for x := 0; x < 3; x++ {
			if got, want := c.Cell(x, i), w.Cell(x, i); got.Style != want.Style || c.Text(got) != w.Text(want) {
				t.Errorf("SGR %s cell %d: %+v %q, the window has %+v %q", s, x, got.Style, c.Text(got), want.Style, w.Text(want))
			}
		}
		if w.Cell(0, i).Style == (vterm.Style{}) {
			t.Errorf("SGR %s: the window has no style", s)
		}
	}
}

// TestPaintWideAndCombined: wide characters and combining clusters land
// in the same cells.
func TestPaintWideAndCombined(t *testing.T) {
	w := src("日本語 e\u0301 👩\u200d💻 end")
	c := client("", Paint(nil, w, ""))
	if got, want := rowText(c, 0, false), rowText(w, 0, false); got != want {
		t.Fatalf("%q, the window has %q", got, want)
	}
}

// TestReset: what luxmux leaves on detach turns off everything a program
// set and shows the main screen with a visible default cursor.
func TestReset(t *testing.T) {
	c := client(dirty+"\x1b[?1049h", Reset(nil))
	for _, n := range replayModes {
		if c.Mode(n) {
			t.Errorf("mode %d left on", n)
		}
	}
	if c.AltScreen() {
		t.Error("alternate screen left")
	}
	if !c.Mode(7) || c.Mode(6) {
		t.Errorf("autowrap %v origin %v", c.Mode(7), c.Mode(6))
	}
	if top, bot := c.ScrollRegion(); top != 0 || bot != 23 {
		t.Errorf("scroll region %d-%d", top, bot)
	}
	if _, _, vis := c.Cursor(); !vis {
		t.Error("cursor hidden")
	}
	if kp, sh := KeypadAndShape(c); kp || sh != 0 {
		t.Errorf("keypad %v shape %d", kp, sh)
	}
}

// TestKeypadAndShape reads what the program set, not text that looks
// like it.
func TestKeypadAndShape(t *testing.T) {
	for _, tc := range []struct {
		out    string
		keypad bool
		shape  int
	}{
		{"", false, 0},
		{"\x1b=", true, 0},
		{"\x1b=\x1b>", false, 0},
		{"\x1b[6 q", false, 6},
		{"\x1b[6 q\x1b[0 q", false, 0},
		{"text 2 q and = signs", false, 0},
		{"\x1b]2;a 3 q title\x07\x1b[2 q", false, 2},
	} {
		kp, sh := KeypadAndShape(src(tc.out))
		if kp != tc.keypad || sh != tc.shape {
			t.Errorf("%q: keypad %v shape %d, want %v %d", tc.out, kp, sh, tc.keypad, tc.shape)
		}
	}
}
