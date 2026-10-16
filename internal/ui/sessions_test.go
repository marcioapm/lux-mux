package ui

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/marcioapm/lux-mux/internal/mux"
	"github.com/marcioapm/lux-mux/internal/sessions"
)

type fakeSessionHost struct {
	list     []mux.SessionInfo
	previews []string // names previewed, in order
	renamed  [][2]string
	killed   []string
	killErr  error
}

func (f *fakeSessionHost) Sessions() ([]mux.SessionInfo, error) {
	return append([]mux.SessionInfo{}, f.list...), nil
}

func (f *fakeSessionHost) SessionPreview(name string, rows, cols int) []string {
	f.previews = append(f.previews, name)
	return []string{"screen of " + name}
}

func (f *fakeSessionHost) RenameSession(old, new string) error {
	f.renamed = append(f.renamed, [2]string{old, new})
	for i := range f.list {
		if f.list[i].Name == old {
			f.list[i].Name = new
		}
	}
	return nil
}

func (f *fakeSessionHost) KillSession(name string) error {
	if f.killErr != nil {
		return f.killErr
	}
	f.killed = append(f.killed, name)
	for i := range f.list {
		if f.list[i].Name == name {
			f.list = append(f.list[:i], f.list[i+1:]...)
			break
		}
	}
	return nil
}

func sessionHost() *fakeSessionHost {
	t0 := time.Now().Add(-3 * time.Hour)
	return &fakeSessionHost{list: []mux.SessionInfo{
		{Info: sessions.Info{Name: "api", Windows: 2, DefaultProfile: "dev", Created: t0}},
		{Info: sessions.Info{Name: "main", Windows: 1, Created: t0}, Current: true},
		{Info: sessions.Info{Name: "web", Windows: 3, DefaultProfile: "ops", Created: t0, Elsewhere: true, Pid: "42"}},
	}}
}

func skeys(c *SessionChooser, ks ...string) {
	for _, k := range ks {
		c.Key(k)
	}
}

// TestSessionChooserColumnsAndPreview: one row per session with its
// window count, where it is attached, its default profile and age; the
// current one is marked and highlighted first; the preview follows the
// highlight, and a session attached elsewhere is not previewed.
func TestSessionChooserColumnsAndPreview(t *testing.T) {
	h := sessionHost()
	c := NewSessionChooser(h, 24, 80)
	if s, _ := c.Selected(); s.Name != "main" {
		t.Fatalf("starts on %q", s.Name)
	}
	out := plain(c.View())
	for _, want := range []string{"NAME", "WINDOWS", "ATTACHED", "DEFAULT PROFILE", "AGE",
		"main *", "here", "elsewhere", "dev", "ops", "3h", "screen of main"} {
		if !strings.Contains(out, want) {
			t.Errorf("view lacks %q:\n%s", want, out)
		}
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "api") && !strings.Contains(line, " 2 ") {
			t.Errorf("api's window count: %q", line)
		}
	}
	skeys(c, "j")
	out = plain(c.View())
	if !strings.Contains(out, elsewhereNote) || strings.Contains(out, "screen of web") {
		t.Fatalf("web's preview:\n%s", out)
	}
	if h.previews[len(h.previews)-1] == "web" {
		t.Fatal("a session attached elsewhere was asked for its preview")
	}
	skeys(c, "k", "k")
	if !strings.Contains(plain(c.View()), "screen of api") {
		t.Fatal("api's preview")
	}
}

// TestSessionChooserFilterAndSwitch: / filters by name or default
// profile as typed; Enter switches to the highlighted session, and is
// refused for one attached elsewhere; Enter on the current one closes.
func TestSessionChooserFilterAndSwitch(t *testing.T) {
	c := NewSessionChooser(sessionHost(), 24, 80)
	skeys(c, "/", "o", "p")
	if v := c.Visible(); len(v) != 1 || v[0].Name != "web" {
		t.Fatalf("filter op: %+v", v)
	}
	skeys(c, "enter", "enter")
	if c.Done || c.Switch != "" || !strings.Contains(plain(c.View()), "web is attached elsewhere") {
		t.Fatalf("enter on web: done %v switch %q\n%s", c.Done, c.Switch, plain(c.View()))
	}
	skeys(c, "/", "esc")
	if len(c.Visible()) != 3 {
		t.Fatal("esc clears the filter")
	}
	skeys(c, "/", "d", "e", "v", "enter")
	if v := c.Visible(); len(v) != 1 || v[0].Name != "api" {
		t.Fatalf("filter dev: %+v", v)
	}
	skeys(c, "enter")
	if !c.Done || c.Switch != "api" {
		t.Fatalf("enter on api: done %v switch %q", c.Done, c.Switch)
	}

	c = NewSessionChooser(sessionHost(), 24, 80)
	skeys(c, "enter")
	if !c.Done || c.Switch != "" {
		t.Fatalf("enter on the current session: done %v switch %q", c.Done, c.Switch)
	}
	c = NewSessionChooser(sessionHost(), 24, 80)
	skeys(c, "q")
	if !c.Done || c.Switch != "" {
		t.Fatal("q closes without a switch")
	}
}

// TestSessionChooserRenameAndKill: , renames the highlighted session and
// keeps it highlighted; x asks before killing it; the current session and
// one attached elsewhere are refused with a message, and a host's
// refusal is shown.
func TestSessionChooserRenameAndKill(t *testing.T) {
	h := sessionHost()
	c := NewSessionChooser(h, 24, 80)
	skeys(c, "k", ",")
	if !strings.Contains(plain(c.View()), "rename session: api_") {
		t.Fatalf("rename prompt:\n%s", plain(c.View()))
	}
	skeys(c, "backspace", "backspace", "backspace", "b", "a", "c", "k", "enter")
	if !reflect.DeepEqual(h.renamed, [][2]string{{"api", "back"}}) {
		t.Fatalf("renamed %v", h.renamed)
	}
	if s, _ := c.Selected(); s.Name != "back" {
		t.Fatalf("after rename on %q", s.Name)
	}
	skeys(c, "x")
	if !strings.Contains(plain(c.View()), "kill session back") {
		t.Fatalf("kill confirm:\n%s", plain(c.View()))
	}
	skeys(c, "n")
	if len(h.killed) != 0 {
		t.Fatal("killed without y")
	}
	skeys(c, "x", "y")
	if !reflect.DeepEqual(h.killed, []string{"back"}) || len(c.all) != 2 {
		t.Fatalf("killed %v, %d left", h.killed, len(c.all))
	}
	if c.Done {
		t.Fatal("a kill closed the chooser")
	}

	c = NewSessionChooser(h, 24, 80)
	skeys(c, "x")
	if c.mode == modeConfirmKill || !strings.Contains(plain(c.View()), "cannot be killed from itself") {
		t.Fatalf("x on the current session:\n%s", plain(c.View()))
	}
	skeys(c, "j", "x")
	if c.mode == modeConfirmKill || !strings.Contains(plain(c.View()), "attached elsewhere: detach it there first") {
		t.Fatalf("x on web:\n%s", plain(c.View()))
	}
	skeys(c, ",")
	if c.mode == modeRename {
		t.Fatal("rename of a session attached elsewhere")
	}

	h = sessionHost()
	h.killErr = errors.New("terminate failed")
	c = NewSessionChooser(h, 24, 80)
	skeys(c, "k", "x", "y")
	if !strings.Contains(plain(c.View()), "kill: terminate failed") {
		t.Fatalf("host refusal:\n%s", plain(c.View()))
	}
}

// TestSessionChooserNewOpensDialog: n opens the new-session dialog over
// the chooser; Esc there goes back to the list, a completed dialog closes
// the chooser with its request.
func TestSessionChooserNewOpensDialog(t *testing.T) {
	dh := newHost(t)
	p := &sessionProgram{c: NewSessionChooser(sessionHost(), 40, 100), newDialog: func(rows, cols int) *Dialog {
		return NewSessionDialogModel(context.Background(), dh, rows, cols, nil)
	}}
	// feed delivers msgs and what their commands lead to; spinner ticks
	// and the chooser's own ticks are dropped.
	var feed func(msgs ...tea.Msg)
	feed = func(msgs ...tea.Msg) {
		for _, m := range msgs {
			switch m.(type) {
			case nil, tea.QuitMsg, spinner.TickMsg, tickMsg:
				continue
			case tea.BatchMsg:
				for _, c := range m.(tea.BatchMsg) {
					if c != nil {
						feed(c())
					}
				}
				continue
			}
			if _, cmd := p.Update(m); cmd != nil {
				feed(cmd())
			}
		}
	}
	feed(runes("n"))
	if p.d == nil || !strings.Contains(plain(p.View()), "New session") {
		t.Fatalf("n:\n%s", plain(p.View()))
	}
	feed(tea.KeyMsg{Type: tea.KeyEsc})
	if p.d != nil || p.c.Done || !strings.Contains(plain(p.View()), "NAME") {
		t.Fatalf("esc in the dialog:\n%s", plain(p.View()))
	}
	feed(runes("n"))
	for _, r := range "work" {
		feed(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	feed(tea.KeyMsg{Type: tea.KeyCtrlS})
	if !p.c.Done || p.New == nil || p.New.Name != "work" || p.New.ProfileName != "dev" {
		t.Fatalf("done %v request %+v", p.c.Done, p.New)
	}
}

// TestNewSessionDialog: the new-session dialog asks for a name, the
// first window's profile and its repository rows, nothing else; a bad or
// taken name is marked on its row; the request carries the profile with
// the rows' repositories, checked by lux's dry run.
func TestNewSessionDialog(t *testing.T) {
	h := newHost(t)
	var checked []string
	taken := errors.New("session work exists")
	d := NewSessionDialogModel(context.Background(), h, 40, 100, func(_ context.Context, name string) error {
		checked = append(checked, name)
		if name == "work" {
			return taken
		}
		return nil
	})
	drain(d, d.Init())
	if want := []stop{stName, stProfile, repoURL(0), repoBranch(0), stAddRepo, stCreate, stCancel}; !reflect.DeepEqual(d.stops(), want) {
		t.Fatalf("stops %v", d.stops())
	}
	out := plain(d.View())
	for _, want := range []string{"New session", "Session name", "Profile", "Repository", "Branch",
		"https://example.com/app.git", "+ add repository"} {
		if !strings.Contains(out, want) {
			t.Errorf("view lacks %q:\n%s", want, out)
		}
	}
	for _, not := range []string{"Image", "Pool", "Warm spare", "Save as profile"} {
		if strings.Contains(out, not) {
			t.Errorf("view has %q:\n%s", not, out)
		}
	}

	press(d, "ctrl+s")
	if d.Done || d.errs[stName] == "" || len(h.validated) != 0 {
		t.Fatalf("no name: done %v errs %v", d.Done, d.errs)
	}
	press(d, "b a d/")
	press(d, "ctrl+s")
	if d.Done || d.errs[stName] == "" {
		t.Fatal("a name with a slash was accepted")
	}
	press(d, "ctrl+u", "work", "ctrl+s")
	if d.Done || d.errs[stName] != taken.Error() || d.focus != stName {
		t.Fatalf("taken name: done %v errs %v focus %v", d.Done, d.errs, d.focus)
	}
	press(d, "ctrl+u", "api-2")
	to(t, d, stProfile)
	press(d, "enter", "ops", "enter")
	to(t, d, repoURL(0))
	press(d, "https://example.com/ops.git", "tab", "release")
	press(d, "ctrl+s")
	if !d.Done || d.SessionRequest == nil || d.Request != nil {
		t.Fatalf("done %v session %+v window %+v status %q errs %v", d.Done, d.SessionRequest, d.Request, d.status, d.errs)
	}
	r := d.SessionRequest
	if r.Name != "api-2" || r.ProfileName != "ops" || r.Profile.Image != "alpine:3" || r.Profile.CPUs != 2 ||
		r.Profile.Repository != "https://example.com/ops.git" || r.Profile.Branch != "release" ||
		!reflect.DeepEqual(r.Profile.Secrets, []string{"OPS_KEY"}) {
		t.Fatalf("request %+v", r)
	}
	if !reflect.DeepEqual(checked, []string{"work", "api-2"}) || len(h.validated) != 1 {
		t.Fatalf("checked %v, %d dry runs", checked, len(h.validated))
	}

	// Esc: nothing.
	d = NewSessionDialogModel(context.Background(), h, 40, 100, nil)
	press(d, "x", "esc")
	if !d.Done || d.SessionRequest != nil {
		t.Fatal("esc")
	}
}
