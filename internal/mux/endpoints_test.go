package mux

import (
	"bytes"
	"context"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/marcioapm/lux/api"

	"github.com/marcioapm/lux-mux/internal/config"
	"github.com/marcioapm/lux-mux/internal/fakelux"
	"github.com/marcioapm/lux-mux/internal/luxapi"
	"github.com/marcioapm/lux-mux/internal/state"
)

// Obviously fake keys, one per fake luxd; each luxd refuses the other's.
const (
	keyA      = "lux_fake_key_AAAA_endpoint_test"
	keyB      = "lux_fake_key_BBBB_endpoint_test"
	remoteEnv = "LMX_TEST_REMOTE_KEY"
)

// cfgRemote has a profile on the lux CLI configuration's lux (base) and
// one on url, tenant example, its key in remoteEnv.
func cfgRemote(url string) string {
	return `
default_profile = "base"
[profiles.base]
image = "base-image"
[profiles.remote]
image = "remote-image"
lux_url = "` + url + `"
tenant = "example"
api_key_env = "` + remoteEnv + `"
`
}

// twoLux is a harness on a luxd with keyA and a second luxd with keyB, a
// profile of which is on it.
type twoLux struct {
	*harness
	b *fakelux.Server
}

func newTwoLux(t *testing.T, opts func(*Options)) *twoLux {
	t.Helper()
	b := fakelux.NewWithKey(keyB)
	b.IDPrefix = "run_b"
	t.Cleanup(b.Close)
	h := &harness{t: t, lux: fakelux.NewWithKey(keyA), rows: 24, cols: 80, dir: t.TempDir(),
		env: map[string]string{remoteEnv: keyB}, crashed: make(chan string, 1), opts: opts}
	t.Cleanup(h.lux.Close)
	cfg, err := config.Parse([]byte(cfgRemote(b.URL)))
	if err != nil {
		t.Fatal(err)
	}
	h.cfg = cfg
	h.start(nil)
	return &twoLux{harness: h, b: b}
}

// liveOnB waits until window i is attached to its Run on b.
func (x *twoLux) liveOnB(i int) string {
	x.t.Helper()
	x.waitFor("window live on b", func() bool {
		x.m.mu.Lock()
		defer x.m.mu.Unlock()
		return i < len(x.m.windows) && x.m.windows[i].phase == phaseLive && x.m.windows[i].getConn() != nil
	})
	id := x.window(i)
	x.waitFor("stream open on b", func() bool { return slices.Contains(x.b.RunIDs(), id) && x.b.Streams(id) > 0 })
	return id
}

// newRemote opens a window of profile remote through the dialog.
func (x *twoLux) newRemote() {
	x.t.Helper()
	n := x.nwindows()
	x.m.opt.UI = &fakeUI{choose: -1, req: &NewWindowRequest{ProfileName: "remote", Profile: x.cfg.Profiles["remote"]}}
	x.type_("\x02C")
	x.waitFor("remote window", func() bool { return x.nwindows() == n+1 })
}

// submitOn submits a Run as new-window does, for session s, on lux l.
func submitOn(t *testing.T, l *fakelux.Server, p config.Profile, profile, name string) string {
	t.Helper()
	sp, err := config.RunSpec(p, config.Target{Session: "s", Profile: profile}, nil)
	if err != nil {
		t.Fatal(err)
	}
	sp.Labels[config.LabelExternal], sp.Labels[config.LabelWindow] = "true", name
	c, _ := luxapi.New(l.Config())
	r, err := c.Submit(context.Background(), sp)
	if err != nil {
		t.Fatal(err)
	}
	return r.ID
}

// noKeys fails if either fake key is anywhere in dir's files or in out.
func noKeys(t *testing.T, dir string, out ...[]byte) {
	t.Helper()
	for _, l := range fakelux.KeyLeaks([]string{keyA, keyB}, dir, out...) {
		t.Errorf("%s holds a key", l)
	}
}

// TestTwoEndpointsRouting: windows of two profiles on two luxes are each
// submitted, attached, resumed and terminated on their own lux with its
// own key and tenant, and the session file records the endpoint, never
// the key.
func TestTwoEndpointsRouting(t *testing.T) {
	x := newTwoLux(t, nil)
	a0 := x.live(0)
	x.newRemote()
	b1 := x.liveOnB(1)
	if !strings.HasPrefix(b1, "run_b") || slices.Contains(x.lux.RunIDs(), b1) || slices.Contains(x.b.RunIDs(), a0) {
		t.Fatalf("window 0 %s on a, window 1 %s on b: a has %v, b has %v", a0, b1, x.lux.RunIDs(), x.b.RunIDs())
	}
	if r := x.b.Run(b1); r.Labels[config.LabelProfile] != "remote" || r.Spec.Image.Ref != "remote-image" {
		t.Fatalf("b's Run %+v", r)
	}
	for _, tn := range x.b.Tenants() {
		if tn != "example" {
			t.Fatalf("a request to b without its tenant: %q", tn)
		}
	}
	if slices.Contains(x.lux.Tenants(), "example") {
		t.Fatal("the remote profile's tenant sent to a")
	}

	x.b.LoseSecrets(b1)
	x.waitFor("resumed on b", func() bool { return len(x.b.Resumes()) == 1 && x.b.Resumes()[0].RunID == b1 })
	x.liveOnB(1)
	if len(x.lux.Resumes()) != 0 {
		t.Fatalf("resume sent to a: %+v", x.lux.Resumes())
	}

	x.detach()
	s, _, _ := state.Load(x.dir, "s")
	if len(s.Windows) != 2 || s.Windows[1].Endpoint == nil || s.Windows[1].Endpoint.URL != x.b.URL ||
		s.Windows[1].Endpoint.Tenant != "example" || s.Windows[1].Endpoint.KeyEnv != remoteEnv {
		t.Fatalf("file windows %+v", s.Windows)
	}
	x.start(nil)
	x.live(0)
	x.liveOnB(1)
	x.m.Kill(1)
	x.waitFor("terminated on b", func() bool { return slices.Contains(x.b.Terminated(), b1) })
	if slices.Contains(x.lux.Terminated(), b1) {
		t.Fatal("terminate sent to a")
	}
	x.m.Kill(0)
	x.waitFor("terminated on a", func() bool { return slices.Contains(x.lux.Terminated(), a0) })
	if slices.Contains(x.b.Terminated(), a0) {
		t.Fatal("terminate sent to b")
	}
	if x.lux.Refused() != 0 || x.b.Refused() != 0 {
		t.Fatalf("a request with the wrong key: a refused %d, b refused %d", x.lux.Refused(), x.b.Refused())
	}
	noKeys(t, x.dir, x.term.rawBytes())
}

// TestOneFeedPerEndpoint: the windows on one lux share one event stream:
// two luxes, three windows, two streams.
func TestOneFeedPerEndpoint(t *testing.T) {
	x := newTwoLux(t, nil)
	x.live(0)
	x.newRemote()
	x.liveOnB(1)
	x.newRemote()
	x.liveOnB(2)
	x.type_("\x02c")
	x.waitFor("a second window on a", func() bool { return x.nwindows() == 4 })
	x.live(3)
	// Long enough for a second stream to have been opened.
	time.Sleep(200 * time.Millisecond)
	if a, b := x.lux.FeedStreams(), x.b.FeedStreams(); a != 1 || b != 1 {
		t.Fatalf("event streams open: a %d, b %d", a, b)
	}
	if a, b := x.lux.FeedOpens(), x.b.FeedOpens(); a != 1 || b != 1 {
		t.Fatalf("event streams opened: a %d, b %d", a, b)
	}
	x.detach()
}

// TestAdoptFromBothEndpoints: Runs new-window made on either lux are
// adopted, each window on the lux its Run is on.
func TestAdoptFromBothEndpoints(t *testing.T) {
	x := newTwoLux(t, nil)
	x.live(0)
	onA := submitOn(t, x.lux, x.cfg.Profiles["base"], "base", "ext-a")
	onB := submitOn(t, x.b, x.cfg.Profiles["remote"], "remote", "ext-b")
	x.waitFor("both adopted", func() bool { return x.nwindows() == 3 })
	for i := 1; i <= 2; i++ {
		x.m.mu.Lock()
		w := x.m.windows[i]
		id, ep := w.RunID, w.Endpoint
		x.m.mu.Unlock()
		switch id {
		case onA:
			if ep.URL != x.lux.URL {
				t.Fatalf("%s adopted on %s", id, ep.URL)
			}
		case onB:
			if ep.URL != x.b.URL || ep.KeyEnv != remoteEnv {
				t.Fatalf("%s adopted on %+v", id, ep)
			}
		default:
			t.Fatalf("window %d is %s", i, id)
		}
	}
	x.waitFor("both attached", func() bool { return x.lux.Streams(onA) > 0 && x.b.Streams(onB) > 0 })
	x.detach()
	noKeys(t, x.dir, x.term.rawBytes())
}

// TestEndpointDown: a window whose lux does not answer says so while the
// other lux's windows work; a session rebuilt while a lux was down gets
// that lux's windows once it answers.
func TestEndpointDown(t *testing.T) {
	x := newTwoLux(t, nil)
	x.live(0)
	x.newRemote()
	b1 := x.liveOnB(1)
	x.detach()

	x.b.SetDown(true)
	x.start(nil)
	a0 := x.live(0)
	x.waitFor("window 1 unreachable", func() bool { return strings.Contains(x.reason(1), "endpoint unreachable") })
	x.type_("\x020")
	x.waitFor("window 0 shown", func() bool { return x.activeIndex() == 0 })
	x.lux.Term(a0).Output([]byte("a-still-works\r\n"))
	x.waitScreen("window 0 output", x.contains("a-still-works"))
	x.type_("\x021")
	x.waitScreen("window 1 says its lux is unreachable", x.contains("endpoint unreachable"))
	noKeys(t, x.dir, x.term.rawBytes())
	// b answers again: the window attaches to its Run there.
	x.b.SetDown(false)
	if id := x.liveOnB(1); id != b1 {
		t.Fatalf("window 1 is %s after b came back, want %s", id, b1)
	}
	x.detach()
	x.b.SetDown(true)

	// The file is gone: rebuilt from a's listing, b's failed.
	if err := state.Remove(x.dir, "s"); err != nil {
		t.Fatal(err)
	}
	c, _ := luxapi.New(x.lux.Config())
	runs, err := c.ListSession(context.Background(), "s")
	if err != nil {
		t.Fatal(err)
	}
	def := config.DefaultEndpoint(x.lux.Config())
	remote := x.cfg.Profiles["remote"].Endpoint(x.lux.Config())
	sess := state.RebuildFrom("s", []state.Listing{{Endpoint: def, Runs: runs}, {Endpoint: remote, Err: context.DeadlineExceeded}}, def, x.cfg.Profiles)
	if len(sess.Windows) != 1 || len(sess.Unlisted) != 1 {
		t.Fatalf("rebuilt %+v", sess)
	}
	x.start(sess)
	x.live(0)
	x.waitFor("b's unlisted notice", func() bool { return bytes.Contains(x.term.rawBytes(), []byte("endpoint unreachable; its")) })
	noKeys(t, x.dir, x.term.rawBytes())
	time.Sleep(100 * time.Millisecond)
	if x.nwindows() != 1 {
		t.Fatalf("%d windows while b is down", x.nwindows())
	}
	x.b.SetDown(false)
	x.waitFor("b's window added", func() bool { return x.nwindows() == 2 && x.window(1) == b1 })
	x.liveOnB(1)
	x.detach()
	if s, _, _ := state.Load(x.dir, "s"); len(s.Unlisted) != 0 || len(s.Windows) != 2 || s.Windows[1].Endpoint.URL != x.b.URL {
		t.Fatalf("file %+v", s)
	}
	noKeys(t, x.dir, x.term.rawBytes())
}

// TestKeyUnsetThenRelaunch: a window whose lux's key cannot be read at
// attach waits, saying why, and is not ended: r does nothing to it, and
// once the key is back it attaches to the same Run, none terminated.
func TestKeyUnsetThenRelaunch(t *testing.T) {
	x := newTwoLux(t, nil)
	x.live(0)
	x.newRemote()
	b1 := x.liveOnB(1)
	x.detach()
	x.setEnv(remoteEnv, "", false)
	x.start(nil)
	x.live(0)
	x.waitFor("window 1 waits for its key", func() bool { return strings.Contains(x.reason(1), remoteEnv+" is not set") })
	if p := x.phase(1); p == phaseDead {
		t.Fatalf("window 1 ended on a key it could not read: %q", x.reason(1))
	}
	x.m.mu.Lock()
	w := x.m.windows[1]
	x.m.mu.Unlock()
	x.m.relaunchOrResume(w)
	x.setEnv(remoteEnv, keyB, true)
	if id := x.liveOnB(1); id != b1 {
		t.Fatalf("window 1 is %s, want %s", id, b1)
	}
	if len(x.b.Terminated()) != 0 || len(x.b.RunIDs()) != 1 {
		t.Fatalf("b terminated %v, has %v", x.b.Terminated(), x.b.RunIDs())
	}
	x.detach()
	noKeys(t, x.dir, x.term.rawBytes())
}

// TestKeyUnsetThenKill: closing a window whose lux's key cannot be read
// keeps it, with a notice: its Run could not be terminated, and dropping
// it would leave the Run unrecorded. With the key back it is terminated.
func TestKeyUnsetThenKill(t *testing.T) {
	x := newTwoLux(t, func(o *Options) { o.NoticeFor = 5 * time.Second })
	x.live(0)
	x.newRemote()
	b1 := x.liveOnB(1)
	x.detach()
	x.setEnv(remoteEnv, "", false)
	x.start(nil)
	x.live(0)
	x.waitFor("window 1 waits for its key", func() bool { return strings.Contains(x.reason(1), remoteEnv+" is not set") })
	x.m.Kill(1)
	x.waitScreen("cannot terminate notice", x.contains("cannot terminate on "+strings.TrimPrefix(x.b.URL, "http://")))
	if x.nwindows() != 2 || x.window(1) != b1 {
		t.Fatalf("window 1 dropped: %d windows", x.nwindows())
	}
	noKeys(t, x.dir, x.term.rawBytes())
	x.detach()
	if s, _, _ := state.Load(x.dir, "s"); len(s.Windows) != 2 || s.Windows[1].RunID != b1 {
		t.Fatalf("file %+v", s.Windows)
	}
	if r := x.b.Run(b1); r.State != "running" || len(x.b.Terminated()) != 0 {
		t.Fatalf("b1 %s, b terminated %v", r.State, x.b.Terminated())
	}
	x.setEnv(remoteEnv, keyB, true)
	x.start(nil)
	x.live(0)
	x.liveOnB(1)
	x.m.Kill(1)
	x.waitFor("terminated on b", func() bool { return slices.Contains(x.b.Terminated(), b1) })
	x.detach()
}

// gate holds what passes it while closed.
type gate struct {
	mu sync.Mutex
	ch chan struct{}
}

func newGate() *gate { return &gate{ch: make(chan struct{})} }

func (g *gate) wait() {
	g.mu.Lock()
	ch := g.ch
	g.mu.Unlock()
	<-ch
}

func (g *gate) open() {
	g.mu.Lock()
	defer g.mu.Unlock()
	select {
	case <-g.ch:
	default:
		close(g.ch)
	}
}

// TestDetachWithHungSpareLux: a warm-spare profile on a lux that takes
// the spare's submit and does not answer holds a detach up only for
// spareDetachWait. The Run that lux makes after all is recorded as the
// slot's spare by the next attach's listing, and no other is submitted;
// another unrecorded spare of the session, its slot taken, is terminated.
func TestDetachWithHungSpareLux(t *testing.T) {
	b := fakelux.NewWithKey(keyB)
	b.IDPrefix = "run_b"
	g := newGate()
	t.Cleanup(g.open)
	b.SubmitHook = g.wait
	t.Cleanup(b.Close)
	h := &harness{t: t, lux: fakelux.NewWithKey(keyA), rows: 24, cols: 80, dir: t.TempDir(),
		env: map[string]string{remoteEnv: keyB}, crashed: make(chan string, 1),
		opts: func(o *Options) { o.spareDetachWait = 300 * time.Millisecond }}
	t.Cleanup(h.lux.Close)
	cfg, err := config.Parse([]byte(`
default_profile = "here"
[profiles.here]
image = "alpine"
[profiles.there]
image = "alpine"
warm_spare = true
lux_url = "` + b.URL + `"
api_key_env = "` + remoteEnv + `"
`))
	if err != nil {
		t.Fatal(err)
	}
	h.cfg = cfg
	h.start(nil)
	h.live(0)
	h.waitFor("the spare's submit reached b", func() bool { return slices.Contains(b.Requests(), "POST /v1/runs") })
	start := time.Now()
	h.detach()
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("detach took %v", d)
	}
	g.open()
	h.waitFor("b made the spare", func() bool { return len(b.RunIDs()) == 1 })
	late := b.RunIDs()[0]

	// One more spare of the session's, recorded nowhere, for the same slot.
	sp, err := config.RunSpec(cfg.Profiles["there"], config.Target{Session: "s", Profile: "there", Spare: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := luxapi.New(api.Config{URL: b.URL, APIKey: keyB})
	stray, err := c.Submit(context.Background(), sp)
	if err != nil {
		t.Fatal(err)
	}

	h.start(nil)
	h.live(0)
	slot := config.SpareSlot(config.SpecKey(cfg.Profiles["there"]), cfg.Profiles["there"].Endpoint(h.lux.Config()), config.DefaultEndpoint(h.lux.Config()))
	h.waitFor("the stray spare terminated", func() bool { return slices.Contains(b.Terminated(), stray.ID) || slices.Contains(b.Terminated(), late) })
	// The newer (stray) one keeps the slot; the late one is terminated.
	if got := h.spares()[slot]; got != stray.ID || !slices.Equal(b.Terminated(), []string{late}) {
		t.Fatalf("slot %s = %s; b terminated %v (late %s, stray %s)", slot, got, b.Terminated(), late, stray.ID)
	}
	h.detach()
	if n := len(b.RunIDs()); n != 2 {
		t.Fatalf("b has %v", b.RunIDs())
	}
	if s, _, _ := state.Load(h.dir, "s"); s.Spares[slot] != stray.ID || s.SpareEndpoints[slot].URL != b.URL {
		t.Fatalf("file spares %v endpoints %v", s.Spares, s.SpareEndpoints)
	}
}

// slowPoll leaves only events to tell a supervisor its Run changed: it
// reads the Run again only after seconds.
func slowPoll(o *Options) {
	o.Poll, o.Backoff, o.MaxBackoff = 3*time.Second, 3*time.Second, 3*time.Second
}

// TestEventWakesWindowOnItsLux: an event of b's feed wakes the window
// whose Run is on b, at once (polling would take seconds).
func TestEventWakesWindowOnItsLux(t *testing.T) {
	x := newTwoLux(t, slowPoll)
	x.live(0)
	x.b.SetHold(true)
	x.newRemote()
	x.waitFor("submitted on b", func() bool { return len(x.b.RunIDs()) == 1 })
	id := x.b.RunIDs()[0]
	x.waitFor("window 1 has its Run", func() bool { return x.window(1) == id })
	// The supervisor read the held Run and waits for an event or Poll.
	x.waitFor("b read the Run", func() bool { return slices.Contains(x.b.Requests(), "GET /v1/runs/"+id) })
	time.Sleep(50 * time.Millisecond)
	start := time.Now()
	x.b.Start(id)
	x.liveOnB(1)
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("live after %v: not woken by b's event", d)
	}
	x.detach()
}

// TestEventRoutedByEndpoint: two luxes whose Run ids collide: an event of
// a's feed for an id never reaches the window of that id on b, and b's
// event reaches it at once. (Windows are indexed by Run id alone, so with
// colliding ids only the later window is found; real ids do not collide.)
func TestEventRoutedByEndpoint(t *testing.T) {
	b := fakelux.NewWithKey(keyB)
	t.Cleanup(b.Close)
	h := &harness{t: t, lux: fakelux.NewWithKey(keyA), rows: 24, cols: 80, dir: t.TempDir(),
		env: map[string]string{remoteEnv: keyB}, crashed: make(chan string, 1), opts: slowPoll}
	t.Cleanup(h.lux.Close)
	cfg, err := config.Parse([]byte(cfgRemote(b.URL)))
	if err != nil {
		t.Fatal(err)
	}
	h.cfg = cfg
	// Window 0's Run on a and window 1's on b have the same id.
	idA := submitOn(t, h.lux, cfg.Profiles["base"], "base", "on-a")
	idB := submitOn(t, b, cfg.Profiles["remote"], "remote", "on-b")
	if idA != idB {
		t.Fatalf("ids %s and %s", idA, idB)
	}
	h.lux.SetHold(true)
	b.SetHold(true)
	h.lux.SetState(idA, "submitted", "")
	b.SetState(idB, "submitted", "")
	def := config.DefaultEndpoint(h.lux.Config())
	remote := cfg.Profiles["remote"].Endpoint(h.lux.Config())
	h.start(&state.Session{Name: "s", Windows: []state.Window{
		{RunID: idA, Name: "on-a", Profile: "base", Endpoint: &def},
		{RunID: idB, Name: "on-b", Profile: "remote", Endpoint: &remote},
	}})
	x := &twoLux{harness: h, b: b}
	steps := func(i int) []string {
		x.m.mu.Lock()
		defer x.m.mu.Unlock()
		return slices.Clone(x.m.windows[i].steps.steps)
	}
	x.waitFor("both feeds open", func() bool { return h.lux.FeedStreams() == 1 && b.FeedStreams() == 1 })
	x.waitFor("b read the Run", func() bool { return slices.Contains(b.Requests(), "GET /v1/runs/"+idB) })
	time.Sleep(50 * time.Millisecond)
	before := steps(1)
	h.lux.Start(idA)
	x.waitFor("a's events sent", func() bool { return h.lux.Run(idA).State == "running" })
	time.Sleep(200 * time.Millisecond)
	if got := steps(1); !slices.Equal(got, before) {
		t.Fatalf("a's events reached window 1 on b: %v, was %v", got, before)
	}
	start := time.Now()
	b.Start(idB)
	x.liveOnB(1)
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("window 1 live after %v: not woken by b's event", d)
	}
	x.detach()
}

// TestWrongKeyWindow: a window on a lux that refuses the profile's key
// says the key was refused, not that its lux is unreachable, and shows
// no key; nothing is made there.
func TestWrongKeyWindow(t *testing.T) {
	x := newTwoLux(t, func(o *Options) { o.NoticeFor = 5 * time.Second })
	x.live(0)
	x.newRemote()
	b1 := x.liveOnB(1)
	x.detach()
	x.setEnv(remoteEnv, keyA, true)
	x.start(nil)
	x.live(0)
	x.waitFor("window 1 refused", func() bool { return strings.Contains(x.reason(1), "refused the key") })
	x.type_("\x021")
	x.waitScreen("the refusal on screen", x.contains("endpoint refused the key"))
	if strings.Contains(x.term.screen(), "unreachable") {
		t.Fatalf("a refused key shown as unreachable:\n%s", x.term.screen())
	}
	noKeys(t, x.dir, x.term.rawBytes())
	x.newRemote()
	x.waitFor("window 2 ended", func() bool { return x.phase(2) == phaseDead })
	if r := x.reason(2); !strings.Contains(r, "invalid API key") {
		t.Fatalf("window 2: %q", r)
	}
	if ids := x.b.RunIDs(); !slices.Equal(ids, []string{b1}) || x.b.Run(b1).State != "running" {
		t.Fatalf("b has %v", ids)
	}
	noKeys(t, x.dir, x.term.rawBytes())
	x.detach()
}

// TestKeyUnsetSpareKeptAtEnd: when the last window closes while a spare's
// lux has no client (its key unset), the spare is not dropped: the file
// keeps it, and luxmux spares can list it.
func TestKeyUnsetSpareKeptAtEnd(t *testing.T) {
	b := fakelux.NewWithKey(keyB)
	b.IDPrefix = "run_b"
	t.Cleanup(b.Close)
	h := &harness{t: t, lux: fakelux.NewWithKey(keyA), rows: 24, cols: 80, dir: t.TempDir(),
		env: map[string]string{remoteEnv: keyB}, crashed: make(chan string, 1)}
	t.Cleanup(h.lux.Close)
	cfg, err := config.Parse([]byte(`
default_profile = "here"
[profiles.here]
image = "alpine"
[profiles.there]
image = "alpine"
warm_spare = true
lux_url = "` + b.URL + `"
api_key_env = "` + remoteEnv + `"
`))
	if err != nil {
		t.Fatal(err)
	}
	h.cfg = cfg
	h.start(nil)
	h.live(0)
	h.waitFor("b's spare recorded", func() bool {
		sp := h.spares()
		return len(sp) == 1 && !slices.Contains(slices.Collect(maps.Values(sp)), "")
	})
	spare := slices.Collect(maps.Values(h.spares()))[0]
	h.detach()
	h.setEnv(remoteEnv, "", false)
	h.start(nil)
	h.live(0)
	h.m.Kill(0)
	select {
	case <-h.done:
	case <-timeout():
		t.Fatal("the session did not end")
	}
	s, ok, _ := state.Load(h.dir, "s")
	if !ok || !slices.Contains(slices.Collect(maps.Values(s.Spares)), spare) {
		t.Fatalf("file %v %+v", ok, s)
	}
	if r := b.Run(spare); r.State != "running" || len(b.Terminated()) != 0 {
		t.Fatalf("spare %s, b terminated %v", r.State, b.Terminated())
	}
	noKeys(t, h.dir, h.term.rawBytes())
}

// TestRelaunchWithoutClientKeepsRun: r on an ended window whose lux has
// no client keeps the window's Run, with a notice. No path ends a window
// before its client was made (and clients are kept), so the window is
// ended here directly.
func TestRelaunchWithoutClientKeepsRun(t *testing.T) {
	x := newTwoLux(t, func(o *Options) { o.NoticeFor = 5 * time.Second })
	x.live(0)
	x.newRemote()
	b1 := x.liveOnB(1)
	x.detach()
	x.setEnv(remoteEnv, "", false)
	x.start(nil)
	x.live(0)
	x.waitFor("window 1 waits for its key", func() bool { return strings.Contains(x.reason(1), remoteEnv+" is not set") })
	x.m.mu.Lock()
	w := x.m.windows[1]
	x.m.mu.Unlock()
	x.m.markDead(w, "ended for the test")
	x.m.relaunchOrResume(w)
	x.waitScreen("cannot terminate notice", x.contains("cannot terminate on"))
	if id := x.window(1); id != b1 {
		t.Fatalf("window 1 is %q, want %s", id, b1)
	}
	x.detach()
	if s, _, _ := state.Load(x.dir, "s"); len(s.Windows) != 2 || s.Windows[1].RunID != b1 {
		t.Fatalf("file %+v", s.Windows)
	}
	if len(x.b.RunIDs()) != 1 || len(x.b.Terminated()) != 0 {
		t.Fatalf("b has %v, terminated %v", x.b.RunIDs(), x.b.Terminated())
	}
}

// TestSparesPerEndpoint: a warm-spare profile on each lux keeps its spare
// on its own lux, and a window of each claims the spare of its lux.
func TestSparesPerEndpoint(t *testing.T) {
	b := fakelux.NewWithKey(keyB)
	b.IDPrefix = "run_b"
	t.Cleanup(b.Close)
	h := &harness{t: t, lux: fakelux.NewWithKey(keyA), rows: 24, cols: 80, dir: t.TempDir(),
		env: map[string]string{remoteEnv: keyB}, crashed: make(chan string, 1)}
	t.Cleanup(h.lux.Close)
	// The same spec on both luxes: two slots all the same.
	cfg, err := config.Parse([]byte(`
default_profile = "here"
[profiles.here]
image = "alpine"
warm_spare = true
[profiles.there]
image = "alpine"
warm_spare = true
lux_url = "` + b.URL + `"
api_key_env = "` + remoteEnv + `"
`))
	if err != nil {
		t.Fatal(err)
	}
	h.cfg = cfg
	h.start(nil)
	x := &twoLux{harness: h, b: b}
	x.live(0)
	spareOn := func(l *fakelux.Server) string {
		for _, id := range l.RunIDs() {
			if r := l.Run(id); r.Labels[config.LabelSpare] == "true" && r.State != "terminated" && !slices.Contains(x.claimed(), id) {
				return id
			}
		}
		return ""
	}
	x.waitFor("a spare on each lux", func() bool {
		sp := x.spares()
		// A slot reserved while its submit is in flight is "": the spare
		// lux made is not claimable until it is recorded.
		return spareOn(x.lux) != "" && spareOn(x.b) != "" && len(sp) == 2 && !slices.Contains(slices.Collect(maps.Values(sp)), "")
	})
	spareB := spareOn(x.b)
	x.m.opt.UI = &fakeUI{choose: -1, req: &NewWindowRequest{ProfileName: "there", Profile: cfg.Profiles["there"]}}
	x.type_("\x02C")
	x.waitFor("window on b", func() bool { return x.nwindows() == 2 })
	if id := x.liveOnB(1); id != spareB {
		t.Fatalf("window on b is %s, not b's spare %s", id, spareB)
	}
	x.waitFor("b's spare replaced on b", func() bool { s := spareOn(x.b); return s != "" && s != spareB })
	x.detach()
	s, _, _ := state.Load(x.dir, "s")
	if len(s.Spares) != 2 || len(s.SpareEndpoints) != 2 {
		t.Fatalf("file spares %v endpoints %v", s.Spares, s.SpareEndpoints)
	}
	for slot, ep := range s.SpareEndpoints {
		if want := map[bool]string{true: x.b.URL, false: x.lux.URL}[strings.Contains(slot, "@")]; ep.URL != want {
			t.Fatalf("slot %s on %s, want %s", slot, ep.URL, want)
		}
	}
	noKeys(t, x.dir, x.term.rawBytes())
}

// TestSpareOnFormerDefault: a spare on the lux CLI configuration's lux is
// terminated there when the session ends, though the lux CLI points at
// another lux by then.
func TestSpareOnFormerDefault(t *testing.T) {
	h := newHarness(t, `
[profiles.default]
image = "alpine"
warm_spare = true
`, nil)
	h.live(0)
	h.waitFor("spare", func() bool {
		id := h.spares()[config.SpecKey(h.cfg.Profiles["default"])]
		return id != ""
	})
	spare := h.spares()[config.SpecKey(h.cfg.Profiles["default"])]
	h.detach()

	// The lux CLI now points at another lux; the key is the same, so the
	// window recorded on the first still attaches there.
	other := fakelux.New()
	t.Cleanup(other.Close)
	h.opts = func(o *Options) { o.Endpoints = luxapi.NewEndpoints(other.Config(), o.Lookup) }
	h.start(nil)
	h.live(0)
	h.m.Kill(0)
	select {
	case r := <-h.done:
		if r != "exited" {
			t.Fatalf("Run returned %q", r)
		}
	case <-timeout():
		t.Fatal("the session did not end")
	}
	if !slices.Contains(h.lux.Terminated(), spare) {
		t.Fatalf("spare %s left %s on its lux; terminated there %v, on the new default %v", spare, h.lux.Run(spare).State, h.lux.Terminated(), other.Terminated())
	}
}
