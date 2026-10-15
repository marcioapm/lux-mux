package ui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/marcioapm/lux-mux/internal/mux"
)

// Host is what the chooser needs of the multiplexer.
type Host interface {
	Windows() []mux.WindowInfo
	Preview(i, rows, cols int) []string
	Rename(i int, name string)
	Kill(i int)
}

type chooserMode int

const (
	modeList chooserMode = iota
	modeFilter
	modeRename
	modeConfirmKill
)

// Chooser is the window chooser's model: a filtered list of windows, the
// highlighted one previewed below it.
type Chooser struct {
	host   Host
	all    []mux.WindowInfo
	filter string
	cursor int // index into Visible()
	mode   chooserMode
	input  string
	rows   int
	cols   int
	now    func() time.Time
	// Chosen is the window index to switch to (-1: none) once Done.
	Chosen int
	Done   bool
}

// NewChooser starts on the active window.
func NewChooser(h Host, rows, cols int) *Chooser {
	c := &Chooser{host: h, rows: rows, cols: cols, Chosen: -1, now: time.Now}
	c.reload()
	for i, w := range c.all {
		if w.Active {
			c.cursor = i
		}
	}
	return c
}

func (c *Chooser) reload() {
	c.all = c.host.Windows()
	c.clampCursor()
}

// Visible are the windows the filter keeps: those whose index, name,
// state, image or repository contain it (case-insensitive).
func (c *Chooser) Visible() []mux.WindowInfo {
	if c.filter == "" {
		return c.all
	}
	f := strings.ToLower(c.filter)
	var out []mux.WindowInfo
	for _, w := range c.all {
		hay := strings.ToLower(fmt.Sprintf("%d %s %s %s %s", w.Index, w.Name, w.State, w.Image, w.Repo))
		if strings.Contains(hay, f) {
			out = append(out, w)
		}
	}
	return out
}

func (c *Chooser) clampCursor() {
	n := len(c.Visible())
	if c.cursor >= n {
		c.cursor = n - 1
	}
	if c.cursor < 0 {
		c.cursor = 0
	}
}

// Selected is the highlighted window (ok false when none is visible).
func (c *Chooser) Selected() (mux.WindowInfo, bool) {
	v := c.Visible()
	if c.cursor < 0 || c.cursor >= len(v) {
		return mux.WindowInfo{}, false
	}
	return v[c.cursor], true
}

// Key handles one key, by its bubbletea name ("up", "enter", "a", ...).
func (c *Chooser) Key(k string) {
	switch c.mode {
	case modeFilter:
		switch k {
		case "enter":
			c.mode = modeList
		case "esc":
			c.filter, c.mode = "", modeList
		case "backspace":
			if r := []rune(c.filter); len(r) > 0 {
				c.filter = string(r[:len(r)-1])
			}
		case "up", "down":
			c.move(k)
		default:
			if len([]rune(k)) == 1 {
				c.filter += k
			} else if k == "space" || k == " " {
				c.filter += " "
			}
		}
		c.clampCursor()
		return
	case modeRename:
		switch k {
		case "enter":
			if w, ok := c.Selected(); ok && c.input != "" {
				c.host.Rename(w.Index, c.input)
				c.reload()
			}
			c.mode = modeList
		case "esc":
			c.mode = modeList
		case "backspace":
			if r := []rune(c.input); len(r) > 0 {
				c.input = string(r[:len(r)-1])
			}
		default:
			if len([]rune(k)) == 1 {
				c.input += k
			}
		}
		return
	case modeConfirmKill:
		if w, ok := c.Selected(); ok && (k == "y" || k == "Y") {
			c.host.Kill(w.Index)
			c.reload()
			if len(c.all) == 0 {
				c.Done = true
			}
		}
		c.mode = modeList
		return
	}
	switch k {
	case "up", "k", "down", "j", "home", "g", "end", "G":
		c.move(k)
	case "enter":
		if w, ok := c.Selected(); ok {
			c.Chosen = w.Index
		}
		c.Done = true
	case "esc", "q", "ctrl+c":
		c.Done = true
	case "/":
		c.mode = modeFilter
	case ",":
		if w, ok := c.Selected(); ok {
			c.mode, c.input = modeRename, w.Name
		}
	case "x":
		if _, ok := c.Selected(); ok {
			c.mode = modeConfirmKill
		}
	default:
		if len(k) == 1 && k[0] >= '0' && k[0] <= '9' {
			for i, w := range c.Visible() {
				if w.Index == int(k[0]-'0') {
					c.cursor = i
				}
			}
		}
	}
}

func (c *Chooser) move(k string) {
	n := len(c.Visible())
	switch k {
	case "up", "k":
		c.cursor--
	case "down", "j":
		c.cursor++
	case "home", "g":
		c.cursor = 0
	case "end", "G":
		c.cursor = n - 1
	}
	c.clampCursor()
}

func age(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

func fit(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		if n <= 1 {
			return string(r[:max(n, 0)])
		}
		return string(r[:n-1]) + "…"
	}
	return s + strings.Repeat(" ", n-len(r))
}

// View renders the chooser: list on top, preview of the highlighted
// window below, and a status line.
func (c *Chooser) View() string {
	var b strings.Builder
	vis := c.Visible()
	listRows := min(max(len(vis), 1), max(c.rows/3, 3))
	header := fmt.Sprintf("  %-3s %-14s %-12s %-22s %-10s %-18s %s", "#", "NAME", "STATE", "IMAGE", "SIZE", "REPO", "AGE")
	b.WriteString(fit(header, c.cols) + "\n")
	start := max(0, c.cursor-listRows+1)
	for i := start; i < start+listRows; i++ {
		if i >= len(vis) {
			b.WriteString("\n")
			continue
		}
		w := vis[i]
		mark := "  "
		if i == c.cursor {
			mark = "> "
		}
		act := " "
		if w.Active {
			act = "*"
		}
		line := fmt.Sprintf("%s%-3s %-14s %-12s %-22s %-10s %-18s %s", mark, fmt.Sprintf("%d%s", w.Index, act),
			fit(w.Name, 14), fit(w.State, 12), fit(w.Image, 22), fit(w.Size, 10), fit(w.Repo, 18), age(c.now().Sub(w.Created)))
		line = fit(line, c.cols)
		if i == c.cursor {
			line = "\x1b[7m" + line + "\x1b[m"
		}
		b.WriteString(line + "\n")
	}
	b.WriteString(strings.Repeat("─", max(c.cols, 1)) + "\n")
	prevRows := c.rows - listRows - 3
	var prev []string
	if w, ok := c.Selected(); ok && prevRows > 0 {
		prev = c.host.Preview(w.Index, prevRows, c.cols)
	}
	for i := 0; i < prevRows; i++ {
		if i < len(prev) {
			b.WriteString(fit(prev[i], c.cols))
		}
		b.WriteString("\n")
	}
	var status string
	switch c.mode {
	case modeFilter:
		status = "filter: " + c.filter + "_"
	case modeRename:
		status = "rename: " + c.input + "_"
	case modeConfirmKill:
		w, _ := c.Selected()
		status = fmt.Sprintf("kill window %d (%s)? (y/N)", w.Index, w.Name)
	default:
		status = "↑↓/jk move  enter switch  / filter  , rename  x kill  esc close"
		if c.filter != "" {
			status = "[" + c.filter + "] " + status
		}
	}
	b.WriteString(fit(status, c.cols))
	return b.String()
}

// chooserProgram adapts Chooser to bubbletea.
type chooserProgram struct{ c *Chooser }

func (p chooserProgram) Init() tea.Cmd { return nil }

func (p chooserProgram) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch m := msg.(type) {
	case tea.KeyMsg:
		k := m.String()
		if m.Type == tea.KeySpace {
			k = " "
		}
		p.c.Key(k)
	case tea.WindowSizeMsg:
		p.c.rows, p.c.cols = m.Height, m.Width
	case tickMsg:
		// Refresh states and the preview while the chooser is open.
		p.c.reload()
		return p, tick()
	}
	if p.c.Done {
		return p, tea.Quit
	}
	return p, nil
}

func (p chooserProgram) View() string { return p.c.View() }

type tickMsg struct{}

func tick() tea.Cmd {
	return tea.Tick(500*time.Millisecond, func(time.Time) tea.Msg { return tickMsg{} })
}
