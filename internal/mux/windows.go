package mux

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/marcioapm/lux/api"

	"github.com/marcioapm/lux-mux/internal/config"
	"github.com/marcioapm/lux-mux/internal/luxapi"
	"github.com/marcioapm/lux-mux/internal/state"
)

// NewWindowRequest is what the new-window dialog asks for.
type NewWindowRequest struct {
	ProfileName string
	Profile     config.Profile
	// SaveAs, when set, saves Profile under this name in the configuration.
	SaveAs string
	// UseAsDefault makes the window's profile (SaveAs, else ProfileName)
	// the session's default.
	UseAsDefault bool
}

// ChooseResult is what the chooser did. Its renames and kills happen
// through the Mux as they are made; Select is the window to show (-1: none).
type ChooseResult struct {
	Select int
}

// newWindowFrom makes a window for a new Run of profile p and shows it at
// once; the Run is submitted (or a warm spare claimed) in the background,
// the window showing its progress.
func (m *Mux) newWindowFrom(name string, p config.Profile, title string, _ bool) {
	if title == "" {
		title = name
	}
	m.mu.Lock()
	ep := m.eps.Of(p)
	w := m.newWindow(title, name, p, ep)
	w.steps.note("submitting")
	m.windows = append(m.windows, w)
	m.followEndpointLocked(ep)
	m.selectLocked(len(m.windows) - 1)
	m.mu.Unlock()
	m.wg.Add(1)
	m.goSafe(func() {
		defer m.wg.Done()
		m.launch(w)
	})
}

// launch gets w a Run on its endpoint: a waiting spare of the same spec,
// else a new one.
func (m *Mux) launch(w *Window) {
	if id := m.claimSpare(w); id != "" {
		m.mu.Lock()
		if w.closed {
			m.mu.Unlock()
			m.terminate(w.Endpoint, id)
			return
		}
		w.RunID = id
		m.byRun[id] = w
		w.steps.note("claimed warm spare " + id)
		m.saveLocked()
		m.startSupervisor(w)
		m.mu.Unlock()
		w.poke()
		return
	}
	lux, err := m.clientOf(w.Endpoint)
	if err != nil {
		m.markDead(w, err.Error())
		return
	}
	m.mu.Lock()
	spec, profile := w.Spec, w.Profile
	m.mu.Unlock()
	secrets, err := spec.ResolveSecrets(m.opt.Lookup)
	if err != nil {
		m.markDead(w, err.Error())
		return
	}
	sp, err := config.RunSpec(spec, config.Target{Session: m.label, Profile: profile}, secrets)
	if err != nil {
		m.markDead(w, err.Error())
		return
	}
	var run *api.Run
	bo := m.opt.Backoff
	for {
		ctx, cancel := context.WithTimeout(m.ctx, 30*time.Second)
		run, err = lux.Submit(ctx, sp)
		cancel()
		if err == nil || m.ctx.Err() != nil || luxapi.IsStatus(err, 400) || luxapi.IsStatus(err, 422) ||
			luxapi.IsStatus(err, 401) || luxapi.IsStatus(err, 403) || luxapi.IsStatus(err, 409) {
			break
		}
		m.setWaiting(w, "submit failed, retrying: "+luxapi.Describe(err))
		select {
		case <-m.ctx.Done():
		case <-time.After(bo):
		}
		bo = min(2*bo, m.opt.MaxBackoff)
	}
	if err != nil {
		if m.ctx.Err() == nil {
			m.markDead(w, "submit: "+luxapi.Describe(err))
		}
		return
	}
	m.mu.Lock()
	if w.closed {
		m.mu.Unlock()
		m.terminate(w.Endpoint, run.ID)
		return
	}
	w.RunID = run.ID
	w.reason = ""
	m.byRun[run.ID] = w
	w.steps.note("submitted " + run.ID)
	m.startSupervisor(w)
	m.saveLocked()
	m.mu.Unlock()
}

// relaunch replaces an ended window's Run with a fresh one of the same
// profile, in the same slot. While the window's lux has no client (its
// key unreadable) the old Run could not be terminated: the window is
// kept as it is, with a notice.
func (m *Mux) relaunch(w *Window) {
	m.mu.Lock()
	if w.phase != phaseDead || w.closed {
		m.mu.Unlock()
		return
	}
	old := w.RunID
	m.mu.Unlock()
	// A client once made is kept, so the terminate below gets this one.
	if old != "" {
		if _, err := m.clientOf(w.Endpoint); err != nil {
			m.Notice(cannotTerminate(w.Endpoint, err))
			return
		}
	}
	m.mu.Lock()
	if w.phase != phaseDead || w.closed || w.RunID != old {
		m.mu.Unlock()
		return
	}
	if w.cancel != nil {
		w.cancel()
		w.cancel = nil
	}
	delete(m.byRun, old)
	m.considerLocked(w.Endpoint, old)
	m.dropClaimedLocked(old)
	w.RunID = ""
	w.phase = phaseStarting
	w.reason = ""
	w.resumable = false
	w.status = ""
	w.hasScreen = false
	w.attachedOnce = false
	w.epoch = 0
	w.resetKeysLocked()
	w.dropInput()
	w.steps = progress{}
	w.steps.note("relaunching")
	w.q.restart()
	m.repaintLocked()
	m.mu.Unlock()
	if old != "" {
		m.goSafe(func() { m.terminate(w.Endpoint, old) })
	}
	m.wg.Add(1)
	m.goSafe(func() {
		defer m.wg.Done()
		m.launch(w)
	})
}

// spareStates are the states a waiting spare may be claimed in.
var spareStates = map[string]bool{"running": true, "submitted": true, "scheduled": true, "provisioning": true, "starting": true}

// claimSpare takes the waiting spare of w's spec on w's endpoint (its
// slot, config.SpareSlot), if its profile keeps one and it is running or
// on its way. The Run keeps its spare label, so the claim is recorded in
// the session file. A spare found gone, stopped or ended is terminated
// (unless it is) and replaced.
func (m *Mux) claimSpare(w *Window) string {
	m.mu.Lock()
	slot := ""
	if w.Spec.WarmSpare {
		slot = config.SpareSlot(config.SpecKey(w.Spec), w.Endpoint, m.eps.Default())
	}
	id := ""
	if slot != "" {
		id = m.sess.Spares[slot]
		if id != "" {
			m.dropSpareLocked(slot)
			m.saveLocked()
		}
	}
	m.mu.Unlock()
	if slot == "" {
		return ""
	}
	// Whether or not this one is taken, the profile's slot is refilled.
	defer m.replaceSpare(slot)
	if id == "" {
		return ""
	}
	lux, err := m.clientOf(w.Endpoint)
	if err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(m.ctx, 15*time.Second)
	run, err := lux.Get(ctx, id)
	cancel()
	if err != nil || !spareStates[run.State] {
		if err != nil || state.Live(run.State) {
			m.goSafe(func() { m.terminate(w.Endpoint, id) })
		}
		return ""
	}
	m.mu.Lock()
	m.sess.Claimed = append(m.sess.Claimed, id)
	m.saveLocked()
	m.mu.Unlock()
	return id
}

// spareSlotOf is the slot of profile p's spare: its spec on its endpoint.
func (m *Mux) spareSlotOf(p config.Profile) string {
	return config.SpareSlot(config.SpecKey(p), m.eps.Of(p), m.eps.Default())
}

// spareEndpointLocked is the endpoint slot's spare lives on.
func (m *Mux) spareEndpointLocked(slot string) config.Endpoint {
	if ep, ok := m.sess.SpareEndpoints[slot]; ok {
		return ep
	}
	return m.eps.Default()
}

// dropSpareLocked forgets slot's spare.
func (m *Mux) dropSpareLocked(slot string) {
	delete(m.sess.Spares, slot)
	delete(m.sess.SpareEndpoints, slot)
}

// replaceSpare submits a new spare for slot when a configured profile
// with warm_spare has it: spares are kept for profiles only, not for a
// dialog's unsaved spec. It is called from goroutines in m.wg, so the
// session's shutdown waits for the submit too.
func (m *Mux) replaceSpare(slot string) {
	cfg := m.config()
	for _, n := range cfg.ProfileNames() {
		if p := cfg.Profiles[n]; p.WarmSpare && m.spareSlotOf(p) == slot {
			m.wg.Add(1)
			m.goSafe(func() {
				defer m.wg.Done()
				m.ensureSpare(n, p)
			})
			return
		}
	}
}

// dropClaimedLocked forgets a claimed spare whose window no longer has it.
func (m *Mux) dropClaimedLocked(id string) {
	m.sess.Claimed = slices.DeleteFunc(m.sess.Claimed, func(c string) bool { return c == id })
}

// keepSpares keeps one waiting spare of every configured profile with
// warm_spare on, on the profile's endpoint.
func (m *Mux) keepSpares() {
	defer m.wg.Done()
	cfg := m.config()
	for _, n := range cfg.ProfileNames() {
		if p := cfg.Profiles[n]; p.WarmSpare {
			m.ensureSpare(n, p)
		}
	}
}

// ensureSpare submits a spare of profile name on its endpoint, in the
// slot of its spec there, unless there is one. A submit in flight when
// the session detaches is waited for (shutdown waits on m.wg) and its Run
// recorded; one in flight when the session ends is terminated.
func (m *Mux) ensureSpare(name string, p config.Profile) {
	slot := m.spareSlotOf(p)
	if slot == "" {
		return
	}
	ep := m.eps.Of(p)
	m.mu.Lock()
	if m.ending || m.ctx.Err() != nil {
		m.mu.Unlock()
		return
	}
	if m.sess.Spares == nil {
		m.sess.Spares = map[string]string{}
	}
	if _, ok := m.sess.Spares[slot]; ok {
		m.mu.Unlock()
		return
	}
	m.sess.Spares[slot] = "" // reserved while submitting
	if ep != m.eps.Default() {
		if m.sess.SpareEndpoints == nil {
			m.sess.SpareEndpoints = map[string]config.Endpoint{}
		}
		m.sess.SpareEndpoints[slot] = ep
	}
	m.mu.Unlock()
	lux, err := m.clientOf(ep)
	var run *api.Run
	if err == nil {
		var secrets map[string]string
		secrets, err = p.ResolveSecrets(m.opt.Lookup)
		if err == nil {
			var sp api.RunSpec
			sp, err = config.RunSpec(p, config.Target{Session: m.label, Profile: name, Spare: true}, secrets)
			if err == nil {
				// Not cancelled by a detach: lux may make the Run anyway,
				// and luxmux must learn its id to record it. A switch does
				// cancel it (stopSpares): the next attach's listing records
				// the Run lux made (sessions.Open).
				ctx, cancel := context.WithTimeout(m.spareCtx, 30*time.Second)
				run, err = lux.Submit(ctx, sp)
				cancel()
			}
		}
	}
	m.mu.Lock()
	if m.ending {
		m.mu.Unlock()
		if err == nil {
			m.terminate(ep, run.ID)
		}
		return
	}
	defer m.mu.Unlock()
	if err != nil {
		m.dropSpareLocked(slot)
		if m.ctx.Err() == nil {
			m.setNoticeLocked(fmt.Sprintf("warm spare for %s: %s", name, luxapi.Describe(err)))
		}
		return
	}
	m.sess.Spares[slot] = run.ID
	m.saveLocked()
}

// openNewDialog runs the new-window dialog, then makes the window.
func (m *Mux) openNewDialog() {
	if m.opt.UI == nil {
		m.Notice("no dialog in this build")
		return
	}
	m.suspend(true)
	req, err := m.opt.UI.NewDialog(m)
	m.suspend(false)
	if err != nil {
		m.Notice("new window: " + err.Error())
		return
	}
	if req == nil {
		return
	}
	m.mu.Lock()
	m.sess.RecentImages = state.AddRecent(m.sess.RecentImages, req.Profile.Image)
	m.sess.RecentRepos = state.AddRecent(m.sess.RecentRepos, req.Profile.Repository)
	m.saveLocked()
	m.mu.Unlock()
	name := req.ProfileName
	if req.SaveAs != "" {
		name = req.SaveAs
		if err := SaveProfile(req.SaveAs, req.Profile); err != nil {
			m.Notice("save profile: " + err.Error())
		} else {
			m.addProfile(req.SaveAs, req.Profile)
		}
	}
	if req.UseAsDefault {
		m.mu.Lock()
		m.sess.DefaultProfile = name
		m.saveLocked()
		m.mu.Unlock()
	}
	m.newWindowFrom(name, req.Profile, "", false)
}

// openChooser runs the window chooser.
func (m *Mux) openChooser() {
	if m.opt.UI == nil {
		m.Notice("no chooser in this build")
		return
	}
	m.suspend(true)
	res, err := m.opt.UI.Choose(m)
	m.suspend(false)
	if err != nil {
		m.Notice("chooser: " + err.Error())
		return
	}
	if res.Select >= 0 {
		m.Select(res.Select)
	}
}

// openHelp shows the key help.
func (m *Mux) openHelp() {
	if m.opt.UI == nil {
		m.Notice("no key help in this build")
		return
	}
	m.suspend(true)
	err := m.opt.UI.Help(m)
	m.suspend(false)
	if err != nil {
		m.Notice("help: " + err.Error())
	}
}

// config is the configuration now. It is never changed in place: a
// profile saved from the dialog replaces it with a copy (addProfile), so
// what config returned may be read without the lock.
func (m *Mux) config() *config.Config {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.opt.Config
}

// addProfile adds profile p as name to a copy of the configuration.
func (m *Mux) addProfile(name string, p config.Profile) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := *m.opt.Config
	c.Profiles = maps.Clone(c.Profiles)
	if c.Profiles == nil {
		c.Profiles = map[string]config.Profile{}
	}
	c.Profiles[name] = p
	m.opt.Config = &c
}

// Recent are the images and repositories typed in the dialog lately.
func (m *Mux) Recent() (images, repos []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.sess.RecentImages), slices.Clone(m.sess.RecentRepos)
}

// Validate is lux's dry run of the spec a profile makes, on its lux.
func (m *Mux) Validate(ctx context.Context, name string, p config.Profile) (*api.SubmitAnswer, error) {
	sp, err := config.RunSpec(p, config.Target{Session: m.label, Profile: name}, nil)
	if err != nil {
		return nil, err
	}
	lux, err := m.clientOf(m.eps.Of(p))
	if err != nil {
		return nil, err
	}
	return lux.Validate(ctx, sp)
}

// SaveProfileFunc writes a profile into the configuration file; tests
// replace it.
var SaveProfile = func(name string, p config.Profile) error { return config.AppendProfile(name, p) }
