package screen

import (
	"strings"
	"unicode/utf8"

	"github.com/marcioapm/lux/vterm"
)

// Overlay appends what draws text on row y of a rows×cols terminal in
// reverse video, then puts the cursor and the modes Overlay touched back
// as t has them. Autowrap is off while it draws, so the last column never
// scrolls the screen; origin mode is off, so the row is absolute.
func Overlay(b []byte, t *vterm.Terminal, y int, text string) []byte {
	_, cols := t.Size()
	b = append(b, "\x1b[?25l\x1b[?6l\x1b[?7l"...)
	b = CUP(b, y, 0)
	b = append(b, "\x1b[0;7m"...)
	b = append(b, Fit(text, cols)...)
	b = append(b, "\x1b[m"...)
	if t.Mode(7) {
		b = Mode(b, 7, true)
	}
	return Cursor(b, t)
}

// Restore appends what redraws row y from t and puts the cursor back:
// taking an overlay down.
func Restore(b []byte, t *vterm.Terminal, y int) []byte {
	_, cols := t.Size()
	b = append(b, "\x1b[?25l\x1b[?6l\x1b[?7l"...)
	b = Row(b, t, y, cols)
	if t.Mode(7) {
		b = Mode(b, 7, true)
	}
	return Cursor(b, t)
}

// Fit pads or cuts s to exactly n columns (one column per rune; luxmux's
// own texts are narrow).
func Fit(s string, n int) string {
	c := utf8.RuneCountInString(s)
	if c >= n {
		r := []rune(s)
		return string(r[:max(n, 0)])
	}
	return s + strings.Repeat(" ", n-c)
}

// Preview is rows lines of t's screen, cols wide, as plain text: the rows
// that end at the cursor's row (or at the last non-blank row below it), so
// a shell's latest lines and prompt are what shows.
func Preview(t *vterm.Terminal, rows, cols int) []string {
	trows, tcols := t.Size()
	_, cy, _ := t.Cursor()
	end := cy
	for y := trows - 1; y > end; y-- {
		if !blankRow(t, y, tcols) {
			end = y
			break
		}
	}
	start := max(0, end-rows+1)
	out := make([]string, 0, rows)
	for y := start; y <= end && len(out) < rows; y++ {
		var sb strings.Builder
		w := 0
		for x := 0; x < tcols && w < cols; x++ {
			c := t.Cell(x, y)
			if c.Flags&vterm.FlagCont != 0 {
				continue
			}
			s := t.Text(c)
			if c.Flags&vterm.FlagWide != 0 {
				if w+2 > cols {
					break
				}
				w++
			}
			sb.WriteString(s)
			w++
		}
		out = append(out, strings.TrimRight(sb.String(), " "))
	}
	return out
}

func blankRow(t *vterm.Terminal, y, cols int) bool {
	for x := 0; x < cols; x++ {
		if c := t.Cell(x, y); c.Ch != 0 && c.Ch != ' ' {
			return false
		}
	}
	return true
}

// Text appends a screen of plain lines: cleared, each line written at its
// row, the cursor hidden. It draws luxmux's own screens (a window's
// progress, its end, the key help).
func Text(b []byte, lines []string, rows, cols int) []byte {
	b = append(b, "\x1b[?25l\x1b[?6l\x1b[r\x1b[m\x1b>\x1b[0 q\x1b[?7l\x1b[?1049l\x1b[H\x1b[2J"...)
	for i, l := range lines {
		if i >= rows {
			break
		}
		b = CUP(b, i, 0)
		b = append(b, Fit(l, cols)...)
	}
	return append(b, "\x1b[?7h"...)
}
