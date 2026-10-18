package mux

import (
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marcioapm/lux-mux/internal/config"
)

const cfgSharedSpare = `
default_profile = "plain"
[profiles.plain]
image = "busybox"
[profiles.a]
image = "alpine"
warm_spare = true
[profiles.b]
image = "alpine"
warm_spare = true
`

// A claim that hands the spare back (profile mismatch) after a session
// rename that happened while the claim held it must leave the spare due a
// rename to <new>/spare.
func TestRunNameSpareReturnedAfterSessionRename(t *testing.T) {
	var target atomic.Value
	target.Store("")
	entered, release := make(chan struct{}), make(chan struct{})
	var once atomic.Bool
	h := nameHarness(t, cfgSharedSpare, nameTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodGet && target.Load().(string) != "" && r.URL.Path == "/v1/runs/"+target.Load().(string) && once.CompareAndSwap(false, true) {
			close(entered)
			<-release
		}
		return http.DefaultTransport.RoundTrip(r)
	}))
	h.live(0)
	defer h.detach()
	slot := h.m.spareSlotOf(h.cfg.Profiles["a"])
	var spare string
	h.waitFor("shared spare", func() bool { spare = h.spares()[slot]; return spare != "" })
	if got := runName(h, spare); got != "s/spare" {
		t.Fatalf("spare name %q", got)
	}
	owner := h.lux.Run(spare).Labels[config.LabelProfile]
	other := map[string]string{"a": "b", "b": "a"}[owner]
	target.Store(spare)
	h.m.opt.UI = &fakeUI{choose: -1, req: &NewWindowRequest{ProfileName: other, Profile: h.cfg.Profiles[other]}}
	h.type_("\x02C")
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("claim GET not seen")
	}
	if err := h.m.RenameSession("s", "new"); err != nil {
		close(release)
		t.Fatal(err)
	}
	close(release)
	h.waitFor("other profile window", func() bool { return h.nwindows() == 2 })
	id := h.live(1)
	if id == spare {
		t.Fatal("mismatched spare claimed")
	}
	h.waitFor("spare back in its slot", func() bool { return h.spares()[slot] == spare })
	h.waitFor("returned spare renamed to new/spare", func() bool { return runName(h, spare) == "new/spare" })
}
