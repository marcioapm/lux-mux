package mux

import (
	"context"
	"io"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/marcioapm/lux/vterm"

	"github.com/marcioapm/lux-mux/internal/config"
	"github.com/marcioapm/lux-mux/internal/fakelux"
	"github.com/marcioapm/lux-mux/internal/luxapi"
	"github.com/marcioapm/lux-mux/internal/sessions"
	"github.com/marcioapm/lux-mux/internal/state"
)

// multi runs sessions one after another in one process, as luxmux
// attach does: one Console, sessions switched by keys.
type multi struct {
	t    *testing.T
	lux  *fakelux.Server
	cl   *luxapi.Client
	dir  string
	cfg  *config.Config
	term *clientTerm
	keys *io.PipeWriter
	con  *Console
	ui   *fakeUI
	// done gets Attach's results.
	done chan [2]string
}

func newMulti(t *testing.T) *multi {
	t.Helper()
	h := &multi{t: t, lux: fakelux.New(), dir: t.TempDir(), term: newClientTerm(24, 80), ui: &fakeUI{choose: -1}}
	t.Cleanup(h.lux.Close)
	h.cl, _ = luxapi.New(h.lux.Config())
	cfg, err := config.Parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	h.cfg = cfg
	pr, pw := io.Pipe()
	h.keys = pw
	h.con = NewConsole(pr, h.term)
	t.Cleanup(func() { pw.Close() })
	return h
}

// attach opens session name as luxmux attach -t does (creating it when
// it has neither file nor Runs) and runs Attach in the background.
func (h *multi) attach(name string) {
	h.t.Helper()
	unlock, err := state.Lock(h.dir, name)
	if err != nil {
		h.t.Fatal(err)
	}
	sess, label, _, err := sessions.Open(context.Background(), h.cl, h.dir, h.cfg, name)
	if err != nil {
		h.t.Fatal(err)
	}
	opt := Options{Session: name, Label: label, Unlock: unlock, Console: h.con, Config: h.cfg, Lux: h.cl, StateDir: h.dir,
		Size: func() (int, int) { return 24, 80 }, UI: h.ui, Lookup: func(string) (string, bool) { return "", false },
		Crash:   func(msg string) { h.t.Errorf("crash: %s", msg); runtime.Goexit() },
		Backoff: 5 * time.Millisecond, MaxBackoff: 50 * time.Millisecond, NoticeFor: 300 * time.Millisecond,
		RestoreWait: 300 * time.Millisecond, Poll: 50 * time.Millisecond}
	h.done = make(chan [2]string, 1)
	go func() {
		r, n, err := Attach(context.Background(), opt, sess)
		if err != nil {
			h.t.Errorf("Attach: %v", err)
		}
		h.done <- [2]string{r, n}
	}()
}

// detach detaches the shown session and waits for Attach to return.
func (h *multi) detach() {
	h.t.Helper()
	h.type_("\x02d")
	select {
	case r := <-h.done:
		if r[0] != "detached" {
			h.t.Fatalf("Attach returned %q", r)
		}
	case <-time.After(10 * time.Second):
		h.t.Fatal("detach did not return")
	}
}

func (h *multi) type_(s string) {
	h.t.Helper()
	if _, err := h.keys.Write([]byte(s)); err != nil {
		h.t.Fatal(err)
	}
}

func (h *multi) waitFor(what string, cond func() bool) {
	h.t.Helper()
	if !fakelux.WaitFor(context.Background(), 10*time.Second, cond) {
		h.t.Fatalf("waiting for %s; screen:\n%s", what, h.term.screen())
	}
}

func (h *multi) waitScreen(what, s string) {
	h.t.Helper()
	h.waitFor(what, func() bool { return strings.Contains(h.term.screen(), s) })
}

// waitDrawn waits until s was written to the terminal since mark (a
// notice may be gone from the screen by the time it is read).
func (h *multi) waitDrawn(what string, mark int, s string) {
	h.t.Helper()
	h.waitFor(what, func() bool { return strings.Contains(string(h.term.rawBytes()[mark:]), s) })
}

// on waits until session name is shown with a live first window, and
// returns that window's Run.
func (h *multi) on(name string) string {
	h.t.Helper()
	h.waitFor("session "+name+" shown", func() bool {
		m := h.con.Current()
		if m == nil || m.Session() != name || m.ctx.Err() != nil {
			return false
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		w := m.activeLocked()
		return w != nil && w.phase == phaseLive && w.getConn() != nil
	})
	m := h.con.Current()
	m.mu.Lock()
	id := m.activeLocked().RunID
	m.mu.Unlock()
	h.waitFor("stream of "+id, func() bool { return h.lux.Streams(id) > 0 })
	return id
}

// make creates session name with one window showing text, then detaches.
func (h *multi) make(name, text string) string {
	h.t.Helper()
	h.attach(name)
	id := h.on(name)
	h.lux.Term(id).Output([]byte(text + "\r\n"))
	h.waitScreen(name+"'s text", text)
	h.detach()
	return id
}

// muxGoroutines counts goroutines running a session's code: its
// supervisors, stream readers, key writers, emulator feeders, event feed,
// discovery and preview.
func muxGoroutines() int {
	buf := make([]byte, 1<<22)
	buf = buf[:runtime.Stack(buf, true)]
	n := 0
	for _, g := range strings.Split(string(buf), "\n\n") {
		for _, f := range []string{"mux.(*Mux).supervise", "mux.(*Mux).attachOnce", "mux.(*Mux).keyWriter",
			"mux.(*Window).feeder", "mux.(*Mux).followEvents", "mux.(*Mux).discoverWindows", "mux.(*Mux).followPreview",
			"mux.(*Mux).inputLoop", "mux.(*Mux).keepSpares"} {
			if strings.Contains(g, f) {
				n++
				break
			}
		}
	}
	return n
}

// TestSwitchSessionAndBack: C-b s switches to another session in the same
// terminal and back; each screen is restored from lux, the lock moves,
// the Runs keep running, and nothing of the session left (streams,
// event feed, goroutines) survives.
func TestSwitchSessionAndBack(t *testing.T) {
	h := newMulti(t)
	idB := h.make("b", "B-SCREEN")
	h.attach("a")
	idA := h.on("a")
	h.lux.Term(idA).Output([]byte("A-SCREEN\r\n"))
	h.waitScreen("a's text", "A-SCREEN")
	h.waitFor("one feed", func() bool { return h.lux.Feeds() == 1 })
	before := muxGoroutines()

	h.ui.session = SessionChoice{Switch: "b"}
	h.type_("\x02s")
	if got := h.on("b"); got != idB {
		t.Fatalf("b shows %s, want %s", got, idB)
	}
	h.waitFor("b restored", func() bool {
		s := h.term.screen()
		return strings.Contains(s, "B-SCREEN") && !strings.Contains(s, "A-SCREEN")
	})
	if h.term.title() != "luxmux:b [0] default" {
		t.Fatalf("title %q", h.term.title())
	}
	h.waitFor("a's stream closed", func() bool { return h.lux.Streams(idA) == 0 })
	h.waitFor("one feed after the switch", func() bool { return h.lux.Feeds() == 1 })
	h.waitFor("a's goroutines gone", func() bool { return muxGoroutines() == before })
	if _, held := state.Held(h.dir, "a"); held {
		t.Fatal("a's lock is still held")
	}
	if _, err := state.Lock(h.dir, "b"); err == nil {
		t.Fatal("b's lock is not held")
	}
	if st := h.lux.Run(idA).State; st != "running" {
		t.Fatalf("a's Run %s after the switch", st)
	}
	if s, ok, _ := state.Load(h.dir, "a"); !ok || len(s.Windows) != 1 || s.Windows[0].RunID != idA {
		t.Fatalf("a's state after the switch: %+v", s)
	}

	h.ui.session = SessionChoice{Switch: "a"}
	h.type_("\x02s")
	h.on("a")
	h.waitFor("a restored", func() bool {
		s := h.term.screen()
		return strings.Contains(s, "A-SCREEN") && !strings.Contains(s, "B-SCREEN")
	})
	h.waitFor("b's stream closed", func() bool { return h.lux.Streams(idB) == 0 })
	h.waitFor("one feed after switching back", func() bool { return h.lux.Feeds() == 1 })
	h.waitFor("b's goroutines gone", func() bool { return muxGoroutines() == before })
	if _, held := state.Held(h.dir, "b"); held {
		t.Fatal("b's lock is still held")
	}
	// Keys go to the session shown, once.
	h.type_("typed-in-a")
	h.waitFor("keys in a", func() bool {
		return strings.Contains(termText(h.lux.Term(idA)), "typed-in-a") && !strings.Contains(termText(h.lux.Term(idB)), "typed-in-a")
	})
	h.detach()
	h.waitFor("no goroutines after detach", func() bool { return muxGoroutines() == 0 })
	if h.lux.Feeds() != 0 || h.lux.Streams(idA) != 0 || h.lux.Streams(idB) != 0 {
		t.Fatalf("after detach: %d feeds, streams %d %d", h.lux.Feeds(), h.lux.Streams(idA), h.lux.Streams(idB))
	}
}

// TestSwitchToLockedSessionStays: a target another process holds is
// refused before anything is torn down; one taken between the check and
// the lock leaves the current session reattached, with a notice.
func TestSwitchToLockedSessionStays(t *testing.T) {
	h := newMulti(t)
	h.make("b", "B-SCREEN")
	h.attach("a")
	idA := h.on("a")
	h.lux.Term(idA).Output([]byte("A-SCREEN\r\n"))
	unlock, err := state.Lock(h.dir, "b")
	if err != nil {
		t.Fatal(err)
	}
	m := h.con.Current()
	mark := len(h.term.rawBytes())
	if m.SwitchTo("b", "") {
		t.Fatal("switched to a locked session")
	}
	h.waitDrawn("refusal", mark, "session b is attached elsewhere")
	if h.con.Current() != m || m.ctx.Err() != nil {
		t.Fatal("the session was torn down for a refused switch")
	}
	// Past the check: the lock is taken when the switch tries it.
	mark = len(h.term.rawBytes())
	m.startSwitch(&switchTarget{name: "b"})
	h.waitFor("a again", func() bool { return h.con.Current() != m })
	h.on("a")
	h.waitDrawn("notice", mark, "session b: session is attached elsewhere")
	h.waitScreen("a restored", "A-SCREEN")
	if _, err := state.Lock(h.dir, "a"); err == nil {
		t.Fatal("a's lock is not held after the failed switch")
	}
	unlock()
	h.detach()
}

// TestSessionOrderSkipsLocked: C-b ) and C-b ( step through the sessions
// by name, wrapping, past one attached elsewhere, with a notice.
func TestSessionOrderSkipsLocked(t *testing.T) {
	h := newMulti(t)
	for _, n := range []string{"b", "c", "d"} {
		h.make(n, strings.ToUpper(n)+"-SCREEN")
	}
	unlock, err := state.Lock(h.dir, "c")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	h.attach("a")
	h.on("a")
	var order []string
	for _, k := range []string{")", ")", ")", "(", "("} {
		cur := h.con.Current()
		mark := len(h.term.rawBytes())
		h.type_("\x02" + k)
		h.waitFor("switched", func() bool { return h.con.Current() != cur })
		name := h.con.Current().Session()
		h.on(name)
		order = append(order, name)
		if name == "d" && k == ")" || name == "b" && k == "(" {
			h.waitDrawn("skip notice", mark, "skipped c: attached elsewhere")
		}
	}
	if want := []string{"b", "d", "a", "d", "b"}; !slices.Equal(order, want) {
		t.Fatalf("order %v, want %v", order, want)
	}
	h.detach()
}

// TestRenameSessionAndRebuild: C-b $ renames the session: its file, its
// lock and its title move to the new name, its Runs keep their label
// (mapped in the state directory), new windows get it too, the old name
// is refused, and a rebuild from labels finds the session by its new
// name.
func TestRenameSessionAndRebuild(t *testing.T) {
	h := newMulti(t)
	h.attach("a")
	id := h.on("a")
	h.lux.Term(id).Output([]byte("A-SCREEN\r\n"))
	h.type_("\x02$")
	h.waitScreen("prompt", "rename session: a")
	h.type_("\x15alpha\r")
	h.waitFor("renamed", func() bool { return h.con.Current().Session() == "alpha" })
	h.waitFor("title", func() bool { return h.term.title() == "luxmux:alpha [0] default" })
	if _, ok, _ := state.Load(h.dir, "a"); ok {
		t.Fatal("a.json is left")
	}
	if s, ok, _ := state.Load(h.dir, "alpha"); !ok || len(s.Windows) != 1 || s.Windows[0].RunID != id {
		t.Fatalf("alpha.json %+v", s)
	}
	if r, _ := state.Renames(h.dir); r["alpha"] != "a" {
		t.Fatalf("renames %v", r)
	}
	if _, held := state.Held(h.dir, "a"); held {
		t.Fatal("a's lock is still held")
	}
	if _, held := state.Held(h.dir, "alpha"); !held {
		t.Fatal("alpha's lock is not held")
	}
	h.type_("\x02c")
	h.waitFor("second window", func() bool { return len(h.lux.RunIDs()) == 2 })
	if l := h.lux.Run(h.lux.RunIDs()[1]).Labels[config.LabelSession]; l != "a" {
		t.Fatalf("a window made after the rename is labelled %q", l)
	}
	h.on("alpha")
	// The old name is the Runs' label: not free, and not attachable.
	ctx := context.Background()
	if err := sessions.CheckFree(ctx, h.cl, h.dir, "a", ""); err == nil {
		t.Fatal("the old name is free")
	}
	if _, err := sessions.Label(h.dir, "a"); err == nil || !strings.Contains(err.Error(), "renamed to alpha") {
		t.Fatalf("old name: %v", err)
	}
	h.detach()
	list, err := sessions.List(ctx, h.cl, h.dir)
	if err != nil || len(list) != 1 || list[0].Name != "alpha" || list[0].Windows != 2 {
		t.Fatalf("list %+v %v", list, err)
	}

	// Its file gone, alpha is rebuilt from the Runs' labels.
	if err := state.Remove(h.dir, "alpha"); err != nil {
		t.Fatal(err)
	}
	sess, label, existed, err := sessions.Open(ctx, h.cl, h.dir, h.cfg, "alpha")
	if err != nil || !existed || label != "a" || len(sess.Windows) != 2 || sess.Windows[0].RunID != id {
		t.Fatalf("rebuilt %+v %q %v %v", sess, label, existed, err)
	}
	h.lux.Term(id).Output([]byte("\x1b[2J\x1b[HREBUILT-A\r\n"))
	h.attach("alpha")
	h.on("alpha")
	h.waitScreen("rebuilt alpha restored", "REBUILT-A")
	h.detach()
}

// TestKillSessionFromChooser: the chooser's kill terminates that
// session's Runs and forgets it; the session attached here is refused.
func TestKillSessionFromChooser(t *testing.T) {
	h := newMulti(t)
	idB := h.make("b", "B-SCREEN")
	h.attach("a")
	idA := h.on("a")
	errs := make(chan [2]error, 1)
	h.ui.sessionOpen = func(m *Mux) {
		errs <- [2]error{m.KillSession("b"), m.KillSession("a")}
	}
	h.type_("\x02s")
	got := <-errs
	if got[0] != nil {
		t.Fatalf("kill b: %v", got[0])
	}
	if got[1] != ErrKillCurrent {
		t.Fatalf("kill of the current session: %v", got[1])
	}
	if st := h.lux.Run(idB).State; st != "terminated" {
		t.Fatalf("b's Run %s", st)
	}
	if st := h.lux.Run(idA).State; st != "running" {
		t.Fatalf("a's Run %s", st)
	}
	if _, ok, _ := state.Load(h.dir, "b"); ok {
		t.Fatal("b.json is left")
	}
	list, err := h.con.Current().Sessions()
	if err != nil || len(list) != 1 || list[0].Name != "a" || !list[0].Current {
		t.Fatalf("sessions after the kill: %+v %v", list, err)
	}
	h.detach()
}

// TestSessionPreview: the preview of another session's shown window comes
// from a restore attach that does not resize it; one attached elsewhere
// is not attached at all; the preview's stream ends with the chooser.
func TestSessionPreview(t *testing.T) {
	h := newMulti(t)
	idB := h.make("b", "B-SCREEN")
	idC := h.make("c", "C-SCREEN")
	h.attach("a")
	h.on("a")
	unlock, err := state.Lock(h.dir, "c")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	prev := make(chan [2][]string, 1)
	h.ui.sessionOpen = func(m *Mux) {
		var b []string
		fakelux.WaitFor(context.Background(), 5*time.Second, func() bool {
			b = m.SessionPreview("b", 10, 80)
			return strings.Contains(strings.Join(b, "\n"), "B-SCREEN")
		})
		prev <- [2][]string{b, m.SessionPreview("c", 10, 80)}
	}
	resizes := len(h.lux.Term(idB).Resizes())
	attachesC := 0
	for _, a := range h.lux.Attaches() {
		if a.RunID == idC {
			attachesC++
		}
	}
	h.type_("\x02s")
	got := <-prev
	if !strings.Contains(strings.Join(got[0], "\n"), "B-SCREEN") {
		t.Fatalf("b's preview %q", got[0])
	}
	if !strings.Contains(strings.Join(got[1], "\n"), "attached elsewhere — preview unavailable") {
		t.Fatalf("c's preview %q", got[1])
	}
	h.waitFor("preview stream closed", func() bool { return h.lux.Streams(idB) == 0 })
	if n := len(h.lux.Term(idB).Resizes()); n != resizes {
		t.Fatalf("the preview resized b's terminal: %v", h.lux.Term(idB).Resizes())
	}
	n := 0
	for _, a := range h.lux.Attaches() {
		if a.RunID == idC {
			n++
		}
	}
	if n != attachesC {
		t.Fatal("a session attached elsewhere was attached for its preview")
	}
	h.detach()
}

// termText is a fake Run terminal's screen as text.
func termText(t *fakelux.Terminal) string {
	var b strings.Builder
	t.Screen(func(vt *vterm.Terminal) {
		rows, cols := vt.Size()
		for y := range rows {
			for x := range cols {
				b.WriteString(vt.Text(vt.Cell(x, y)))
			}
			b.WriteByte('\n')
		}
	})
	return b.String()
}
