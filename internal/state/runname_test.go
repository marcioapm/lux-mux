package state

import (
	"testing"

	"github.com/marcioapm/lux/api"

	"github.com/marcioapm/lux-mux/internal/config"
)

// TestRebuildWindowNamesFromRunNames: rebuilt windows take their names
// from their Runs' names <session>/<window> (split at the first '/', so a
// window name may hold one), for the session's name or its Runs' label.
// A Run named otherwise (by hand, for another session, or as luxmux named
// Runs before) falls back to its window label, else its profile.
func TestRebuildWindowNamesFromRunNames(t *testing.T) {
	named := func(r *api.Run, name string) *api.Run { r.Name = name; return r }
	runs := []*api.Run{
		named(run("r1", "old", "dev", "running", 1, false), "old/api"),
		named(run("r2", "old", "dev", "running", 2, false), "new/web/v2"),
		named(run("r3", "old", "dev", "running", 3, false), "luxmux-old"),
		named(run("r4", "old", "dev", "running", 4, false), "my own name"),
		named(run("r5", "old", "dev", "running", 5, false), "other/x"),
		named(run("r6", "old", "dev", "running", 6, false), "old/"),
		named(run("r7", "old", "dev", "running", 7, false), "other/x"),
		// A claimed spare that kept its spare name, then the waiting one.
		named(run("r8", "old", "dev", "running", 8, true), "old/spare"),
		named(run("r9", "old", "dev", "running", 9, true), "old/spare"),
	}
	// Its spec read back, so its window label names it, as before.
	runs[6].Labels[config.LabelWindow] = "labelled"
	runs[6].Spec.Image.Ref = "alpine"
	for _, r := range runs {
		r.Labels[config.LabelSpec] = "k"
	}
	s := RebuildLabelledFrom("new", "old", []Listing{{Endpoint: testEP, Runs: runs}}, testEP, nil)
	var got []string
	for _, w := range s.Windows {
		got = append(got, w.RunID+"="+w.Name)
	}
	want := []string{"r1=api", "r2=web/v2", "r3=dev", "r4=dev", "r5=dev", "r6=dev", "r7=labelled", "r8=dev"}
	if len(got) != len(want) {
		t.Fatalf("windows %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("windows %v, want %v", got, want)
		}
	}
	if s.Spares[config.SpareSlot("k", testEP, testEP)] != "r9" {
		t.Fatalf("spares %v", s.Spares)
	}
}

// TestRebuildRunNameWithSpec: a Run lux returns a spec for (rebuilt
// through WindowFromRun) keeps its name too.
func TestRebuildRunNameWithSpec(t *testing.T) {
	r := run("r1", "s", "dev", "running", 1, false)
	r.Name, r.Spec.Image.Ref = "s/a/b", "alpine"
	s := RebuildFrom("s", []Listing{{Endpoint: testEP, Runs: []*api.Run{r}}}, testEP, nil)
	if len(s.Windows) != 1 || s.Windows[0].Name != "a/b" || s.Windows[0].Spec == nil || s.Windows[0].Spec.Image != "alpine" {
		t.Fatalf("%+v", s.Windows)
	}
}
