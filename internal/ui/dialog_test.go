package ui

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/marcioapm/lux/api"

	"github.com/marcioapm/lux-mux/internal/config"
)

const dialogCfg = `
default_profile = "dev"
[profiles.dev]
image = "ubuntu:24.04"
pool = "m"
size = "m-2"
repository = "https://example.com/app.git"
branch = "main"
secrets = ["API_TOKEN"]
[profiles.ops]
image = "alpine:3"
pool = "plain"
cpus = 2
memory = "4Gi"
secrets = ["OPS_KEY"]
`

type fakeDialogHost struct {
	cfg      *config.Config
	pools    []api.Pool
	poolsErr error
	ans      *api.SubmitAnswer
	valErr   error
	// validated are the profiles the dry run was asked about.
	validated []config.Profile
}

func (f *fakeDialogHost) Config() *config.Config { return f.cfg }
func (f *fakeDialogHost) Recent() ([]string, []string) {
	return []string{"debian:12"}, []string{"https://example.com/recent.git"}
}
func (f *fakeDialogHost) Pools(context.Context) ([]api.Pool, error) { return f.pools, f.poolsErr }
func (f *fakeDialogHost) Validate(_ context.Context, _ string, p config.Profile) (*api.SubmitAnswer, error) {
	f.validated = append(f.validated, p)
	if f.valErr != nil {
		return nil, f.valErr
	}
	if f.ans != nil {
		return f.ans, nil
	}
	return &api.SubmitAnswer{Valid: true, WouldStartNow: true}, nil
}
func (f *fakeDialogHost) Background(rows, cols int) []string {
	return []string{"$ echo behind", "behind"}
}

func testPools() []api.Pool {
	yes := true
	return []api.Pool{
		{Name: "m", IsDefault: &yes, Sizes: []api.PoolSize{
			{Name: "m-1", CPUs: 1, Memory: 2 << 30, Disk: 10 << 30},
			{Name: "m-2", CPUs: 2, Memory: 4 << 30, Disk: 20 << 30},
			{Name: "m-4", CPUs: 4, Memory: 8 << 30, Disk: 40 << 30},
		}},
		{Name: "plain"},
		{Name: "busy"},
	}
}

func newHost(t *testing.T) *fakeDialogHost {
	t.Helper()
	cfg, err := config.Parse([]byte(dialogCfg))
	if err != nil {
		t.Fatal(err)
	}
	return &fakeDialogHost{cfg: cfg, pools: testPools()}
}

// open makes a dialog and runs its pool load.
func open(t *testing.T, h *fakeDialogHost, rows, cols int) *Dialog {
	t.Helper()
	d := NewDialogModel(context.Background(), h, rows, cols)
	drain(d, d.Init())
	return d
}

// drain runs cmd and what it leads to, feeding messages back to d; the
// spinner's ticks are dropped.
func drain(d *Dialog, cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	switch m := cmd().(type) {
	case tea.BatchMsg:
		for _, c := range m {
			drain(d, c)
		}
	case tea.QuitMsg, spinner.TickMsg, nil:
	default:
		_, next := d.Update(m)
		drain(d, next)
	}
}

// press types keys by name: "tab", "enter", "ctrl+s", "space", or text.
func press(d *Dialog, ks ...string) {
	for _, k := range ks {
		var msg tea.KeyMsg
		switch k {
		case "tab":
			msg = tea.KeyMsg{Type: tea.KeyTab}
		case "shift+tab":
			msg = tea.KeyMsg{Type: tea.KeyShiftTab}
		case "up":
			msg = tea.KeyMsg{Type: tea.KeyUp}
		case "down":
			msg = tea.KeyMsg{Type: tea.KeyDown}
		case "right":
			msg = tea.KeyMsg{Type: tea.KeyRight}
		case "left":
			msg = tea.KeyMsg{Type: tea.KeyLeft}
		case "enter":
			msg = tea.KeyMsg{Type: tea.KeyEnter}
		case "esc":
			msg = tea.KeyMsg{Type: tea.KeyEsc}
		case "space":
			msg = tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}}
		case "backspace":
			msg = tea.KeyMsg{Type: tea.KeyBackspace}
		case "ctrl+s":
			msg = tea.KeyMsg{Type: tea.KeyCtrlS}
		case "ctrl+u":
			msg = tea.KeyMsg{Type: tea.KeyCtrlU}
		default:
			for _, r := range k {
				_, cmd := d.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
				drain(d, cmd)
			}
			continue
		}
		_, cmd := d.Update(msg)
		drain(d, cmd)
	}
}

// to moves the focus to s with Tab, failing if it never gets there.
func to(t *testing.T, d *Dialog, s stop) {
	t.Helper()
	for range 20 {
		if d.focus == s {
			return
		}
		press(d, "tab")
	}
	t.Fatalf("never reached %v; at %v", s, d.focus)
}

func plain(s string) string { return ansi.Strip(s) }

func TestDialogStartsFromDefaultProfile(t *testing.T) {
	h := newHost(t)
	d := open(t, h, 30, 100)
	v := d.values()
	if v.Base != "dev" || v.Image != "ubuntu:24.04" || v.Pool != "m" || v.Size != "m-2" || v.Repository != "https://example.com/app.git" || v.Branch != "main" || v.WarmSpare {
		t.Fatalf("values %+v", v)
	}
	out := plain(d.View())
	for _, want := range []string{"New window", "Profile", "Image", "Pool", "Size", "m-2  2 cpus, 4Gi, 20Gi", "Repository",
		"Branch", "Secrets", "API_TOKEN", "Warm spare", "[ ]", "billed while it waits", "Save as profile", "don't save",
		"[ Create ]", "[ Cancel ]", "ctrl-s create", "behind"} {
		if !strings.Contains(out, want) {
			t.Errorf("view lacks %q:\n%s", want, out)
		}
	}
}

func TestDialogNavigationWraps(t *testing.T) {
	d := open(t, newHost(t), 30, 100)
	want := []stop{stImage, stPool, stSize, stRepo, stBranch, stSpare, stSaveAs, stCreate, stCancel, stProfile}
	for i, s := range want {
		press(d, "tab")
		if d.focus != s {
			t.Fatalf("tab %d: at %v, want %v", i+1, d.focus, s)
		}
	}
	press(d, "shift+tab")
	if d.focus != stCancel {
		t.Fatalf("shift-tab from the first field: %v", d.focus)
	}
	press(d, "up", "up")
	if d.focus != stSaveAs {
		t.Fatalf("up: %v", d.focus)
	}
	press(d, "down", "down", "down")
	if d.focus != stProfile {
		t.Fatalf("down wraps: %v", d.focus)
	}
	// Enter on a text field moves on.
	press(d, "tab", "enter")
	if d.focus != stPool {
		t.Fatalf("enter on image: %v", d.focus)
	}
	if !strings.Contains(plain(d.View()), "▸ Pool") {
		t.Fatalf("focused field not marked:\n%s", plain(d.View()))
	}
}

func TestDialogDropdownOpenFilterPick(t *testing.T) {
	d := open(t, newHost(t), 30, 100)
	to(t, d, stSize)
	press(d, "enter")
	if !d.size.open {
		t.Fatal("enter did not open the list")
	}
	out := plain(d.View())
	if !strings.Contains(out, "m-1  1 cpus") || !strings.Contains(out, "m-4  4 cpus") || !strings.Contains(out, "filter:") {
		t.Fatalf("list not shown:\n%s", out)
	}
	press(d, "m", "-", "4")
	if v := d.size.visible(); len(v) != 1 || v[0].value != "m-4" {
		t.Fatalf("filter m-4: %+v", v)
	}
	press(d, "enter")
	if d.size.open || d.size.value != "m-4" || d.values().Size != "m-4" {
		t.Fatalf("pick: open %v value %q", d.size.open, d.size.value)
	}
	// Esc closes the list without picking, and leaves the dialog open.
	press(d, "enter", "down", "down", "esc")
	if d.size.open || d.size.value != "m-4" || d.Done {
		t.Fatalf("esc on the list: open %v value %q done %v", d.size.open, d.size.value, d.Done)
	}
	// ↑/↓ choose in the list.
	press(d, "enter", "up", "enter")
	if d.size.value != "m-2" {
		t.Fatalf("up then enter: %q", d.size.value)
	}
}

func TestDialogProfileChangeRefillsUneditedFields(t *testing.T) {
	d := open(t, newHost(t), 30, 100)
	to(t, d, stRepo)
	press(d, "ctrl+u", "https://example.com/mine.git")
	to(t, d, stProfile)
	press(d, "enter", "o", "p", "s", "enter")
	v := d.values()
	if v.Base != "ops" {
		t.Fatalf("profile %q", v.Base)
	}
	if v.Repository != "https://example.com/mine.git" {
		t.Fatalf("edited repository replaced: %q", v.Repository)
	}
	if v.Image != "alpine:3" || v.Pool != "plain" || v.CPUs != "2" || v.Memory != "4Gi" || v.Branch != "" {
		t.Fatalf("unedited fields not refilled: %+v", v)
	}
	if out := plain(d.View()); !strings.Contains(out, "OPS_KEY") || strings.Contains(out, "API_TOKEN") {
		t.Fatalf("secrets line follows the profile:\n%s", out)
	}
	// Back to dev: the profile itself was not an edit.
	press(d, "enter", "d", "e", "v", "enter")
	if v := d.values(); v.Image != "ubuntu:24.04" || v.Pool != "m" || v.Repository != "https://example.com/mine.git" {
		t.Fatalf("back to dev: %+v", v)
	}
}

func TestDialogPoolChangeSwitchesSizeShape(t *testing.T) {
	d := open(t, newHost(t), 30, 100)
	if !slices.Contains(d.stops(), stSize) || slices.Contains(d.stops(), stCPUs) {
		t.Fatalf("shaped pool m: stops %v", d.stops())
	}
	to(t, d, stPool)
	press(d, "enter", "p", "l", "a", "enter")
	if d.values().Pool != "plain" {
		t.Fatalf("pool %q", d.values().Pool)
	}
	if slices.Contains(d.stops(), stSize) || !slices.Contains(d.stops(), stCPUs) {
		t.Fatalf("unshaped pool: stops %v", d.stops())
	}
	out := plain(d.View())
	if !strings.Contains(out, "cpus") || !strings.Contains(out, "memory") || !strings.Contains(out, "disk") || strings.Contains(out, "2 cpus, 4Gi") {
		t.Fatalf("size row not cpus/memory/disk:\n%s", out)
	}
	press(d, "tab")
	if d.focus != stCPUs {
		t.Fatalf("after pool: %v", d.focus)
	}
	press(d, "3", "tab", "6", "G", "i", "tab", "3", "0", "G", "i", "tab")
	if v := d.values(); v.CPUs != "3" || v.Memory != "6Gi" || v.Disk != "30Gi" || d.focus != stRepo {
		t.Fatalf("cpus/memory/disk %+v at %v", v, d.focus)
	}
	// Back to the default pool: a size list again.
	to(t, d, stPool)
	press(d, "enter", "t", "e", "n", "enter")
	if !slices.Contains(d.stops(), stSize) || !strings.Contains(plain(d.View()), "Size") {
		t.Fatalf("tenant default pool is shaped: %v", d.stops())
	}
}

func TestDialogCheckboxToggle(t *testing.T) {
	d := open(t, newHost(t), 30, 100)
	to(t, d, stSpare)
	press(d, "space")
	if !d.spare || !strings.Contains(plain(d.View()), "[x]") {
		t.Fatal("space did not check the box")
	}
	press(d, "space")
	if d.spare {
		t.Fatal("space did not uncheck the box")
	}
	press(d, "space", "ctrl+s")
	if d.Request == nil || !d.Request.Profile.WarmSpare {
		t.Fatalf("request %+v", d.Request)
	}
}

func TestDialogInlineValidationErrors(t *testing.T) {
	h := newHost(t)
	d := open(t, h, 30, 100)
	to(t, d, stPool)
	press(d, "enter", "p", "l", "a", "enter", "tab", "x", "y")
	to(t, d, stSaveAs)
	press(d, "bad name", "ctrl+s")
	if d.Done || len(h.validated) != 0 {
		t.Fatal("invalid values went to the dry run")
	}
	if !strings.Contains(d.errs[stCPUs], "not a positive number") || !strings.Contains(d.errs[stSaveAs], "save as") {
		t.Fatalf("errors %v", d.errs)
	}
	if d.focus != stCPUs {
		t.Fatalf("focus not on the first bad field: %v", d.focus)
	}
	out := plain(d.View())
	if !strings.Contains(out, "✗ cpus: \"xy\" is not a positive number") || !strings.Contains(out, "✗ save as:") {
		t.Fatalf("inline errors not shown:\n%s", out)
	}
	// Fixing a field clears its error; the typed values are kept.
	press(d, "ctrl+u", "2")
	if _, ok := d.errs[stCPUs]; ok {
		t.Fatal("error kept after the field changed")
	}
	if d.values().SaveAs != "bad name" {
		t.Fatal("lost what was typed")
	}
	// An empty image.
	to(t, d, stImage)
	press(d, "ctrl+u", "ctrl+s")
	if !strings.Contains(d.errs[stImage], "image is required") {
		t.Fatalf("errors %v", d.errs)
	}
}

func TestDialogRefusalKeepsValuesAndMarksField(t *testing.T) {
	h := newHost(t)
	h.valErr = &api.APIError{Status: 422, Code: "invalid_spec", Message: "image.ref: no such image"}
	d := open(t, h, 30, 100)
	to(t, d, stBranch)
	press(d, "ctrl+u", "feature")
	press(d, "ctrl+s")
	if d.Done || d.Request != nil || len(h.validated) != 1 {
		t.Fatalf("done %v request %v validated %d", d.Done, d.Request, len(h.validated))
	}
	if d.errs[stImage] == "" || d.errs[stBranch] != "" {
		t.Fatalf("marked %v", d.errs)
	}
	out := plain(d.View())
	if !strings.Contains(out, "lux refuses it: image.ref: no such image (invalid_spec)") {
		t.Fatalf("status:\n%s", out)
	}
	if v := d.values(); v.Branch != "feature" || v.Image != "ubuntu:24.04" {
		t.Fatalf("values lost: %+v", v)
	}
	// Fixed, it is checked again and created.
	h.valErr = nil
	to(t, d, stImage)
	press(d, "ctrl+u", "ubuntu:22.04", "ctrl+s")
	if !d.Done || d.Request == nil || d.Request.Profile.Image != "ubuntu:22.04" || d.Request.Profile.Branch != "feature" {
		t.Fatalf("request %+v", d.Request)
	}
}

func TestDialogWouldNotStartNeedsSecondCreate(t *testing.T) {
	h := newHost(t)
	h.ans = &api.SubmitAnswer{Valid: true, Blockers: []api.AdmissionBlocker{{Code: "not_enough_spare", Hosts: 2}}}
	d := open(t, h, 30, 100)
	press(d, "ctrl+s")
	if d.Done {
		t.Fatal("created without the second confirm")
	}
	out := plain(d.View())
	if !strings.Contains(out, "would not start now") || !strings.Contains(d.status, "not_enough_spare (2 hosts)") {
		t.Fatalf("blockers not shown:\n%s", out)
	}
	if d.focus != stCreate {
		t.Fatalf("focus %v", d.focus)
	}
	press(d, "enter")
	if !d.Done || d.Request == nil || d.Request.ProfileName != "dev" || len(h.validated) != 1 {
		t.Fatalf("second create: done %v req %+v validated %d", d.Done, d.Request, len(h.validated))
	}

	// An edit after the warning asks lux again.
	h.validated = nil
	d = open(t, h, 30, 100)
	press(d, "ctrl+s")
	to(t, d, stBranch)
	press(d, "x", "ctrl+s")
	if d.Done || len(h.validated) != 2 {
		t.Fatalf("an edited form was created on the old answer: done %v, validated %d", d.Done, len(h.validated))
	}
}

func TestDialogEscCancels(t *testing.T) {
	d := open(t, newHost(t), 30, 100)
	to(t, d, stRepo)
	press(d, "x", "esc")
	if !d.Done || d.Request != nil {
		t.Fatalf("esc: done %v request %v", d.Done, d.Request)
	}
	d = open(t, newHost(t), 30, 100)
	to(t, d, stCancel)
	press(d, "enter")
	if !d.Done || d.Request != nil {
		t.Fatal("[Cancel]")
	}
}

func TestDialogCtrlSSubmits(t *testing.T) {
	h := newHost(t)
	d := open(t, h, 30, 100)
	to(t, d, stRepo)
	press(d, "ctrl+u", "https://example.com/other.git", "tab", "ctrl+u", "dev-branch")
	to(t, d, stSaveAs)
	press(d, "mine", "ctrl+s")
	if !d.Done || d.Request == nil {
		t.Fatal("ctrl-s did not create")
	}
	r := d.Request
	if r.ProfileName != "dev" || r.SaveAs != "mine" || r.Profile.Repository != "https://example.com/other.git" ||
		r.Profile.Branch != "dev-branch" || r.Profile.Size != "m-2" || r.Profile.Pool != "m" || r.Profile.Image != "ubuntu:24.04" {
		t.Fatalf("request %+v", r)
	}
	if len(h.validated) != 1 || h.validated[0].Repository != r.Profile.Repository {
		t.Fatal("not dry-run first")
	}
	// Enter on [Create] does the same.
	d = open(t, newHost(t), 30, 100)
	to(t, d, stCreate)
	press(d, "enter")
	if !d.Done || d.Request == nil {
		t.Fatal("enter on [Create]")
	}
}

func TestDialogSuggestions(t *testing.T) {
	d := open(t, newHost(t), 30, 100)
	to(t, d, stImage)
	press(d, "ctrl+u", "deb", "tab")
	if got := d.values().Image; got != "debian:12" || d.focus != stImage {
		t.Fatalf("tab accepts the suggestion: %q at %v", got, d.focus)
	}
	press(d, "tab")
	if d.focus != stPool {
		t.Fatalf("tab with nothing to complete moves on: %v", d.focus)
	}
	to(t, d, stRepo)
	press(d, "ctrl+u", "https://example.com/r", "right")
	if got := d.values().Repository; got != "https://example.com/recent.git" {
		t.Fatalf("→ accepts the suggestion: %q", got)
	}
}

func TestDialogPoolsFailFreeText(t *testing.T) {
	h := newHost(t)
	h.poolsErr = errors.New("connection refused")
	d := open(t, h, 30, 100)
	out := plain(d.View())
	if !strings.Contains(out, "pools unavailable: connection refused") {
		t.Fatalf("status:\n%s", out)
	}
	to(t, d, stPool)
	press(d, "ctrl+u", "gpu")
	if d.values().Pool != "gpu" {
		t.Fatalf("pool %q", d.values().Pool)
	}
	// No sizes known: cpus, memory and disk.
	if !slices.Contains(d.stops(), stCPUs) {
		t.Fatal(d.stops())
	}
}

func TestDialogPoolsLoading(t *testing.T) {
	d := NewDialogModel(context.Background(), newHost(t), 30, 100)
	if !strings.Contains(plain(d.View()), "loading…") {
		t.Fatalf("no loading marker:\n%s", plain(d.View()))
	}
	press(d, "ctrl+s")
	if d.Done {
		t.Fatal("submitted before the pools were known")
	}
}

// boxLeft is the column of the box's top-left corner in a view.
func boxLeft(t *testing.T, view string) (col, rows int) {
	t.Helper()
	lines := strings.Split(plain(view), "\n")
	for _, l := range lines {
		if i := strings.Index(l, "╭"); i >= 0 {
			return ansi.StringWidth(l[:i]), len(lines)
		}
	}
	t.Fatalf("no box:\n%s", plain(view))
	return 0, 0
}

func TestDialogCenteredAndTooSmall(t *testing.T) {
	for _, sz := range [][2]int{{24, 80}, {50, 200}} {
		d := open(t, newHost(t), sz[0], sz[1])
		col, rows := boxLeft(t, d.View())
		w := boxWidth(sz[1])
		if want := (sz[1] - w) / 2; col < want-1 || col > want+1 || rows != sz[0] {
			t.Fatalf("%dx%d: box at column %d (want %d), %d rows", sz[1], sz[0], col, want, rows)
		}
		for _, l := range strings.Split(plain(d.View()), "\n") {
			if ansi.StringWidth(l) > sz[1] {
				t.Fatalf("%dx%d: line wider than the terminal: %q", sz[1], sz[0], l)
			}
		}
	}
	d := open(t, newHost(t), 10, 30)
	if out := plain(d.View()); !strings.Contains(out, "too small") || strings.Contains(out, "╭") {
		t.Fatalf("30x10:\n%s", out)
	}
	// A resize re-centers.
	d.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	if col, _ := boxLeft(t, d.View()); col != (120-boxWidth(120))/2 {
		t.Fatalf("after resize: column %d", col)
	}
}

func TestDialogScrollsOnShortTerminal(t *testing.T) {
	d := open(t, newHost(t), 14, 80)
	if !strings.Contains(plain(d.View()), "Profile") {
		t.Fatalf("top:\n%s", plain(d.View()))
	}
	to(t, d, stCreate)
	out := plain(d.View())
	if !strings.Contains(out, "[ Create ]") || strings.Contains(out, "Profile") {
		t.Fatalf("not scrolled to the buttons:\n%s", out)
	}
	if n := len(strings.Split(out, "\n")); n != 14 {
		t.Fatalf("%d rows", n)
	}
}
