package state

import (
	"os"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/marcioapm/lux/api"

	"github.com/marcioapm/lux-mux/internal/config"
)

// TestHeld: a lock held by another holder is reported with its pid, a
// released one is not, and a stale pid in a lock file nobody holds is
// not either.
func TestHeld(t *testing.T) {
	dir := t.TempDir()
	if _, held := Held(dir, "s"); held {
		t.Fatal("no lock file: held")
	}
	unlock, err := Lock(dir, "s")
	if err != nil {
		t.Fatal(err)
	}
	pid, held := Held(dir, "s")
	if !held || pid != strconv.Itoa(os.Getpid()) {
		t.Fatalf("held %v pid %q", held, pid)
	}
	unlock()
	unlock() // a second unlock is harmless
	if _, held := Held(dir, "s"); held {
		t.Fatal("released: held")
	}
	if err := os.WriteFile(dir+"/s.lock", []byte("999999\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, held := Held(dir, "s"); held {
		t.Fatal("a stale pid nobody holds: held")
	}
	if u, err := Lock(dir, "s"); err != nil {
		t.Fatalf("Lock after the probe: %v", err)
	} else {
		u()
	}
}

// TestRenames: the map moves with a rename, is left by renaming back to
// the label, is dropped on request, and is not listed as a session.
func TestRenames(t *testing.T) {
	dir := t.TempDir()
	if err := SetRenamed(dir, "a", "b", "a"); err != nil {
		t.Fatal(err)
	}
	if err := SetRenamed(dir, "b", "c", "a"); err != nil {
		t.Fatal(err)
	}
	if m, _ := Renames(dir); !reflect.DeepEqual(m, map[string]string{"c": "a"}) {
		t.Fatalf("after two renames: %v", m)
	}
	if err := Save(dir, &Session{Name: "c"}); err != nil {
		t.Fatal(err)
	}
	if l, _ := List(dir); !reflect.DeepEqual(l, []string{"c"}) {
		t.Fatalf("List %v", l)
	}
	if err := SetRenamed(dir, "c", "a", "a"); err != nil {
		t.Fatal(err)
	}
	if m, _ := Renames(dir); len(m) != 0 {
		t.Fatalf("renamed back to its label: %v", m)
	}
	_ = SetRenamed(dir, "a", "z", "a")
	if err := DropRenamed(dir, "z"); err != nil {
		t.Fatal(err)
	}
	if m, _ := Renames(dir); len(m) != 0 {
		t.Fatalf("dropped: %v", m)
	}
}

// TestRebuildLabelled: a renamed session is rebuilt from the Runs that
// carry its old name, under its new one.
func TestRebuildLabelled(t *testing.T) {
	now := time.Now()
	runs := []*api.Run{
		{ID: "r2", State: "running", CreatedAt: now.Add(time.Second), Labels: map[string]string{config.LabelSession: "old", config.LabelProfile: "p"}},
		{ID: "r1", State: "running", CreatedAt: now, Labels: map[string]string{config.LabelSession: "old", config.LabelProfile: "p"}},
		{ID: "r3", State: "running", CreatedAt: now, Labels: map[string]string{config.LabelSession: "new", config.LabelProfile: "p"}},
	}
	s := RebuildLabelled("new", "old", runs, nil)
	if s.Name != "new" || len(s.Windows) != 2 || s.Windows[0].RunID != "r1" || s.Windows[1].RunID != "r2" {
		t.Fatalf("%+v", s)
	}
}
