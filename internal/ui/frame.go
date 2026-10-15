package ui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// The smallest terminal a dialog box is drawn in.
const (
	minCols = 40
	minRows = 12
)

var (
	styleBorder  = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	styleTitle   = lipgloss.NewStyle().Bold(true)
	styleFocus   = lipgloss.NewStyle().Foreground(lipgloss.Color("12")).Bold(true)
	styleDim     = lipgloss.NewStyle().Faint(true)
	styleErr     = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
	styleReverse = lipgloss.NewStyle().Reverse(true)
	styleSection = lipgloss.NewStyle().Bold(true).Underline(true)
)

// boxWidth is a dialog box's outer width on a terminal cols wide.
func boxWidth(cols int) int { return min(cols-4, 80) }

// innerWidth is the width of a box's text: the border and a column of
// padding on each side taken off.
func innerWidth(cols int) int { return boxWidth(cols) - 4 }

// maxBody is how many lines of text a box may hold: the borders and one
// line of margin above and below taken off.
func maxBody(rows int) int { return rows - 4 }

func tooSmall(rows, cols int) bool { return rows < minRows || cols < minCols }

// tooSmallView is what a dialog shows on a terminal smaller than
// minCols×minRows.
func tooSmallView(rows, cols int) string {
	msg := fmt.Sprintf("terminal too small (need %dx%d)", minCols, minRows)
	if cols > 0 && ansi.StringWidth(msg) > cols {
		msg = ansi.Truncate(msg, cols, "")
	}
	lines := make([]string, max(rows, 1))
	pad := max(0, (cols-ansi.StringWidth(msg))/2)
	lines[max(rows, 1)/2] = strings.Repeat(" ", pad) + msg
	return strings.Join(lines, "\n")
}

// fitLine cuts s to w cells and pads it to w.
func fitLine(s string, w int) string {
	if n := ansi.StringWidth(s); n > w {
		s = ansi.Truncate(s, w, "…")
	}
	return s + strings.Repeat(" ", max(0, w-ansi.StringWidth(s)))
}

// frame draws a bordered box titled title around body, centered on a
// rows×cols screen, with bg (the window behind, plain text) dimmed around
// it. body's lines are cut or padded to the box's inner width.
func frame(rows, cols int, bg []string, title string, body []string) string {
	w := boxWidth(cols)
	inner := w - 4
	h := len(body) + 2
	x := (cols - w) / 2
	y := max(0, (rows-h)/2)
	title = ansi.Truncate(title, w-5, "")
	box := make([]string, 0, h)
	box = append(box, styleBorder.Render("╭─")+styleTitle.Render(" "+title+" ")+
		styleBorder.Render(strings.Repeat("─", max(0, w-5-ansi.StringWidth(title)))+"╮"))
	for _, l := range body {
		box = append(box, styleBorder.Render("│")+" "+fitLine(l, inner)+" "+styleBorder.Render("│"))
	}
	box = append(box, styleBorder.Render("╰"+strings.Repeat("─", w-2)+"╯"))
	out := make([]string, rows)
	for r := 0; r < rows; r++ {
		var line string
		if r < len(bg) {
			line = bg[r]
		}
		if r < y || r >= y+len(box) {
			out[r] = dimmed(fitLine(line, cols))
			continue
		}
		left := fitLine(ansi.Truncate(line, x, ""), x)
		right := ""
		if cols-x-w > 0 {
			right = fitLine(ansi.TruncateLeft(line, x+w, ""), cols-x-w)
		}
		out[r] = dimmed(left) + box[r-y] + dimmed(right)
	}
	return strings.Join(out, "\n")
}

func dimmed(s string) string {
	if strings.TrimSpace(s) == "" {
		return s
	}
	return styleDim.Render(s)
}
