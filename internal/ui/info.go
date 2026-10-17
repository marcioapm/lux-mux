package ui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/marcioapm/lux-mux/internal/info"
)

// InfoEvery is how often the window-info dialog reads its window again
// while it is open.
const InfoEvery = 3 * time.Second

// InfoHost is what the window-info dialog asks of the multiplexer.
// InfoTarget only reads what luxmux knows; FetchInfo waits on the
// network and is only called from a command's goroutine.
type InfoHost interface {
	InfoTarget() (info.Target, bool)
	FetchInfo(ctx context.Context, t info.Target) info.Window
}

// infoTickMsg asks for a refresh; gen is the dialog's generation that
// scheduled it.
type infoTickMsg struct{ gen int }

// infoResultMsg is a fetch's answer for the target of generation gen.
type infoResultMsg struct {
	gen int
	w   info.Window
}

// InfoDialog is the window-info dialog's model: the shown window's info,
// read again every InfoEvery while it is open, one read at a time. The
// reads run in bubbletea commands, off the multiplexer's lock and off its
// input loop; closing the dialog, or the shown window changing, cancels
// the read in flight, and an answer for an earlier window or after the
// close is dropped.
type InfoDialog struct {
	host   InfoHost
	parent context.Context
	// after schedules msg in d (tea.Tick; tests run a fake clock).
	after func(d time.Duration, msg tea.Msg) tea.Cmd
	now   func() time.Time
	// prefix is the session's prefix as bubbletea names it ("ctrl+b"):
	// prefix then i closes the dialog, as it opened it.
	prefix    string
	afterPfx  bool
	target    info.Target
	hasTarget bool
	// cur is the last good read (or Base(target) before one); errLine
	// the failure of the latest read, if it failed.
	cur      info.Window
	good     bool
	errLine  string
	gen      int
	inFlight bool
	cancel   context.CancelFunc

	bg         []string
	background func(rows, cols int) []string
	rows, cols int
	scroll     int
	Done       bool
}

// NewInfoDialog shows host's shown window; parent ends with the session.
// prefix is the session's prefix key as bubbletea names it.
func NewInfoDialog(parent context.Context, host InfoHost, prefix string, background func(rows, cols int) []string, rows, cols int) *InfoDialog {
	d := &InfoDialog{host: host, parent: parent, prefix: prefix, background: background, now: time.Now,
		after: func(dur time.Duration, msg tea.Msg) tea.Cmd {
			return tea.Tick(dur, func(time.Time) tea.Msg { return msg })
		}}
	d.target, d.hasTarget = host.InfoTarget()
	d.cur = info.Base(d.target)
	d.resize(rows, cols)
	return d
}

func (d *InfoDialog) resize(rows, cols int) {
	d.rows, d.cols = rows, cols
	if d.background != nil {
		d.bg = d.background(rows, cols)
	}
}

func (d *InfoDialog) finished() bool { return d.Done }

// Init starts the first read and the refresh clock.
func (d *InfoDialog) Init() tea.Cmd {
	return tea.Batch(d.fetch(), d.after(InfoEvery, infoTickMsg{d.gen}))
}

// fetch starts a read of the target unless one is in flight or the
// dialog is closed; its command runs FetchInfo in its own goroutine.
func (d *InfoDialog) fetch() tea.Cmd {
	if d.Done || d.inFlight || !d.hasTarget || d.parent.Err() != nil {
		return nil
	}
	ctx, cancel := context.WithCancel(d.parent)
	d.cancel, d.inFlight = cancel, true
	gen, t, host := d.gen, d.target, d.host
	return func() tea.Msg {
		defer cancel()
		return infoResultMsg{gen: gen, w: host.FetchInfo(ctx, t)}
	}
}

// stop cancels the read in flight.
func (d *InfoDialog) stop() {
	if d.cancel != nil {
		d.cancel()
		d.cancel = nil
	}
	d.inFlight = false
}

// Close closes the dialog: the read in flight is cancelled and nothing is
// read again.
func (d *InfoDialog) Close() {
	d.Done = true
	d.stop()
}

func (d *InfoDialog) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	switch m := msg.(type) {
	case tea.WindowSizeMsg:
		d.resize(m.Height, m.Width)
	case tea.KeyMsg:
		d.key(m)
	case infoTickMsg:
		cmd = d.tick(m)
	case infoResultMsg:
		cmd = d.result(m)
	}
	d.scroll = min(max(d.scroll, 0), d.maxScroll())
	if d.Done {
		d.stop()
		return d, tea.Quit
	}
	return d, cmd
}

// tick follows the shown window, starts a read when none is in flight,
// and schedules the next tick; a closed dialog schedules none.
func (d *InfoDialog) tick(m infoTickMsg) tea.Cmd {
	if d.Done || m.gen != d.gen {
		return nil
	}
	d.follow()
	return tea.Batch(d.fetch(), d.after(InfoEvery, infoTickMsg{d.gen}))
}

// follow makes the shown window the target. Another window shown (the
// one shown closed, or another was selected) cancels the read in flight
// and drops what was read: it is not this window's. It reports whether
// the target changed.
func (d *InfoDialog) follow() bool {
	t, ok := d.host.InfoTarget()
	if ok == d.hasTarget && t.RunID == d.target.RunID && t.Index == d.target.Index && t.Endpoint == d.target.Endpoint {
		d.target.Name, d.cur.Name = t.Name, t.Name
		return false
	}
	d.stop()
	d.gen++
	d.target, d.hasTarget = t, ok
	d.cur, d.good, d.errLine, d.scroll = info.Base(t), false, "", 0
	return true
}

// result takes a read's answer: dropped when it is for an earlier
// target or the dialog closed; a failure keeps the last good values and
// says why.
func (d *InfoDialog) result(m infoResultMsg) tea.Cmd {
	if d.Done || m.gen != d.gen {
		return nil
	}
	d.inFlight, d.cancel = false, nil
	if d.follow() {
		// Read for a window no longer shown: the new one's read starts,
		// and its own clock, since the pending tick is of the old gen.
		return tea.Batch(d.fetch(), d.after(InfoEvery, infoTickMsg{d.gen}))
	}
	if m.w.Problem != "" {
		d.errLine = m.w.Error
		return nil
	}
	d.cur, d.good, d.errLine = m.w, true, ""
	d.cur.Name = d.target.Name
	return nil
}

func (d *InfoDialog) key(m tea.KeyMsg) {
	k := m.String()
	if d.afterPfx {
		d.afterPfx = false
		if k == "i" {
			d.Close()
			return
		}
	}
	switch k {
	case "esc", "q", "ctrl+c":
		d.Close()
	case d.prefix:
		d.afterPfx = true
	case "up", "k":
		d.scroll--
	case "down", "j":
		d.scroll++
	case "pgup":
		d.scroll -= d.page()
	case "pgdown", " ":
		d.scroll += d.page()
	case "home", "g":
		d.scroll = 0
	case "end", "G":
		d.scroll = d.maxScroll()
	}
}

// page is how many body lines fit: the box less the blank line, two
// status lines and the foot.
func (d *InfoDialog) page() int { return max(1, maxBody(d.rows)-4) }

func (d *InfoDialog) maxScroll() int { return max(0, len(d.lines())-d.page()) }

// lines are the dialog's body: the info's sections, key column aligned,
// each line cut to the box (never wrapped).
func (d *InfoDialog) lines() []string {
	ls := info.Lines(d.cur, d.now())
	width := info.LineWidth(ls)
	var out []string
	for _, l := range ls {
		if l.Key == "" {
			if len(out) > 0 {
				out = append(out, "")
			}
			out = append(out, palette.section.Render(l.Section))
			continue
		}
		v := l.Value
		if l.Key == "error" {
			v = palette.err.Render(v)
		}
		out = append(out, "  "+palette.accent.Render(fmt.Sprintf("%-*s", width, l.Key))+"  "+v)
	}
	if !d.hasTarget {
		out = append(out, palette.dim.Render("  the session has no window"))
	}
	return out
}

// status is the lines under the body: reading, or the latest failure
// and the age of what is shown, or when it was read.
func (d *InfoDialog) status() []string {
	var age string
	if d.cur.FetchedAt != nil {
		age = info.Duration(d.now().Sub(*d.cur.FetchedAt))
	}
	switch {
	case d.errLine != "" && d.good:
		return []string{palette.err.Render(d.errLine), palette.dim.Render("shown as read " + age + " ago")}
	case d.errLine != "":
		return []string{palette.err.Render(d.errLine)}
	case !d.good && d.hasTarget:
		return []string{palette.dim.Render("reading from lux…")}
	case d.good:
		return []string{palette.dim.Render("read " + age + " ago · every " + info.Duration(InfoEvery))}
	}
	return nil
}

func (d *InfoDialog) View() string {
	if d.Done {
		return ""
	}
	if tooSmall(d.rows, d.cols) {
		return tooSmallView(d.rows, d.cols)
	}
	all := d.lines()
	page := d.page()
	lines := all
	more := ""
	if len(all) > page {
		end := min(len(all), d.scroll+page)
		lines = all[d.scroll:end]
		more = fmt.Sprintf("%d–%d of %d · ", d.scroll+1, end, len(all))
	}
	w := innerWidth(d.cols)
	body := make([]string, 0, len(lines)+4)
	for _, l := range lines {
		body = append(body, ansi.Truncate(l, w, "…"))
	}
	body = append(body, "")
	for _, l := range d.status() {
		body = append(body, ansi.Truncate(l, w, "…"))
	}
	foot := more + "↑↓ scroll · esc close"
	if w-len(more) >= 50 {
		foot = more + "↑↓ pgup/pgdn scroll · esc, q or " + d.prefixWord() + " i close"
	}
	body = append(body, keyHints(ansi.Truncate(foot, w, "…")))
	return frame(d.rows, d.cols, d.bg, "Window "+fmt.Sprint(d.target.Index)+" · "+d.target.Name, body)
}

// prefixWord is the prefix as the help names it: ctrl+b is C-b.
func (d *InfoDialog) prefixWord() string {
	if p, ok := strings.CutPrefix(d.prefix, "ctrl+"); ok {
		return "C-" + p
	}
	return d.prefix
}
