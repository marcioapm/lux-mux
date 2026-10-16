package ui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/marcioapm/lux-mux/internal/mux"
)

// SessionHost is what the session chooser needs of the multiplexer.
type SessionHost interface {
	Sessions() ([]mux.SessionInfo, error)
	SessionPreview(name string, rows, cols int) []string
	RenameSession(old, new string) error
	KillSession(name string) error
}

// elsewhereNote is the preview of a session another process has.
const elsewhereNote = "attached elsewhere — preview unavailable"

// SessionChooser is the session chooser's model: the window chooser's
// look and keys over sessions, the highlighted one's shown window
// previewed below the list.
type SessionChooser struct {
	host   SessionHost
	all    []mux.SessionInfo
	filter string
	cursor int // index into Visible()
	mode   chooserMode
	input  string
	// msg is the last refusal or error, on the status line until the
	// next key.
	msg  string
	rows int
	cols int
	now  func() time.Time
	// Switch is the session to switch to once Done ("": none); WantNew:
	// n was pressed, the new-session dialog is due.
	Switch  string
	WantNew bool
	Done    bool
}

// NewSessionChooser starts on the current session.
func NewSessionChooser(h SessionHost, rows, cols int) *SessionChooser {
	c := &SessionChooser{host: h, rows: rows, cols: cols, now: time.Now}
	c.reload()
	for i, s := range c.all {
		if s.Current {
			c.cursor = i
		}
	}
	return c
}

// reload reads the sessions again, the highlight staying on the same one.
func (c *SessionChooser) reload() {
	cur, had := c.Selected()
	all, err := c.host.Sessions()
	if err != nil {
		c.msg = "sessions: " + err.Error()
		return
	}
	c.all = all
	if had {
		for i, s := range c.Visible() {
			if s.Name == cur.Name {
				c.cursor = i
			}
		}
	}
	c.clampCursor()
}

// Visible are the sessions the filter keeps: those whose name or default
// profile contain it (case-insensitive).
func (c *SessionChooser) Visible() []mux.SessionInfo {
	if c.filter == "" {
		return c.all
	}
	f := strings.ToLower(c.filter)
	var out []mux.SessionInfo
	for _, s := range c.all {
		if strings.Contains(strings.ToLower(s.Name+" "+s.DefaultProfile), f) {
			out = append(out, s)
		}
	}
	return out
}

func (c *SessionChooser) clampCursor() {
	c.cursor = min(c.cursor, len(c.Visible())-1)
	c.cursor = max(c.cursor, 0)
}

// Selected is the highlighted session (ok false when none is visible).
func (c *SessionChooser) Selected() (mux.SessionInfo, bool) {
	v := c.Visible()
	if c.cursor < 0 || c.cursor >= len(v) {
		return mux.SessionInfo{}, false
	}
	return v[c.cursor], true
}

// Key handles one key, by its bubbletea name.
func (c *SessionChooser) Key(k string) {
	c.msg = ""
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
			}
		}
		c.clampCursor()
		return
	case modeRename:
		switch k {
		case "enter":
			if s, ok := c.Selected(); ok && c.input != "" && c.input != s.Name {
				if err := c.host.RenameSession(s.Name, c.input); err != nil {
					c.msg = "rename: " + err.Error()
				} else {
					c.filter = ""
					c.reload()
					c.selectName(c.input)
				}
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
		if s, ok := c.Selected(); ok && (k == "y" || k == "Y") {
			if err := c.host.KillSession(s.Name); err != nil {
				c.msg = "kill: " + err.Error()
			}
			c.reload()
		}
		c.mode = modeList
		return
	}
	switch k {
	case "up", "k", "down", "j", "home", "g", "end", "G":
		c.move(k)
	case "enter":
		if s, ok := c.Selected(); ok && !s.Current {
			if s.Elsewhere {
				c.msg = fmt.Sprintf("session %s is attached elsewhere", s.Name)
				return
			}
			c.Switch = s.Name
		}
		c.Done = true
	case "esc", "q", "ctrl+c":
		c.Done = true
	case "/":
		c.mode = modeFilter
	case "n":
		c.WantNew = true
	case ",":
		if s, ok := c.Selected(); ok {
			if s.Elsewhere {
				c.msg = fmt.Sprintf("session %s is attached elsewhere: rename it there", s.Name)
				return
			}
			c.mode, c.input = modeRename, s.Name
		}
	case "x":
		if s, ok := c.Selected(); ok {
			switch {
			case s.Current:
				c.msg = "this is the session attached here: it cannot be killed from itself"
			case s.Elsewhere:
				c.msg = fmt.Sprintf("session %s is attached elsewhere: detach it there first", s.Name)
			default:
				c.mode = modeConfirmKill
			}
		}
	}
}

func (c *SessionChooser) selectName(name string) {
	for i, s := range c.Visible() {
		if s.Name == name {
			c.cursor = i
		}
	}
}

func (c *SessionChooser) move(k string) {
	switch k {
	case "up", "k":
		c.cursor--
	case "down", "j":
		c.cursor++
	case "home", "g":
		c.cursor = 0
	case "end", "G":
		c.cursor = len(c.Visible()) - 1
	}
	c.clampCursor()
}

// View renders the chooser: the list on top, the highlighted session's
// shown window below, and a status line.
func (c *SessionChooser) View() string {
	var b strings.Builder
	vis := c.Visible()
	listRows := min(max(len(vis), 1), max(c.rows/3, 3))
	header := fmt.Sprintf("  %-18s %-7s %-10s %-16s %s", "NAME", "WINDOWS", "ATTACHED", "DEFAULT PROFILE", "AGE")
	b.WriteString(palette.section.Render(fit(header, c.cols)) + "\n")
	start := max(0, c.cursor-listRows+1)
	for i := start; i < start+listRows; i++ {
		if i >= len(vis) {
			b.WriteString("\n")
			continue
		}
		s := vis[i]
		mark := "  "
		if i == c.cursor {
			mark = "▸ "
		}
		name := s.Name
		if s.Current {
			name += " *"
		}
		attached := "-"
		switch {
		case s.Current:
			attached = "here"
		case s.Elsewhere:
			attached = "elsewhere"
		}
		attachedCol := fit(attached, 10)
		if s.Elsewhere {
			attachedCol = palette.warning.Render(attachedCol)
		}
		prof, when := s.DefaultProfile, "-"
		if prof == "" {
			prof = "-"
		}
		if !s.Created.IsZero() {
			when = age(c.now().Sub(s.Created))
		}
		line := fmt.Sprintf("%s%-18s %-7d %s %-16s %s", mark, fit(name, 18), s.Windows, attachedCol, fit(prof, 16), when)
		line = fitLine(line, c.cols)
		if i == c.cursor {
			line = palette.focus.Render(fitLine(ansi.Strip(line), c.cols))
		}
		b.WriteString(line + "\n")
	}
	b.WriteString(palette.border.Render(strings.Repeat("─", max(c.cols, 1))) + "\n")
	prevRows := c.rows - listRows - 3
	var prev []string
	if s, ok := c.Selected(); ok && prevRows > 0 {
		if s.Elsewhere {
			prev = []string{"  " + elsewhereNote}
		} else {
			prev = c.host.SessionPreview(s.Name, prevRows, c.cols)
		}
	}
	for i := 0; i < prevRows; i++ {
		if i < len(prev) {
			b.WriteString(fit(prev[i], c.cols))
		}
		b.WriteString("\n")
	}
	var status string
	switch {
	case c.mode == modeFilter:
		status = "filter: " + c.filter + "_"
	case c.mode == modeRename:
		status = "rename session: " + c.input + "_"
	case c.mode == modeConfirmKill:
		s, _ := c.Selected()
		status = fmt.Sprintf("kill session %s (terminates its %d windows' Runs)? (y/N)", s.Name, s.Windows)
	case c.msg != "":
		status = c.msg
	default:
		status = "↑↓/jk move  enter switch  / filter  n new  , rename  x kill  esc close"
		if c.filter != "" {
			status = "[" + c.filter + "] " + status
		}
	}
	status = fit(status, c.cols)
	switch {
	case c.mode == modeFilter || c.mode == modeRename:
		status = palette.focus.Render(status)
	case c.mode == modeConfirmKill:
		status = palette.warning.Render(status)
	case c.msg != "":
		status = palette.err.Render(status)
	default:
		status = keyHints(status)
	}
	b.WriteString(status)
	return b.String()
}

// sessionProgram adapts the session chooser, and the new-session dialog
// n opens over it, to bubbletea.
type sessionProgram struct {
	c *SessionChooser
	// newDialog makes the new-session dialog.
	newDialog func(rows, cols int) *Dialog
	d         *Dialog
	// New is the session the dialog asked for.
	New   *mux.NewSessionRequest
	ticks int
}

func (p *sessionProgram) Init() tea.Cmd { return tick() }

func (p *sessionProgram) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if p.d != nil {
		if _, ok := msg.(tickMsg); ok {
			return p, tick()
		}
		if m, ok := msg.(tea.WindowSizeMsg); ok {
			p.c.rows, p.c.cols = m.Height, m.Width
		}
		_, cmd := p.d.Update(msg)
		if p.d.Done {
			if p.d.SessionRequest != nil {
				p.New = p.d.SessionRequest
				p.c.Done = true
				return p, tea.Quit
			}
			// Cancelled: back to the list.
			p.d = nil
			p.c.reload()
			return p, nil
		}
		return p, cmd
	}
	switch m := msg.(type) {
	case tea.KeyMsg:
		k := m.String()
		if m.Type == tea.KeySpace {
			k = " "
		}
		p.c.Key(k)
		if p.c.WantNew {
			p.c.WantNew = false
			p.d = p.newDialog(p.c.rows, p.c.cols)
			return p, p.d.Init()
		}
	case tea.WindowSizeMsg:
		p.c.rows, p.c.cols = m.Height, m.Width
	case tickMsg:
		// The preview every tick; the list, which asks lux, every 2 s.
		if p.ticks++; p.ticks%4 == 0 {
			p.c.reload()
		}
		return p, tick()
	}
	if p.c.Done {
		return p, tea.Quit
	}
	return p, nil
}

func (p *sessionProgram) View() string {
	if p.d != nil {
		return p.d.View()
	}
	return p.c.View()
}

func (p *sessionProgram) finished() bool { return p.c.Done }
