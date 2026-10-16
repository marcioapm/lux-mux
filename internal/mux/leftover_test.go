package mux

import (
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/marcioapm/lux/api"

	"github.com/marcioapm/lux-mux/internal/config"
	"github.com/marcioapm/lux-mux/internal/state"
)

// bEndpoint is the endpoint of x's profile remote, on b.
func (x *twoLux) bEndpoint() config.Endpoint {
	return x.cfg.Profiles["remote"].Endpoint(x.lux.Config())
}

// waitListedB waits for b's first listing: the windows it adopts are
// added before it counts as listed.
func (x *twoLux) waitListedB() {
	x.t.Helper()
	ep := x.bEndpoint()
	x.waitFor("b listed", func() bool {
		x.m.mu.Lock()
		c := x.m.listed[ep]
		x.m.mu.Unlock()
		if c == nil {
			return false
		}
		select {
		case <-c:
			return true
		default:
			return false
		}
	})
}

// runIDs are the windows' Run ids.
func (h *harness) runIDs() []string {
	h.m.mu.Lock()
	defer h.m.mu.Unlock()
	var ids []string
	for _, w := range h.m.windows {
		ids = append(ids, w.RunID)
	}
	return ids
}

func recordedSpare(sp map[string]string, id string) bool {
	return (&state.Session{Spares: sp}).RecordsSpare(id)
}

// TestExternalLeftoverNotReadopted: a new-window Run on b replaced by r
// while b fails every terminate is a leftover; the next attach does not
// adopt it as a window (the session has one window on b, the new Run),
// and once b terminates again the leftover is terminated and forgotten,
// leaving no ended window behind.
func TestExternalLeftoverNotReadopted(t *testing.T) {
	x := newTwoLux(t, func(o *Options) { o.NoticeFor = 5 * time.Second })
	a0 := x.live(0)
	ext := submitOn(t, x.b, x.cfg.Profiles["remote"], "remote", "far")
	x.waitFor("adopted", func() bool { return x.nwindows() == 2 })
	if b1 := x.liveOnB(1); b1 != ext {
		t.Fatalf("window 1 %s, ext %s", b1, ext)
	}
	x.b.FailTerminates(http.StatusServiceUnavailable)
	x.m.mu.Lock()
	w := x.m.windows[1]
	x.m.mu.Unlock()
	x.m.markDead(w, "ended for the test")
	x.m.relaunchOrResume(w)
	x.waitScreen("cannot terminate notice", x.contains("cannot terminate on"))
	b2 := x.liveOnB(1)
	if b2 == ext {
		t.Fatal("r kept the old Run")
	}
	x.detach()

	x.start(nil)
	x.live(0)
	x.waitListedB()
	x.waitScreen("cannot terminate notice", x.contains("cannot terminate on"))
	if ids := x.runIDs(); !slices.Equal(ids, []string{a0, b2}) {
		t.Fatalf("windows %v, want %s and %s; leftover %s", ids, a0, b2, ext)
	}
	x.detach()
	if s, _, _ := state.Load(x.dir, "s"); len(s.Windows) != 2 || s.Windows[1].RunID != b2 || s.Spares[state.LeftoverPrefix+ext] != ext {
		t.Fatalf("file windows %+v spares %v", s.Windows, s.Spares)
	}

	x.b.FailTerminates(0)
	x.start(nil)
	x.live(0)
	x.waitListedB()
	x.waitFor("the leftover terminated", func() bool { return slices.Contains(x.b.Terminated(), ext) })
	x.waitFor("and forgotten", func() bool { return !recordedSpare(x.spares(), ext) })
	if ids := x.runIDs(); !slices.Equal(ids, []string{a0, b2}) {
		t.Fatalf("windows %v, want %s and %s", ids, a0, b2)
	}
	x.detach()
	s, _, _ := state.Load(x.dir, "s")
	if len(s.Windows) != 2 || s.Windows[1].RunID != b2 || recordedSpare(s.Spares, ext) {
		t.Fatalf("file windows %+v spares %v", s.Windows, s.Spares)
	}
}

// TestAdoptRunSkipsRecordedSpare: adoptRun makes no window of a Run the
// session records as a leftover, and does make one of a Run it does not.
func TestAdoptRunSkipsRecordedSpare(t *testing.T) {
	x := newTwoLux(t, nil)
	x.live(0)
	run := func(id string) *api.Run {
		return &api.Run{ID: id, State: "running", Labels: map[string]string{
			config.LabelSession: "s", config.LabelExternal: "true", config.LabelProfile: "remote", config.LabelWindow: id}}
	}
	ep := x.bEndpoint()
	x.m.mu.Lock()
	x.m.sess.SetSpare(state.LeftoverPrefix+"run_b9001", "run_b9001", ep)
	x.m.mu.Unlock()
	x.m.adoptRun(ep, run("run_b9001"))
	if n := x.nwindows(); n != 1 {
		t.Fatalf("a leftover adopted: %v", x.runIDs())
	}
	x.m.adoptRun(ep, run("run_b9002"))
	if x.nwindows() != 2 || x.window(1) != "run_b9002" {
		t.Fatalf("windows %v", x.runIDs())
	}
	x.detach()
}

// TestUnlistedLeftoverNotAdded: a session whose b could not be listed
// when attach rebuilt it, with a new-window Run on b recorded as a
// leftover: once b answers, the leftover is not added as a window.
func TestUnlistedLeftoverNotAdded(t *testing.T) {
	x := newTwoLux(t, nil)
	a0 := x.live(0)
	x.detach()
	ext := submitOn(t, x.b, x.cfg.Profiles["remote"], "remote", "far")
	x.b.FailTerminates(http.StatusServiceUnavailable)
	s, _, _ := state.Load(x.dir, "s")
	s.Unlisted = []config.Endpoint{x.bEndpoint()}
	s.SetSpare(state.LeftoverPrefix+ext, ext, x.bEndpoint())
	x.start(s)
	x.live(0)
	x.waitListedB()
	if ids := x.runIDs(); !slices.Equal(ids, []string{a0}) {
		t.Fatalf("windows %v; leftover %s", ids, ext)
	}
	x.detach()
}
