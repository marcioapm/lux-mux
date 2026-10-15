package mux

import (
	"strings"
	"testing"

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

func spareOf(h *harness) string {
	h.m.mu.Lock()
	defer h.m.mu.Unlock()
	return h.m.sess.Spares["dev"]
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
	if len(s.Claimed) != 1 || s.Claimed[0] != spare || s.Spares["dev"] != spareOf(h) {
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
// they "own" the terminal.
type fakeUI struct {
	choose int
	req    *NewWindowRequest
}

func (f *fakeUI) Choose(m *Mux) (ChooseResult, error) {
	out, _, _ := m.Terminal()
	_, _ = out.Write([]byte("\x1b[2J\x1b[HCHOOSER-OPEN"))
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
