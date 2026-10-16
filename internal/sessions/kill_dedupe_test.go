package sessions

import (
	"context"
	"testing"

	"github.com/marcioapm/lux-mux/internal/state"
)

// TestKillKeepsFileNamesDownLuxOnce: a file with windows on one lux under
// two key sources, that lux down, is kept, and the error names the lux
// once.
func TestKillKeepsFileNamesDownLuxOnce(t *testing.T) {
	f := newFixture(t)
	b, ep, x := f.secondLux()
	b.SetDown(true)
	keyed := ep
	keyed.KeyEnv = "LMX_TEST_SECOND_KEY"
	s := &state.Session{Name: "s", Windows: []state.Window{{RunID: "run_b1", Endpoint: &ep}, {RunID: "run_b2", Endpoint: &keyed}}}
	if err := state.Save(f.dir, s); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	k, err := PlanKill(ctx, x, f.dir, "s")
	if err != nil {
		t.Fatal(err)
	}
	err = k.Do(ctx, x, f.dir, nil)
	want := "session file kept: " + ep.Label() + ", which it has windows on, did not answer"
	if err == nil || err.Error() != want {
		t.Fatalf("kill with b down: %v; want %q", err, want)
	}
	if _, ok, _ := state.Load(f.dir, "s"); !ok {
		t.Fatal("file removed while its lux is down")
	}
}
