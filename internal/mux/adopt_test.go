package mux

import (
	"context"
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
	sp, err := config.RunSpec(h.cfg.Profiles[profile], config.Target{Session: "s", Profile: profile}, nil)
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
// (before this process attached) is found by the listing at start.
func TestAdoptAtStart(t *testing.T) {
	h := newHarness(t, cfgTwo, nil)
	h.live(0)
	h.detach()
	id := submitExternal(t, h, "base", "while-detached")
	sess, _, _ := state.Load(h.dir, "s")
	h.start(sess)
	h.waitFor("adopted at start", func() bool { return h.nwindows() == 2 && h.window(1) == id })
	if names := h.windowNames(); names[1] != "while-detached" {
		t.Fatalf("names %v", names)
	}
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
