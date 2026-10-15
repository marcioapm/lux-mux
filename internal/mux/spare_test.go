package mux

import (
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/marcioapm/lux-mux/internal/config"
	"github.com/marcioapm/lux-mux/internal/state"
)

const cfgSpare = `
default_profile = "dev"
[profiles.dev]
image = "alpine"
warm_spare = true
[profiles.plain]
image = "alpine"
`

// spareOf is the waiting spare of profile dev.
func spareOf(h *harness) string {
	h.m.mu.Lock()
	defer h.m.mu.Unlock()
	return h.m.sess.Spares[config.SpecKey(h.cfg.Profiles["dev"])]
}

func (h *harness) spares() map[string]string {
	h.m.mu.Lock()
	defer h.m.mu.Unlock()
	return maps.Clone(h.m.sess.Spares)
}

func (h *harness) claimed() []string {
	h.m.mu.Lock()
	defer h.m.mu.Unlock()
	return slices.Clone(h.m.sess.Claimed)
}

// TestWarmSpare: a profile with warm_spare keeps one extra Run labelled
// as a spare; a new window claims it at once and a replacement is made.
// A profile without it has none.
func TestWarmSpare(t *testing.T) {
	h := newHarness(t, cfgSpare, nil)
	first := h.live(0)
	h.waitFor("spare submitted", func() bool { return spareOf(h) != "" })
	spare := spareOf(h)
	if spare == first {
		t.Fatal("the first window must not be its own spare")
	}
	if l := h.lux.Run(spare).Labels; l[config.LabelSpare] != "true" || l[config.LabelProfile] != "dev" || l[config.LabelSession] != "s" {
		t.Fatalf("spare labels %v", l)
	}
	before := len(h.lux.Submits())
	h.type_("\x02c")
	h.waitFor("window 1 on the spare", func() bool { return h.window(1) == spare })
	h.live(1)
	h.waitFor("replacement spare", func() bool { s := spareOf(h); return s != "" && s != spare })
	if n := len(h.lux.Submits()) - before; n != 1 {
		t.Fatalf("claiming must submit only the replacement, submitted %d", n)
	}
	s, _, _ := state.Load(h.dir, "s")
	if len(s.Claimed) != 1 || s.Claimed[0] != spare || s.Spares[config.SpecKey(h.cfg.Profiles["dev"])] != spareOf(h) {
		t.Fatalf("state: claimed %v spares %v", s.Claimed, s.Spares)
	}
	// The last window's exit ends the session and terminates the spare.
	repl := spareOf(h)
	h.lux.Term(h.window(1)).Exit()
	h.waitFor("one window", func() bool { return h.nwindows() == 1 })
	h.lux.Term(first).Exit()
	select {
	case <-h.done:
	case <-timeout():
		t.Fatal("session did not end")
	}
	if h.lux.Run(repl).State != "terminated" {
		t.Fatal("the waiting spare must be terminated with the session")
	}
}

func TestNoSpareByDefault(t *testing.T) {
	h := newHarness(t, "", nil)
	h.live(0)
	h.type_("\x02c")
	h.live(1)
	for _, sp := range h.lux.Submits() {
		if sp.Labels[config.LabelSpare] != "" {
			t.Fatal("a spare was made without warm_spare")
		}
	}
	h.detach()
}

// fakeUI is a UI whose dialogs answer at once, writing a marker while
// they "own" the terminal. With hold set, Choose waits for it to be
// closed first; read, when set, is how many keyboard bytes Choose reads.
type fakeUI struct {
	choose int
	req    *NewWindowRequest
	hold   chan struct{}
	opened chan struct{}
	read   int
	got    []byte
}

func (f *fakeUI) Choose(m *Mux) (ChooseResult, error) {
	out, _, _ := m.Terminal()
	_, _ = out.Write([]byte("\x1b[2J\x1b[HCHOOSER-OPEN"))
	if f.opened != nil {
		close(f.opened)
	}
	if f.hold != nil {
		<-f.hold
	}
	if f.read > 0 {
		kb := m.KeyboardReader()
		buf := make([]byte, f.read)
		for len(f.got) < f.read {
			n, err := kb.Read(buf[:f.read-len(f.got)])
			f.got = append(f.got, buf[:n]...)
			if err != nil {
				break
			}
		}
		kb.Close()
	}
	return ChooseResult{Select: f.choose}, nil
}

func (f *fakeUI) NewDialog(m *Mux) (*NewWindowRequest, error) { return f.req, nil }

// TestDialogsHandBackTheTerminal: after the chooser closes, the chosen
// window is painted from its emulator over what the chooser drew.
func TestDialogsHandBackTheTerminal(t *testing.T) {
	h := newHarness(t, "", nil)
	h.m.opt.UI = &fakeUI{choose: 0, req: &NewWindowRequest{ProfileName: "default", Profile: config.Profile{Image: "busybox"}}}
	h.live(0)
	h.type_("zero-screen\r\n")
	h.waitScreen("echo", h.contains("zero-screen"))
	h.type_("\x02C")
	h.waitFor("dialog window", func() bool { return h.nwindows() == 2 })
	id := h.live(1)
	if img := h.lux.Run(id).Spec.Image.Ref; img != "busybox" {
		t.Fatalf("dialog's image not used: %s", img)
	}
	h.type_("\x02w")
	h.waitScreen("window 0 repainted", func(s string) bool {
		return strings.Contains(s, "zero-screen") && !strings.Contains(s, "CHOOSER-OPEN")
	})
	if h.title() != "luxmux:s [0] default" {
		t.Fatalf("title %q", h.title())
	}
	img, _ := h.m.Recent()
	if len(img) == 0 || img[0] != "busybox" {
		t.Fatalf("recent images %v", img)
	}
	h.detach()
}

// TestDialogSpecGetsItsOwnSpare: a window from the dialog whose spec
// differs from its base profile's does not take that profile's spare, and
// leaves the profile's spare slot as it was; an unsaved dialog spec gets
// no spare of its own.
func TestDialogSpecGetsItsOwnSpare(t *testing.T) {
	h := newHarness(t, cfgSpare, nil)
	edited := h.cfg.Profiles["dev"]
	edited.Image = "edited-image"
	h.m.opt.UI = &fakeUI{req: &NewWindowRequest{ProfileName: "dev", Profile: edited}}
	h.live(0)
	h.waitFor("spare submitted", func() bool { return spareOf(h) != "" })
	spare := spareOf(h)
	h.type_("\x02C")
	h.waitFor("dialog window", func() bool { return h.nwindows() == 2 })
	id := h.live(1)
	if id == spare || h.lux.Run(id).Spec.Image.Ref != "edited-image" {
		t.Fatalf("window 1 is %s (%s), the profile's spare is %s", id, h.lux.Run(id).Spec.Image.Ref, spare)
	}
	time.Sleep(100 * time.Millisecond)
	if sp := h.spares(); len(sp) != 1 || spareOf(h) != spare {
		t.Fatalf("spares %v, want only the profile's %s", sp, spare)
	}
	if h.lux.Run(spare).Spec.Image.Ref != "alpine" {
		t.Fatal("the profile's spare changed")
	}
	h.detach()
}

// TestDialogSameSpecClaimsSpare: a dialog window whose spec is the
// profile's own does claim its spare.
func TestDialogSameSpecClaimsSpare(t *testing.T) {
	h := newHarness(t, cfgSpare, nil)
	h.m.opt.UI = &fakeUI{req: &NewWindowRequest{ProfileName: "dev", Profile: h.cfg.Profiles["dev"]}}
	h.live(0)
	h.waitFor("spare submitted", func() bool { return spareOf(h) != "" })
	spare := spareOf(h)
	h.type_("\x02C")
	h.waitFor("window 1 on the spare", func() bool { return h.window(1) == spare })
	h.waitFor("replacement", func() bool { s := spareOf(h); return s != "" && s != spare })
	h.detach()
}

// TestGoneSpareReplaced: a waiting spare that a person stopped is not
// claimed: it is terminated, the window gets a new Run, a replacement
// spare is submitted, and nothing stays in Claimed.
func TestGoneSpareReplaced(t *testing.T) {
	h := newHarness(t, cfgSpare, nil)
	h.live(0)
	h.waitFor("spare submitted", func() bool { return spareOf(h) != "" })
	spare := spareOf(h)
	h.lux.End(spare, "stopped", "stop")
	h.type_("\x02c")
	id := h.live(1)
	if id == spare {
		t.Fatal("a stopped spare was claimed")
	}
	h.waitFor("old spare terminated", func() bool { return h.lux.Run(spare).State == "terminated" })
	h.waitFor("replacement", func() bool { s := spareOf(h); return s != "" && s != spare })
	if c := h.claimed(); len(c) != 0 {
		t.Fatalf("claimed %v", c)
	}
	h.detach()
}

// TestClaimedSpareForgottenWhenItsWindowCloses: a claimed spare leaves
// Claimed when its window closes.
func TestClaimedSpareForgottenWhenItsWindowCloses(t *testing.T) {
	h := newHarness(t, cfgSpare, nil)
	h.live(0)
	h.waitFor("spare submitted", func() bool { return spareOf(h) != "" })
	spare := spareOf(h)
	h.type_("\x02c")
	h.waitFor("window 1 on the spare", func() bool { return h.window(1) == spare })
	h.live(1)
	h.lux.Term(spare).Exit()
	h.waitFor("window 1 closed", func() bool { return h.nwindows() == 1 })
	if c := h.claimed(); len(c) != 0 {
		t.Fatalf("claimed %v", c)
	}
	h.detach()
}

// TestWindowClosedDuringSubmitTerminatesRun: a window closed while its
// Run was being submitted does not leave the Run behind.
func TestWindowClosedDuringSubmitTerminatesRun(t *testing.T) {
	h := newHarness(t, cfgDefault, nil)
	h.live(0)
	release := make(chan struct{})
	h.lux.SubmitHook = func() { <-release }
	h.type_("\x02c")
	h.waitFor("window 1", func() bool { return h.nwindows() == 2 })
	h.m.Kill(1)
	close(release)
	h.waitFor("submitted", func() bool { return len(h.lux.RunIDs()) == 2 })
	id := h.lux.RunIDs()[1]
	h.waitFor("terminated", func() bool { return h.lux.Run(id).State == "terminated" })
	h.detach()
}

// TestRelaunchTerminatesOldRun: r on an ended window terminates the old
// Run (a failed one is still resumable and billed for its volume).
func TestRelaunchTerminatesOldRun(t *testing.T) {
	h := newHarness(t, cfgDefault, nil)
	id := h.live(0)
	h.lux.End(id, "failed", "timeout")
	h.waitFor("dead", func() bool { return h.phase(0) == phaseDead })
	h.type_("r")
	h.waitFor("relaunched", func() bool { return h.window(0) != "" && h.window(0) != id })
	h.waitFor("old Run terminated", func() bool { return h.lux.Run(id).State == "terminated" })
	h.detach()
}

// TestSavedDialogProfile: a profile saved from the dialog is added to the
// configuration without a data race with readers of the profiles, and,
// with warm_spare on, gets a spare of its own spec.
func TestSavedDialogProfile(t *testing.T) {
	saved := make(chan string, 1)
	old := SaveProfile
	SaveProfile = func(name string, p config.Profile) error { saved <- name; return nil }
	t.Cleanup(func() { SaveProfile = old })
	h := newHarness(t, cfgSpare, nil)
	p := h.cfg.Profiles["dev"]
	p.Image = "saved-image"
	h.m.opt.UI = &fakeUI{req: &NewWindowRequest{ProfileName: "dev", Profile: p, SaveAs: "mine"}}
	h.live(0)
	stop := make(chan struct{})
	readers := make(chan struct{})
	go func() {
		defer close(readers)
		for {
			select {
			case <-stop:
				return
			default:
			}
			for n, q := range h.m.Config().Profiles {
				_, _ = n, q.Image
			}
		}
	}()
	h.type_("\x02C")
	if n := <-saved; n != "mine" {
		t.Fatalf("saved as %q", n)
	}
	h.waitFor("window 1", func() bool { return h.nwindows() == 2 })
	h.live(1)
	close(stop)
	<-readers
	if got := h.m.Config().Profiles["mine"].Image; got != "saved-image" {
		t.Fatalf("profile mine: %q", got)
	}
	key := config.SpecKey(p)
	h.waitFor("spare of the saved profile", func() bool { return h.spares()[key] != "" })
	if l := h.lux.Run(h.spares()[key]).Labels; l[config.LabelProfile] != "mine" || l[config.LabelSpec] != key {
		t.Fatalf("labels %v", l)
	}
	h.detach()
}
