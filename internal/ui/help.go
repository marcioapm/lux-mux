package ui

import (
	"fmt"
	"slices"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/marcioapm/lux-mux/internal/config"
	keymap "github.com/marcioapm/lux-mux/internal/keys"
)

// opWords says what each command does, for the help.
var opWords = map[string]string{
	config.OpNew:       "new window (the session's default profile)",
	config.OpNewDialog: "new window (dialog)",
	config.OpNext:      "next window",
	config.OpPrev:      "previous window",
	config.OpSelect:    "select window",
	config.OpLast:      "last window",
	config.OpPrompt:    "select window by index",
	config.OpChoose:    "window chooser",
	config.OpRename:    "rename window",
	config.OpKill:      "kill window",
	config.OpRelaunch:  "resume a stopped Run / relaunch an ended one",
	config.OpDetach:    "detach (the Runs keep running)",
	config.OpHelp:      "this help",
}

// helpSections lists the prefix table's commands by section, in order.
var helpSections = []struct {
	title string
	ops   []string
}{
	{"Windows", []string{config.OpNew, config.OpNewDialog, config.OpNext, config.OpPrev, config.OpSelect, config.OpLast,
		config.OpPrompt, config.OpChoose, config.OpRename, config.OpKill, config.OpRelaunch}},
	{"Session", []string{config.OpDetach, config.OpHelp}},
}

// helpEntry is one line of the help: a section title (key empty) or a
// key and what it does.
type helpEntry struct {
	section, key, words string
}

// HelpEntries are the help's lines for cfg's prefix and direct bindings.
func HelpEntries(cfg *config.Config) []helpEntry {
	p := cfg.Prefix
	var out []helpEntry
	for _, s := range helpSections {
		out = append(out, helpEntry{section: s.title})
		for _, op := range s.ops {
			var ks []string
			if op == config.OpSelect {
				ks = []string{"0-9"}
			}
			for k, c := range keymap.PrefixKeys {
				if c.Op == op {
					ks = append(ks, string(rune(k)))
				}
			}
			slices.Sort(ks)
			for _, k := range ks {
				out = append(out, helpEntry{s.title, p + " " + k, opWords[op]})
			}
		}
	}
	out = append(out, helpEntry{section: "Prefix"},
		helpEntry{"Prefix", p, "the prefix: the next key is a command"},
		helpEntry{"Prefix", p + " " + p, "send " + p + " to the window"})
	out = append(out, helpEntry{section: "Direct bindings"})
	if len(cfg.Keys.Direct) == 0 {
		out = append(out, helpEntry{"Direct bindings", "", "none configured"})
	} else {
		names := make([]string, 0, len(cfg.Keys.Direct))
		for k := range cfg.Keys.Direct {
			names = append(names, k)
		}
		slices.Sort(names)
		for _, k := range names {
			v := cfg.Keys.Direct[k]
			words := v
			if c, err := config.ParseCommand(v); err == nil {
				if w, ok := opWords[c.Op]; ok {
					words = w
					if c.Op == config.OpSelect {
						words = fmt.Sprintf("select window %d", c.Arg)
					}
				}
			}
			out = append(out, helpEntry{"Direct bindings", k, words})
		}
	}
	out = append(out, helpEntry{section: "In dialogs"},
		helpEntry{"In dialogs", "Tab / Shift-Tab", "next / previous field"},
		helpEntry{"In dialogs", "Enter", "open a list, pick, or go to the next field"},
		helpEntry{"In dialogs", "Space", "toggle a checkbox"},
		helpEntry{"In dialogs", "Ctrl-S", "create"},
		helpEntry{"In dialogs", "Esc", "close a list, or the dialog"})
	return out
}

// Help is the key help's model: the entries in sections, scrolled and
// filtered.
type Help struct {
	entries    []helpEntry
	bg         []string
	background func(rows, cols int) []string
	rows, cols int
	scroll     int
	filtering  bool
	filter     string
	Done       bool
}

// NewHelp shows cfg's keys; background is the window drawn behind.
func NewHelp(cfg *config.Config, background func(rows, cols int) []string, rows, cols int) *Help {
	h := &Help{entries: HelpEntries(cfg), background: background}
	h.resize(rows, cols)
	return h
}

func (h *Help) resize(rows, cols int) {
	h.rows, h.cols = rows, cols
	if h.background != nil {
		h.bg = h.background(rows, cols)
	}
}

func (h *Help) Init() tea.Cmd { return nil }

func (h *Help) finished() bool { return h.Done }

// lines are the help's text lines after the filter: a section's title is
// kept when one of its entries matches.
func (h *Help) lines() []string {
	f := strings.ToLower(h.filter)
	var out []string
	for i, e := range h.entries {
		if e.key == "" && e.words == "" {
			// A section title: kept when an entry of it matches.
			var kept []string
			for _, x := range h.entries[i+1:] {
				if x.key == "" && x.words == "" {
					break
				}
				if f == "" || strings.Contains(strings.ToLower(x.key+" "+x.words+" "+x.section), f) {
					kept = append(kept, fmt.Sprintf("  %s %s", palette.accent.Render(fmt.Sprintf("%-18s", x.key)), x.words))
				}
			}
			if len(kept) > 0 {
				if len(out) > 0 {
					out = append(out, "")
				}
				out = append(out, palette.section.Render(e.section))
				out = append(out, kept...)
			}
		}
	}
	if len(out) == 0 {
		out = []string{palette.dim.Render("  no key matches “" + h.filter + "”")}
	}
	return out
}

// page is how many lines of the help fit in the box.
func (h *Help) page() int { return max(1, maxBody(h.rows)-2) }

func (h *Help) maxScroll() int { return max(0, len(h.lines())-h.page()) }

func (h *Help) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch m := msg.(type) {
	case tea.WindowSizeMsg:
		h.resize(m.Height, m.Width)
	case tea.KeyMsg:
		h.key(m)
	}
	h.scroll = min(max(h.scroll, 0), h.maxScroll())
	if h.Done {
		return h, tea.Quit
	}
	return h, nil
}

func (h *Help) key(m tea.KeyMsg) {
	k := m.String()
	if h.filtering {
		switch {
		case k == "esc":
			h.filtering, h.filter = false, ""
		case k == "enter":
			h.filtering = false
		case k == "backspace":
			if r := []rune(h.filter); len(r) > 0 {
				h.filter = string(r[:len(r)-1])
			}
		case k == "ctrl+c":
			h.Done = true
		case m.Type == tea.KeyRunes:
			h.filter += string(m.Runes)
		case m.Type == tea.KeySpace:
			h.filter += " "
		}
		h.scroll = 0
		return
	}
	switch k {
	case "esc", "q", "?", "ctrl+c":
		h.Done = true
	case "up", "k":
		h.scroll--
	case "down", "j":
		h.scroll++
	case "pgup":
		h.scroll -= h.page()
	case "pgdown", " ":
		h.scroll += h.page()
	case "home", "g":
		h.scroll = 0
	case "end", "G":
		h.scroll = h.maxScroll()
	case "/":
		h.filtering, h.filter = true, ""
	}
}

func (h *Help) View() string {
	if h.Done {
		return ""
	}
	if tooSmall(h.rows, h.cols) {
		return tooSmallView(h.rows, h.cols)
	}
	lines := h.lines()
	page := h.page()
	more := ""
	if len(lines) > page {
		end := min(len(lines), h.scroll+page)
		lines = lines[h.scroll:end]
		more = fmt.Sprintf("  %d–%d of %d", h.scroll+1, end, len(h.lines()))
	}
	var foot string
	switch {
	case h.filtering:
		foot = "filter: " + h.filter + "_  (enter keep · esc clear)"
	case h.filter != "":
		foot = "[" + h.filter + "] ↑↓ scroll · / filter · esc close"
	case innerWidth(h.cols) < 60:
		foot = "↑↓ pgup/pgdn scroll · / filter · esc close"
	default:
		foot = "↑↓ pgup/pgdn scroll · / filter · esc, q or ? close"
	}
	foot = ansi.Truncate(foot+more, innerWidth(h.cols), "…")
	if h.filtering {
		foot = palette.focus.Render(fitLine(foot, innerWidth(h.cols)))
	} else {
		foot = keyHints(foot)
	}
	return frame(h.rows, h.cols, h.bg, "Keys", append(lines, "", foot))
}
