package sessions

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/marcioapm/lux-mux/internal/config"
	"github.com/marcioapm/lux-mux/internal/fakelux"
	"github.com/marcioapm/lux-mux/internal/luxapi"
	"github.com/marcioapm/lux-mux/internal/state"
)

type fixture struct {
	t   *testing.T
	lux *fakelux.Server
	c   *luxapi.Client
	x   Luxes
	dir string
	cfg *config.Config
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{t: t, lux: fakelux.New(), dir: t.TempDir()}
	t.Cleanup(f.lux.Close)
	f.c, _ = luxapi.New(f.lux.Config())
	cfg, err := config.Parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	f.cfg = cfg
	f.x = Luxes{Eps: luxapi.NewEndpoints(f.lux.Config(), func(string) (string, bool) { return "", false }), Cfg: cfg}
	return f
}

// run submits a Run labelled session label.
func (f *fixture) run(label string) string {
	f.t.Helper()
	sp, err := config.RunSpec(config.Profile{Image: "alpine"}, config.Target{Session: label, Profile: "default"}, nil)
	if err != nil {
		f.t.Fatal(err)
	}
	r, err := f.c.Submit(context.Background(), sp)
	if err != nil {
		f.t.Fatal(err)
	}
	return r.ID
}

// renamed makes session a, one window, renamed to b; withFile keeps b's
// file (a chooser rename of a session with one), else only the map
// entry is left (a session rebuilt from labels, renamed).
func (f *fixture) renamed(withFile bool) string {
	f.t.Helper()
	id := f.run("a")
	if withFile {
		if err := state.Save(f.dir, &state.Session{Name: "a", Windows: []state.Window{{RunID: id}}}); err != nil {
			f.t.Fatal(err)
		}
	}
	if err := Move(f.dir, "a", "b", "a"); err != nil {
		f.t.Fatal(err)
	}
	if _, ok, _ := state.Load(f.dir, "b"); ok != withFile {
		f.t.Fatalf("b's file after the rename: %v", ok)
	}
	return id
}

// TestRenameEntryEndsWithItsRuns: once a renamed session's Runs were
// terminated from outside luxmux, both its old and its new name are free
// again, with its file (case A) and without one (case B).
func TestRenameEntryEndsWithItsRuns(t *testing.T) {
	for _, withFile := range []bool{true, false} {
		name := map[bool]string{true: "A with a file", false: "B without one"}[withFile]
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			id := f.renamed(withFile)
			ctx := context.Background()
			if err := checkFree(ctx, f.x, f.dir, "a", "", false); err == nil {
				t.Fatal("the old name is free while its Runs live")
			}
			if err := checkFree(ctx, f.x, f.dir, "b", "", false); !errors.Is(err, ErrTaken) {
				t.Fatalf("the new name while its Runs live: %v", err)
			}
			if err := f.c.Terminate(ctx, id); err != nil {
				t.Fatal(err)
			}
			if err := checkFree(ctx, f.x, f.dir, "a", "", false); err != nil {
				t.Fatalf("the old name after its Runs ended: %v", err)
			}
			if err := checkFree(ctx, f.x, f.dir, "b", "", false); err != nil {
				t.Fatalf("the new name after its Runs ended: %v", err)
			}
			if l, err := Label(f.dir, "a"); err != nil || l != "a" {
				t.Fatalf("label of a: %q %v", l, err)
			}
		})
	}
}

// TestRenameEntryKeptWhileAttached: an entry whose Runs all ended is not
// dropped through its old name while a process has the session (it
// ends the session itself when it sees its windows end).
func TestRenameEntryKeptWhileAttached(t *testing.T) {
	f := newFixture(t)
	id := f.renamed(true)
	ctx := context.Background()
	if err := f.c.Terminate(ctx, id); err != nil {
		t.Fatal(err)
	}
	unlock, err := state.Lock(f.dir, "b")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if err := checkFree(ctx, f.x, f.dir, "a", "", false); err == nil {
		t.Fatal("the old name of a session attached elsewhere is free")
	}
	if r, _ := state.Renames(f.dir); r["b"] != "a" {
		t.Fatalf("renames %v", r)
	}
}

// TestEntryHeldHereDroppedOnlyByOpen: with its lock held by this process,
// an ended renamed session's entry and file survive the unlocked
// CheckFree and LiveLabel, and a locked call for its old name. Only a
// caller that has just taken b's lock drops them: Open(b), or
// CheckFree(b) with locked.
func TestEntryHeldHereDroppedOnlyByOpen(t *testing.T) {
	for _, by := range []string{"Open", "CheckFree locked"} {
		t.Run(by, func(t *testing.T) {
			f := newFixture(t)
			id := f.renamed(true)
			ctx := context.Background()
			if err := f.c.Terminate(ctx, id); err != nil {
				t.Fatal(err)
			}
			unlock, err := state.Lock(f.dir, "b")
			if err != nil {
				t.Fatal(err)
			}
			defer unlock()
			kept := func(when string) {
				t.Helper()
				if r, _ := state.Renames(f.dir); r["b"] != "a" {
					t.Fatalf("%s: renames %v", when, r)
				}
				if _, ok, _ := state.Load(f.dir, "b"); !ok {
					t.Fatalf("%s: b.json is gone", when)
				}
			}
			for _, n := range []string{"a", "b"} {
				if err := checkFree(ctx, f.x, f.dir, n, "", false); err == nil {
					t.Fatalf("CheckFree(%s) unlocked while b is held here", n)
				}
				kept("CheckFree(" + n + ")")
			}
			if l, _, err := LiveLabel(ctx, f.x, f.dir, "b", false); err != nil || l != "a" {
				t.Fatalf("LiveLabel(b): %q %v", l, err)
			}
			kept("LiveLabel(b)")
			// b's lock is not a's: the entry naming a as its label stays.
			var re *RenamedError
			if _, _, _, err := Open(ctx, f.x, f.dir, "a"); !errors.As(err, &re) {
				t.Fatalf("open a: %v", err)
			}
			kept("Open(a)")
			if err := checkFree(ctx, f.x, f.dir, "a", "", true); err == nil {
				t.Fatal("CheckFree(a) locked while b is held here")
			}
			kept("CheckFree(a) locked")
			if by == "Open" {
				_, label, existed, err := Open(ctx, f.x, f.dir, "b")
				if err != nil || existed || label != "b" {
					t.Fatalf("open b: %q %v %v", label, existed, err)
				}
			} else if err := checkFree(ctx, f.x, f.dir, "b", "", true); err != nil {
				t.Fatalf("CheckFree(b) locked: %v", err)
			}
			if r, _ := state.Renames(f.dir); len(r) != 0 {
				t.Fatalf("renames after %s: %v", by, r)
			}
			if _, ok, _ := state.Load(f.dir, "b"); ok {
				t.Fatalf("b.json left after %s", by)
			}
		})
	}
}

// TestMoveInterrupted: a rename that stopped after any of its steps is
// recovered when the session is opened by its new name: one file, under
// the new name, with its windows; the old name hidden. A new-name file
// already written wins over the leftover, and another session's file
// with no windows is not taken for the leftover.
func TestMoveInterrupted(t *testing.T) {
	for _, step := range []string{"entry written", "new file written"} {
		t.Run(step, func(t *testing.T) {
			f := newFixture(t)
			id := f.run("a")
			if err := state.Save(f.dir, &state.Session{Name: "empty"}); err != nil {
				t.Fatal(err)
			}
			s := &state.Session{Name: "a", Windows: []state.Window{{RunID: id, Name: "kept"}}}
			if err := state.Save(f.dir, s); err != nil {
				t.Fatal(err)
			}
			if err := state.SetRenamed(f.dir, "a", "b", "a"); err != nil {
				t.Fatal(err)
			}
			if step == "new file written" {
				s.Name = "b"
				if err := state.Save(f.dir, s); err != nil {
					t.Fatal(err)
				}
				old := &state.Session{Name: "a", Windows: []state.Window{{RunID: id, Name: "leftover"}}}
				if err := state.Save(f.dir, old); err != nil {
					t.Fatal(err)
				}
			}
			list, err := List(context.Background(), f.x, f.dir)
			if err != nil || len(list) != 2 || list[0].Name != "b" || list[1].Name != "empty" {
				t.Fatalf("list %+v %v", list, err)
			}
			sess, label, ok, err := Open(context.Background(), f.x, f.dir, "b")
			if err != nil || !ok || label != "a" || len(sess.Windows) != 1 || sess.Windows[0].Name != "kept" {
				t.Fatalf("open b: %+v %q %v %v", sess, label, ok, err)
			}
			if _, ok, _ := state.Load(f.dir, "a"); ok {
				t.Fatal("a.json is left")
			}
			if s, ok, _ := state.Load(f.dir, "b"); !ok || s.Windows[0].RunID != id {
				t.Fatalf("b.json %+v", s)
			}
			if _, ok, _ := state.Load(f.dir, "empty"); !ok {
				t.Fatal("empty.json was taken for the leftover")
			}
		})
	}
}

// TestRecoverMoveKeepsMappedFile: the file of a session that has a rename
// entry of its own is never taken for a leftover of another Move, even
// when its windows are all Runs of the session being opened.
func TestRecoverMoveKeepsMappedFile(t *testing.T) {
	f := newFixture(t)
	id := f.run("a")
	if err := state.SetRenamed(f.dir, "a", "b", "a"); err != nil {
		t.Fatal(err)
	}
	if err := state.SetRenamed(f.dir, "x", "c", "x"); err != nil {
		t.Fatal(err)
	}
	if err := state.Save(f.dir, &state.Session{Name: "c", Windows: []state.Window{{RunID: id, Name: "c's"}}}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := Open(context.Background(), f.x, f.dir, "b"); err != nil {
		t.Fatal(err)
	}
	if s, ok, _ := state.Load(f.dir, "c"); !ok || len(s.Windows) != 1 || s.Windows[0].Name != "c's" {
		t.Fatalf("c.json after opening b: %+v %v", s, ok)
	}
	if s, ok, _ := state.Load(f.dir, "b"); ok && slices.ContainsFunc(s.Windows, func(w state.Window) bool { return w.Name == "c's" }) {
		t.Fatalf("b.json took c's windows: %+v", s)
	}
}

// checkFree is CheckFree's error.
func checkFree(ctx context.Context, x Luxes, dir, name, own string, locked bool) error {
	_, err := CheckFree(ctx, x, dir, name, own, locked)
	return err
}

// withHungLux adds a profile on a lux that takes every request and never
// answers it, and shortens ListTimeout to d.
func (f *fixture) withHungLux(d time.Duration) {
	f.t.Helper()
	prev := ListTimeout
	ListTimeout = d
	f.t.Cleanup(func() { ListTimeout = prev })
	h := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	f.t.Cleanup(func() { h.CloseClientConnections(); h.Close() })
	const keyEnv = "LMX_TEST_HUNG_KEY"
	cfg, err := config.Parse([]byte("[profiles.hung]\nimage = \"x\"\nlux_url = \"" + h.URL + "\"\napi_key_env = \"" + keyEnv + "\"\n"))
	if err != nil {
		f.t.Fatal(err)
	}
	f.cfg = cfg
	f.x = Luxes{Eps: luxapi.NewEndpoints(f.lux.Config(), func(k string) (string, bool) { return "lux_fake_key_hung", k == keyEnv }), Cfg: cfg}
}

// TestOpenRenamedWithHungLux: a renamed session opens within one
// ListTimeout while a second lux never answers: each lux is listed once,
// so the default lux's listing has the whole deadline, and the window is
// kept.
func TestOpenRenamedWithHungLux(t *testing.T) {
	f := newFixture(t)
	f.withHungLux(400 * time.Millisecond)
	id := f.renamed(true)
	// The whole open gets one ListTimeout, as a switch does (switchOpenWait).
	ctx, cancel := context.WithTimeout(context.Background(), ListTimeout)
	defer cancel()
	start := time.Now()
	sess, label, ok, err := Open(ctx, f.x, f.dir, "b")
	d := time.Since(start)
	if err != nil || !ok || label != "a" || len(sess.Windows) != 1 || sess.Windows[0].RunID != id {
		t.Fatalf("open b after %v: %+v %q %v %v", d, sess, label, ok, err)
	}
	if d > ListTimeout*3/2 {
		t.Fatalf("open b took %v, more than one ListTimeout (%v)", d, ListTimeout)
	}
	if r, _ := state.Renames(f.dir); r["b"] != "a" {
		t.Fatalf("renames %v", r)
	}
}

// secondLux is a second fake lux; x names it through profile remote.
func (f *fixture) secondLux() (b *fakelux.Server, ep config.Endpoint, x Luxes) {
	f.t.Helper()
	b = fakelux.New()
	f.t.Cleanup(b.Close)
	b.IDPrefix = "run_b"
	cfg, err := config.Parse([]byte("default_profile = \"base\"\n[profiles.base]\nimage = \"i\"\n[profiles.remote]\nimage = \"img\"\nlux_url = \"" + b.URL + "\"\n"))
	if err != nil {
		f.t.Fatal(err)
	}
	return b, config.Endpoint{URL: b.URL}, Luxes{Eps: f.x.Eps, Cfg: cfg}
}

// submitOn submits a Run on l labelled session label.
func submitOn(t *testing.T, l *fakelux.Server, label string) string {
	t.Helper()
	c, _ := luxapi.New(l.Config())
	sp, err := config.RunSpec(config.Profile{Image: "alpine"}, config.Target{Session: label, Profile: "default"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	r, err := c.Submit(context.Background(), sp)
	if err != nil {
		t.Fatal(err)
	}
	return r.ID
}

// TestCheckFreeRunsOnSecondLux: a name whose live Runs are only on a
// profile's second lux is taken, and the refusal names that lux.
func TestCheckFreeRunsOnSecondLux(t *testing.T) {
	f := newFixture(t)
	b, _, x := f.secondLux()
	submitOn(t, b, "x")
	err := checkFree(context.Background(), x, f.dir, "x", "", false)
	if !errors.Is(err, ErrTaken) || !strings.Contains(err.Error(), "on lux "+strings.TrimPrefix(b.URL, "http://")) {
		t.Fatalf("x with Runs on b only: %v", err)
	}
}

// TestPruneKeepsEntryWhileLuxDown: a rename entry whose file has a
// window on a lux that does not answer is kept, with the file, though
// the default lux lists no live Run of its label.
func TestPruneKeepsEntryWhileLuxDown(t *testing.T) {
	f := newFixture(t)
	b, ep, _ := f.secondLux()
	id := submitOn(t, b, "a")
	if err := state.Save(f.dir, &state.Session{Name: "a", Windows: []state.Window{{RunID: id, Endpoint: &ep}}}); err != nil {
		t.Fatal(err)
	}
	if err := Move(f.dir, "a", "b", "a"); err != nil {
		t.Fatal(err)
	}
	b.SetDown(true)
	if _, _, err := LiveLabel(context.Background(), f.x, f.dir, "b", false); err != nil {
		t.Fatalf("LiveLabel(b): %v", err)
	}
	if r, _ := state.Renames(f.dir); r["b"] != "a" {
		t.Fatalf("rename entry dropped while b's lux is down: %v", r)
	}
	if _, ok, _ := state.Load(f.dir, "b"); !ok {
		t.Fatal("b's file dropped while its lux is down")
	}
}

// TestRenameRefusedWhileLuxDown: renaming to a name while a configured
// lux, which may hold Runs labelled with it, does not answer is refused;
// a new session of a name is not (that lux is its Unlisted).
func TestRenameRefusedWhileLuxDown(t *testing.T) {
	f := newFixture(t)
	b, _, x := f.secondLux()
	b.SetDown(true)
	ctx := context.Background()
	if err := checkFree(ctx, x, f.dir, "newname", "s", true); err == nil || !strings.Contains(err.Error(), "did not answer") {
		t.Fatalf("rename with a lux down: %v", err)
	}
	ls, err := CheckFree(ctx, x, f.dir, "fresh", "", true)
	if err != nil || len(ls.Unlisted()) != 1 || ls.Unlisted()[0].URL != b.URL {
		t.Fatalf("new with a lux down: %v, unlisted %v", err, ls.Unlisted())
	}
}
