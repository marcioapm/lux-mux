package ui

import (
	"context"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/marcioapm/lux/api"

	"github.com/marcioapm/lux-mux/internal/config"
	"github.com/marcioapm/lux-mux/internal/mux"
)

// TestUnknownCSIProgramAcknowledgesInput: an unknown CSI parsed by a real
// bubbletea program is acknowledged as input; the closing key is written
// only after that acknowledgement, as the one-key Reader does.
func TestUnknownCSIProgramAcknowledgesInput(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	in, out := io.Pipe()
	defer in.Close()
	defer out.Close()
	ack := make(chan struct{}, 1)
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		if _, err := out.Write([]byte("\x1b[999~")); err != nil {
			return
		}
		select {
		case <-ack:
			_, _ = out.Write([]byte("q"))
		case <-ctx.Done():
		}
	}()
	h := NewHelp(helpCfg(t, ""), nil, 30, 100)
	model := releasing{h, func() { out.Close() }, func() {
		select {
		case ack <- struct{}{}:
		default:
		}
	}}
	p := tea.NewProgram(model, tea.WithInput(in), tea.WithOutput(io.Discard), tea.WithoutRenderer(), tea.WithoutSignalHandler(), tea.WithContext(ctx))
	if _, err := p.Run(); err != nil {
		t.Fatal(err)
	}
	if !h.Done {
		t.Fatal("unknown CSI prevented the closing key")
	}
	<-writerDone
}

type fakeHost struct {
	wins    []mux.WindowInfo
	renamed map[int]string
	killed  []int
}

func (f *fakeHost) Windows() []mux.WindowInfo { return append([]mux.WindowInfo{}, f.wins...) }
func (f *fakeHost) Preview(i, rows, cols int) []string {
	return []string{"preview of " + f.wins[indexOf(f.wins, i)].Name}
}
func (f *fakeHost) Rename(i int, name string) {
	f.renamed[i] = name
	f.wins[indexOf(f.wins, i)].Name = name
}
func (f *fakeHost) Kill(i int) {
	f.killed = append(f.killed, i)
	j := indexOf(f.wins, i)
	f.wins = append(f.wins[:j], f.wins[j+1:]...)
}

func indexOf(ws []mux.WindowInfo, i int) int {
	for j, w := range ws {
		if w.Index == i {
			return j
		}
	}
	return -1
}

func host() *fakeHost {
	t0 := time.Now().Add(-time.Hour)
	return &fakeHost{renamed: map[int]string{}, wins: []mux.WindowInfo{
		{Index: 0, Name: "editor", State: "running", Image: "ubuntu:24.04", Repo: "app@main", Created: t0},
		{Index: 1, Name: "logs", State: "running", Image: "alpine", Created: t0, Active: true},
		{Index: 2, Name: "build", State: "reconnecting", Image: "ubuntu:24.04", Repo: "lib", Created: t0},
	}}
}

func keys(c *Chooser, ks ...string) {
	for _, k := range ks {
		c.Key(k)
	}
}

func TestChooserMovesAndSelects(t *testing.T) {
	h := host()
	c := NewChooser(h, 24, 100)
	if w, _ := c.Selected(); w.Index != 1 {
		t.Fatalf("starts on the active window, got %d", w.Index)
	}
	keys(c, "down", "down", "down")
	if w, _ := c.Selected(); w.Index != 2 {
		t.Fatalf("cursor stops at the end: %d", w.Index)
	}
	keys(c, "k", "k", "k", "j")
	if w, _ := c.Selected(); w.Index != 1 {
		t.Fatalf("k/j: %d", w.Index)
	}
	if !strings.Contains(c.View(), "preview of logs") {
		t.Fatalf("preview follows the cursor:\n%s", c.View())
	}
	keys(c, "up")
	if !strings.Contains(c.View(), "preview of editor") {
		t.Fatal("preview did not follow")
	}
	keys(c, "enter")
	if !c.Done || c.Chosen != 0 {
		t.Fatalf("enter: done %v chosen %d", c.Done, c.Chosen)
	}
}

func TestChooserFilter(t *testing.T) {
	c := NewChooser(host(), 24, 100)
	keys(c, "/", "u", "b", "u")
	vis := c.Visible()
	if len(vis) != 2 || vis[0].Name != "editor" || vis[1].Name != "build" {
		t.Fatalf("filter ubu: %+v", vis)
	}
	keys(c, "enter", "down", "enter")
	if c.Chosen != 2 {
		t.Fatalf("chose %d", c.Chosen)
	}
	c = NewChooser(host(), 24, 100)
	keys(c, "/", "z", "z")
	if len(c.Visible()) != 0 {
		t.Fatal("no match")
	}
	keys(c, "enter", "enter")
	if !c.Done || c.Chosen != -1 {
		t.Fatal("enter with nothing visible chooses nothing")
	}
	c = NewChooser(host(), 24, 100)
	keys(c, "/", "l", "o", "g", "esc")
	if len(c.Visible()) != 3 {
		t.Fatal("esc clears the filter")
	}
	keys(c, "/", "r", "e", "c", "o", "n", "backspace")
	if v := c.Visible(); len(v) != 1 || v[0].Name != "build" {
		t.Fatalf("state filter: %+v", v)
	}
	// Case-insensitive, and the repository is searched too.
	c = NewChooser(host(), 24, 100)
	keys(c, "/", "E", "D", "I", "T")
	if v := c.Visible(); len(v) != 1 || v[0].Name != "editor" {
		t.Fatalf("filter EDIT: %+v", v)
	}
	c = NewChooser(host(), 24, 100)
	keys(c, "/", "l", "i", "b")
	if v := c.Visible(); len(v) != 1 || v[0].Name != "build" {
		t.Fatalf("filter by repository: %+v", v)
	}
}

func TestChooserRenameKillClose(t *testing.T) {
	h := host()
	c := NewChooser(h, 24, 100)
	keys(c, ",", "backspace", "backspace", "backspace", "backspace", "t", "a", "i", "l", "enter")
	if h.renamed[1] != "tail" {
		t.Fatalf("rename: %v", h.renamed)
	}
	keys(c, "x", "n")
	if len(h.killed) != 0 {
		t.Fatal("x n killed")
	}
	keys(c, "x")
	if !strings.Contains(c.View(), "kill window 1 (tail)? (y/N)") {
		t.Fatalf("confirm:\n%s", c.View())
	}
	keys(c, "y")
	if !reflect.DeepEqual(h.killed, []int{1}) || len(c.Visible()) != 2 {
		t.Fatalf("killed %v", h.killed)
	}
	keys(c, "q")
	if !c.Done || c.Chosen != -1 {
		t.Fatal("q closes without choosing")
	}
	c = NewChooser(host(), 24, 100)
	keys(c, "2", "esc")
	if !c.Done || c.Chosen != -1 {
		t.Fatal("esc")
	}
}

func TestDialogValuesProfile(t *testing.T) {
	base := config.Profile{Image: "ubuntu:24.04", Pool: "m", Size: "m-2-arm64", Repository: "https://e.com/a.git",
		Branch: "main", GitCredential: "TOK", Shell: []string{"zsh"}, Secrets: []string{"X"}, WarmSpare: true}
	v := ValuesFrom("dev", base)
	if v.WarmSpare {
		t.Fatal("the warm spare box starts off")
	}
	p, err := v.Profile(base, true)
	if err != nil {
		t.Fatal(err)
	}
	want := base
	want.WarmSpare = false
	if !reflect.DeepEqual(p, want) {
		t.Fatalf("unchanged dialog:\n%+v\nwant\n%+v", p, want)
	}
	// Another repository, an unshaped pool with cpus and memory, a spare.
	v.Repos, v.Pool, v.CPUs, v.Memory, v.WarmSpare = []RepoRow{{URL: "https://e.com/b.git"}}, "plain", "4", "8Gi", true
	p, err = v.Profile(base, false)
	if err != nil {
		t.Fatal(err)
	}
	if p.Size != "" || p.CPUs != 4 || p.Memory != "8Gi" || p.GitCredential != "" || p.Branch != "" || !p.WarmSpare || p.Pool != "plain" {
		t.Fatalf("%+v", p)
	}
	sp, err := config.RunSpec(p, config.Target{Session: "s", Profile: "dev"}, nil)
	shell, _, _, ok := config.ParseWindowCommand(sp.Workload.Command)
	if err != nil || sp.Resources.CPUs != 4 || sp.Resources.Memory != 8<<30 || sp.Git.Repositories[0].URL != "https://e.com/b.git" ||
		sp.Git.Repositories[0].Credential != "" || !ok || !reflect.DeepEqual(shell, []string{"zsh"}) {
		t.Fatalf("spec %+v %v", sp, err)
	}
	v.CPUs = "lots"
	if _, err := v.Profile(base, false); err == nil {
		t.Fatal("bad cpus accepted")
	}
	for _, c := range []string{"0", "-2"} {
		v.CPUs = c
		if _, err := v.Profile(base, false); err == nil {
			t.Fatalf("cpus %s accepted", c)
		}
	}
	v.CPUs, v.Disk = "2", " 20Gi "
	if p, err := v.Profile(base, false); err != nil || p.Disk != "20Gi" {
		t.Fatalf("disk %q, %v", p.Disk, err)
	}
	if p, err := v.Profile(base, true); err != nil || p.Disk != "" {
		t.Fatalf("disk on a shaped pool %q, %v", p.Disk, err)
	}
	v.CPUs, v.Image = "", ""
	if _, err := v.Profile(base, false); err == nil {
		t.Fatal("empty image accepted")
	}
	v.Image, v.SaveAs = "x", "bad name"
	if _, err := v.Profile(base, false); err == nil {
		t.Fatal("bad save-as accepted")
	}
}

func TestShapedAndBlockers(t *testing.T) {
	yes := true
	pools := []api.Pool{{Name: "plain"}, {Name: "m", IsDefault: &yes, Sizes: []api.PoolSize{{Name: "m-1-arm64"}, {Name: "m-2-arm64"}}}}
	if s, _ := Shaped(pools, "plain"); s {
		t.Fatal("plain")
	}
	if s, sizes := Shaped(pools, ""); !s || len(sizes) != 2 {
		t.Fatal("default pool is m")
	}
	b := Blockers(&api.SubmitAnswer{Blockers: []api.AdmissionBlocker{{Code: "not_enough_spare", Hosts: 2}, {Code: "constraint", Hosts: 1, Reason: "requires arch"}}})
	if !reflect.DeepEqual(b, []string{"not_enough_spare (2 hosts)", "constraint (1 hosts): requires arch"}) {
		t.Fatal(b)
	}
}

// TestChooserMarksOtherEndpoint: a window on a lux other than the lux
// CLI's is marked @ in the list, and its lux and tenant head the preview.
func TestChooserMarksOtherEndpoint(t *testing.T) {
	h := host()
	h.wins[2].OtherEndpoint, h.wins[2].Endpoint = true, "lux2.example.com · tenant example"
	h.wins[0].Endpoint = "lux.example.com"
	c := NewChooser(h, 24, 120)
	out := ansi.Strip(c.View())
	if !strings.Contains(out, "2@") || strings.Contains(out, "0@") || strings.Contains(out, "1*@") {
		t.Fatalf("marks:\n%s", out)
	}
	if strings.Contains(out, "@ lux:") {
		t.Fatalf("lux line for a default-endpoint window:\n%s", out)
	}
	keys(c, "down")
	out = ansi.Strip(c.View())
	if !strings.Contains(out, "@ lux: lux2.example.com · tenant example") || !strings.Contains(out, "preview of build") {
		t.Fatalf("selected remote window:\n%s", out)
	}
}
