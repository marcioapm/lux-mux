package mux

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"runtime/debug"
	"slices"
	"strings"
	"sync/atomic"
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
	rt   *hookRT
	dir  string
	cfg  *config.Config
	term *clientTerm
	keys *io.PipeWriter
	con  *Console
	ui   *fakeUI
	// opts, when set, changes each session's Options.
	opts func(*Options)
	// done gets Attach's results.
	done chan [2]string
}

// hookRT is the lux client's transport: hook, when set, sees each request
// first and fails it by returning an error (it may block first).
type hookRT struct {
	base http.RoundTripper
	hook atomic.Pointer[func(*http.Request) error]
}

func (h *hookRT) RoundTrip(req *http.Request) (*http.Response, error) {
	if f := h.hook.Load(); f != nil {
		if err := (*f)(req); err != nil {
			return nil, err
		}
	}
	return h.base.RoundTrip(req)
}

// set installs f as the hook (nil: none).
func (h *hookRT) set(f func(*http.Request) error) {
	if f == nil {
		h.hook.Store(nil)
		return
	}
	h.hook.Store(&f)
}

// listing reports a request listing session label's Runs.
func listing(req *http.Request, label string) bool {
	return req.Method == http.MethodGet && req.URL.Path == "/v1/runs" &&
		req.URL.Query().Get("label") == config.LabelSession+"="+label
}

// hang blocks a request until the caller gives up on it.
func hang(req *http.Request) error {
	<-req.Context().Done()
	return req.Context().Err()
}

func newMulti(t *testing.T) *multi {
	t.Helper()
	h := &multi{t: t, lux: fakelux.New(), dir: t.TempDir(), term: newClientTerm(24, 80), ui: &fakeUI{choose: -1}}
	t.Cleanup(h.lux.Close)
	h.cl, _ = luxapi.New(h.lux.Config())
	h.rt = &hookRT{base: http.DefaultTransport}
	h.cl.C.HTTP = &http.Client{Transport: h.rt}
	cfg, err := config.Parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	h.cfg = cfg
	pr, pw := io.Pipe()
	h.keys = pw
	h.con = NewConsole(pr, h.term)
	t.Cleanup(func() {
		pw.Close()
		h.con.Close(time.Second)
	})
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
	if h.opts != nil {
		h.opts(&opt)
	}
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
// discovery and preview. Their stacks are returned for failure messages.
func muxGoroutines() (int, string) {
	buf := make([]byte, 1<<22)
	buf = buf[:runtime.Stack(buf, true)]
	n := 0
	var found []string
	for _, g := range strings.Split(string(buf), "\n\n") {
		for _, f := range []string{"mux.(*Mux).supervise", "mux.(*Mux).attachOnce", "mux.(*Mux).keyWriter",
			"mux.(*Window).feeder", "mux.(*Mux).followEvents", "mux.(*Mux).discoverWindows", "mux.(*Mux).followPreview",
			"mux.(*Mux).inputLoop", "mux.(*Mux).keepSpares"} {
			if strings.Contains(g, f) {
				n++
				found = append(found, g)
				break
			}
		}
	}
	return n, strings.Join(found, "\n\n")
}

// goroutinesAtMost waits until at most n session goroutines run. Other
// tests' sessions may still be ending: n is counted against a baseline
// taken before this test started any.
func (h *multi) goroutinesAtMost(what string, n int) {
	h.t.Helper()
	if !fakelux.WaitFor(context.Background(), 10*time.Second, func() bool { c, _ := muxGoroutines(); return c <= n }) {
		c, stacks := muxGoroutines()
		h.t.Fatalf("%s: %d session goroutines, want at most %d:\n%s", what, c, n, stacks)
	}
}

// TestSwitchSessionAndBack: C-b s switches to another session in the same
// terminal and back; each screen is restored from lux, the lock moves,
// the Runs keep running, and nothing of the session left (streams,
// event feed, goroutines) survives.
func TestSwitchSessionAndBack(t *testing.T) {
	h := newMulti(t)
	base, _ := muxGoroutines()
	idB := h.make("b", "B-SCREEN")
	h.attach("a")
	idA := h.on("a")
	h.lux.Term(idA).Output([]byte("A-SCREEN\r\n"))
	h.waitScreen("a's text", "A-SCREEN")
	h.waitFor("one feed", func() bool { return h.lux.Feeds() == 1 })
	before, _ := muxGoroutines()
	if before <= base {
		t.Fatalf("%d session goroutines with a attached, %d before: the count sees none", before, base)
	}

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
	h.goroutinesAtMost("a's goroutines gone after the switch", before)
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
	h.goroutinesAtMost("b's goroutines gone after switching back", before)
	if _, held := state.Held(h.dir, "b"); held {
		t.Fatal("b's lock is still held")
	}
	// Keys go to the session shown, once.
	h.type_("typed-in-a")
	h.waitFor("keys in a", func() bool {
		return strings.Contains(termText(h.lux.Term(idA)), "typed-in-a") && !strings.Contains(termText(h.lux.Term(idB)), "typed-in-a")
	})
	h.detach()
	h.goroutinesAtMost("no session goroutines after the detach", base)
	h.waitFor("feeds and streams closed after detach", func() bool {
		return h.lux.Feeds() == 0 && h.lux.Streams(idA) == 0 && h.lux.Streams(idB) == 0
	})
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

// stayedOn checks that a failed switch from m left session name shown
// again, with a notice that drew want since mark, and Attach running.
func (h *multi) stayedOn(m *Mux, name string, mark int, want string) {
	h.t.Helper()
	h.waitFor(name+" again", func() bool { return h.con.Current() != m })
	h.on(name)
	h.waitDrawn("switching notice", mark, "switching to session ")
	h.waitDrawn("failure notice", mark, want)
	select {
	case r := <-h.done:
		h.t.Fatalf("Attach returned %q after a failed switch", r)
	default:
	}
}

// TestSwitchTargetHangsStays: lux hanging on the target's listing ends the
// switch after its own timeout; the session is reattached from the state
// it saved, with a notice, and luxmux goes on.
func TestSwitchTargetHangsStays(t *testing.T) {
	h := newMulti(t)
	h.opts = func(o *Options) { o.switchOpenWait = time.Second }
	h.make("b", "B-SCREEN")
	h.attach("a")
	idA := h.on("a")
	h.lux.Term(idA).Output([]byte("A-SCREEN\r\n"))
	h.waitScreen("a's text", "A-SCREEN")
	h.rt.set(func(req *http.Request) error {
		if listing(req, "b") {
			return hang(req)
		}
		return nil
	})
	m := h.con.Current()
	mark := len(h.term.rawBytes())
	start := time.Now()
	m.startSwitch(&switchTarget{name: "b"})
	h.stayedOn(m, "a", mark, "session b: ")
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("back on a after %v", d)
	}
	h.waitScreen("a restored", "A-SCREEN")
	if _, held := state.Held(h.dir, "b"); held {
		t.Fatal("b's lock is held after its open timed out")
	}
	h.rt.set(nil)
	h.detach()
}

// TestSwitchTimedOutSkipsRefresh: when the target's open timed out, the
// session switched back to is not re-read from lux (which hangs on it
// too) but reattached from its saved state, well within two
// switchOpenWaits.
func TestSwitchTimedOutSkipsRefresh(t *testing.T) {
	h := newMulti(t)
	h.opts = func(o *Options) { o.switchOpenWait = 1500 * time.Millisecond }
	h.make("b", "B-SCREEN")
	h.attach("a")
	h.on("a")
	h.rt.set(func(req *http.Request) error {
		if listing(req, "b") || listing(req, "a") {
			return hang(req)
		}
		return nil
	})
	m := h.con.Current()
	mark := len(h.term.rawBytes())
	start := time.Now()
	m.startSwitch(&switchTarget{name: "b"})
	h.stayedOn(m, "a", mark, "session b: ")
	if d := time.Since(start); d > 2500*time.Millisecond {
		t.Fatalf("back on a after %v: the refresh waited on lux too", d)
	}
	h.rt.set(nil)
	h.detach()
}

// TestSwitchLuxRefusesStays: with lux refusing every listing, a failed
// switch does not need lux to go back: the session is reattached from
// the state it saved, with a notice, and luxmux goes on.
func TestSwitchLuxRefusesStays(t *testing.T) {
	h := newMulti(t)
	h.make("b", "B-SCREEN")
	h.attach("a")
	idA := h.on("a")
	h.lux.Term(idA).Output([]byte("A-SCREEN\r\n"))
	h.waitScreen("a's text", "A-SCREEN")
	refused := errors.New("lux refused the listing")
	h.rt.set(func(req *http.Request) error {
		if req.Method == http.MethodGet && req.URL.Path == "/v1/runs" {
			return refused
		}
		return nil
	})
	m := h.con.Current()
	mark := len(h.term.rawBytes())
	m.startSwitch(&switchTarget{name: "b"})
	// The notice is cut to the terminal's width before the error's end.
	h.stayedOn(m, "a", mark, "session b: Get ")
	h.waitScreen("a restored", "A-SCREEN")
	if got := h.con.Current().windowRuns(); !slices.Equal(got, []string{idA}) {
		t.Fatalf("a's windows %v, want [%s]", got, idA)
	}
	h.rt.set(nil)
	h.detach()
}

// TestStepWhileLuxHangsKeepsKeys: C-b ) and C-b ( with lux hanging on
// the sessions' listing leave the keyboard to the session: what is typed
// next reaches its shell, and C-b d detaches at once.
func TestStepWhileLuxHangsKeepsKeys(t *testing.T) {
	h := newMulti(t)
	h.make("b", "B-SCREEN")
	h.attach("a")
	idA := h.on("a")
	var lists atomic.Int32
	h.rt.set(func(req *http.Request) error {
		if req.Method == http.MethodGet && req.URL.Path == "/v1/runs" && req.URL.Query().Get("label") == config.LabelApp+"="+config.AppName {
			lists.Add(1)
			return hang(req)
		}
		return nil
	})
	h.type_("\x02)")
	h.waitFor("the listing asked", func() bool { return lists.Load() == 1 })
	h.type_("\x02(")
	h.type_("typed-while-listing")
	h.waitFor("keys in a", func() bool { return strings.Contains(termText(h.lux.Term(idA)), "typed-while-listing") })
	if lists.Load() != 1 {
		t.Fatalf("%d listings: a step while one lists starts another", lists.Load())
	}
	start := time.Now()
	h.detach()
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("detach took %v with a listing hung", d)
	}
}

// TestSwitchToVanishedSessionStays: a target gone between the chooser's
// listing and Enter (killed elsewhere) leaves this session reattached
// with a notice, and the target's lock free. GC is off: a leaked lock's
// descriptor must not be closed by its finalizer.
func TestSwitchToVanishedSessionStays(t *testing.T) {
	defer debug.SetGCPercent(debug.SetGCPercent(-1))
	h := newMulti(t)
	h.attach("a")
	idA := h.on("a")
	h.lux.Term(idA).Output([]byte("A-SCREEN\r\n"))
	m := h.con.Current()
	mark := len(h.term.rawBytes())
	m.startSwitch(&switchTarget{name: "ghost"})
	h.stayedOn(m, "a", mark, "session ghost: no session ghost")
	h.waitScreen("a restored", "A-SCREEN")
	if _, held := state.Held(h.dir, "ghost"); held {
		t.Fatal("ghost's lock is held after its open failed")
	}
	if u, err := state.Lock(h.dir, "ghost"); err != nil {
		t.Fatalf("ghost's lock after the failed switch: %v", err)
	} else {
		u()
	}
	h.detach()
}

// TestSwitchRelocksAfterBriefHold: a failed switch retries this
// session's lock while another process holds it for a moment (luxmux
// new-window -t), instead of ending luxmux. The 150 ms hold outlasts
// relockTries tries without relockEvery between them.
func TestSwitchRelocksAfterBriefHold(t *testing.T) {
	h := newMulti(t)
	h.attach("a")
	h.on("a")
	var held atomic.Bool
	m := h.con.Current()
	m.mu.Lock()
	m.opt.afterSwitchUnlock = func() {
		unlock, err := state.Lock(h.dir, "a")
		if err != nil {
			t.Errorf("taking a's lock in the gap: %v", err)
			return
		}
		held.Store(true)
		time.AfterFunc(150*time.Millisecond, unlock)
	}
	m.mu.Unlock()
	mark := len(h.term.rawBytes())
	m.startSwitch(&switchTarget{name: "ghost"})
	h.stayedOn(m, "a", mark, "session ghost: no session ghost")
	if !held.Load() {
		t.Fatal("the lock was not taken in the gap")
	}
	h.detach()
}

// TestSwitchDoesNotWaitForSpareSubmit: a switch cancels a spare submit
// lux has not answered instead of waiting for it; the spare lux made
// anyway is recorded when the session is attached again, and no second
// one is submitted.
func TestSwitchDoesNotWaitForSpareSubmit(t *testing.T) {
	h := newMulti(t)
	cfg, err := config.Parse([]byte(cfgSpare))
	if err != nil {
		t.Fatal(err)
	}
	h.cfg = cfg
	h.make("b", "B-SCREEN")
	held := make(chan struct{}, 1)
	h.rt.set(func(req *http.Request) error {
		if req.Method != http.MethodPost || req.URL.Path != "/v1/runs" || req.URL.RawQuery != "" {
			return nil
		}
		body, _ := io.ReadAll(req.Body)
		req.Body = io.NopCloser(bytes.NewReader(body))
		if !bytes.Contains(body, []byte(`"`+config.LabelSpare+`":"true"`)) || !bytes.Contains(body, []byte(`"`+config.LabelSession+`":"a"`)) {
			return nil
		}
		// lux makes the Run; its answer never reaches luxmux.
		resp, err := http.DefaultTransport.RoundTrip(req)
		if err == nil {
			resp.Body.Close()
		}
		select {
		case held <- struct{}{}:
		default:
		}
		return hang(req)
	})
	h.attach("a")
	h.on("a")
	<-held
	spares := func() []string {
		var ids []string
		for _, id := range h.lux.RunIDs() {
			r := h.lux.Run(id)
			if r.Labels[config.LabelSpare] == "true" && r.Labels[config.LabelSession] == "a" {
				ids = append(ids, id)
			}
		}
		return ids
	}
	late := spares()
	if len(late) != 1 {
		t.Fatalf("a's spares %v", late)
	}
	h.ui.session = SessionChoice{Switch: "b"}
	start := time.Now()
	h.type_("\x02s")
	h.on("b")
	if d := time.Since(start); d > time.Second {
		t.Fatalf("the switch took %v with a spare submit in flight", d)
	}
	h.rt.set(nil)
	h.ui.session = SessionChoice{Switch: "a"}
	h.type_("\x02s")
	h.on("a")
	key := config.SpecKey(cfg.Profiles["dev"])
	h.waitFor("the late spare recorded", func() bool {
		m := h.con.Current()
		m.mu.Lock()
		defer m.mu.Unlock()
		return m.sess.Spares[key] == late[0]
	})
	time.Sleep(100 * time.Millisecond)
	if got := spares(); !slices.Equal(got, late) {
		t.Fatalf("a's spares %v after reattaching, want only %v", got, late)
	}
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
	if err := sessions.CheckFree(ctx, h.cl, h.dir, "a", "", false); err == nil {
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

// TestRenameSessionCollision: renaming onto an existing session, or onto
// the label of another renamed session's Runs, is refused, for the
// session attached here and for another one; the other session's file
// is untouched.
func TestRenameSessionCollision(t *testing.T) {
	h := newMulti(t)
	idB := h.make("b", "B-SCREEN")
	idC := h.make("c", "C-SCREEN")
	h.attach("a")
	h.on("a")
	m := h.con.Current()
	if err := m.RenameSession("c", "d"); err != nil {
		t.Fatalf("rename c to d: %v", err)
	}
	for _, r := range [][2]string{{"a", "b"}, {"a", "d"}, {"b", "d"}, {"d", "b"}} {
		if err := m.RenameSession(r[0], r[1]); !errors.Is(err, sessions.ErrTaken) {
			t.Errorf("rename %s onto %s: %v", r[0], r[1], err)
		}
	}
	for _, r := range [][2]string{{"a", "c"}, {"b", "c"}} {
		if err := m.RenameSession(r[0], r[1]); err == nil || !strings.Contains(err.Error(), "label of session d's Runs") {
			t.Errorf("rename %s onto d's label: %v", r[0], err)
		}
	}
	for name, id := range map[string]string{"b": idB, "d": idC} {
		if s, ok, _ := state.Load(h.dir, name); !ok || len(s.Windows) != 1 || s.Windows[0].RunID != id {
			t.Fatalf("%s's file after the refused renames: %+v", name, s)
		}
	}
	if r, _ := state.Renames(h.dir); len(r) != 1 || r["d"] != "c" {
		t.Fatalf("renames %v", r)
	}
	if m.Session() != "a" {
		t.Fatalf("session is %q", m.Session())
	}
	h.detach()
}

// TestLockedCallersReuseAbandonedName: a renamed session's name whose
// entry is abandoned (no file, no live Run, nobody attached) is free to
// the callers that take its lock before the name check: a rename of
// another session, a rename of the session attached here, and a switch
// that creates a session, without an unlocked check pruning it first.
func TestLockedCallersReuseAbandonedName(t *testing.T) {
	h := newMulti(t)
	idY := h.make("y", "Y-SCREEN")
	for _, e := range [][2]string{{"p", "b"}, {"q", "c"}, {"r", "e"}} {
		if err := state.SetRenamed(h.dir, e[0], e[1], e[0]); err != nil {
			t.Fatal(err)
		}
	}
	h.attach("x")
	h.on("x")
	m := h.con.Current()
	if err := m.RenameSession("y", "b"); err != nil {
		t.Fatalf("rename y onto the abandoned b: %v", err)
	}
	if s, ok, _ := state.Load(h.dir, "b"); !ok || len(s.Windows) != 1 || s.Windows[0].RunID != idY {
		t.Fatalf("b.json after the rename: %+v", s)
	}
	if err := m.RenameSession("x", "c"); err != nil {
		t.Fatalf("rename x onto the abandoned c: %v", err)
	}
	if r, _ := state.Renames(h.dir); len(r) != 3 || r["b"] != "y" || r["c"] != "x" || r["e"] != "r" {
		t.Fatalf("renames after the renames %v", r)
	}
	p := h.cfg.Profiles["default"]
	if !m.startSwitch(&switchTarget{name: "e", create: true, defaultProfile: "default", first: &p, notice: "new session e"}) {
		t.Fatal("switch to e did not start")
	}
	idE := h.on("e")
	if r, _ := state.Renames(h.dir); len(r) != 2 || r["b"] != "y" || r["c"] != "x" {
		t.Fatalf("renames after creating e: %v", r)
	}
	if l := h.lux.Run(idE).Labels[config.LabelSession]; l != "e" {
		t.Fatalf("e's first Run is labelled %q", l)
	}
	h.detach()
}

// TestNameCheckKeepsAttachedRenamedSession: a renamed session attached
// here whose only Run ended keeps both its names: the new-session
// dialog's name check and NewSession refuse its name and leave its file
// and rename entry, so a relaunch and the next attach still use its
// Runs' label.
func TestNameCheckKeepsAttachedRenamedSession(t *testing.T) {
	h := newMulti(t)
	h.attach("a")
	id := h.on("a")
	m := h.con.Current()
	if err := m.RenameSession("a", "b"); err != nil {
		t.Fatalf("rename a to b: %v", err)
	}
	h.lux.End(id, "terminated", "terminated")
	h.waitFor("window ended", func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		return m.windows[0].phase == phaseDead
	})
	kept := func(when string) {
		t.Helper()
		if r, _ := state.Renames(h.dir); len(r) != 1 || r["b"] != "a" {
			t.Fatalf("%s: renames %v", when, r)
		}
		if _, ok, _ := state.Load(h.dir, "b"); !ok {
			t.Fatalf("%s: b.json is gone", when)
		}
	}
	if err := m.CheckSessionName(context.Background(), "b"); !errors.Is(err, sessions.ErrTaken) {
		t.Fatalf("CheckSessionName(b): %v", err)
	}
	kept("after CheckSessionName")
	if m.NewSession(&NewSessionRequest{Name: "b", ProfileName: "default", Profile: h.cfg.Profiles["default"]}) {
		t.Fatal("NewSession(b) started")
	}
	kept("after NewSession")
	if m.Session() != "b" {
		t.Fatalf("session is %q", m.Session())
	}
	h.type_("r")
	h.waitFor("relaunched", func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		return m.windows[0].RunID != "" && m.windows[0].RunID != id
	})
	m.mu.Lock()
	nid := m.windows[0].RunID
	m.mu.Unlock()
	if l := h.lux.Run(nid).Labels[config.LabelSession]; l != "a" {
		t.Fatalf("the relaunched Run is labelled %q", l)
	}
	h.detach()
	kept("after the detach")
	list, err := sessions.List(context.Background(), h.cl, h.dir)
	if err != nil || len(list) != 1 || list[0].Name != "b" || list[0].Label != "a" {
		t.Fatalf("list %+v %v", list, err)
	}
	sess, label, existed, err := sessions.Open(context.Background(), h.cl, h.dir, h.cfg, "b")
	if err != nil || !existed || label != "a" || len(sess.Windows) != 1 || sess.Windows[0].RunID != nid {
		t.Fatalf("open b: %+v %q %v %v", sess, label, existed, err)
	}
}

// TestSessionPreviewClosedOnMove: moving the chooser's highlight from
// one previewed session to another closes the first preview's stream
// while the chooser is still open.
func TestSessionPreviewClosedOnMove(t *testing.T) {
	h := newMulti(t)
	idB := h.make("b", "B-SCREEN")
	idC := h.make("c", "C-SCREEN")
	h.attach("a")
	h.on("a")
	res := make(chan string, 1)
	h.ui.sessionOpen = func(m *Mux) {
		preview := func(name, want string) bool {
			return fakelux.WaitFor(context.Background(), 5*time.Second, func() bool {
				return strings.Contains(strings.Join(m.SessionPreview(name, 10, 80), "\n"), want)
			})
		}
		if !preview("b", "B-SCREEN") || h.lux.Streams(idB) != 1 {
			res <- "b's preview did not open"
			return
		}
		if !preview("c", "C-SCREEN") {
			res <- "c's preview did not open"
			return
		}
		if !fakelux.WaitFor(context.Background(), 3*time.Second, func() bool { return h.lux.Streams(idB) == 0 }) {
			res <- "b's preview stream still open after the highlight moved to c"
			return
		}
		if n := h.lux.Streams(idC); n != 1 {
			res <- fmt.Sprintf("%d streams of c", n)
			return
		}
		res <- ""
	}
	h.type_("\x02s")
	if msg := <-res; msg != "" {
		t.Fatal(msg)
	}
	h.detach()
}

// TestSessionPreviewDebounced: a highlight moved through sessions faster
// than previewDelay attaches none of those it left; the one it stays on
// is attached once.
func TestSessionPreviewDebounced(t *testing.T) {
	h := newMulti(t)
	idB := h.make("b", "B-SCREEN")
	idC := h.make("c", "C-SCREEN")
	idD := h.make("d", "D-SCREEN")
	h.attach("a")
	h.on("a")
	attaches := func(id string) int {
		n := 0
		for _, a := range h.lux.Attaches() {
			if a.RunID == id {
				n++
			}
		}
		return n
	}
	before := map[string]int{idB: attaches(idB), idC: attaches(idC), idD: attaches(idD)}
	res := make(chan string, 1)
	h.ui.sessionOpen = func(m *Mux) {
		for range 3 {
			for _, n := range []string{"b", "c"} {
				m.SessionPreview(n, 10, 80)
				time.Sleep(previewDelay / 5)
			}
		}
		if !fakelux.WaitFor(context.Background(), 5*time.Second, func() bool {
			return strings.Contains(strings.Join(m.SessionPreview("d", 10, 80), "\n"), "D-SCREEN")
		}) {
			res <- "d's preview did not come"
			return
		}
		time.Sleep(2 * previewDelay)
		res <- ""
	}
	h.type_("\x02s")
	if msg := <-res; msg != "" {
		t.Fatal(msg)
	}
	for id, n := range map[string]int{idB: 0, idC: 0, idD: 1} {
		if got := attaches(id) - before[id]; got != n {
			t.Errorf("%s attached %d times for previews, want %d", id, got, n)
		}
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
		// c first: previewing a session attached elsewhere ends the
		// preview before it, so b's must be the one open when the
		// chooser closes.
		c := m.SessionPreview("c", 10, 80)
		var b []string
		fakelux.WaitFor(context.Background(), 5*time.Second, func() bool {
			b = m.SessionPreview("b", 10, 80)
			return strings.Contains(strings.Join(b, "\n"), "B-SCREEN")
		})
		prev <- [2][]string{b, c}
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

// windowRuns are m's windows' Runs, in order.
func (m *Mux) windowRuns() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var ids []string
	for _, w := range m.windows {
		ids = append(ids, w.RunID)
	}
	return ids
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
