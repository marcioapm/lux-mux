// Package ui is luxmux's full-screen dialogs: the window chooser and the
// new-window dialog. They own the terminal while open; the multiplexer
// repaints the active window when they close.
package ui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"
	"github.com/marcioapm/lux/api"

	"github.com/marcioapm/lux-mux/internal/config"
	"github.com/marcioapm/lux-mux/internal/mux"
)

// New is the dialogs.
func New() mux.UI { return dialogs{} }

type dialogs struct{}

// run runs model on the session's terminal with the keyboard, in the
// alternate screen, until it quits.
func run(m *mux.Mux, model tea.Model) (tea.Model, error) {
	out, _, _ := m.Terminal()
	kb := m.KeyboardReader()
	defer kb.Close()
	p := tea.NewProgram(model, tea.WithInput(kb), tea.WithOutput(out), tea.WithAltScreen(),
		tea.WithContext(m.Context()), tea.WithoutSignalHandler())
	res, err := p.Run()
	// The program's last frame is still on the alternate screen: the
	// multiplexer's repaint replaces it.
	_, _ = io.WriteString(out, "\x1b[?1049l")
	if errors.Is(err, tea.ErrProgramKilled) {
		err = nil
	}
	return res, err
}

func (dialogs) Choose(m *mux.Mux) (mux.ChooseResult, error) {
	_, rows, cols := m.Terminal()
	c := NewChooser(m, rows, cols)
	_, err := run(m, chooserProgram{c})
	return mux.ChooseResult{Select: c.Chosen}, err
}

// formModel runs a huh form until it completes or is aborted.
type formModel struct{ f *huh.Form }

func (fm formModel) Init() tea.Cmd { return fm.f.Init() }

func (fm formModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if k, ok := msg.(tea.KeyMsg); ok && k.String() == "ctrl+c" {
		fm.f.State = huh.StateAborted
		return fm, tea.Quit
	}
	f, cmd := fm.f.Update(msg)
	fm.f = f.(*huh.Form)
	if fm.f.State != huh.StateNormal {
		return fm, tea.Quit
	}
	return fm, cmd
}

func (fm formModel) View() string {
	if fm.f.State != huh.StateNormal {
		return ""
	}
	return fm.f.View()
}

// form runs groups as one form; ok is false when the user aborted (Esc).
func form(m *mux.Mux, groups ...*huh.Group) (bool, error) {
	_, rows, cols := m.Terminal()
	f := huh.NewForm(groups...).WithShowHelp(true).WithWidth(min(cols, 100)).WithHeight(rows - 1)
	km := huh.NewDefaultKeyMap()
	km.Quit.SetKeys("esc", "ctrl+c")
	f = f.WithKeyMap(km)
	res, err := run(m, formModel{f})
	if err != nil {
		return false, err
	}
	return res.(formModel).f.State == huh.StateCompleted, nil
}

// NewDialog asks for a window's profile, starting from one of the
// configured ones, checks it with lux's dry run, and returns it.
func (dialogs) NewDialog(m *mux.Mux) (*mux.NewWindowRequest, error) {
	cfg := m.Config()
	base := cfg.DefaultProfile
	var opts []huh.Option[string]
	for _, n := range cfg.ProfileNames() {
		opts = append(opts, huh.NewOption(n, n))
	}
	if ok, err := form(m, huh.NewGroup(huh.NewSelect[string]().Title("New window: start from profile").
		Options(opts...).Value(&base))); err != nil || !ok {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(m.Context(), 15*time.Second)
	pools, perr := m.Lux().Pools(ctx)
	cancel()
	v := ValuesFrom(base, cfg.Profiles[base])
	images, repos := m.Recent()
	imageSugg := uniq(append(profileField(cfg, func(p config.Profile) string { return p.Image }), images...))
	repoSugg := uniq(append(profileField(cfg, func(p config.Profile) string { return p.Repository }), repos...))
	notes := ""
	if perr != nil {
		notes = "pools unavailable: " + perr.Error()
	}
	for {
		poolOpts := []huh.Option[string]{huh.NewOption("(tenant default)", "")}
		for _, p := range pools {
			poolOpts = append(poolOpts, huh.NewOption(p.Name, p.Name))
		}
		g1 := []huh.Field{
			huh.NewInput().Title("Image").Suggestions(imageSugg).Value(&v.Image),
		}
		if len(pools) > 0 {
			g1 = append(g1, huh.NewSelect[string]().Title("Pool").Options(poolOpts...).Value(&v.Pool))
		} else {
			g1 = append(g1, huh.NewInput().Title("Pool (empty: default)").Value(&v.Pool))
		}
		if notes != "" {
			g1 = append([]huh.Field{huh.NewNote().Title("Check").Description(notes)}, g1...)
		}
		if ok, err := form(m, huh.NewGroup(g1...)); err != nil || !ok {
			return nil, err
		}
		shaped, sizes := Shaped(pools, v.Pool)
		var g2 []huh.Field
		if shaped {
			var so []huh.Option[string]
			so = append(so, huh.NewOption("(pool default)", ""))
			for _, s := range sizes {
				so = append(so, huh.NewOption(fmt.Sprintf("%s  %g cpus, %s, %s", s.Name, s.CPUs, gib(s.Memory), gib(s.Disk)), s.Name))
			}
			g2 = append(g2, huh.NewSelect[string]().Title("Size").Options(so...).Value(&v.Size))
		} else {
			g2 = append(g2,
				huh.NewInput().Title("CPUs (empty: lux's default)").Value(&v.CPUs),
				huh.NewInput().Title("Memory, e.g. 8Gi").Value(&v.Memory),
				huh.NewInput().Title("Disk, e.g. 20Gi").Value(&v.Disk))
		}
		g2 = append(g2,
			huh.NewInput().Title("Repository (empty: none)").Suggestions(repoSugg).Value(&v.Repository),
			huh.NewInput().Title("Branch (empty: the remote's HEAD)").Value(&v.Branch),
			huh.NewConfirm().Title("Keep a warm spare of this profile? (billed while it waits)").Value(&v.WarmSpare),
			huh.NewInput().Title("Save as profile (empty: don't)").Value(&v.SaveAs))
		if ok, err := form(m, huh.NewGroup(g2...)); err != nil || !ok {
			return nil, err
		}
		prof, err := v.Profile(cfg.Profiles[base], shaped)
		if err != nil {
			notes = err.Error()
			continue
		}
		ctx, cancel := context.WithTimeout(m.Context(), 30*time.Second)
		ans, err := m.Validate(ctx, base, prof)
		cancel()
		if err != nil {
			notes = "lux refuses it: " + err.Error()
			continue
		}
		summary := "valid"
		if ans.EffectivePool != "" {
			summary += "; pool " + ans.EffectivePool
		}
		if !ans.WouldStartNow {
			summary += "\nwould not start now (it waits for capacity):\n  " + strings.Join(Blockers(ans), "\n  ")
		}
		create := true
		if ok, err := form(m, huh.NewGroup(huh.NewNote().Title("lux dry run").Description(summary),
			huh.NewConfirm().Title("Create the window?").Affirmative("Create").Negative("Edit").Value(&create))); err != nil || !ok {
			return nil, err
		}
		if !create {
			notes = ""
			continue
		}
		name := base
		return &mux.NewWindowRequest{ProfileName: name, Profile: prof, SaveAs: v.SaveAs}, nil
	}
}

func gib(b api.Bytes) string { return fmt.Sprintf("%gGi", float64(b)/(1<<30)) }

func profileField(c *config.Config, f func(config.Profile) string) []string {
	var out []string
	for _, n := range c.ProfileNames() {
		if s := f(c.Profiles[n]); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func uniq(l []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range l {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
