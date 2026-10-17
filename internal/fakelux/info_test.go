package fakelux

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marcioapm/lux/api"

	"github.com/marcioapm/lux-mux/internal/config"
	"github.com/marcioapm/lux-mux/internal/luxapi"
)

func submitted(t *testing.T, s *Server) (*luxapi.Client, string) {
	t.Helper()
	c, _ := luxapi.New(s.Config())
	sp, err := config.RunSpec(config.Profile{Image: "alpine"}, config.Target{Session: "s", Profile: "p"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	r, err := c.Submit(context.Background(), sp)
	if err != nil {
		t.Fatal(err)
	}
	// Submit answers before Start runs: wait for it, or Start would
	// overwrite what the test sets next.
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
		if got, err := c.Get(context.Background(), r.ID); err == nil && got.State == "running" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never ran", r.ID)
		}
	}
	return c, r.ID
}

// TestInfoFields: what Update, SetCost and SetResume set is what GET and
// the cost endpoint answer; without them those fields are absent.
func TestInfoFields(t *testing.T) {
	s := New()
	defer s.Close()
	c, id := submitted(t, s)
	ctx := context.Background()
	r, err := c.Get(ctx, id)
	if err != nil || r.Host != "host-1" || r.Epoch != 1 || r.Image != nil || r.RuntimeSince != nil || r.Resume != nil {
		t.Fatalf("defaults: %+v %v", r, err)
	}
	if cost, err := c.Cost(ctx, id); err != nil || cost.Status != "pending" || len(cost.Totals) != 0 {
		t.Fatalf("default cost: %+v %v", cost, err)
	}
	since := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	s.Update(id, func(r *api.Run) {
		r.Host, r.Epoch, r.RuntimeSince, r.RuntimeSeconds = "host-9", 4, &since, 3600
		r.PlacementWaitSeconds, r.PlacementStartSeconds = 3, 2
		r.Image = &api.ImageResolution{ImageID: "sha256:feed"}
		r.Spec.Network.Egress = []api.EgressRule{{Host: "example.com"}}
	})
	s.SetCost(id, &luxapi.RunCost{Status: "complete", Totals: []api.CostTotal{{Currency: "USD", Amount: "0.5"}}})
	snap := "snap_1"
	s.SetResume(id, &api.Resumability{Snapshot: &snap, Blockers: []string{"quota"}})
	r, err = c.Get(ctx, id)
	if err != nil || r.Host != "host-9" || r.Epoch != 4 || !r.RuntimeSince.Equal(since) || r.RuntimeSeconds != 3600 ||
		r.PlacementWaitSeconds != 3 || r.Image.ImageID != "sha256:feed" || r.Spec.Network.Egress[0].Host != "example.com" ||
		r.Resume == nil || *r.Resume.Snapshot != "snap_1" {
		t.Fatalf("updated: %+v %v", r, err)
	}
	if cost, err := c.Cost(ctx, id); err != nil || cost.Status != "complete" || cost.Totals[0].Amount != "0.5" {
		t.Fatalf("cost: %+v %v", cost, err)
	}
	s.SetResume(id, nil)
	if r, _ = c.Get(ctx, id); r.Resume != nil {
		t.Fatal("resume kept")
	}
	if _, err := c.Cost(ctx, "run_none"); !luxapi.IsStatus(err, 404) {
		t.Fatalf("cost of no Run: %v", err)
	}
}

// TestExecResponder: canned output, a failure, and a hang that ends with
// the client's context.
func TestExecResponder(t *testing.T) {
	s := New()
	defer s.Close()
	c, id := submitted(t, s)
	ctx := context.Background()
	if _, _, err := c.ExecOutput(ctx, id, []string{"true"}, 10); !luxapi.IsStatus(err, 409) {
		t.Fatal("exec without ExecEnv or responder accepted")
	}
	want := ResourcesOutput("max 100000", "max", "42", "7", 100, 50)
	var mode atomic.Value
	mode.Store("ok")
	s.SetExecResponder(func(runID string, cmd []string) (ExecAnswer, bool) {
		switch mode.Load() {
		case "fail":
			return ExecAnswer{Error: "exec failed"}, true
		case "hang":
			return ExecAnswer{Hang: true}, true
		case "status":
			return ExecAnswer{Output: []byte("x"), Code: 3}, true
		}
		return ExecAnswer{Output: want}, true
	})
	out, code, err := c.ExecOutput(ctx, id, []string{"sh"}, 4096)
	if err != nil || code != 0 || string(out) != string(want) {
		t.Fatalf("ok: %q %d %v", out, code, err)
	}
	if out, _, _ := c.ExecOutput(ctx, id, []string{"sh"}, 3); string(out) != string(want[:3]) {
		t.Fatalf("limit: %q", out)
	}
	mode.Store("status")
	if _, code, err := c.ExecOutput(ctx, id, []string{"sh"}, 10); err != nil || code != 3 {
		t.Fatalf("status: %d %v", code, err)
	}
	mode.Store("fail")
	if _, _, err := c.ExecOutput(ctx, id, []string{"sh"}, 10); err == nil || err.Error() != "exec failed" {
		t.Fatalf("fail: %v", err)
	}
	mode.Store("hang")
	hctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, _, err := c.ExecOutput(hctx, id, []string{"sh"}, 10); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("hang: %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("hang outlived the client's deadline: %v", d)
	}
	if n := len(s.Execs()); n != 5 {
		t.Fatalf("%d execs recorded", n)
	}
	s.SetState(id, "stopped", "stop")
	mode.Store("ok")
	if _, _, err := c.ExecOutput(ctx, id, []string{"sh"}, 10); !luxapi.IsStatus(err, 409) {
		t.Fatalf("exec of a stopped Run: %v", err)
	}
}
