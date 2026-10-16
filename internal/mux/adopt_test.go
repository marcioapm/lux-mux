package mux

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marcioapm/lux/api"

	"github.com/marcioapm/lux-mux/internal/config"
	"github.com/marcioapm/lux-mux/internal/fakelux"
	"github.com/marcioapm/lux-mux/internal/luxapi"
	"github.com/marcioapm/lux-mux/internal/state"
)

const cfgTwo = `
default_profile = "base"
[profiles.base]
image = "base-image"
[profiles.dev]
image = "dev-image"
repositories = [{ url = "https://example.com/api.git", branch = "main" }, { url = "https://example.com/web.git", path = "/workspace/web" }]
`

// submitExternal submits a Run as luxmux new-window does: labelled for
// session s and for adoption, named name.
func submitExternal(t *testing.T, h *harness, profile, name string) string {
	t.Helper()
	return submitExternalAs(t, h, profile, name, h.cfg.Profiles[profile])
}

// submitExternalAs is submitExternal of p, profile with new-window's
// overrides on it.
func submitExternalAs(t *testing.T, h *harness, profile, name string, p config.Profile) string {
	t.Helper()
	sp, err := config.RunSpec(p, config.Target{Session: "s", Profile: profile}, nil)
	if err != nil {
		t.Fatal(err)
	}
	sp.Labels[config.LabelExternal] = "true"
	if name != "" {
		sp.Labels[config.LabelWindow] = name
	}
	c, _ := luxapi.New(h.lux.Config())
	r, err := c.Submit(context.Background(), sp)
	if err != nil {
		t.Fatal(err)
	}
	return r.ID
}

func (h *harness) windowNames() []string {
	h.m.mu.Lock()
	defer h.m.mu.Unlock()
	var out []string
	for _, w := range h.m.windows {
		out = append(out, w.Name)
	}
	return out
}

func (h *harness) activeIndex() int {
	h.m.mu.Lock()
	defer h.m.mu.Unlock()
	return h.m.active
}

// TestSessionDefaultProfile: a session whose default is dev (luxmux new
// -p dev) makes C-b c windows of dev, not of default_profile; the default
// survives a detach.
func TestSessionDefaultProfile(t *testing.T) {
	h := newHarness(t, cfgTwo, nil)
	h.live(0)
	h.detach()
	sess, _, _ := state.Load(h.dir, "s")
	sess.DefaultProfile = "dev"
	h.start(sess)
	h.live(0)
	h.type_("\x02c")
	h.waitFor("window 1", func() bool { return h.nwindows() == 2 })
	h.live(1)
	sp := h.lux.Run(h.window(1)).Spec
	if sp.Labels[config.LabelProfile] != "dev" || sp.Image.Ref != "dev-image" || len(sp.Git.Repositories) != 2 {
		t.Fatalf("C-b c made %+v", sp)
	}
	h.detach()
	if s, _, _ := state.Load(h.dir, "s"); s.DefaultProfile != "dev" {
		t.Fatalf("default not kept: %q", s.DefaultProfile)
	}
}

// TestSessionDefaultFallsBack: without one, or when the configuration no
// longer has that profile, C-b c uses default_profile.
func TestSessionDefaultFallsBack(t *testing.T) {
	for _, def := range []string{"", "removed"} {
		h := newHarness(t, cfgTwo, nil)
		h.live(0)
		h.m.mu.Lock()
		h.m.sess.DefaultProfile = def
		h.m.mu.Unlock()
		h.type_("\x02c")
		h.waitFor("window 1", func() bool { return h.nwindows() == 2 })
		h.live(1)
		if p := h.lux.Run(h.window(1)).Labels[config.LabelProfile]; p != "base" {
			t.Fatalf("default %q: C-b c made a %q window", def, p)
		}
		h.detach()
	}
}

// TestFirstWindowFromOptions: the first window of a new session is made
// from Options.First (luxmux new's profile plus overrides), not from the
// configured profile.
func TestFirstWindowFromOptions(t *testing.T) {
	first := config.Profile{Image: "override-image", Repositories: []config.Repository{{URL: "https://example.com/x.git"}}}
	h := newHarnessOpts(t, cfgTwo, nil, func(o *Options) {
		o.First = &first
	})
	id := h.live(0)
	sp := h.lux.Run(id).Spec
	if sp.Image.Ref != "override-image" || sp.Git.Repositories[0].URL != "https://example.com/x.git" {
		t.Fatalf("first window %+v", sp)
	}
	h.detach()
}

// TestDialogSetsSessionDefault: the dialog's checkbox makes the window's
// profile the session's default, which C-b c then uses; unchecked, it
// changes nothing.
func TestDialogSetsSessionDefault(t *testing.T) {
	h := newHarness(t, cfgTwo, nil)
	h.live(0)
	ui := &fakeUI{choose: -1, req: &NewWindowRequest{ProfileName: "dev", Profile: h.cfg.Profiles["dev"]}}
	h.m.opt.UI = ui
	h.type_("\x02C")
	h.waitFor("dialog window", func() bool { return h.nwindows() == 2 })
	if h.m.DefaultProfile() != "base" {
		t.Fatalf("unchecked box changed the default to %q", h.m.DefaultProfile())
	}
	ui.req = &NewWindowRequest{ProfileName: "dev", Profile: h.cfg.Profiles["dev"], UseAsDefault: true}
	h.type_("\x02C")
	h.waitFor("second dialog window", func() bool { return h.nwindows() == 3 })
	h.type_("\x02c")
	h.waitFor("C-b c window", func() bool { return h.nwindows() == 4 })
	h.live(3)
	if p := h.lux.Run(h.window(3)).Labels[config.LabelProfile]; p != "dev" {
		t.Fatalf("C-b c after the checkbox made %q", p)
	}
	h.detach()
	if s, _, _ := state.Load(h.dir, "s"); s.DefaultProfile != "dev" {
		t.Fatalf("file default %q", s.DefaultProfile)
	}
}

// TestAdoptExternalWindow: a Run new-window made for this session becomes
// the last window, with its name and a notice, recorded in the session's
// file; the shown window does not change, and its spec is the Run's.
func TestAdoptExternalWindow(t *testing.T) {
	h := newHarness(t, cfgTwo, nil)
	h.live(0)
	h.type_("\x02c")
	h.waitFor("window 1", func() bool { return h.nwindows() == 2 })
	h.live(1)
	h.type_("\x020")
	h.waitFor("window 0 shown", func() bool { return h.activeIndex() == 0 })
	h.lux.Term(h.window(0)).Output([]byte("on-window-zero\r\n"))
	h.waitScreen("window 0's output", h.contains("on-window-zero"))

	id := submitExternal(t, h, "dev", "external")
	h.waitScreen("adoption notice", h.contains("window 2 added: external"))
	if h.activeIndex() != 0 || !strings.Contains(h.term.screen(), "on-window-zero") {
		t.Fatalf("adoption switched windows: active %d\n%s", h.activeIndex(), h.term.screen())
	}
	if h.window(2) != id {
		t.Fatalf("window 2 is %q, want %s", h.window(2), id)
	}
	h.live(2)
	s, _, _ := state.Load(h.dir, "s")
	if len(s.Windows) != 3 || s.Windows[2].RunID != id || s.Windows[2].Name != "external" || s.Windows[2].Profile != "dev" ||
		s.Active != 0 {
		t.Fatalf("file %+v", s)
	}
	if got := config.SpecKey(*s.Windows[2].Spec); got != config.SpecKey(h.cfg.Profiles["dev"]) {
		t.Fatalf("adopted spec %+v differs from the profile's", *s.Windows[2].Spec)
	}
	h.detach()
}

// TestAdoptOnlyNewWindowRuns: Runs of this session that new-window did
// not make (another process's spare or window), Runs of another session,
// and a window closed here are never adopted.
func TestAdoptOnlyNewWindowRuns(t *testing.T) {
	h := newHarness(t, cfgTwo, nil)
	h.live(0)
	c, _ := luxapi.New(h.lux.Config())
	submit := func(sp api.RunSpec) string {
		r, err := c.Submit(context.Background(), sp)
		if err != nil {
			t.Fatal(err)
		}
		return r.ID
	}
	plain, _ := config.RunSpec(h.cfg.Profiles["base"], config.Target{Session: "s", Profile: "base"}, nil)
	submit(plain)
	other, _ := config.RunSpec(h.cfg.Profiles["base"], config.Target{Session: "other", Profile: "base"}, nil)
	other.Labels[config.LabelExternal] = "true"
	submit(other)
	spare, _ := config.RunSpec(h.cfg.Profiles["base"], config.Target{Session: "s", Profile: "base", Spare: true}, nil)
	spare.Labels[config.LabelExternal] = "true"
	submit(spare)
	// A marker adopted after them: once it is in, the others were seen.
	marker := submitExternal(t, h, "base", "marker")
	h.waitFor("marker adopted", func() bool { return h.nwindows() == 2 && h.window(1) == marker })
	h.live(1)
	h.m.Kill(1)
	h.waitFor("marker closed", func() bool { return h.nwindows() == 1 })
	// More events of the killed Run (its terminate) must not bring it back.
	second := submitExternal(t, h, "base", "second")
	h.waitFor("second adopted", func() bool { return h.nwindows() == 2 && h.window(1) == second })
	if names := h.windowNames(); len(names) != 2 || names[1] != "second" {
		t.Fatalf("windows %v", names)
	}
	h.detach()
}

// TestAdoptAtStart: a Run new-window made while the feed was not followed
// (before this process attached) is found by the listing at start, from
// what the listing says: its adoption does not wait for a read of it.
func TestAdoptAtStart(t *testing.T) {
	h := newHarness(t, cfgTwo, nil)
	h.live(0)
	h.detach()
	id := submitExternal(t, h, "base", "while-detached")
	held := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(held) }) }
	t.Cleanup(release)
	h.lux.SetGetHook(func(got string) {
		if got == id {
			<-held
		}
	})
	sess, _, _ := state.Load(h.dir, "s")
	h.start(sess)
	h.waitFor("adopted at start", func() bool { return h.nwindows() == 2 && h.window(1) == id })
	release()
	if names := h.windowNames(); names[1] != "while-detached" {
		t.Fatalf("names %v", names)
	}
	h.detach()
}

// gets counts the GETs of Run id received.
func (h *harness) gets(id string) int {
	n := 0
	for _, r := range h.lux.Requests() {
		if r == "GET /v1/runs/"+id {
			n++
		}
	}
	return n
}

// TestListingAdoptsOnlyLiveExternal: of the Runs the session's listing
// returns, only live ones new-window made are adopted; terminated ones,
// ones being terminated, ordinary windows and spares are not, and none of
// them is read on its own, at start or at any later listing.
func TestListingAdoptsOnlyLiveExternal(t *testing.T) {
	h := newHarnessFull(t, cfgTwo, nil, func(l *fakelux.Server) { l.HoldTerminate = true },
		func(o *Options) { o.discoverEvery = 20 * time.Millisecond })
	h.live(0)
	h.detach()
	c, _ := luxapi.New(h.lux.Config())
	var skip []string
	for range 3 {
		id := submitExternal(t, h, "base", "ended")
		h.lux.End(id, "terminated", "terminated")
		skip = append(skip, id)
	}
	stopping := submitExternal(t, h, "base", "stopping")
	h.waitFor("running", func() bool { return h.lux.Run(stopping).State == "running" })
	if err := c.Terminate(context.Background(), stopping); err != nil {
		t.Fatal(err)
	}
	skip = append(skip, stopping)
	plain, _ := config.RunSpec(h.cfg.Profiles["base"], config.Target{Session: "s", Profile: "base"}, nil)
	r, _ := c.Submit(context.Background(), plain)
	skip = append(skip, r.ID)
	spare, _ := config.RunSpec(h.cfg.Profiles["base"], config.Target{Session: "s", Profile: "base", Spare: true}, nil)
	spare.Labels[config.LabelExternal] = "true"
	r, _ = c.Submit(context.Background(), spare)
	skip = append(skip, r.ID)
	live := submitExternal(t, h, "base", "live")
	sess, _, _ := state.Load(h.dir, "s")
	h.start(sess)
	h.waitFor("live adopted", func() bool { return h.nwindows() == 2 && h.window(1) == live })
	lists := func() int {
		n := 0
		for _, r := range h.lux.Requests() {
			if strings.HasPrefix(r, "GET /v1/runs") && !strings.HasPrefix(r, "GET /v1/runs/") {
				n++
			}
		}
		return n
	}
	at := lists()
	h.waitFor("more listings", func() bool { return lists() >= at+5 })
	if names := h.windowNames(); len(names) != 2 {
		t.Fatalf("windows %v", names)
	}
	for _, id := range skip {
		if n := h.gets(id); n != 0 {
			t.Errorf("Run %s (%s) read %d times", id, h.lux.Run(id).State, n)
		}
	}
	h.detach()
}

// TestFeedTerminatedExternalNotAdopted: a Run new-window made whose read
// after its submitted event finds it terminated is not adopted.
func TestFeedTerminatedExternalNotAdopted(t *testing.T) {
	h := newHarness(t, cfgTwo, nil)
	h.live(0)
	var victim atomic.Value
	victim.Store("")
	ended := make(chan struct{}, 1)
	h.lux.SetGetHook(func(id string) {
		if id != "" && id == victim.Load().(string) {
			h.lux.End(id, "terminated", "terminated")
			ended <- struct{}{}
		}
	})
	h.lux.SetHold(true)
	c, _ := luxapi.New(h.lux.Config())
	sp, _ := config.RunSpec(h.cfg.Profiles["base"], config.Target{Session: "s", Profile: "base"}, nil)
	sp.Labels[config.LabelExternal] = "true"
	// The id is known before the submit is answered: fakelux numbers Runs.
	victim.Store(fmt.Sprintf("run_%04d", len(h.lux.RunIDs())+1))
	r, err := c.Submit(context.Background(), sp)
	if err != nil || r.ID != victim.Load().(string) {
		t.Fatalf("%v %v", r, err)
	}
	select {
	case <-ended:
	case <-timeout():
		t.Fatal("the feed did not lead to a read")
	}
	h.lux.SetHold(false)
	h.lux.SetGetHook(nil)
	marker := submitExternal(t, h, "base", "marker")
	h.waitFor("marker adopted", func() bool { return h.nwindows() == 2 && h.window(1) == marker })
	if names := h.windowNames(); len(names) != 2 {
		t.Fatalf("windows %v", names)
	}
	h.detach()
}

// TestFailedReadLeftToListing: a Run whose read after its submitted event
// fails is adopted by a later listing.
func TestFailedReadLeftToListing(t *testing.T) {
	h := newHarnessOpts(t, cfgTwo, nil, func(o *Options) { o.discoverEvery = 300 * time.Millisecond })
	h.live(0)
	lists := func() int {
		n := 0
		for _, r := range h.lux.Requests() {
			if strings.HasPrefix(r, "GET /v1/runs") && !strings.HasPrefix(r, "GET /v1/runs/") {
				n++
			}
		}
		return n
	}
	// Submitted just after a listing: the feed's read comes long before
	// the next one.
	n := lists()
	h.waitFor("a listing", func() bool { return lists() > n })
	id := fmt.Sprintf("run_%04d", len(h.lux.RunIDs())+1)
	h.lux.FailGets(id, true)
	if got := submitExternal(t, h, "base", "late"); got != id {
		t.Fatalf("submitted %s, want %s", got, id)
	}
	h.waitFor("the failed read", func() bool { return h.gets(id) > 0 })
	h.lux.FailGets(id, false)
	h.waitFor("adopted by a listing", func() bool { return h.nwindows() == 2 && h.window(1) == id })
	h.detach()
}

// TestAdoptSizedRun: a sized profile's Run comes back from lux with the
// size resolved into its resources and kept in sizing; the adopted
// window's spec is the profile's again (its SpecKey matches, so its warm
// spares do).
func TestAdoptSizedRun(t *testing.T) {
	yes := true
	h := newHarness(t, cfgTwo+"[profiles.sized]\nimage = \"sized-image\"\nsize = \"m-2\"\n", func(l *fakelux.Server) {
		l.SetPools([]api.Pool{{Name: "m", IsDefault: &yes, Sizes: []api.PoolSize{
			{Name: "m-1", CPUs: 1, Memory: 2 << 30, Disk: 10 << 30, Default: true},
			{Name: "m-2", CPUs: 2, Memory: 4 << 30, Disk: 20 << 30},
		}}})
	})
	h.live(0)
	id := submitExternal(t, h, "sized", "sized")
	if r := h.lux.Run(id); r.Spec.Resources.Size != "" || r.Spec.Resources.CPUs != 2 {
		t.Fatalf("lux stored %+v", r.Spec.Resources)
	}
	h.waitFor("adopted", func() bool { return h.nwindows() == 2 && h.window(1) == id })
	h.detach()
	s, _, _ := state.Load(h.dir, "s")
	if got := *s.Windows[1].Spec; got.Size != "m-2" || got.CPUs != 0 || config.SpecKey(got) != config.SpecKey(h.cfg.Profiles["sized"]) {
		t.Fatalf("adopted spec %+v", got)
	}
}

// TestAdoptedWindowRelaunchClaimsSpare: an adopted window of an unsized
// warm_spare profile, whose Run lux stores with its default resources,
// keeps the profile's spec: r on it once ended claims the profile's
// waiting spare.
func TestAdoptedWindowRelaunchClaimsSpare(t *testing.T) {
	h := newHarness(t, cfgSpare, nil)
	h.live(0)
	h.waitFor("spare submitted", func() bool { return spareOf(h) != "" })
	spare := spareOf(h)
	ext := submitExternal(t, h, "dev", "ext")
	h.waitFor("adopted", func() bool { return h.nwindows() == 2 && h.window(1) == ext })
	h.live(1)
	h.lux.End(ext, "failed", "timeout")
	h.type_("\x021")
	h.waitFor("window 1 ended", func() bool { return h.activeIndex() == 1 && h.phase(1) == phaseDead })
	h.type_("\x02r")
	h.waitFor("relaunched", func() bool { id := h.window(1); return id != "" && id != ext })
	if got := h.window(1); got != spare {
		t.Fatalf("relaunch made %s, want the spare %s", got, spare)
	}
	h.detach()
}

// TestAdoptedOverrideRelaunchKeepsResources: an adopted window made by
// new-window --cpus 4 on the unsized warm_spare profile, on an unshaped
// pool where lux records no sizing.requested, keeps its 4 CPUs: r on it
// once ended submits cpus 4 and leaves the profile's spare waiting.
func TestAdoptedOverrideRelaunchKeepsResources(t *testing.T) {
	h := newHarness(t, cfgSpare, nil)
	h.live(0)
	h.waitFor("spare submitted", func() bool { return spareOf(h) != "" })
	spare := spareOf(h)
	p := h.cfg.Profiles["dev"]
	p.CPUs = 4
	ext := submitExternalAs(t, h, "dev", "ext", p)
	if r := h.lux.Run(ext); r.Sizing != nil {
		t.Fatalf("an unshaped pool's Run has sizing %+v", r.Sizing)
	}
	h.waitFor("adopted", func() bool { return h.nwindows() == 2 && h.window(1) == ext })
	h.live(1)
	h.lux.End(ext, "failed", "timeout")
	h.type_("\x021")
	h.waitFor("window 1 ended", func() bool { return h.activeIndex() == 1 && h.phase(1) == phaseDead })
	submits := len(h.lux.Submits())
	h.type_("\x02r")
	h.waitFor("relaunched", func() bool { id := h.window(1); return id != "" && id != ext })
	if got := h.window(1); got == spare {
		t.Fatalf("relaunch claimed the unsized profile's spare %s", got)
	}
	var relaunch *api.RunSpec
	for _, sp := range h.lux.Submits()[submits:] {
		if sp.Labels[config.LabelSpare] != "true" {
			relaunch = &sp
		}
	}
	if relaunch == nil {
		t.Fatal("no window Run submitted for the relaunch")
	}
	if relaunch.Resources.CPUs != 4 {
		t.Fatalf("relaunch submitted cpus %v, want 4", relaunch.Resources.CPUs)
	}
	if got := spareOf(h); got != spare {
		t.Fatalf("the profile's spare is %q, want %s still waiting", got, spare)
	}
	h.detach()
}

// TestConsideredPruned: Run ids read after their submitted event that
// were not adoptable (another session's) are forgotten at the next
// listing, which does not list them.
func TestConsideredPruned(t *testing.T) {
	h := newHarnessOpts(t, cfgTwo, nil, func(o *Options) { o.discoverEvery = 20 * time.Millisecond })
	h.live(0)
	c, _ := luxapi.New(h.lux.Config())
	var others []string
	for range 5 {
		sp, _ := config.RunSpec(h.cfg.Profiles["base"], config.Target{Session: "other", Profile: "base"}, nil)
		sp.Labels[config.LabelExternal] = "true"
		r, err := c.Submit(context.Background(), sp)
		if err != nil {
			t.Fatal(err)
		}
		others = append(others, r.ID)
	}
	h.waitFor("each read once", func() bool {
		for _, id := range others {
			if h.gets(id) != 1 {
				return false
			}
		}
		return true
	})
	h.waitFor("forgotten", func() bool {
		h.m.mu.Lock()
		defer h.m.mu.Unlock()
		for _, id := range others {
			if h.m.considered[id] {
				return false
			}
		}
		return true
	})
	h.detach()
}

// TestRelaunchedRunNotAdoptedBack: r on an ended window that new-window
// made (here loaded from the session's file by a later attach) replaces
// its Run; the old one, until lux has terminated it, is still listed as
// live and must not come back as a window.
func TestRelaunchedRunNotAdoptedBack(t *testing.T) {
	held := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(held) }) }
	var victim atomic.Value
	victim.Store("")
	h := newHarnessFull(t, cfgTwo, nil, func(l *fakelux.Server) {
		l.TerminateHook = func(id string) {
			if id == victim.Load().(string) {
				<-held
			}
		}
	}, func(o *Options) { o.discoverEvery = 20 * time.Millisecond })
	t.Cleanup(release)
	h.live(0)
	old := submitExternal(t, h, "base", "short-lived")
	h.waitFor("adopted", func() bool { return h.nwindows() == 2 && h.window(1) == old })
	h.detach()
	h.start(nil)
	h.live(1)
	victim.Store(old)
	h.lux.End(old, "failed", "timeout")
	h.type_("\x021")
	h.waitFor("window 1 ended", func() bool { return h.activeIndex() == 1 && h.phase(1) == phaseDead })
	h.type_("\x02r")
	h.waitFor("relaunched", func() bool { id := h.window(1); return id != "" && id != old })
	// Several listings while the old Run is still live.
	time.Sleep(200 * time.Millisecond)
	if n := h.nwindows(); n != 2 || h.lux.Run(old).State != "failed" {
		t.Fatalf("%d windows %v, old Run %s", n, h.windowNames(), h.lux.Run(old).State)
	}
	release()
	h.detach()
}

// TestRepoWord: the chooser's repository column shows the first
// repository and how many more there are.
func TestRepoWord(t *testing.T) {
	for _, c := range []struct {
		p    config.Profile
		want string
	}{
		{config.Profile{}, "-"},
		{config.Profile{Repository: "https://example.com/App.git", Branch: "dev"}, "app@dev"},
		{config.Profile{Repositories: []config.Repository{{URL: "https://example.com/api.git", Ref: "v1"}, {URL: "https://example.com/web.git"}, {URL: "https://example.com/lib.git"}}}, "api@v1 +2"},
		{config.Profile{Repositories: []config.Repository{{URL: "https://example.com/api.git", Name: "server"}}}, "server"},
	} {
		if got := repoWord(c.p); got != c.want {
			t.Errorf("%+v: %q, want %q", c.p, got, c.want)
		}
	}
}
