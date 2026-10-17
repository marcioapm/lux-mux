package mux

import (
	"bytes"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marcioapm/lux/api"

	"github.com/marcioapm/lux-mux/internal/fakelux"
	"github.com/marcioapm/lux-mux/internal/info"
)

// infoUI is the harness's fakeUI, collecting the info reads it starts.
func (h *harness) infoUI() *fakeUI {
	f := h.m.opt.UI.(*fakeUI)
	f.helpMu.Lock()
	if f.infoGot == nil {
		f.infoGot = make(chan info.Window, 8)
	}
	f.helpMu.Unlock()
	return f
}

func (f *fakeUI) nextInfo(t *testing.T) info.Window {
	t.Helper()
	select {
	case w := <-f.infoGot:
		return w
	case <-time.After(10 * time.Second):
		t.Fatal("no info read finished")
		return info.Window{}
	}
}

// TestInfoReadsShownWindowOnItsLux: C-b i reads the shown window from the
// lux it lives on, with that lux's key and tenant: window 1's Run is on
// b, and a (the lux CLI configuration's) is never asked for it.
func TestInfoReadsShownWindowOnItsLux(t *testing.T) {
	x := newTwoLux(t, nil)
	a0 := x.live(0)
	x.newRemote()
	b1 := x.liveOnB(1)
	x.lux.Update(a0, func(r *api.Run) { r.Host = "host-on-a" })
	x.b.Update(b1, func(r *api.Run) { r.Host = "host-on-b" })
	x.b.SetExecResponder(func(string, []string) (fakelux.ExecAnswer, bool) {
		return fakelux.ExecAnswer{Output: fakelux.ResourcesOutput("max 100000", "max", "4096", "10", 100, 10)}, true
	})
	f := x.infoUI()
	x.type_("\x02i")
	w := f.nextInfo(t)
	if w.RunID != b1 || w.Index != 1 || w.Placement == nil || w.Placement.Host != "host-on-b" || w.Problem != "" ||
		w.Endpoint != x.cfg.Profiles["remote"].Endpoint(x.lux.Config()).Label() {
		t.Fatalf("window 1's info: %+v", w)
	}
	if w.Resources == nil || *w.Resources.MemoryBytes != 4096 || len(x.b.Execs()) == 0 {
		t.Fatalf("resources from b: %+v", w.Resources)
	}
	for _, r := range x.lux.Requests() {
		if strings.Contains(r, b1) {
			t.Fatalf("a was asked about b's Run: %s", r)
		}
	}
	x.type_("\x020\x02i")
	if w := f.nextInfo(t); w.RunID != a0 || w.Placement == nil || w.Placement.Host != "host-on-a" {
		t.Fatalf("window 0's info: %+v", w)
	}
	if x.lux.Refused() != 0 || x.b.Refused() != 0 {
		t.Fatal("a request with the wrong key")
	}
	noKeys(t, x.dir, x.term.rawBytes())
	x.detach()
}

// TestInputResponsiveWhileInfoHangs: with lux hanging on the info read
// (its Run GET), keystrokes still reach the shown window's shell at once,
// output still paints, and C-b d detaches without waiting for the read.
func TestInputResponsiveWhileInfoHangs(t *testing.T) {
	var keys inputLog
	h := newHarness(t, cfgDefault, func(l *fakelux.Server) { l.InputHook = keys.hook })
	id := h.live(0)
	h.type_("ready\r\n")
	h.waitScreen("echo", h.contains("ready"))
	g := newGate()
	t.Cleanup(g.open)
	var held atomic.Int32
	h.lux.SetGetHook(func(runID string) {
		held.Add(1)
		g.wait()
	})
	f := h.infoUI()
	h.type_("\x02i")
	h.waitFor("the info read hangs in lux", func() bool { return held.Load() > 0 })
	// Each key reaches the stream within the bound while the read hangs.
	const bound = 500 * time.Millisecond
	for i := range 20 {
		k := []byte{'a' + byte(i)}
		before := bytes.Count(keys.of(id), k)
		start := time.Now()
		h.type_(string(k))
		h.waitFor("key delivered", func() bool { return bytes.Count(keys.of(id), k) > before })
		if d := time.Since(start); d > bound {
			t.Fatalf("key %d took %v with the info read hung", i, d)
		}
	}
	h.lux.Term(id).Output([]byte("\r\nPAINTED-WHILE-HUNG\r\n"))
	h.waitScreen("output painted", h.contains("PAINTED-WHILE-HUNG"))
	select {
	case w := <-f.infoGot:
		t.Fatalf("the read answered while lux hung: %+v", w)
	default:
	}
	start := time.Now()
	h.detach()
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("detach took %v with an info read hung", d)
	}
	g.open()
	// The session ended: the read's context did, so it ends too.
	if w := f.nextInfo(t); w.Problem == "" {
		t.Fatalf("a read cut off by the session's end: %+v", w)
	}
}

// TestInfoTargetIsShownWindow: InfoTarget names the shown window by its
// index, name, profile, session and lux, and follows a switch and a
// rename.
func TestInfoTargetIsShownWindow(t *testing.T) {
	h := newHarness(t, cfgDefault, nil)
	a := h.live(0)
	tg, ok := h.m.InfoTarget()
	if !ok || tg.RunID != a || tg.Index != 0 || tg.Session != "s" || tg.Label != "s" || tg.Endpoint != h.m.eps.Default() {
		t.Fatalf("%+v %v", tg, ok)
	}
	h.type_("\x02c")
	h.waitFor("second window", func() bool { return h.nwindows() == 2 })
	b := h.live(1)
	h.m.Rename(1, "renamed")
	if tg, _ := h.m.InfoTarget(); tg.RunID != b || tg.Index != 1 || tg.Name != "renamed" {
		t.Fatalf("after the switch: %+v", tg)
	}
	h.detach()
}
