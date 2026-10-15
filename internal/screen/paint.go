// Package screen turns a vterm.Terminal's state into the bytes that show
// it on a real terminal: whole-screen repaints when a window is switched
// to, single rows when an overlay is taken down, and previews.
package screen

import (
	"strconv"

	"github.com/marcioapm/lux/vterm"
)

// replayModes are the DEC private modes a remote program sets that change
// how the client terminal behaves; vterm.Snapshot restores the same list.
var replayModes = []int{
	1,                // application cursor keys
	5,                // reverse video
	12,               // cursor blink
	66,               // application keypad (DECNKM)
	1004,             // focus events
	1005, 1006, 1015, // mouse encodings
	9, 1000, 1001, 1002, 1003, // mouse tracking
	2004, // bracketed paste
}

// Paint appends to b what makes a terminal of the same size show t: the
// main or alternate screen, every row (written in place, so nothing is
// pushed into the terminal's scrollback), the scroll region, the modes in
// replayModes, autowrap and origin mode, then the cursor. title, when not
// empty, is set with OSC 2. The pen ends in the default style: vterm does
// not export the remote program's current pen.
func Paint(b []byte, t *vterm.Terminal, title string) []byte {
	rows, cols := t.Size()
	// Leave whatever state a previous window left that changes how the
	// rows below are drawn: origin mode, scroll region, insert mode, pen,
	// line-drawing character sets.
	b = append(b, "\x1b[?25l\x1b[?6l\x1b[r\x1b[4l\x1b[m\x1b(B\x1b)B\x0f\x1b[?7h"...)
	if t.AltScreen() {
		b = append(b, "\x1b[?1049h"...)
	} else {
		b = append(b, "\x1b[?1049l"...)
	}
	for y := 0; y < rows; y++ {
		b = Row(b, t, y, cols)
	}
	top, bot := t.ScrollRegion()
	if top != 0 || bot != rows-1 {
		b = append(b, "\x1b["...)
		b = strconv.AppendInt(b, int64(top+1), 10)
		b = append(b, ';')
		b = strconv.AppendInt(b, int64(bot+1), 10)
		b = append(b, 'r')
	}
	for _, n := range replayModes {
		b = Mode(b, n, t.Mode(n))
	}
	if !t.Mode(7) {
		b = Mode(b, 7, false)
	}
	if title != "" {
		b = append(b, "\x1b]2;"...)
		b = append(b, title...)
		b = append(b, 0x07)
	}
	return Cursor(b, t)
}

// Cursor appends what puts the cursor where t has it, with origin mode and
// visibility as t has them.
func Cursor(b []byte, t *vterm.Terminal) []byte {
	x, y, visible := t.Cursor()
	if t.Mode(6) {
		top, _ := t.ScrollRegion()
		b = Mode(b, 6, true)
		y -= top
	}
	b = CUP(b, y, x)
	return Mode(b, 25, visible)
}

// Row appends what redraws row y of t (cols wide) in place: the cursor
// moved to its start, its cells with their styles, then the rest of the
// line erased in the default style. It leaves the pen in the default style.
func Row(b []byte, t *vterm.Terminal, y, cols int) []byte {
	b = CUP(b, y, 0)
	end := cols
	for end > 0 && t.Cell(end-1, y) == (vterm.Cell{}) {
		end--
	}
	b = Cells(b, t, y, 0, end)
	if end < cols {
		b = append(b, "\x1b[K"...)
	}
	return b
}

// Cells appends cells [x0, x1) of row y with their styles, ending in the
// default style.
func Cells(b []byte, t *vterm.Terminal, y, x0, x1 int) []byte {
	var pen vterm.Style
	for x := x0; x < x1; x++ {
		c := t.Cell(x, y)
		if c.Flags&vterm.FlagCont != 0 && x > x0 {
			continue
		}
		if c.Style != pen {
			b = SGR(b, c.Style)
			pen = c.Style
		}
		if c.Flags&vterm.FlagCont != 0 || (c.Flags&vterm.FlagWide != 0 && x+1 >= x1) {
			// Half a wide character at the region's edge: a blank.
			b = append(b, ' ')
			continue
		}
		b = append(b, t.Text(c)...)
	}
	if pen != (vterm.Style{}) {
		b = append(b, "\x1b[m"...)
	}
	return b
}

// SGR appends the sequence that sets s from the default style.
func SGR(b []byte, s vterm.Style) []byte {
	b = append(b, "\x1b[0"...)
	a := s.Attr
	for _, f := range [...]struct {
		bit vterm.Attr
		n   byte
	}{{vterm.AttrBold, '1'}, {vterm.AttrFaint, '2'}, {vterm.AttrItalic, '3'}, {vterm.AttrBlink, '5'},
		{vterm.AttrReverse, '7'}, {vterm.AttrInvisible, '8'}, {vterm.AttrStrike, '9'}} {
		if a&f.bit != 0 {
			b = append(b, ';', f.n)
		}
	}
	switch u := a.Underline(); u {
	case 0:
	case 1:
		b = append(b, ";4"...)
	default:
		b = append(b, ";4:"...)
		b = strconv.AppendInt(b, int64(u), 10)
	}
	b = color(b, s.Fg, 30, 90, "38")
	b = color(b, s.Bg, 40, 100, "48")
	return append(b, 'm')
}

// vterm's Color: 0 default; the top byte says indexed (1) or RGB (2).
const (
	kindIndexed = 1 << 24
	kindRGB     = 2 << 24
	kindMask    = 0xff << 24
)

func color(b []byte, c vterm.Color, base, bright int, ext string) []byte {
	switch c & kindMask {
	case kindIndexed:
		n := int(c & 0xff)
		b = append(b, ';')
		switch {
		case n < 8:
			b = strconv.AppendInt(b, int64(base+n), 10)
		case n < 16:
			b = strconv.AppendInt(b, int64(bright+n-8), 10)
		default:
			b = append(b, ext...)
			b = append(b, ";5;"...)
			b = strconv.AppendInt(b, int64(n), 10)
		}
	case kindRGB:
		b = append(b, ';')
		b = append(b, ext...)
		b = append(b, ";2;"...)
		b = strconv.AppendInt(b, int64(c>>16&0xff), 10)
		b = append(b, ';')
		b = strconv.AppendInt(b, int64(c>>8&0xff), 10)
		b = append(b, ';')
		b = strconv.AppendInt(b, int64(c&0xff), 10)
	}
	return b
}

// CUP moves the cursor to row y, column x (0-based).
func CUP(b []byte, y, x int) []byte {
	b = append(b, "\x1b["...)
	b = strconv.AppendInt(b, int64(y+1), 10)
	b = append(b, ';')
	b = strconv.AppendInt(b, int64(x+1), 10)
	return append(b, 'H')
}

// Mode sets or resets DEC private mode n.
func Mode(b []byte, n int, on bool) []byte {
	b = append(b, "\x1b[?"...)
	b = strconv.AppendInt(b, int64(n), 10)
	if on {
		return append(b, 'h')
	}
	return append(b, 'l')
}

// Reset is what luxmux leaves the client terminal with on detach: the
// modes a remote program may have set turned off, the main screen, the
// default pen, the cursor shown.
func Reset(b []byte) []byte {
	b = append(b, "\x1b[?6l\x1b[r\x1b[m\x1b[4l\x1b(B\x1b)B\x0f\x1b[?7h"...)
	for _, n := range replayModes {
		b = Mode(b, n, false)
	}
	b = append(b, "\x1b>"...)
	b = append(b, "\x1b[?1049l"...)
	return Mode(b, 25, true)
}
