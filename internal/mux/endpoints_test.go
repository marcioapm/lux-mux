package mux

import (
	"bytes"
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

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

// onB reports whether window i is attached to its Run on b.
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
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, _ := os.ReadFile(p)
		for _, k := range []string{keyA, keyB} {
			if bytes.Contains(b, []byte(k)) {
				t.Errorf("%s holds a key", p)
			}
		}
		return nil
	})
	for i, o := range out {
		for _, k := range []string{keyA, keyB} {
			if bytes.Contains(o, []byte(k)) {
				t.Errorf("output %d holds a key", i)
			}
		}
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
	x.detach()

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
	x.waitFor("a spare on each lux", func() bool { return spareOn(x.lux) != "" && spareOn(x.b) != "" && len(x.spares()) == 2 })
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
	if len(s.Spares) != 2 || len(s.SpareEndpoints) != 1 {
		t.Fatalf("file spares %v endpoints %v", s.Spares, s.SpareEndpoints)
	}
	noKeys(t, x.dir, x.term.rawBytes())
}
