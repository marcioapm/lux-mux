package ui

import (
	"io"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/marcioapm/lux-mux/internal/config"
)

func helpCfg(t *testing.T, src string) *config.Config {
	t.Helper()
	c, err := config.Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func helpKeys(h *Help, ks ...tea.KeyMsg) {
	for _, k := range ks {
		h.Update(k)
	}
}

func runes(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }

func TestHelpSectionsAndConfiguredKeys(t *testing.T) {
	cfg := helpCfg(t, "prefix = \"C-a\"\n[keys.direct]\n\"M-n\" = \"next\"\n\"M-1\" = \"select 1\"\n")
	h := NewHelp(cfg, func(int, int) []string { return []string{"$ shell behind"} }, 60, 100)
	out := plain(h.View())
	for _, want := range []string{"Keys", "Windows", "Session", "Prefix", "Direct bindings", "In dialogs",
		"C-a c", "new window (default profile)", "C-a C", "new window (dialog)", "C-a n", "C-a p", "C-a 0-9",
		"C-a l", "C-a '", "C-a w", "window chooser", "C-a ,", "rename window", "C-a &", "kill window",
		"C-a d", "detach", "C-a ?", "C-a C-a", "send C-a to the window",
		"M-n", "next window", "M-1", "select window 1",
		"Tab / Shift-Tab", "Enter", "Space", "Ctrl-S", "Esc", "shell behind"} {
		if !strings.Contains(out, want) {
			t.Errorf("help lacks %q", want)
		}
	}
	if strings.Contains(out, "C-b") || strings.Contains(out, "none configured") {
		t.Fatalf("not built from the configuration:\n%s", out)
	}
	// Sections in order.
	last := -1
	for _, s := range []string{"Windows", "Session", "Prefix", "Direct bindings", "In dialogs"} {
		i := strings.Index(out, s)
		if i <= last {
			t.Fatalf("section %s out of order:\n%s", s, out)
		}
		last = i
	}

	h = NewHelp(helpCfg(t, ""), nil, 60, 100)
	out = plain(h.View())
	if !strings.Contains(out, "none configured") || !strings.Contains(out, "C-b C-b") {
		t.Fatalf("default config:\n%s", out)
	}
}

func TestHelpFilter(t *testing.T) {
	h := NewHelp(helpCfg(t, ""), nil, 60, 100)
	helpKeys(h, runes("/"), runes("d"), runes("e"), runes("t"))
	out := plain(h.View())
	if !strings.Contains(out, "detach") || strings.Contains(out, "window chooser") || strings.Contains(out, "Windows") {
		t.Fatalf("filter det:\n%s", out)
	}
	if !strings.Contains(out, "Session") || !strings.Contains(out, "filter: det_") {
		t.Fatalf("section title or filter line missing:\n%s", out)
	}
	// q while filtering is text, not close.
	helpKeys(h, runes("q"))
	if h.Done || !strings.Contains(plain(h.View()), "no key matches") {
		t.Fatalf("q in the filter: done %v\n%s", h.Done, plain(h.View()))
	}
	helpKeys(h, tea.KeyMsg{Type: tea.KeyBackspace}, tea.KeyMsg{Type: tea.KeyEnter})
	if out := plain(h.View()); !strings.Contains(out, "[det]") || !strings.Contains(out, "detach") {
		t.Fatalf("filter kept:\n%s", out)
	}
	helpKeys(h, runes("/"), tea.KeyMsg{Type: tea.KeyEsc})
	if h.Done || !strings.Contains(plain(h.View()), "window chooser") {
		t.Fatal("esc in the filter clears it and keeps the help")
	}
}

func TestHelpScroll(t *testing.T) {
	h := NewHelp(helpCfg(t, ""), nil, 14, 80)
	total := len(h.lines())
	if total <= h.page() {
		t.Fatalf("%d lines fit in %d: the test does not scroll", total, h.page())
	}
	first := plain(h.View())
	if !strings.Contains(first, "Windows") || strings.Contains(first, "In dialogs") {
		t.Fatalf("top:\n%s", first)
	}
	helpKeys(h, tea.KeyMsg{Type: tea.KeyDown})
	if h.scroll != 1 {
		t.Fatalf("down: %d", h.scroll)
	}
	helpKeys(h, tea.KeyMsg{Type: tea.KeyPgDown}, tea.KeyMsg{Type: tea.KeyPgDown}, tea.KeyMsg{Type: tea.KeyPgDown})
	if h.scroll != h.maxScroll() || !strings.Contains(plain(h.View()), "Ctrl-S") {
		t.Fatalf("pgdown to the end: %d of %d\n%s", h.scroll, h.maxScroll(), plain(h.View()))
	}
	helpKeys(h, tea.KeyMsg{Type: tea.KeyPgUp})
	if h.scroll != max(0, h.maxScroll()-h.page()) {
		t.Fatalf("pgup: %d", h.scroll)
	}
	helpKeys(h, tea.KeyMsg{Type: tea.KeyUp}, tea.KeyMsg{Type: tea.KeyPgUp}, tea.KeyMsg{Type: tea.KeyPgUp})
	if h.scroll != 0 {
		t.Fatalf("up stops at the top: %d", h.scroll)
	}
	if n := len(strings.Split(plain(h.View()), "\n")); n != 14 {
		t.Fatalf("%d rows", n)
	}
}

func TestHelpCloseKeys(t *testing.T) {
	for _, k := range []tea.KeyMsg{{Type: tea.KeyEsc}, runes("q"), runes("?")} {
		h := NewHelp(helpCfg(t, ""), nil, 30, 100)
		_, cmd := h.Update(k)
		if !h.Done || cmd == nil {
			t.Fatalf("%v did not close", k)
		}
	}
	h := NewHelp(helpCfg(t, ""), nil, 30, 100)
	helpKeys(h, runes("x"), tea.KeyMsg{Type: tea.KeyEnter}, tea.KeyMsg{Type: tea.KeyDown})
	if h.Done {
		t.Fatal("closed by another key")
	}
}

// TestHelpIgnoresKeyReleases: kitty key-release reports for the keys that
// opened the help (read by the help, after it opened) do not close it.
func TestHelpIgnoresKeyReleases(t *testing.T) {
	h := NewHelp(helpCfg(t, ""), nil, 30, 100)
	in := strings.NewReader("\x1b[63;2:3u\x1b[98;5:3u\x1b[Bq")
	p := tea.NewProgram(h, tea.WithInput(in), tea.WithOutput(io.Discard), tea.WithoutSignalHandler(), tea.WithoutRenderer())
	if _, err := p.Run(); err != nil {
		t.Fatal(err)
	}
	if !h.Done || h.scroll != 1 {
		t.Fatalf("done %v scroll %d: a release closed the help before ↓", h.Done, h.scroll)
	}
}

func TestHelpLayout(t *testing.T) {
	for _, sz := range [][2]int{{24, 80}, {50, 200}} {
		h := NewHelp(helpCfg(t, ""), nil, sz[0], sz[1])
		col, rows := boxLeft(t, h.View())
		if want := (sz[1] - boxWidth(sz[1])) / 2; col != want || rows != sz[0] {
			t.Fatalf("%dx%d: column %d (want %d), %d rows", sz[1], sz[0], col, want, rows)
		}
	}
	h := NewHelp(helpCfg(t, ""), nil, 10, 30)
	if out := plain(h.View()); !strings.Contains(out, "too small") {
		t.Fatalf("30x10:\n%s", out)
	}
	h.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	if col, _ := boxLeft(t, h.View()); col != (100-boxWidth(100))/2 {
		t.Fatalf("after resize: %d", col)
	}
}
