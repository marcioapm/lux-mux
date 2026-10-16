package mux

import (
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/marcioapm/lux-mux/internal/config"
	"github.com/marcioapm/lux-mux/internal/fakelux"
	"github.com/marcioapm/lux-mux/internal/luxapi"
	"github.com/marcioapm/lux-mux/internal/state"
)

// TestKill503KeepsWindow: C-b x on b's window while b answers 503 keeps
// the window and its Run, with a notice, in the file too.
func TestKill503KeepsWindow(t *testing.T) {
	x := newTwoLux(t, func(o *Options) { o.NoticeFor = 5 * time.Second })
	x.live(0)
	x.newRemote()
	b1 := x.liveOnB(1)
	x.b.SetDown(true)
	x.m.Kill(1)
	x.waitScreen("cannot terminate notice", x.contains("cannot terminate on "))
	if x.nwindows() != 2 || x.window(1) != b1 {
		t.Fatalf("window dropped on 503: %v", x.runIDs())
	}
	x.b.SetDown(false)
	x.detach()
	if s, _, _ := state.Load(x.dir, "s"); len(s.Windows) != 2 || s.Windows[1].RunID != b1 {
		t.Fatalf("file %+v", s.Windows)
	}
	if len(x.b.Terminated()) != 0 || x.b.Run(b1).State != "running" {
		t.Fatalf("b1 %s, b terminated %v", x.b.Run(b1).State, x.b.Terminated())
	}
}

// TestKill409ClosesWindow: C-b x on b's window when b answers 409 (the
// Run is already ending) closes the window.
func TestKill409ClosesWindow(t *testing.T) {
	x := newTwoLux(t, nil)
	x.live(0)
	x.newRemote()
	x.liveOnB(1)
	x.b.FailTerminates(http.StatusConflict)
	x.m.Kill(1)
	x.waitFor("window 1 closed", func() bool { return x.nwindows() == 1 })
	x.detach()
	if s, _, _ := state.Load(x.dir, "s"); len(s.Windows) != 1 {
		t.Fatalf("file %+v", s.Windows)
	}
}

// TestKillDuringRelaunchTerminatesNewRun: C-b x whose terminate is still
// in flight when r replaces the window's Run terminates the new Run too
// before the window closes.
func TestKillDuringRelaunchTerminatesNewRun(t *testing.T) {
	x := newTwoLux(t, nil)
	x.live(0)
	x.newRemote()
	b1 := x.liveOnB(1)
	g := newGate()
	t.Cleanup(g.open)
	held := make(chan struct{})
	var once sync.Once
	x.b.SetTerminateHook(func(id string) {
		if id == b1 {
			once.Do(func() { close(held); g.wait() })
		}
	})
	x.m.mu.Lock()
	w := x.m.windows[1]
	x.m.mu.Unlock()
	x.m.markDead(w, "ended for the test")
	killed := make(chan struct{})
	go func() { x.m.Kill(1); close(killed) }()
	<-held
	x.m.relaunchOrResume(w)
	x.waitFor("window 1 relaunched", func() bool { id := x.window(1); return id != "" && id != b1 })
	b2 := x.window(1)
	g.open()
	<-killed
	x.waitFor("window 1 closed", func() bool { return x.nwindows() == 1 })
	if !slices.Contains(x.b.Terminated(), b2) {
		t.Fatalf("the relaunched Run %s left running; b terminated %v", b2, x.b.Terminated())
	}
	x.detach()
}

// TestRelaunchRecordsLeftoverAtOnce: r records the old Run in the file
// as a leftover before the new Run is made, so a luxmux that dies
// meanwhile leaves it recorded.
func TestRelaunchRecordsLeftoverAtOnce(t *testing.T) {
	x := newTwoLux(t, nil)
	x.live(0)
	x.newRemote()
	b1 := x.liveOnB(1)
	g := newGate()
	t.Cleanup(g.open)
	x.b.SetSubmitHook(g.wait)
	x.b.FailTerminates(http.StatusServiceUnavailable)
	x.m.mu.Lock()
	w := x.m.windows[1]
	x.m.mu.Unlock()
	x.m.markDead(w, "ended for the test")
	x.m.relaunchOrResume(w)
	x.waitFor("the leftover in the file", func() bool {
		s, _, _ := state.Load(x.dir, "s")
		return s.Spares[state.LeftoverPrefix+b1] == b1
	})
	g.open()
	x.detach()
}

// TestShellExitTerminateFails: the shell of b's window exits while b
// fails every terminate: the window is kept, ended with why, and r then
// terminates its Run and replaces it.
func TestShellExitTerminateFails(t *testing.T) {
	x := newTwoLux(t, func(o *Options) { o.NoticeFor = 5 * time.Second })
	x.live(0)
	x.newRemote()
	b1 := x.liveOnB(1)
	x.b.FailTerminates(http.StatusServiceUnavailable)
	x.b.Term(b1).Exit()
	x.waitFor("window 1 ended", func() bool {
		return x.phase(1) == phaseDead && strings.Contains(x.reason(1), "shell exited; cannot terminate on")
	})
	if x.nwindows() != 2 || x.window(1) != b1 {
		t.Fatalf("windows %v", x.runIDs())
	}
	x.b.FailTerminates(0)
	x.type_("r")
	x.waitFor("b1 terminated", func() bool { return slices.Contains(x.b.Terminated(), b1) })
	if b2 := x.liveOnB(1); b2 == b1 {
		t.Fatal("r kept the exited Run")
	}
	x.detach()
}

// TestClaimRefusedKeepsSpare: a window of profile there claims b's spare
// while b refuses the key: the spare cannot be read or terminated, so it
// stays recorded, running; with the key back the next attach terminates
// it and forgets it.
func TestClaimRefusedKeepsSpare(t *testing.T) {
	h, b, spare := spareOnB(t)
	h.opts = func(o *Options) { o.NoticeFor = 5 * time.Second }
	h.detach()
	h.setEnv(remoteEnv, keyA, true)
	h.start(nil)
	h.live(0)
	h.m.opt.UI = &fakeUI{choose: -1, req: &NewWindowRequest{ProfileName: "there", Profile: h.cfg.Profiles["there"]}}
	h.type_("\x02C")
	h.waitFor("window 1", func() bool { return h.nwindows() == 2 })
	h.waitFor("window 1 ended", func() bool { return h.phase(1) == phaseDead })
	h.waitFor("the spare kept as a leftover", func() bool { return h.spares()[state.LeftoverPrefix+spare] == spare })
	h.detach()
	if s, _, _ := state.Load(h.dir, "s"); !recordedSpare(s.Spares, spare) {
		t.Fatalf("file spares %v", s.Spares)
	}
	if r := b.Run(spare); r.State != "running" || len(b.Terminated()) != 0 {
		t.Fatalf("spare %s, b terminated %v", r.State, b.Terminated())
	}

	h.setEnv(remoteEnv, keyB, true)
	h.start(nil)
	h.live(0)
	h.waitFor("the spare terminated", func() bool { return slices.Contains(b.Terminated(), spare) })
	h.waitFor("and forgotten", func() bool { return !recordedSpare(h.spares(), spare) })
	h.detach()
	if s, _, _ := state.Load(h.dir, "s"); recordedSpare(s.Spares, spare) {
		t.Fatalf("file spares %v", s.Spares)
	}
}

// TestClaimWithoutClientKeepsSpare: a window of profile there while b's
// key variable is unset cannot claim b's spare; the spare stays in its
// slot, running.
func TestClaimWithoutClientKeepsSpare(t *testing.T) {
	h, b, spare := spareOnB(t)
	h.detach()
	h.setEnv(remoteEnv, "", false)
	h.start(nil)
	h.live(0)
	h.m.opt.UI = &fakeUI{choose: -1, req: &NewWindowRequest{ProfileName: "there", Profile: h.cfg.Profiles["there"]}}
	h.type_("\x02C")
	h.waitFor("window 1 ended", func() bool { return h.nwindows() == 2 && h.phase(1) == phaseDead })
	h.detach()
	slot := h.m.spareSlotOf(h.cfg.Profiles["there"])
	if s, _, _ := state.Load(h.dir, "s"); s.Spares[slot] != spare {
		t.Fatalf("file spares %v, want %s in %s", s.Spares, spare, slot)
	}
	if r := b.Run(spare); r.State != "running" || len(b.Terminated()) != 0 {
		t.Fatalf("spare %s, b terminated %v", r.State, b.Terminated())
	}
}

// TestExtraSpareTerminateFails: an unrecorded spare of the session on b
// whose slot is taken is terminated at attach; while b fails every
// terminate it is kept as a leftover, and the next attach ends it.
func TestExtraSpareTerminateFails(t *testing.T) {
	h, b, spare := spareOnB(t)
	h.opts = func(o *Options) { o.NoticeFor = 5 * time.Second }
	h.detach()
	sp, err := config.RunSpec(h.cfg.Profiles["there"], config.Target{Session: "s", Profile: "there", Spare: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := luxapi.New(b.Config())
	extra, err := c.Submit(t.Context(), sp)
	if err != nil {
		t.Fatal(err)
	}
	b.FailTerminates(http.StatusServiceUnavailable)
	s, _, _ := state.Load(h.dir, "s")
	for slot := range s.Spares {
		s.Spares[slot] = extra.ID
	}
	if err := state.Save(h.dir, s); err != nil {
		t.Fatal(err)
	}
	h.start(nil)
	h.live(0)
	h.waitFor("the extra spare kept as a leftover", func() bool { return h.spares()[state.LeftoverPrefix+spare] == spare })
	if sp := h.spares(); !recordedSpare(sp, extra.ID) {
		t.Fatalf("spares %v; %s in its slot", sp, extra.ID)
	}
	h.detach()
	if s, _, _ := state.Load(h.dir, "s"); !recordedSpare(s.Spares, spare) {
		t.Fatalf("file spares %v", s.Spares)
	}
	if b.Run(spare).State != "running" {
		t.Fatalf("spare %s", b.Run(spare).State)
	}

	b.FailTerminates(0)
	h.start(nil)
	h.live(0)
	h.waitFor("the extra spare terminated", func() bool { return slices.Contains(b.Terminated(), spare) })
	h.waitFor("and forgotten", func() bool { return !recordedSpare(h.spares(), spare) })
	h.detach()
}

// TestClosedDuringSubmitTerminateFails: a window closed while its Run's
// submit to b is held, b then failing every terminate: the Run is kept
// as a leftover, and the next attach terminates it.
func TestClosedDuringSubmitTerminateFails(t *testing.T) {
	b := fakelux.NewWithKey(keyB)
	b.IDPrefix = "run_b"
	g := newGate()
	t.Cleanup(g.open)
	b.SubmitHook = g.wait
	t.Cleanup(b.Close)
	h := &harness{t: t, lux: fakelux.NewWithKey(keyA), rows: 24, cols: 80, dir: t.TempDir(),
		env: map[string]string{remoteEnv: keyB}, crashed: make(chan string, 1),
		opts: func(o *Options) { o.NoticeFor = 5 * time.Second }}
	t.Cleanup(h.lux.Close)
	cfg, err := config.Parse([]byte(cfgRemote(b.URL)))
	if err != nil {
		t.Fatal(err)
	}
	h.cfg = cfg
	h.start(nil)
	x := &twoLux{harness: h, b: b}
	x.live(0)
	x.newRemote()
	x.waitFor("the submit reached b", func() bool { return slices.Contains(b.Requests(), "POST /v1/runs") })
	x.m.Kill(1)
	x.waitFor("window 1 closed", func() bool { return x.nwindows() == 1 })
	b.FailTerminates(http.StatusServiceUnavailable)
	g.open()
	x.waitFor("b made the Run", func() bool { return len(b.RunIDs()) == 1 })
	id := b.RunIDs()[0]
	x.waitFor("the Run kept as a leftover", func() bool { return x.spares()[state.LeftoverPrefix+id] == id })
	x.detach()
	if s, _, _ := state.Load(x.dir, "s"); s.Spares[state.LeftoverPrefix+id] != id || len(s.Windows) != 1 {
		t.Fatalf("file windows %+v spares %v", s.Windows, s.Spares)
	}

	b.FailTerminates(0)
	x.start(nil)
	x.live(0)
	x.waitFor("the Run terminated", func() bool { return slices.Contains(b.Terminated(), id) })
	x.waitFor("and forgotten", func() bool { return !recordedSpare(x.spares(), id) })
	x.detach()
}
