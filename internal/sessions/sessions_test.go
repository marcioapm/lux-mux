package sessions

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/marcioapm/lux-mux/internal/config"
	"github.com/marcioapm/lux-mux/internal/fakelux"
	"github.com/marcioapm/lux-mux/internal/luxapi"
	"github.com/marcioapm/lux-mux/internal/state"
)

type fixture struct {
	t   *testing.T
	lux *fakelux.Server
	c   *luxapi.Client
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
			if err := CheckFree(ctx, f.c, f.dir, "a", ""); err == nil {
				t.Fatal("the old name is free while its Runs live")
			}
			if err := CheckFree(ctx, f.c, f.dir, "b", ""); !errors.Is(err, ErrTaken) {
				t.Fatalf("the new name while its Runs live: %v", err)
			}
			if err := f.c.Terminate(ctx, id); err != nil {
				t.Fatal(err)
			}
			if err := CheckFree(ctx, f.c, f.dir, "a", ""); err != nil {
				t.Fatalf("the old name after its Runs ended: %v", err)
			}
			if err := CheckFree(ctx, f.c, f.dir, "b", ""); err != nil {
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
	if err := CheckFree(ctx, f.c, f.dir, "a", ""); err == nil {
		t.Fatal("the old name of a session attached elsewhere is free")
	}
	if r, _ := state.Renames(f.dir); r["b"] != "a" {
		t.Fatalf("renames %v", r)
	}
}

// TestEntryHeldHereDroppedOnlyByOpen: with its lock held by this process,
// an ended renamed session's entry and file survive CheckFree and
// LiveLabel; Open, attaching that name, drops them.
func TestEntryHeldHereDroppedOnlyByOpen(t *testing.T) {
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
		if err := CheckFree(ctx, f.c, f.dir, n, ""); err == nil {
			t.Fatalf("CheckFree(%s) while b is held here", n)
		}
		kept("CheckFree(" + n + ")")
	}
	if l, err := LiveLabel(ctx, f.c, f.dir, "b"); err != nil || l != "a" {
		t.Fatalf("LiveLabel(b): %q %v", l, err)
	}
	kept("LiveLabel(b)")
	_, label, existed, err := Open(ctx, f.c, f.dir, f.cfg, "b")
	if err != nil || existed || label != "b" {
		t.Fatalf("open b: %q %v %v", label, existed, err)
	}
	if r, _ := state.Renames(f.dir); len(r) != 0 {
		t.Fatalf("renames after Open: %v", r)
	}
	if _, ok, _ := state.Load(f.dir, "b"); ok {
		t.Fatal("b.json left after Open")
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
			list, err := List(context.Background(), f.c, f.dir)
			if err != nil || len(list) != 2 || list[0].Name != "b" || list[1].Name != "empty" {
				t.Fatalf("list %+v %v", list, err)
			}
			sess, label, ok, err := Open(context.Background(), f.c, f.dir, f.cfg, "b")
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
	if _, _, _, err := Open(context.Background(), f.c, f.dir, f.cfg, "b"); err != nil {
		t.Fatal(err)
	}
	if s, ok, _ := state.Load(f.dir, "c"); !ok || len(s.Windows) != 1 || s.Windows[0].Name != "c's" {
		t.Fatalf("c.json after opening b: %+v %v", s, ok)
	}
	if s, ok, _ := state.Load(f.dir, "b"); ok && slices.ContainsFunc(s.Windows, func(w state.Window) bool { return w.Name == "c's" }) {
		t.Fatalf("b.json took c's windows: %+v", s)
	}
}
