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
// previewed below the list. What asks lux (the list, a rename, a kill)
// runs as a bubbletea command: keys and redraws never wait for it.
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
	// loading: a listing is in flight; loaded: one arrived; stale: the
	// last one failed, and the list shown is older; again: a change was
	// made while one was in flight.
	loading, loaded, stale, again bool
	// busy names the rename or kill in flight ("": none).
	busy string
	// selectNext is the session to highlight once the next listing comes.
	selectNext string
	// Switch is the session to switch to once Done ("": none); WantNew:
	// n was pressed, the new-session dialog is due.
	Switch  string
	WantNew bool
	Done    bool
}

// sessionsMsg is a listing of the sessions.
type sessionsMsg struct {
	list []mux.SessionInfo
	err  error
}

// sessionOpMsg is the end of a rename or kill: err, prefixed with what,
// is shown; selectName is highlighted after the listing it is followed by.
type sessionOpMsg struct {
	what       string
	err        error
	selectName string
}

// NewSessionChooser starts empty; the command Load returns lists the
// sessions, the current one highlighted.
func NewSessionChooser(h SessionHost, rows, cols int) *SessionChooser {
	return &SessionChooser{host: h, rows: rows, cols: cols, now: time.Now}
}

// Load lists the sessions in the background. With a listing in flight it
// returns nil, and another is started when that one comes: it may have
// been read before a change this one is for.
func (c *SessionChooser) Load() tea.Cmd {
	if c.loading {
		c.again = true
		return nil
	}
	c.loading, c.again = true, false
	h := c.host
	return func() tea.Msg {
		all, err := h.Sessions()
		return sessionsMsg{all, err}
	}
}

// Handle applies a listing or the end of a rename or kill, and returns
// what to run next.
func (c *SessionChooser) Handle(msg tea.Msg) tea.Cmd {
	switch m := msg.(type) {
	case sessionsMsg:
		c.loading = false
		if m.err != nil {
			c.stale = true
			c.msg = "sessions: " + m.err.Error()
		} else {
			c.apply(m.list)
		}
		if c.again {
			return c.Load()
		}
	case sessionOpMsg:
		c.busy = ""
		if m.err != nil {
			c.msg = m.what + ": " + m.err.Error()
		}
		if m.selectName != "" {
			c.filter = ""
			c.selectNext = m.selectName
		}
		return c.Load()
	}
	return nil
}

// apply takes a listing, the highlight staying on the same session: the
// current one on the first, a renamed one under its new name once the
// listing has it.
func (c *SessionChooser) apply(all []mux.SessionInfo) {
	cur, had := c.Selected()
	c.all, c.stale = all, false
	want := ""
	if had {
		want = cur.Name
	}
	for _, s := range all {
		switch {
		case c.selectNext != "" && s.Name == c.selectNext:
			want, c.selectNext = s.Name, ""
		case !c.loaded && s.Current && c.selectNext == "":
			want = s.Name
		}
	}
	c.loaded = true
	if want != "" {
		c.selectName(want)
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

// Key handles one key, by its bubbletea name, and returns the command it
// starts (a rename or a kill), if any.
func (c *SessionChooser) Key(k string) tea.Cmd {
	c.msg = ""
	switch c.mode {
	case modeFilter:
		switch k {
		case "enter":
			c.mode = modeList
		case "esc":
			c.filter, c.mode = "", modeList
		case "up", "down":
			c.move(k)
		default:
			c.filter = editLine(c.filter, k)
		}
		c.clampCursor()
		return nil
	case modeRename:
		var cmd tea.Cmd
		switch k {
		case "enter":
			if s, ok := c.Selected(); ok && c.input != "" && c.input != s.Name {
				old, new, h := s.Name, c.input, c.host
				c.busy = "renaming " + old + "…"
				cmd = func() tea.Msg {
					if err := h.RenameSession(old, new); err != nil {
						return sessionOpMsg{what: "rename", err: err}
					}
					return sessionOpMsg{what: "rename", selectName: new}
				}
			}
			c.mode = modeList
		case "esc":
			c.mode = modeList
		default:
			c.input = editLine(c.input, k)
		}
		return cmd
	case modeConfirmKill:
		c.mode = modeList
		if s, ok := c.Selected(); ok && (k == "y" || k == "Y") {
			name, h := s.Name, c.host
			c.busy = "killing " + name + "…"
			return func() tea.Msg { return sessionOpMsg{what: "kill", err: h.KillSession(name)} }
		}
		return nil
	}
	switch k {
	case "up", "k", "down", "j", "home", "g", "end", "G":
		c.move(k)
	case "enter":
		if s, ok := c.Selected(); ok && !s.Current {
			if s.Elsewhere {
				c.msg = fmt.Sprintf("session %s is attached elsewhere", s.Name)
				return nil
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
	case ",", "x":
		s, ok := c.Selected()
		switch {
		case !ok:
		case c.busy != "":
			c.msg = "wait: " + c.busy
		case k == "," && s.Elsewhere:
			c.msg = fmt.Sprintf("session %s is attached elsewhere: rename it there", s.Name)
		case k == ",":
			c.mode, c.input = modeRename, s.Name
		case s.Current:
			c.msg = "this is the session attached here: it cannot be killed from itself"
		case s.Elsewhere:
			c.msg = fmt.Sprintf("session %s is attached elsewhere: detach it there first", s.Name)
		default:
			c.mode = modeConfirmKill
		}
	}
	return nil
}

// editLine applies key k to a line being typed: backspace drops its last
// rune, a single rune is appended, any other key leaves it.
func editLine(s, k string) string {
	if k == "backspace" {
		if r := []rune(s); len(r) > 0 {
			return string(r[:len(r)-1])
		}
		return s
	}
	if len([]rune(k)) == 1 {
		return s + k
	}
	return s
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
		if i < len(vis) {
			b.WriteString(c.row(vis[i], i == c.cursor))
		}
		b.WriteString("\n")
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
	b.WriteString(c.status())
	return b.String()
}

// row is session s's line in the list, highlighted when focused.
func (c *SessionChooser) row(s mux.SessionInfo, focused bool) string {
	mark := "  "
	if focused {
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
	if focused {
		line = palette.focus.Render(fitLine(ansi.Strip(line), c.cols))
	}
	return line
}

// status is the chooser's last line: the mode's prompt, a message, or
// the keys.
func (c *SessionChooser) status() string {
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
	case c.busy != "":
		status = c.busy
	case !c.loaded:
		status = "loading sessions…"
	default:
		status = "↑↓/jk move  enter switch  / filter  n new  , rename  x kill  esc close"
		if c.filter != "" {
			status = "[" + c.filter + "] " + status
		}
	}
	if c.stale && c.mode == modeList && c.msg == "" {
		status = "(list out of date) " + status
	}
	status = fit(status, c.cols)
	switch {
	case c.mode == modeFilter || c.mode == modeRename:
		status = palette.focus.Render(status)
	case c.mode == modeConfirmKill:
		status = palette.warning.Render(status)
	case c.msg != "":
		status = palette.err.Render(status)
	case c.busy != "" || !c.loaded || c.stale:
	default:
		status = keyHints(status)
	}
	return status
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

func (p *sessionProgram) Init() tea.Cmd { return tea.Batch(tick(), p.c.Load()) }

func (p *sessionProgram) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg.(type) {
	case sessionsMsg, sessionOpMsg:
		// Under the dialog too: the list is current when it closes.
		return p, p.c.Handle(msg)
	}
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
			return p, p.c.Load()
		}
		return p, cmd
	}
	var cmd tea.Cmd
	switch m := msg.(type) {
	case tea.KeyMsg:
		k := m.String()
		if m.Type == tea.KeySpace {
			k = " "
		}
		cmd = p.c.Key(k)
		if p.c.WantNew {
			p.c.WantNew = false
			p.d = p.newDialog(p.c.rows, p.c.cols)
			return p, tea.Batch(cmd, p.d.Init())
		}
	case tea.WindowSizeMsg:
		p.c.rows, p.c.cols = m.Height, m.Width
	case tickMsg:
		// The preview every tick; the list, which asks lux, every 2 s.
		if p.ticks++; p.ticks%4 == 0 {
			return p, tea.Batch(tick(), p.c.Load())
		}
		return p, tick()
	}
	if p.c.Done {
		return p, tea.Quit
	}
	return p, cmd
}

func (p *sessionProgram) View() string {
	if p.d != nil {
		return p.d.View()
	}
	return p.c.View()
}

func (p *sessionProgram) finished() bool { return p.c.Done }
