package ui

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
)

var colorSGR = regexp.MustCompile(`\x1b\[[0-9;]*(38|48);[0-9;]*m`)
var backgroundSGR = regexp.MustCompile(`\x1b\[[0-9;]*48;[0-9;]*m`)

func trueColor(t *testing.T, dark bool) {
	t.Helper()
	profile := lipgloss.ColorProfile()
	background := lipgloss.HasDarkBackground()
	lipgloss.SetColorProfile(termenv.TrueColor)
	lipgloss.SetHasDarkBackground(dark)
	t.Cleanup(func() {
		lipgloss.SetColorProfile(profile)
		lipgloss.SetHasDarkBackground(background)
	})
}

func viewLine(t *testing.T, view, label string) string {
	t.Helper()
	for _, line := range strings.Split(view, "\n") {
		if strings.Contains(ansi.Strip(line), label) {
			return line
		}
	}
	t.Fatalf("missing row %q", label)
	return ""
}

func TestThemeFocus(t *testing.T) {
	for _, dark := range []bool{false, true} {
		name := "light"
		if dark {
			name = "dark"
		}
		t.Run(name, func(t *testing.T) {
			trueColor(t, dark)
			t.Setenv("NO_COLOR", "")
			d := open(t, newHost(t), 30, 100)
			h := NewHelp(d.cfg, nil, 30, 100)
			h.filtering = true
			c := NewChooser(host(), 30, 100)
			for _, tc := range []struct {
				name, view, focused, other string
			}{
				{"dialog", d.View(), "▸ Profile", "Image"},
				{"help", h.View(), "filter:", "Windows"},
				{"chooser", c.View(), "▸ 1", "editor"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					if !backgroundSGR.MatchString(viewLine(t, tc.view, tc.focused)) {
						t.Fatal("focused row has no background band")
					}
					if backgroundSGR.MatchString(viewLine(t, tc.view, tc.other)) {
						t.Fatal("unfocused row has a background band")
					}
				})
			}
			d.setFocus(stImage)
			if backgroundSGR.MatchString(viewLine(t, d.View(), "Profile")) || !backgroundSGR.MatchString(viewLine(t, d.View(), "▸ Image")) {
				t.Fatal("dialog band did not follow focus")
			}
			c.Key("up")
			if backgroundSGR.MatchString(viewLine(t, c.View(), "logs")) || !backgroundSGR.MatchString(viewLine(t, c.View(), "▸ 0")) {
				t.Fatal("chooser band did not follow focus")
			}
			h.filtering = false
			if backgroundSGR.MatchString(viewLine(t, h.View(), "scroll")) {
				t.Fatal("inactive help filter retains its band")
			}
		})
	}
}

func TestThemeNoColor(t *testing.T) {
	trueColor(t, true)
	for _, value := range []string{"1", "0", "false"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("NO_COLOR", value)
			d := open(t, newHost(t), 30, 100)
			d.setFocus(stBranch)
			h := NewHelp(d.cfg, nil, 30, 100)
			h.filtering = true
			c := NewChooser(host(), 30, 100)
			for _, view := range []string{d.View(), h.View(), c.View()} {
				if colorSGR.MatchString(view) {
					t.Fatal("NO_COLOR emitted an SGR colour")
				}
				if !strings.Contains(view, "\x1b[7m") {
					t.Fatal("monochrome focus lost reverse video")
				}
			}
		})
	}
}

// Render output is opt-in so ordinary tests never write artifacts.
func TestThemeRenders(t *testing.T) {
	dir := os.Getenv("UI_RENDER_DIR")
	if dir == "" {
		t.Skip("UI_RENDER_DIR is not set")
	}
	trueColor(t, true)
	t.Setenv("NO_COLOR", "")
	d := open(t, newHost(t), 30, 100)
	h := NewHelp(d.cfg, nil, 30, 100)
	c := NewChooser(host(), 30, 100)
	for name, view := range map[string]string{"dialog": d.View(), "help": h.View(), "chooser": c.View()} {
		if err := os.WriteFile(filepath.Join(dir, "render-"+name+".ansi"), []byte(view), 0600); err != nil {
			t.Fatal(err)
		}
	}
}
