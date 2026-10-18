package mux

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/marcioapm/lux/api"

	"github.com/marcioapm/lux-mux/internal/config"
	"github.com/marcioapm/lux-mux/internal/fakelux"
	"github.com/marcioapm/lux-mux/internal/state"
)

const cfgNamed = `
default_profile = "dev"
[profiles.dev]
image = "alpine"
`

// runName is the name lux has for Run id.
func runName(h *harness, id string) string { return h.lux.Run(id).Name }

// patches counts the renames sent to l, taken or not.
func patches(l *fakelux.Server) int {
	n := 0
	for _, r := range l.Requests() {
		if strings.HasPrefix(r, http.MethodPatch+" ") {
			n++
		}
	}
	return n
}

func (h *harness) windowName(i int) string {
	h.m.mu.Lock()
	defer h.m.mu.Unlock()
	return h.m.windows[i].Name
}

// TestRunNameAtSubmit: a window's Run is submitted named
// <session>/<window>, with no rename after.
func TestRunNameAtSubmit(t *testing.T) {
	h := newHarness(t, cfgNamed, nil)
	id := h.live(0)
	if got := runName(h, id); got != "s/dev" {
		t.Fatalf("first window's Run is named %q", got)
	}
	h.type_("\x02c")
	id1 := h.live(1)
	if got, want := runName(h, id1), "s/"+h.windowName(1); got != want {
		t.Fatalf("second window's Run is named %q, want %q", got, want)
	}
	if n := patches(h.lux); n != 0 {
		t.Fatalf("%d renames of Runs submitted with their names", n)
	}
	h.detach()
}

// TestRunNameSpare: a waiting spare is named <session>/spare; the window
// that claims it renames it to the window's name.
func TestRunNameSpare(t *testing.T) {
	h := newHarness(t, cfgSpare, nil)
	h.live(0)
	var spare string
	h.waitFor("spare", func() bool { spare = spareOf(h); return spare != "" })
	if got := runName(h, spare); got != "s/spare" {
		t.Fatalf("waiting spare is named %q", got)
	}
	h.type_("\x02c")
	h.waitFor("claimed", func() bool { return h.window(1) == spare })
	want := "s/" + h.windowName(1)
	h.waitFor("claimed spare renamed", func() bool { return runName(h, spare) == want })
	var next string
	h.waitFor("replacement spare", func() bool { next = spareOf(h); return next != "" && next != spare })
	if got := runName(h, next); got != "s/spare" {
		t.Fatalf("replacement spare is named %q", got)
	}
	h.detach()
}

// TestRunNameWindowRename: Mux.Rename and C-b , rename the window's Run;
// the env file gets the window name as before.
func TestRunNameWindowRename(t *testing.T) {
	h := newHarness(t, cfgNamed, nil)
	id := h.live(0)
	h.m.Rename(0, "api")
	h.waitFor("Run renamed", func() bool { return runName(h, id) == "s/api" })
	h.type_("\x02,")
	h.waitScreen("rename prompt", h.contains("rename window:"))
	h.type_("\x15web/1\r")
	h.waitFor("Run renamed by C-b ,", func() bool { return runName(h, id) == "s/web/1" })
	h.m.Rename(0, "tab\there")
	h.waitFor("control character sent as a space", func() bool { return runName(h, id) == "s/tab here" })
	if got := h.windowName(0); got != "tab\there" {
		t.Fatalf("window named %q", got)
	}
	h.detach()
}

// TestRunNameSessionRename: renaming the session renames every window's
// Run; their session label stays the original name.
func TestRunNameSessionRename(t *testing.T) {
	h := newHarness(t, cfgNamed, nil)
	a := h.live(0)
	h.type_("\x02c")
	b := h.live(1)
	h.m.Rename(1, "web")
	h.waitFor("window renamed", func() bool { return runName(h, b) == "s/web" })
	if err := h.m.RenameSession("s", "renamed"); err != nil {
		t.Fatal(err)
	}
	h.waitFor("both Runs renamed", func() bool { return runName(h, a) == "renamed/dev" && runName(h, b) == "renamed/web" })
	for _, id := range []string{a, b} {
		if l := h.lux.Run(id).Labels[config.LabelSession]; l != "s" {
			t.Fatalf("%s labelled %q", id, l)
		}
	}
	h.type_("\x02c")
	c := h.live(2)
	if got, want := runName(h, c), "renamed/"+h.windowName(2); got != want {
		t.Fatalf("new window's Run is named %q, want %q", got, want)
	}
	h.detach()
}

// TestRunNameSessionRenamedDetached: a session renamed while not
// attached (from another one) has its Runs renamed at its next attach.
func TestRunNameSessionRenamedDetached(t *testing.T) {
	h := newMulti(t)
	idB := h.make("b", "B-SCREEN")
	if got := h.lux.Run(idB).Name; got != "b/default" {
		t.Fatalf("b's Run is named %q", got)
	}
	h.attach("a")
	h.on("a")
	if err := h.con.Current().RenameSession("b", "c"); err != nil {
		t.Fatal(err)
	}
	if got := h.lux.Run(idB).Name; got != "b/default" {
		t.Fatalf("renamed before c is attached: %q", got)
	}
	h.detach()
	h.attach("c")
	h.on("c")
	h.waitFor("c's Run renamed", func() bool { return h.lux.Run(idB).Name == "c/default" })
	h.waitFor("rename recorded done", func() bool {
		s, ok, _ := state.Load(h.dir, "c")
		return ok && len(s.Windows) == 1 && !s.Windows[0].RenameRun
	})
	h.detach()
}

// TestRunNameOldLux: a lux without the rename API is asked once: the
// window still renames here and in its env file, one notice says so,
// and neither later renames nor reattaches ask again.
func TestRunNameOldLux(t *testing.T) {
	h, dir := shellHarness(t, hooksConfig)
	id := h.live(0)
	h.lux.FailRenames(http.StatusMethodNotAllowed)
	const notice = "cannot rename Runs"
	h.m.Rename(0, "one")
	h.waitScreen("the notice", h.contains(notice))
	h.waitFor("env file renamed", func() bool { return strings.Contains(envFile(t, dir, id), "export LUXMUX_WINDOW='one'") })
	h.waitScreen("the notice gone", func(s string) bool { return !strings.Contains(s, notice) })
	shown := strings.Count(string(h.term.rawBytes()), notice)
	h.m.Rename(0, "two")
	if err := h.m.RenameSession("s", "renamed"); err != nil {
		t.Fatal(err)
	}
	h.waitFor("env file renamed again", func() bool { return strings.Contains(envFile(t, dir, id), "export LUXMUX_WINDOW='two'") })
	attaches := len(h.lux.Attaches())
	h.lux.Blip(id)
	h.waitFor("attached again", func() bool { return len(h.lux.Attaches()) > attaches })
	h.live(0)
	time.Sleep(200 * time.Millisecond)
	if n := patches(h.lux); n != 1 {
		t.Fatalf("%d renames asked of a lux that has no rename", n)
	}
	if got := strings.Count(string(h.term.rawBytes()), notice); got != shown {
		t.Fatalf("notice drawn %d times after the first refusal, %d before", got, shown)
	}
	if h.windowName(0) != "two" || h.m.Session() != "renamed" {
		t.Fatalf("window %q in session %q", h.windowName(0), h.m.Session())
	}
	if got := runName(h, id); got != "s/app" {
		t.Fatalf("Run renamed on an old lux: %q", got)
	}
	h.detach()
}

// TestRunNameRebuild: a session rebuilt from lux (its file gone) names
// its windows after their Runs' names, a '/' in a window name kept; a Run
// named by hand on lux keeps that name and its window falls back to its
// profile; a Run with luxmux's old name is renamed at attach.
func TestRunNameRebuild(t *testing.T) {
	h := newHarness(t, cfgNamed, nil)
	a := h.live(0)
	h.type_("\x02c")
	b := h.live(1)
	h.type_("\x02c")
	c := h.live(2)
	h.m.Rename(0, "web/v2")
	h.waitFor("renamed", func() bool { return runName(h, a) == "s/web/v2" })
	h.lux.Update(b, func(r *api.Run) { r.Name = "by hand" })
	h.lux.Update(c, func(r *api.Run) { r.Name = "luxmux-s" })
	rebuildFromLux(h)
	for i, want := range []string{"web/v2", "dev", "dev"} {
		if got := h.windowName(i); got != want {
			t.Fatalf("window %d named %q, want %q", i, got, want)
		}
	}
	h.live(2)
	h.waitFor("old name replaced", func() bool { return runName(h, c) == "s/dev" })
	h.live(1)
	time.Sleep(100 * time.Millisecond)
	if got := runName(h, b); got != "by hand" {
		t.Fatalf("a name set on lux was replaced by %q", got)
	}
	h.detach()
}

// TestRunNameRetry: a rename lux fails is not retried in a loop: it is
// sent again at the next attach, both a reattach of the stream and a
// new process (the file records it due).
func TestRunNameRetry(t *testing.T) {
	h := newHarness(t, cfgNamed, nil)
	id := h.live(0)
	h.lux.FailRenames(http.StatusInternalServerError)
	h.m.Rename(0, "api")
	h.waitFor("rename tried", func() bool { return patches(h.lux) == 1 })
	time.Sleep(200 * time.Millisecond)
	if n := patches(h.lux); n != 1 {
		t.Fatalf("%d renames before the next attach", n)
	}
	attaches := len(h.lux.Attaches())
	h.lux.Blip(id)
	h.waitFor("attached again", func() bool { return len(h.lux.Attaches()) > attaches })
	h.live(0)
	h.waitFor("retried at the reattach", func() bool { return patches(h.lux) == 2 })
	if s, ok, _ := state.Load(h.dir, "s"); !ok || !s.Windows[0].RenameRun {
		t.Fatalf("a failed rename not recorded due: %+v", s)
	}
	h.detach()

	h.lux.FailRenames(0)
	h.start(nil)
	h.live(0)
	h.waitFor("renamed at the next attach", func() bool { return runName(h, id) == "s/api" })
	h.waitFor("rename recorded done", func() bool {
		s, ok, _ := state.Load(h.dir, "s")
		return ok && !s.Windows[0].RenameRun
	})
	h.detach()
}
