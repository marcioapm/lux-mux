package mux

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marcioapm/lux/api"

	"github.com/marcioapm/lux-mux/internal/config"
	"github.com/marcioapm/lux-mux/internal/fakelux"
	"github.com/marcioapm/lux-mux/internal/luxapi"
	"github.com/marcioapm/lux-mux/internal/sessions"
	"github.com/marcioapm/lux-mux/internal/state"
)

type nameTransport func(*http.Request) (*http.Response, error)

func (f nameTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func nameHarness(t *testing.T, cfg string, tr http.RoundTripper) *harness {
	return newHarnessOpts(t, cfg, nil, func(o *Options) {
		c, err := o.Endpoints.Client(o.Endpoints.Default())
		if err != nil {
			t.Fatal(err)
		}
		c.C.HTTP = &http.Client{Transport: tr}
	})
}

func TestRunNameNewIntentAfterFailure(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	h := nameHarness(t, cfgNamed, nameTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodPatch && calls.Add(1) == 1 {
			close(entered)
			select {
			case <-release:
			case <-r.Context().Done():
			}
			return nil, errors.New("temporary failure")
		}
		return http.DefaultTransport.RoundTrip(r)
	}))
	id := h.live(0)
	defer h.detach()
	h.m.Rename(0, "first")
	<-entered
	h.m.Rename(0, "latest")
	close(release)
	h.waitFor("newer intent delivered after failed flight", func() bool { return runName(h, id) == "s/latest" })
	if calls.Load() != 2 {
		t.Fatalf("PATCH calls=%d", calls.Load())
	}
}

func TestRunNameRecoveryPending(t *testing.T) {
	for _, lost := range []bool{false, true} {
		t.Run(fmt.Sprint(lost), func(t *testing.T) {
			h := newMulti(t)
			id := h.make("b", "B")
			if lost {
				if err := state.Remove(h.dir, "b"); err != nil {
					t.Fatal(err)
				}
			}
			if err := state.SetRenamed(h.dir, "b", "c", "b"); err != nil {
				t.Fatal(err)
			}
			h.attach("c")
			h.on("c")
			defer h.detach()
			h.waitFor("recovered rename delivered", func() bool { return h.lux.Run(id).Name == "c/default" })
		})
	}
}

func TestRunNameWaitingSpare(t *testing.T) {
	h := newHarness(t, cfgSpare, nil)
	h.live(0)
	defer h.detach()
	id := waitSpare(h)
	if err := h.m.RenameSession("s", "new"); err != nil {
		t.Fatal(err)
	}
	h.waitFor("waiting spare renamed", func() bool { return runName(h, id) == "new/spare" })
}

func TestRunNameSpareRetryAndRecovery(t *testing.T) {
	for _, mode := range []string{"failed", "detached", "interrupted", "lost"} {
		t.Run(mode, func(t *testing.T) {
			h := newHarness(t, cfgSpare, nil)
			h.live(0)
			id := waitSpare(h)
			if mode == "failed" {
				h.lux.FailRenames(500)
				if err := h.m.RenameSession("s", "new"); err != nil {
					t.Fatal(err)
				}
				h.waitFor("spare attempted", func() bool { return patches(h.lux) >= 2 })
				h.detach()
				s, ok, err := state.Load(h.dir, "new")
				if err != nil || !ok || len(s.SpareRenames) == 0 {
					t.Fatalf("pending spare lost: %+v %v", s, err)
				}
				h.lux.FailRenames(0)
			} else {
				h.detach()
				if mode == "lost" {
					if err := state.Remove(h.dir, "s"); err != nil {
						t.Fatal(err)
					}
				}
				var err error
				if mode == "detached" {
					err = sessions.Move(h.dir, "s", "new", "s")
				} else {
					err = state.SetRenamed(h.dir, "s", "new", "s")
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			eps := luxapi.NewEndpoints(h.lux.Config(), nil)
			sess, _, _, err := sessions.Reopen(context.Background(), sessions.Luxes{Eps: eps, Cfg: h.cfg}, h.dir, "new")
			if err != nil {
				t.Fatal(err)
			}
			h.opts = func(o *Options) { o.Session = "new" }
			h.start(sess)
			defer h.detach()
			h.live(0)
			h.waitFor("waiting spare delivered on attach", func() bool { return runName(h, id) == "new/spare" })
			if h.lux.Run(id).State != "running" {
				t.Fatal("spare workload restarted or ended")
			}
		})
	}
}

func TestRunNameSpareClaimOrdering(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			var target atomic.Value
			target.Store("")
			var first atomic.Bool
			h := nameHarness(t, cfgSpare, nameTransport(func(r *http.Request) (*http.Response, error) {
				if r.Method == http.MethodPatch && r.URL.Path == "/v1/runs/"+target.Load().(string) && first.CompareAndSwap(false, true) {
					close(entered)
					select {
					case <-release:
					case <-r.Context().Done():
						return nil, r.Context().Err()
					}
					if fail {
						return nil, errors.New("temporary failure")
					}
				}
				return http.DefaultTransport.RoundTrip(r)
			}))
			h.live(0)
			defer h.detach()
			id := waitSpare(h)
			target.Store(id)
			if err := h.m.RenameSession("s", "new"); err != nil {
				t.Fatal(err)
			}
			select {
			case <-entered:
			case <-time.After(time.Second):
				close(release)
				t.Fatal("no spare rename")
			}
			h.type_("\x02c")
			h.waitFor("spare ownership transferred", func() bool { return h.window(1) == id })
			h.waitFor("window sender idle while spare flight held", func() bool { h.m.mu.Lock(); defer h.m.mu.Unlock(); return !h.m.windows[1].runNameSending })
			close(release)
			want := "new/" + h.windowName(1)
			h.waitFor("claimed name delivered after spare flight", func() bool { return runName(h, id) == want })
			h.waitFor("name delivery idle", func() bool {
				h.m.mu.Lock()
				defer h.m.mu.Unlock()
				return !h.m.spareNames[id] && !h.m.windows[1].runNameSending
			})
			if got := runName(h, id); got != want {
				t.Fatalf("stale spare overwrite: %q", got)
			}
		})
	}
}

func TestRunNameEndpointFanout(t *testing.T) {
	gate := make(chan struct{})
	var current, peak atomic.Int32
	h := nameHarness(t, cfgNamed, nameTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodPatch {
			v := current.Add(1)
			for {
				p := peak.Load()
				if v <= p || peak.CompareAndSwap(p, v) {
					break
				}
			}
			defer current.Add(-1)
			select {
			case <-gate:
			case <-r.Context().Done():
				return nil, r.Context().Err()
			}
		}
		return http.DefaultTransport.RoundTrip(r)
	}))
	h.live(0)
	defer h.detach()
	for i := 1; i < 32; i++ {
		h.m.newWindowFrom("dev", h.cfg.Profiles["dev"], fmt.Sprint(i), false)
		h.live(i)
	}
	if err := h.m.RenameSession("s", "new"); err != nil {
		close(gate)
		t.Fatal(err)
	}
	h.waitFor("requests entered", func() bool { return current.Load() > 0 })
	close(gate)
	h.waitFor("all names delivered", func() bool { return len(h.lux.Renames()) == 32 })
	if peak.Load() > 8 {
		t.Fatalf("peak HTTP calls=%d, want <=8", peak.Load())
	}
}

const cfgNamed = `
default_profile = "dev"
[profiles.dev]
image = "alpine"
`

// waitSpare is the dev profile's waiting spare, once there is one.
func waitSpare(h *harness) string {
	var id string
	h.waitFor("waiting spare", func() bool { id = spareOf(h); return id != "" })
	return id
}

// runName is the name lux has for Run id.
func runName(h *harness, id string) string { return h.lux.Run(id).Name }

// patches counts the renames sent to l, taken or not.
func patches(l *fakelux.Server) int {
	n := 0
	for _, r := range l.Requests() {
		if strings.HasPrefix(r, http.MethodPatch+" ") {
			n++
		}
	}
	return n
}

func (h *harness) windowName(i int) string {
	h.m.mu.Lock()
	defer h.m.mu.Unlock()
	return h.m.windows[i].Name
}

// TestRunNameAtSubmit: a window's Run is submitted named
// <session>/<window>, with no rename after.
func TestRunNameAtSubmit(t *testing.T) {
	h := newHarness(t, cfgNamed, nil)
	id := h.live(0)
	if got := runName(h, id); got != "s/dev" {
		t.Fatalf("first window's Run is named %q", got)
	}
	h.type_("\x02c")
	id1 := h.live(1)
	if got, want := runName(h, id1), "s/"+h.windowName(1); got != want {
		t.Fatalf("second window's Run is named %q, want %q", got, want)
	}
	if n := patches(h.lux); n != 0 {
		t.Fatalf("%d renames of Runs submitted with their names", n)
	}
	h.detach()
}

// TestRunNameSpare: a waiting spare is named <session>/spare; the window
// that claims it renames it to the window's name at the claim, before any
// attach (its attaches fail here).
func TestRunNameSpare(t *testing.T) {
	h := newHarness(t, cfgSpare, nil)
	h.live(0)
	spare := waitSpare(h)
	if got := runName(h, spare); got != "s/spare" {
		t.Fatalf("waiting spare is named %q", got)
	}
	h.lux.FailAttaches(spare, "host went away")
	h.type_("\x02c")
	h.waitFor("claimed", func() bool { return h.window(1) == spare })
	want := "s/" + h.windowName(1)
	h.waitFor("claimed spare renamed", func() bool { return runName(h, spare) == want })
	h.lux.FailAttaches(spare, "")
	h.live(1)
	var next string
	h.waitFor("replacement spare", func() bool { next = spareOf(h); return next != "" && next != spare })
	if got := runName(h, next); got != "s/spare" {
		t.Fatalf("replacement spare is named %q", got)
	}
	h.detach()
}

// TestRunNameWindowRename: Mux.Rename and C-b , rename the window's Run;
// the env file gets the window name as before.
func TestRunNameWindowRename(t *testing.T) {
	h := newHarness(t, cfgNamed, nil)
	id := h.live(0)
	h.m.Rename(0, "api")
	h.waitFor("Run renamed", func() bool { return runName(h, id) == "s/api" })
	h.type_("\x02,")
	h.waitScreen("rename prompt", h.contains("rename window:"))
	h.type_("\x15web/1\r")
	h.waitFor("Run renamed by C-b ,", func() bool { return runName(h, id) == "s/web/1" })
	h.m.Rename(0, "tab\there")
	h.waitFor("control character sent as a space", func() bool { return runName(h, id) == "s/tab here" })
	if got := h.windowName(0); got != "tab\there" {
		t.Fatalf("window named %q", got)
	}
	h.detach()
}

// TestRunNameSessionRename: renaming the session renames every window's
// Run; their session label stays the original name.
func TestRunNameSessionRename(t *testing.T) {
	h := newHarness(t, cfgNamed, nil)
	a := h.live(0)
	h.type_("\x02c")
	b := h.live(1)
	h.m.Rename(1, "web")
	h.waitFor("window renamed", func() bool { return runName(h, b) == "s/web" })
	if err := h.m.RenameSession("s", "renamed"); err != nil {
		t.Fatal(err)
	}
	h.waitFor("both Runs renamed", func() bool { return runName(h, a) == "renamed/dev" && runName(h, b) == "renamed/web" })
	for _, id := range []string{a, b} {
		if l := h.lux.Run(id).Labels[config.LabelSession]; l != "s" {
			t.Fatalf("%s labelled %q", id, l)
		}
	}
	h.type_("\x02c")
	c := h.live(2)
	if got, want := runName(h, c), "renamed/"+h.windowName(2); got != want {
		t.Fatalf("new window's Run is named %q, want %q", got, want)
	}
	h.detach()
}

// TestRunNameSessionRenamedDetached: a session renamed while not
// attached (from another one) has its Runs renamed at its next attach.
func TestRunNameSessionRenamedDetached(t *testing.T) {
	h := newMulti(t)
	idB := h.make("b", "B-SCREEN")
	if got := h.lux.Run(idB).Name; got != "b/default" {
		t.Fatalf("b's Run is named %q", got)
	}
	h.attach("a")
	h.on("a")
	if err := h.con.Current().RenameSession("b", "c"); err != nil {
		t.Fatal(err)
	}
	if got := h.lux.Run(idB).Name; got != "b/default" {
		t.Fatalf("renamed before c is attached: %q", got)
	}
	h.detach()
	h.attach("c")
	h.on("c")
	h.waitFor("c's Run renamed", func() bool { return h.lux.Run(idB).Name == "c/default" })
	h.waitFor("rename recorded done", func() bool {
		s, ok, _ := state.Load(h.dir, "c")
		return ok && len(s.Windows) == 1 && !s.Windows[0].RenameRun
	})
	h.detach()
}

// TestRunNameOldLux: a lux without the rename API is asked once: the
// window still renames here and in its env file, one notice says so,
// and neither later renames nor reattaches ask again.
func TestRunNameOldLux(t *testing.T) {
	h, dir := shellHarness(t, hooksConfig)
	id := h.live(0)
	h.lux.FailRenames(http.StatusMethodNotAllowed)
	const notice = "cannot rename Runs"
	h.m.Rename(0, "one")
	h.waitScreen("the notice", h.contains(notice))
	h.waitFor("env file renamed", func() bool { return strings.Contains(envFile(t, dir, id), "export LUXMUX_WINDOW='one'") })
	h.waitScreen("the notice gone", func(s string) bool { return !strings.Contains(s, notice) })
	shown := strings.Count(string(h.term.rawBytes()), notice)
	h.m.Rename(0, "two")
	if err := h.m.RenameSession("s", "renamed"); err != nil {
		t.Fatal(err)
	}
	h.waitFor("env file renamed again", func() bool { return strings.Contains(envFile(t, dir, id), "export LUXMUX_WINDOW='two'") })
	attaches := len(h.lux.Attaches())
	h.lux.Blip(id)
	h.waitFor("attached again", func() bool { return len(h.lux.Attaches()) > attaches })
	h.live(0)
	time.Sleep(200 * time.Millisecond)
	if n := patches(h.lux); n != 1 {
		t.Fatalf("%d renames asked of a lux that has no rename", n)
	}
	if got := strings.Count(string(h.term.rawBytes()), notice); got != shown {
		t.Fatalf("notice drawn %d times after the first refusal, %d before", got, shown)
	}
	if h.windowName(0) != "two" || h.m.Session() != "renamed" {
		t.Fatalf("window %q in session %q", h.windowName(0), h.m.Session())
	}
	if got := runName(h, id); got != "s/app" {
		t.Fatalf("Run renamed on an old lux: %q", got)
	}
	// Not left due either: nothing to retry at the next attach.
	if s, ok, _ := state.Load(h.dir, "renamed"); !ok || s.Windows[0].RenameRun {
		t.Fatalf("a rename an old lux refused is recorded due: %+v", s)
	}
	h.detach()
}

// TestRunNameRebuild: a session rebuilt from lux (its file gone) names
// its windows after their Runs' names, a '/' in a window name kept; a Run
// named by hand on lux keeps that name and its window falls back to its
// profile; a Run with luxmux's old name is renamed at attach.
func TestRunNameRebuild(t *testing.T) {
	h := newHarness(t, cfgNamed, nil)
	a := h.live(0)
	h.type_("\x02c")
	b := h.live(1)
	h.type_("\x02c")
	c := h.live(2)
	h.m.Rename(0, "web/v2")
	h.waitFor("renamed", func() bool { return runName(h, a) == "s/web/v2" })
	h.lux.Update(b, func(r *api.Run) { r.Name = "by hand" })
	h.lux.Update(c, func(r *api.Run) { r.Name = "luxmux-s" })
	rebuildFromLux(h)
	for i, want := range []string{"web/v2", "dev", "dev"} {
		if got := h.windowName(i); got != want {
			t.Fatalf("window %d named %q, want %q", i, got, want)
		}
	}
	h.live(2)
	h.waitFor("old name replaced", func() bool { return runName(h, c) == "s/dev" })
	h.live(1)
	time.Sleep(100 * time.Millisecond)
	if got := runName(h, b); got != "by hand" {
		t.Fatalf("a name set on lux was replaced by %q", got)
	}
	h.detach()
}

// TestRunNameRetry: a rename lux fails is not retried in a loop: it is
// sent again at the next attach, both a reattach of the stream and a
// new process (the file records it due).
func TestRunNameRetry(t *testing.T) {
	h := newHarness(t, cfgNamed, nil)
	id := h.live(0)
	h.lux.FailRenames(http.StatusInternalServerError)
	h.m.Rename(0, "api")
	h.waitFor("rename tried", func() bool { return patches(h.lux) == 1 })
	time.Sleep(200 * time.Millisecond)
	if n := patches(h.lux); n != 1 {
		t.Fatalf("%d renames before the next attach", n)
	}
	attaches := len(h.lux.Attaches())
	h.lux.Blip(id)
	h.waitFor("attached again", func() bool { return len(h.lux.Attaches()) > attaches })
	h.live(0)
	h.waitFor("retried at the reattach", func() bool { return patches(h.lux) == 2 })
	if s, ok, _ := state.Load(h.dir, "s"); !ok || !s.Windows[0].RenameRun {
		t.Fatalf("a failed rename not recorded due: %+v", s)
	}
	h.detach()

	h.lux.FailRenames(0)
	h.start(nil)
	h.live(0)
	h.waitFor("renamed at the next attach", func() bool { return runName(h, id) == "s/api" })
	h.waitFor("rename recorded done", func() bool {
		s, ok, _ := state.Load(h.dir, "s")
		return ok && !s.Windows[0].RenameRun
	})
	h.detach()
}
