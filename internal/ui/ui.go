// Package ui is luxmux's full-screen dialogs: the window chooser and the
// new-window dialog. They own the terminal while open; the multiplexer
// repaints the active window when they close.
package ui

import (
	"context"
	"errors"
	"fmt"
	"io"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/marcioapm/lux/api"

	"github.com/marcioapm/lux-mux/internal/config"
	"github.com/marcioapm/lux-mux/internal/mux"
)

// New is the dialogs.
func New() mux.UI { return dialogs{} }

type dialogs struct{}

// finisher is a model that knows it is done.
type finisher interface{ finished() bool }

// releasing hands the keyboard back as soon as the model is done, so the
// program's read loop ends at once instead of after bubbletea's timeout.
type releasing struct {
	tea.Model
	release    func()
	keyHandled func()
}

func (r releasing) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	m, cmd := r.Model.Update(msg)
	r.Model = m
	if f, ok := m.(finisher); ok && f.finished() {
		r.release()
	} else if inputMsg(msg) && r.keyHandled != nil {
		r.keyHandled()
	}
	return r, cmd
}

// inputMsg reports a message bubbletea makes of keyboard input, the
// unexported ones for bytes and sequences it does not know included.
func inputMsg(msg tea.Msg) bool {
	switch msg.(type) {
	case tea.KeyMsg, tea.MouseMsg, tea.FocusMsg, tea.BlurMsg:
		return true
	}
	switch fmt.Sprintf("%T", msg) {
	case "tea.unknownInputByteMsg", "tea.unknownCSISequenceMsg":
		return true
	}
	return false
}

// run runs model on the session's terminal with the keyboard, in the
// alternate screen, until it quits.
func run(m *mux.Mux, model tea.Model) (tea.Model, error) {
	out, _, _ := m.Terminal()
	kb := m.KeyboardReader()
	kb.OneKeyAtATime()
	defer kb.Close()
	p := tea.NewProgram(releasing{model, func() { kb.Close() }, kb.KeyHandled}, tea.WithInput(kb), tea.WithOutput(out), tea.WithAltScreen(),
		tea.WithContext(m.Context()), tea.WithoutSignalHandler())
	m.OnResize(func(rows, cols int) {
		p.Send(tea.WindowSizeMsg{Width: cols, Height: rows})
	})
	defer m.OnResize(nil)
	res, err := p.Run()
	// The program's last frame is still on the alternate screen: the
	// multiplexer's repaint replaces it.
	_, _ = io.WriteString(out, "\x1b[?1049l")
	if errors.Is(err, tea.ErrProgramKilled) {
		err = nil
	}
	if r, ok := res.(releasing); ok {
		res = r.Model
	}
	return res, err
}

func (dialogs) Choose(m *mux.Mux) (mux.ChooseResult, error) {
	_, rows, cols := m.Terminal()
	c := NewChooser(m, rows, cols)
	_, err := run(m, chooserProgram{c})
	return mux.ChooseResult{Select: c.Chosen}, err
}

// Help shows the keys until it is closed.
func (dialogs) Help(m *mux.Mux) error {
	_, rows, cols := m.Terminal()
	_, err := run(m, NewHelp(m.Config(), muxHost{m}.Background, rows, cols))
	return err
}

// muxHost is the multiplexer as the dialogs see it.
type muxHost struct{ m *mux.Mux }

func (h muxHost) Config() *config.Config                        { return h.m.Config() }
func (h muxHost) Recent() (images, repos []string)              { return h.m.Recent() }
func (h muxHost) Pools(ctx context.Context) ([]api.Pool, error) { return h.m.Lux().Pools(ctx) }
func (h muxHost) Validate(ctx context.Context, name string, p config.Profile) (*api.SubmitAnswer, error) {
	return h.m.Validate(ctx, name, p)
}

// Background is the active window's screen, as the chooser previews it.
func (h muxHost) Background(rows, cols int) []string {
	for _, w := range h.m.Windows() {
		if w.Active {
			return h.m.Preview(w.Index, rows, cols)
		}
	}
	return nil
}

// NewDialog asks for a window's profile in one form, starting from one of
// the configured ones, checks it with lux's dry run, and returns it.
func (dialogs) NewDialog(m *mux.Mux) (*mux.NewWindowRequest, error) {
	_, rows, cols := m.Terminal()
	d := NewDialogModel(m.Context(), muxHost{m}, rows, cols)
	if _, err := run(m, d); err != nil {
		return nil, err
	}
	return d.Request, nil
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
