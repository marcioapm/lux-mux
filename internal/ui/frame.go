package ui

import (
	"fmt"
	"os"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
)

// The smallest terminal a dialog box is drawn in.
const (
	minCols = 40
	minRows = 12
)

type themeStyle struct{ blue, mono lipgloss.Style }

func (s themeStyle) style() lipgloss.Style {
	if os.Getenv("NO_COLOR") != "" {
		return s.mono
	}
	return s.blue
}

func (s themeStyle) Render(text string) string { return s.style().Render(text) }

// NO_COLOR removes colours, not focus or emphasis. The default renderer
// drops all SGR under NO_COLOR; use an ANSI renderer for the colour-free styles.
var monoRenderer = func() *lipgloss.Renderer {
	r := lipgloss.NewRenderer(os.Stdout)
	r.SetColorProfile(termenv.ANSI)
	return r
}()

// Every colour has a light-terminal and dark-terminal variant.
var palette = struct {
	border, title, accent, focus, dim, err, warning, ok, dead, section themeStyle
}{
	border:  themeStyle{blue: lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "#526F89", Dark: "#718DA8"})},
	title:   themeStyle{blue: lipgloss.NewStyle().Bold(true).Foreground(lipgloss.AdaptiveColor{Light: "#294D70", Dark: "#C5DCF2"}).Background(lipgloss.AdaptiveColor{Light: "#E3EDF5", Dark: "#25384B"}), mono: monoRenderer.NewStyle().Bold(true)},
	accent:  themeStyle{blue: lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "#345F85", Dark: "#9EBFDD"})},
	focus:   themeStyle{blue: lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "#F4F8FC", Dark: "#E8F0F8"}).Background(lipgloss.AdaptiveColor{Light: "#375A7A", Dark: "#304D68"}), mono: monoRenderer.NewStyle().Reverse(true)},
	dim:     themeStyle{blue: lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "#586B7D", Dark: "#91A3B5"}), mono: monoRenderer.NewStyle().Faint(true)},
	err:     themeStyle{blue: lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "#A32D38", Dark: "#EF9A9A"})},
	warning: themeStyle{blue: lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "#855600", Dark: "#DEC080"})},
	ok:      themeStyle{blue: lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "#38684C", Dark: "#9CC5A9"})},
	dead:    themeStyle{blue: lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "#96545C", Dark: "#C58E96"}), mono: monoRenderer.NewStyle().Faint(true)},
	section: themeStyle{blue: lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "#345F85", Dark: "#9EBFDD"}).Bold(true), mono: monoRenderer.NewStyle().Bold(true)},
}

// focusBand pads line to w cells and keeps its inner styles, restoring the
// focus band after each SGR reset they emit.
func focusBand(line string, w int) string {
	marker := palette.focus.Render("x")
	start := marker[:strings.Index(marker, "x")]
	if start == "" {
		return fitLine(line, w)
	}
	line = strings.NewReplacer("\x1b[0m", "\x1b[0m"+start, "\x1b[m", "\x1b[m"+start).Replace(fitLine(line, w))
	return start + line + "\x1b[0m"
}

func styleInput(t *textinput.Model) {
	t.TextStyle = lipgloss.NewStyle()
	t.PromptStyle = lipgloss.NewStyle()
	t.PlaceholderStyle = palette.dim.style()
	t.CompletionStyle = palette.dim.style()
	t.Cursor.Style = lipgloss.NewStyle().Reverse(true)
	t.Cursor.TextStyle = lipgloss.NewStyle()
}

func keyHints(s string) string {
	var out strings.Builder
	for _, word := range strings.SplitAfter(s, " ") {
		key := strings.TrimSpace(word)
		switch key {
		case "tab/↑↓", "tab/↓", "⇧tab/↑", "↑/↓", "↑↓", "↑↓/jk", "pgup/pgdn", "tab", "enter", "space", "ctrl-s", "esc", "esc,", "q", "?", "/", ",", "x", "n":
			out.WriteString(palette.accent.Render(word))
		default:
			out.WriteString(palette.dim.Render(word))
		}
	}
	return out.String()
}

func stateStyle(state string) themeStyle {
	switch state {
	case "running":
		return palette.ok
	case "starting", "resuming", "reconnecting", "queued", "pulling", "cloning":
		return palette.warning
	case "stopped", "dead", "terminated", "failed", "succeeded":
		return palette.dead
	default:
		return palette.dim
	}
}

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
	box = append(box, palette.border.Render("╭─")+palette.title.Render(" "+title+" ")+
		palette.border.Render(strings.Repeat("─", max(0, w-5-ansi.StringWidth(title)))+"╮"))
	for _, l := range body {
		box = append(box, palette.border.Render("│")+" "+fitLine(l, inner)+" "+palette.border.Render("│"))
	}
	box = append(box, palette.border.Render("╰"+strings.Repeat("─", w-2)+"╯"))
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
	return palette.dim.Render(s)
}
