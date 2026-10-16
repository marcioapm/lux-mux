package mux

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marcioapm/lux-mux/internal/config"
	"github.com/marcioapm/lux-mux/internal/fakelux"
	"github.com/marcioapm/lux-mux/internal/luxapi"
	"github.com/marcioapm/lux-mux/internal/sessions"
	"github.com/marcioapm/lux-mux/internal/state"
)

// multiTwo is multi on two luxes: the lux CLI configuration's (h.lux) and
// b, which profile remote is on (cfgRemote), its key read from remoteEnv
// as bKey says.
type multiTwo struct {
	*multi
	b    *fakelux.Server
	bKey atomic.Pointer[string]
}

func newMultiTwo(t *testing.T) *multiTwo {
	t.Helper()
	h := &multiTwo{multi: newMulti(t), b: fakelux.NewWithKey(keyB)}
	h.b.IDPrefix = "run_b"
	t.Cleanup(h.b.Close)
	k := keyB
	h.bKey.Store(&k)
	cfg, err := config.Parse([]byte(cfgRemote(h.b.URL)))
	if err != nil {
		t.Fatal(err)
	}
	h.cfg = cfg
	h.eps = luxapi.NewEndpoints(h.lux.Config(), func(n string) (string, bool) {
		if n == remoteEnv {
			return *h.bKey.Load(), true
		}
		return "", false
	})
	h.cl, _ = h.eps.Client(h.eps.Default())
	h.cl.C.HTTP = &http.Client{Transport: h.rt}
	return h
}

// on waits until session name is shown with a live active window whose
// stream is open on its own lux, and returns that window's Run.
func (h *multiTwo) on(name string) string {
	h.t.Helper()
	var id string
	var onB bool
	h.waitFor("session "+name+" shown", func() bool {
		m := h.con.Current()
		if m == nil || m.Session() != name || m.ctx.Err() != nil {
			return false
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		w := m.activeLocked()
		if w == nil || w.phase != phaseLive || w.getConn() == nil {
			return false
		}
		id, onB = w.RunID, w.Endpoint == h.bEP()
		return true
	})
	l := h.lux
	if onB {
		l = h.b
	}
	h.waitFor("stream of "+id, func() bool { return slices.Contains(l.RunIDs(), id) && l.Streams(id) > 0 })
	return id
}

// bEP is the endpoint of profile remote, on b.
func (h *multiTwo) bEP() config.Endpoint { return h.cfg.Profiles["remote"].Endpoint(h.lux.Config()) }

// remoteWindow opens a window of profile remote in the session shown and
// waits until it is attached to its Run on b, which it returns.
func (h *multiTwo) remoteWindow() string {
	h.t.Helper()
	m := h.con.Current()
	n := len(m.windowRuns())
	h.ui.req = &NewWindowRequest{ProfileName: "remote", Profile: h.cfg.Profiles["remote"]}
	h.type_("\x02C")
	var id string
	h.waitFor("remote window live", func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		if len(m.windows) <= n {
			return false
		}
		w := m.windows[n]
		id = w.RunID
		return w.phase == phaseLive && w.getConn() != nil
	})
	h.waitFor("stream on b", func() bool { return h.b.Streams(id) > 0 })
	return id
}

// submitLabelled submits a Run of profile on l labelled session label.
func submitLabelled(t *testing.T, l *fakelux.Server, label, profile string) string {
	t.Helper()
	c, _ := luxapi.New(l.Config())
	sp, err := config.RunSpec(config.Profile{Image: "img"}, config.Target{Session: label, Profile: profile}, nil)
	if err != nil {
		t.Fatal(err)
	}
	r, err := c.Submit(context.Background(), sp)
	if err != nil {
		t.Fatal(err)
	}
	if !fakelux.WaitFor(context.Background(), 5*time.Second, func() bool { return l.Run(r.ID).State == "running" }) {
		t.Fatalf("%s not running", r.ID)
	}
	return r.ID
}

// attachesOn counts l's attaches to Run id.
func attachesOn(l *fakelux.Server, id string) int {
	n := 0
	for _, a := range l.Attaches() {
		if a.RunID == id {
			n++
		}
	}
	return n
}

// TestSwitchAcrossEndpoints: a switch from a session on one lux to one
// with a window on a second lux ends the first session's event feeds,
// discovery and spare goroutines, opens the target on both luxes, and
// attaches each of its windows on its own lux; switching back undoes it.
func TestSwitchAcrossEndpoints(t *testing.T) {
	h := newMultiTwo(t)
	base, _ := muxGoroutines()
	h.attach("b")
	b0 := h.on("b")
	b1 := h.remoteWindow()
	h.b.Term(b1).Output([]byte("B-REMOTE\r\n"))
	h.waitScreen("b1's text", "B-REMOTE")
	h.waitFor("b alone: one feed on each lux", func() bool { return h.lux.Feeds() == 1 && h.b.Feeds() == 1 })
	onlyB, _ := muxGoroutines()
	h.detach()

	h.attach("a")
	idA := h.on("a")
	feeds := func(a, b int) func() bool {
		return func() bool { return h.lux.Feeds() == a && h.b.Feeds() == b }
	}
	h.waitFor("a: one feed on each lux", feeds(1, 1))
	before, _ := muxGoroutines()

	h.ui.session = SessionChoice{Switch: "b"}
	h.type_("\x02s")
	if got := h.on("b"); got != b1 {
		t.Fatalf("b shows %s, want its window on the second lux %s", got, b1)
	}
	h.waitScreen("b1 restored from b", "B-REMOTE")
	h.waitFor("b's windows attached on their own luxes", func() bool {
		return h.lux.Streams(b0) > 0 && h.b.Streams(b1) > 0 && h.lux.Streams(idA) == 0
	})
	if attachesOn(h.lux, b1) != 0 || attachesOn(h.b, b0) != 0 {
		t.Fatal("a window was attached on the other lux")
	}
	h.waitFor("after the switch: one feed on each lux", feeds(1, 1))
	h.goroutinesAtMost("a's goroutines gone after the switch", onlyB)
	if s, _, _ := state.Load(h.dir, "b"); len(s.Windows) != 2 || s.Windows[1].Endpoint == nil || *s.Windows[1].Endpoint != h.bEP() {
		t.Fatalf("b.json %+v", s)
	}

	h.ui.session = SessionChoice{Switch: "a"}
	h.type_("\x02s")
	h.on("a")
	h.waitFor("b's streams closed on both luxes", func() bool { return h.lux.Streams(b0) == 0 && h.b.Streams(b1) == 0 })
	h.waitFor("back on a: one feed on each lux", feeds(1, 1))
	h.goroutinesAtMost("b's goroutines gone after switching back", before)
	h.detach()
	h.waitFor("no feed after the detach", feeds(0, 0))
	h.goroutinesAtMost("no session goroutines after the detach", base)
	noKeys(t, h.dir, h.term.rawBytes())
}

// TestChooserListsOtherLux: the session chooser lists a session whose
// only Runs are on the second lux; with that lux down, a session whose
// file has a window there is marked unreachable and the others listed.
func TestChooserListsOtherLux(t *testing.T) {
	h := newMultiTwo(t)
	far := submitLabelled(t, h.b, "far", "remote")
	ep := h.bEP()
	if err := state.Save(h.dir, &state.Session{Name: "there", Windows: []state.Window{{RunID: far, Endpoint: &ep}}}); err != nil {
		t.Fatal(err)
	}
	h.attach("a")
	h.on("a")
	lists := make(chan []SessionInfo, 2)
	h.ui.sessionOpen = func(m *Mux) {
		l, err := m.Sessions()
		if err != nil {
			t.Errorf("sessions: %v", err)
		}
		lists <- l
	}
	find := func(l []SessionInfo, name string) *SessionInfo {
		for i := range l {
			if l[i].Name == name {
				return &l[i]
			}
		}
		return nil
	}
	h.type_("\x02s")
	l := <-lists
	if s := find(l, "far"); s == nil || s.Windows != 1 || s.Running != 1 || s.HasFile {
		t.Fatalf("far in the chooser: %+v (list %+v)", s, l)
	}
	if s := find(l, "there"); s == nil || len(s.Unreachable) != 0 {
		t.Fatalf("there with b up: %+v", s)
	}
	h.on("a")
	h.b.SetDown(true)
	h.type_("\x02s")
	l = <-lists
	if s := find(l, "there"); s == nil || len(s.Unreachable) != 1 || !strings.Contains(s.Unreachable[0], "endpoint unreachable: "+ep.Label()) {
		t.Fatalf("there with b down: %+v", s)
	}
	if find(l, "a") == nil {
		t.Fatalf("a missing with b down: %+v", l)
	}
	h.on("a")
	h.detach()
}

// TestChooserKillRefusedKeepsFile: the chooser's kill of a session whose
// window and leftover are on the second lux, while that lux refuses the
// terminate or the key, fails naming the lux, and keeps the file with
// its leftover; both Runs keep running.
func TestChooserKillRefusedKeepsFile(t *testing.T) {
	for _, how := range []string{"terminate refused", "key refused"} {
		t.Run(how, func(t *testing.T) {
			h := newMultiTwo(t)
			ep := h.bEP()
			win := submitLabelled(t, h.b, "far", "remote")
			left := submitLabelled(t, h.b, "far", "remote")
			s := &state.Session{Name: "far", Windows: []state.Window{{RunID: win, Profile: "remote", Endpoint: &ep}}}
			s.SetSpare(state.LeftoverPrefix+left, left, ep)
			if err := state.Save(h.dir, s); err != nil {
				t.Fatal(err)
			}
			switch how {
			case "terminate refused":
				h.b.FailTerminates(http.StatusForbidden)
			case "key refused":
				// Before any client for b is made: b refuses every request.
				k := keyA
				h.bKey.Store(&k)
			}
			h.attach("a")
			h.on("a")
			errs := make(chan error, 1)
			h.ui.sessionOpen = func(m *Mux) { errs <- m.KillSession("far") }
			h.type_("\x02s")
			err := <-errs
			want := map[string]string{"terminate refused": ep.Label(), "key refused": "session file kept"}[how]
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("kill: %v, want it to say %q", err, want)
			}
			if how == "terminate refused" && (!strings.Contains(err.Error(), win) || !strings.Contains(err.Error(), left)) {
				t.Fatalf("kill: %v, want both Runs named", err)
			}
			f, ok, _ := state.Load(h.dir, "far")
			if !ok || f.Spares[state.LeftoverPrefix+left] != left || f.SpareEndpoint(state.LeftoverPrefix+left, config.Endpoint{}) != ep ||
				len(f.Windows) != 1 || f.Windows[0].RunID != win {
				t.Fatalf("far.json after the failed kill: %+v %v", f, ok)
			}
			for _, id := range []string{win, left} {
				if st := h.b.Run(id).State; st != "running" {
					t.Fatalf("%s %s after the failed kill", id, st)
				}
			}
			h.on("a")
			h.detach()
		})
	}
}

// TestRenameRebuildAcrossEndpoints: a session with a window on each lux
// renamed with C-b $ keeps its Runs' label on both; a window made on the
// second lux after the rename gets it too; with its file gone, the
// session is rebuilt under its new name from both luxes, each window on
// its own lux, and attached.
func TestRenameRebuildAcrossEndpoints(t *testing.T) {
	h := newMultiTwo(t)
	h.attach("a")
	a0 := h.on("a")
	a1 := h.remoteWindow()
	h.type_("\x02$")
	h.waitScreen("prompt", "rename session: a")
	h.type_("\x15alpha\r")
	h.waitFor("renamed", func() bool { return h.con.Current().Session() == "alpha" })
	a2 := h.remoteWindow()
	if l := h.b.Run(a2).Labels[config.LabelSession]; l != "a" {
		t.Fatalf("a window made on b after the rename is labelled %q", l)
	}
	h.detach()
	ctx := context.Background()
	list, err := sessions.List(ctx, h.luxes(), h.dir)
	if err != nil || len(list) != 1 || list[0].Name != "alpha" || list[0].Windows != 3 {
		t.Fatalf("list %+v %v", list, err)
	}
	if err := checkFree(ctx, h.luxes(), h.dir, "a"); err == nil {
		t.Fatal("the old name, the label of Runs on both luxes, is free")
	}

	if err := state.Remove(h.dir, "alpha"); err != nil {
		t.Fatal(err)
	}
	sess, label, existed, err := sessions.Open(ctx, h.luxes(), h.dir, "alpha")
	if err != nil || !existed || label != "a" || len(sess.Windows) != 3 {
		t.Fatalf("rebuilt %+v %q %v %v", sess, label, existed, err)
	}
	want := map[string]config.Endpoint{a0: config.DefaultEndpoint(h.lux.Config()), a1: h.bEP(), a2: h.bEP()}
	for _, w := range sess.Windows {
		if ep, ok := want[w.RunID]; !ok || w.Endpoint == nil || *w.Endpoint != ep {
			t.Fatalf("rebuilt window %+v, want %v", w, want)
		}
	}
	h.attach("alpha")
	h.on("alpha")
	h.waitFor("each window on its own lux", func() bool {
		return h.lux.Streams(a0) > 0 && h.b.Streams(a1) > 0 && h.b.Streams(a2) > 0
	})
	h.detach()

	// Rebuilt again while b is down: b is unlisted, and its windows are
	// added under the new name once it answers, found by their label.
	if err := state.Remove(h.dir, "alpha"); err != nil {
		t.Fatal(err)
	}
	h.b.SetDown(true)
	h.attach("alpha")
	h.on("alpha")
	m := h.con.Current()
	if got := m.windowRuns(); !slices.Equal(got, []string{a0}) {
		t.Fatalf("windows with b down: %v", got)
	}
	h.b.SetDown(false)
	h.waitFor("b's windows added", func() bool { return len(m.windowRuns()) == 3 })
	h.waitFor("b's windows attached on b", func() bool { return h.b.Streams(a1) > 0 && h.b.Streams(a2) > 0 })
	if s, _, _ := state.Load(h.dir, "alpha"); s == nil || len(s.Windows) != 3 || len(s.Unlisted) != 0 {
		t.Fatalf("alpha.json %+v", s)
	}
	h.detach()
}

// TestSessionPreviewOtherLux: the chooser's preview of a session whose
// shown window is on the second lux is a restore attach there, for a
// session with a file and for one rebuilt from b's Runs alone; lux a is
// never asked for it.
func TestSessionPreviewOtherLux(t *testing.T) {
	h := newMultiTwo(t)
	h.attach("far")
	h.on("far")
	f1 := h.remoteWindow()
	h.b.Term(f1).Output([]byte("\x1b[2J\x1b[HFAR-B-SCREEN\r\n"))
	h.waitScreen("far's text", "FAR-B-SCREEN")
	h.detach()
	only := submitLabelled(t, h.b, "only", "remote")
	h.b.Term(only).Output([]byte("\x1b[2J\x1b[HONLY-B-SCREEN\r\n"))
	h.attach("a")
	h.on("a")
	before := attachesOn(h.b, f1)
	got := make(chan [2]string, 1)
	h.ui.sessionOpen = func(m *Mux) {
		var out [2]string
		for i, c := range [][2]string{{"far", "FAR-B-SCREEN"}, {"only", "ONLY-B-SCREEN"}} {
			fakelux.WaitFor(context.Background(), 5*time.Second, func() bool {
				out[i] = strings.Join(m.SessionPreview(c[0], 10, 80), "\n")
				return strings.Contains(out[i], c[1])
			})
		}
		got <- out
	}
	h.type_("\x02s")
	out := <-got
	if !strings.Contains(out[0], "FAR-B-SCREEN") || !strings.Contains(out[1], "ONLY-B-SCREEN") {
		t.Fatalf("previews %q", out)
	}
	if attachesOn(h.b, f1) <= before || attachesOn(h.b, only) == 0 {
		t.Fatal("no preview attach on b")
	}
	if attachesOn(h.lux, f1) != 0 || attachesOn(h.lux, only) != 0 {
		t.Fatal("a preview was attached on lux a")
	}
	h.on("a")
	h.waitFor("preview streams closed", func() bool { return h.b.Streams(f1) == 0 && h.b.Streams(only) == 0 })
	h.detach()
}

// TestSwitchLeavesLeftoverTerminate: a switch does not wait for a
// leftover's terminate on the second lux that has not answered, and that
// terminate, once it ends, does not touch the file the session saved nor
// the session switched to.
func TestSwitchLeavesLeftoverTerminate(t *testing.T) {
	h := newMultiTwo(t)
	h.make("c", "C-SCREEN")
	ep := h.bEP()
	left := submitLabelled(t, h.b, "a", "remote")
	s := &state.Session{Name: "a"}
	s.SetSpare(state.LeftoverPrefix+left, left, ep)
	if err := state.Save(h.dir, s); err != nil {
		t.Fatal(err)
	}
	gate, entered := make(chan struct{}), make(chan struct{}, 1)
	var once sync.Once
	release := func() { once.Do(func() { close(gate) }) }
	t.Cleanup(release)
	h.b.SetTerminateHook(func(id string) {
		if id == left {
			select {
			case entered <- struct{}{}:
			default:
			}
			<-gate
		}
	})
	h.attach("a")
	h.on("a")
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the leftover's terminate was not sent")
	}
	start := time.Now()
	h.ui.session = SessionChoice{Switch: "c"}
	h.type_("\x02s")
	h.on("c")
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("the switch took %v with a leftover terminate in flight", d)
	}
	release()
	h.waitFor("the leftover terminated", func() bool { return slices.Contains(h.b.Terminated(), left) })
	time.Sleep(200 * time.Millisecond)
	if f, _, _ := state.Load(h.dir, "a"); f == nil || f.Spares[state.LeftoverPrefix+left] != left {
		t.Fatalf("a.json after the switch: %+v", f)
	}
	m := h.con.Current()
	m.mu.Lock()
	_, inC := m.sess.Spares[state.LeftoverPrefix+left]
	m.mu.Unlock()
	if inC || m.Session() != "c" {
		t.Fatalf("session %s holds a's leftover: %v", m.Session(), inC)
	}
	h.detach()
}
