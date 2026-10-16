package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/marcioapm/lux-mux/internal/config"
	"github.com/marcioapm/lux-mux/internal/fakelux"
	"github.com/marcioapm/lux-mux/internal/state"
)

// stateDirOf is the luxmux state directory env points at.
func stateDirOf(env []string) string {
	for _, s := range env {
		if dir, ok := strings.CutPrefix(s, "XDG_STATE_HOME="); ok {
			return filepath.Join(dir, "luxmux")
		}
	}
	return ""
}

// makeSession creates session name with luxmux new, prints text in its
// shell and detaches; it returns the window's Run.
func makeSession(t *testing.T, lux *fakelux.Server, env []string, name, text string) string {
	t.Helper()
	n := len(lux.RunIDs())
	p := startLuxmux(t, env, "new -s "+name)
	p.wait(name+" prompt", has("$"))
	p.typ("echo " + text + "\r")
	p.wait(name+" text", has("\n"+text))
	p.typ("\x02d")
	p.exited()
	return lux.RunIDs()[n]
}

// TestSessionsSwitchAndBack: in one luxmux, C-b s lists the sessions,
// previews another's shown window, and Enter switches to it with its
// screen restored; C-b s again switches back, with the first screen
// restored. The session left keeps its Runs and its lock is free.
func TestSessionsSwitchAndBack(t *testing.T) {
	lux, env := e2eEnv(t)
	dir := stateDirOf(env)
	idB := makeSession(t, lux, env, "beta", "in-beta")
	p := startLuxmux(t, env, "new -s alpha")
	p.wait("alpha prompt", has("$"))
	idA := lux.RunIDs()[1]
	p.typ("echo in-alpha\r")
	p.wait("alpha text", has("\nin-alpha"))

	p.typ("\x02s")
	p.wait("session chooser", func(s string) bool {
		return strings.Contains(s, "DEFAULT PROFILE") && strings.Contains(s, "alpha *") && strings.Contains(s, "beta")
	})
	p.typ("j")
	p.wait("beta previewed", has("in-beta"))
	p.typ("\r")
	p.wait("on beta", func(s string) bool {
		return p.scr.title() == "luxmux:beta [0] default" && strings.Contains(s, "in-beta") &&
			!strings.Contains(s, "in-alpha") && !strings.Contains(s, "DEFAULT PROFILE")
	})
	p.wait("alpha's stream closed", func(string) bool { return lux.Streams(idA) == 0 })
	if _, held := state.Held(dir, "alpha"); held {
		t.Fatal("alpha's lock is held after the switch")
	}
	if pid, held := state.Held(dir, "beta"); !held || pid != strconv.Itoa(p.cmd.Process.Pid) {
		t.Fatalf("beta's lock: %v %q", held, pid)
	}
	p.typ("echo still-beta\r")
	p.wait("beta's shell takes keys", has("\nstill-beta"))

	p.typ("\x02s")
	p.wait("chooser on beta", has("beta *"))
	p.typ("k\r")
	p.wait("back on alpha", func(s string) bool {
		return p.scr.title() == "luxmux:alpha [0] default" && strings.Contains(s, "in-alpha") && !strings.Contains(s, "still-beta")
	})
	p.wait("beta's stream closed", func(string) bool { return lux.Streams(idB) == 0 })
	for _, id := range []string{idA, idB} {
		if st := lux.Run(id).State; st != "running" {
			t.Fatalf("%s %s after the switches", id, st)
		}
	}
	p.typ("\x02d")
	p.exited()
	if !strings.Contains(p.scr.text(), "[detached (from session alpha)]") {
		t.Fatalf("detach message:\n%s", p.scr.text())
	}
	p.restored()
}

// TestSessionsStepSkipsLocked: C-b ) and C-b ( go to the next and
// previous session by name, past one another process has, with a notice.
func TestSessionsStepSkipsLocked(t *testing.T) {
	lux, env := e2eEnv(t)
	dir := stateDirOf(env)
	makeSession(t, lux, env, "s2", "in-s2")
	makeSession(t, lux, env, "s3", "in-s3")
	makeSession(t, lux, env, "s4", "in-s4")
	unlock, err := state.Lock(dir, "s3")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	p := startLuxmux(t, env, "new -s s1")
	p.wait("s1 prompt", has("$"))
	p.typ("echo in-s1\r")
	p.wait("s1 text", has("\nin-s1"))
	var seen []string
	for _, k := range []string{")", ")", "(", "("} {
		before := p.scr.title()
		mark := p.rawLen()
		p.typ("\x02" + k)
		p.wait("switched", func(string) bool { return p.scr.title() != before && strings.HasSuffix(p.scr.title(), "[0] default") })
		name := strings.TrimSuffix(strings.TrimPrefix(p.scr.title(), "luxmux:"), " [0] default")
		seen = append(seen, name)
		if name == "s4" && k == ")" || name == "s2" && k == "(" {
			p.wait("skip notice", func(string) bool { return strings.Contains(p.rawSince(mark), "skipped s3: attached elsewhere") })
		}
		p.wait(name+" restored", has("in-"+name))
	}
	if want := []string{"s2", "s4", "s2", "s1"}; !slices.Equal(seen, want) {
		t.Fatalf("order %v, want %v", seen, want)
	}
	p.typ("\x02d")
	p.exited()
}

// TestSessionsRenameAndRebuild: C-b $ renames the attached session;
// luxmux ls and attach know it by its new name, the old name is refused,
// and with its file gone it is rebuilt from its Runs' labels under the
// new name.
func TestSessionsRenameAndRebuild(t *testing.T) {
	lux, env := e2eEnv(t)
	dir := stateDirOf(env)
	p := startLuxmux(t, env, "new -s old")
	p.wait("prompt", has("$"))
	p.typ("echo before-rename\r")
	p.wait("text", has("\nbefore-rename"))
	p.typ("\x02$")
	p.wait("prompt for the name", has("rename session: old"))
	p.typ("\x15renamed\r")
	p.wait("title", func(string) bool { return p.scr.title() == "luxmux:renamed [0] default" })
	p.typ("\x02c")
	p.wait("second window", func(string) bool { return p.scr.title() == "luxmux:renamed [1] default" })
	p.wait("second prompt", has("$"))
	p.typ("\x02d")
	p.exited()
	if !strings.Contains(p.scr.text(), "[detached (from session renamed)]") {
		t.Fatalf("detach message:\n%s", p.scr.text())
	}
	for _, id := range lux.RunIDs() {
		if l := lux.Run(id).Labels[config.LabelSession]; l != "old" {
			t.Fatalf("Run %s labelled %q", id, l)
		}
	}

	ls := startLuxmux(t, env, "ls")
	ls.exited()
	if out := ls.scr.text(); !strings.Contains(out, "renamed") || !strings.Contains(out, "Runs labelled old") {
		t.Fatalf("ls:\n%s", out)
	}
	q := startLuxmux(t, env, "attach -t old")
	if code := q.exitCode(); code == 0 || !strings.Contains(q.scr.text(), "renamed to renamed") {
		t.Fatalf("attach of the old name: %d\n%s", code, q.scr.text())
	}
	q = startLuxmux(t, env, "new -s old")
	if code := q.exitCode(); code == 0 {
		t.Fatalf("new -s with the old name: %d\n%s", code, q.scr.text())
	}

	if err := os.Remove(filepath.Join(dir, "renamed.json")); err != nil {
		t.Fatal(err)
	}
	p = startLuxmux(t, env, "attach -t renamed")
	p.wait("rebuilt from labels", func(s string) bool {
		return strings.HasPrefix(p.scr.title(), "luxmux:renamed [0]") && strings.Contains(s, "before-rename")
	})
	p.typ("\x021")
	p.wait("both windows rebuilt", func(string) bool { return strings.HasPrefix(p.scr.title(), "luxmux:renamed [1]") })
	p.typ("\x02d")
	p.exited()
	if s, ok, _ := state.Load(dir, "renamed"); !ok || len(s.Windows) != 2 {
		t.Fatalf("rebuilt file %+v", s)
	}
}

// TestSessionsKillFromChooser: x in the session chooser, confirmed,
// terminates the other session's Runs and forgets it; on the current
// session it is refused with a message and nothing is terminated.
func TestSessionsKillFromChooser(t *testing.T) {
	lux, env := e2eEnv(t)
	dir := stateDirOf(env)
	idB := makeSession(t, lux, env, "victim", "in-victim")
	p := startLuxmux(t, env, "new -s keeper")
	p.wait("prompt", has("$"))
	idA := lux.RunIDs()[1]
	p.typ("\x02s")
	p.wait("chooser", has("keeper *"))
	p.typ("x")
	p.wait("refusal", has("cannot be killed from itself"))
	p.typ("j")
	p.wait("on victim", func(s string) bool { return strings.Contains(s, "in-victim") })
	p.typ("x")
	p.wait("confirm", has("kill session victim"))
	p.typ("y")
	if !fakelux.WaitFor(context.Background(), 15*time.Second, func() bool { return lux.Run(idB).State == "terminated" }) {
		t.Fatalf("victim's Run %s; screen:\n%s", lux.Run(idB).State, p.scr.text())
	}
	p.wait("victim gone from the list", func(s string) bool { return !strings.Contains(s, "victim") && strings.Contains(s, "keeper *") })
	if st := lux.Run(idA).State; st != "running" {
		t.Fatalf("keeper's Run %s", st)
	}
	if _, ok, _ := state.Load(dir, "victim"); ok {
		t.Fatal("victim.json is left")
	}
	p.typ("\x1b")
	p.wait("chooser closed", func(s string) bool { return !strings.Contains(s, "DEFAULT PROFILE") })
	p.typ("echo after-kill\r")
	p.wait("keeper's shell", has("\nafter-kill"))
	p.typ("\x02d")
	p.exited()
}

// TestSessionsNewFromChooser: n in the session chooser opens the
// new-session dialog; its session is created and switched to, with a
// first window of the chosen profile, and the session left is detached.
func TestSessionsNewFromChooser(t *testing.T) {
	lux, env := e2eEnv(t)
	dir := stateDirOf(env)
	p := startLuxmux(t, env, "new -s first")
	p.wait("prompt", has("$"))
	p.typ("\x02s")
	p.wait("chooser", has("first *"))
	p.typ("n")
	p.wait("dialog", has("New session"))
	p.typ("second")
	p.wait("name typed", has("second"))
	p.typ("\t\t")
	p.wait("on Repository", has("▸ Repository"))
	p.typ("https://example.com/team/svc.git")
	p.wait("repo typed", has("svc.git"))
	p.typ("\x13")
	p.wait("on second", func(s string) bool { return p.scr.title() == "luxmux:second [0] default" && strings.Contains(s, "$") })
	if n := len(lux.Submits()); n != 2 {
		t.Fatalf("%d submits", n)
	}
	sp := lux.Submits()[1]
	if sp.Labels[config.LabelSession] != "second" || sp.Git == nil || sp.Git.Repositories[0].URL != "https://example.com/team/svc.git" {
		t.Fatalf("second's submit %+v", sp)
	}
	if s, ok, _ := state.Load(dir, "second"); !ok || s.DefaultProfile != "default" || len(s.Windows) != 1 {
		t.Fatalf("second's file %+v", s)
	}
	if _, held := state.Held(dir, "first"); held {
		t.Fatal("first is still locked")
	}
	p.typ("\x02d")
	p.exited()
}
