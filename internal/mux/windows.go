package mux

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
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
			m.terminateOrKeep(w.Endpoint, id)
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
	lux, err := m.eps.Client(w.Endpoint)
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
		m.terminateOrKeep(w.Endpoint, run.ID)
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

// terminateOrKeep terminates Run id on ep, which nothing else records (a
// Run made for a window closed meanwhile, or a spare submitted as the
// session ended); one terminate cannot end is kept as a leftover.
func (m *Mux) terminateOrKeep(ep config.Endpoint, id string) {
	if err := m.terminate(ep, id); err != nil {
		m.mu.Lock()
		m.keepLeftoverLocked(ep, id, err)
		m.mu.Unlock()
	}
}

// unterminatedLocked tells of Run id on ep, which terminate could not
// end: a notice while the session runs; once it is ending, a line of
// Unterminated.
func (m *Mux) unterminatedLocked(ep config.Endpoint, id string, err error) {
	if m.ending {
		m.unterminated = append(m.unterminated, fmt.Sprintf("%s on %s: %v", id, ep.Label(), err))
		return
	}
	m.setNoticeLocked(cannotTerminate(ep, err))
}

// Unterminated are the Runs the session's end could not terminate, each
// with why; they stay recorded in the session's file.
func (m *Mux) Unterminated() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.unterminated)
}

// leftoverPrefix starts the Spares key of a leftover: a Run of the
// session no window or slot has, recorded until it is terminated.
// config.SpareSlot never makes a key starting with it, so none is claimed.
const leftoverPrefix = "leftover:"

// keepLeftoverLocked records leftover Run id on ep, which terminate could
// not end; the next attach (terminateLeftovers) or the session's end
// tries again.
func (m *Mux) keepLeftoverLocked(ep config.Endpoint, id string, err error) {
	m.recordLeftoverLocked(ep, id)
	m.unterminatedLocked(ep, id, err)
}

// recordLeftoverLocked records Run id on ep as a leftover and writes the
// file.
func (m *Mux) recordLeftoverLocked(ep config.Endpoint, id string) {
	m.sess.SetSpare(leftoverPrefix+id, id, ep)
	m.persistLocked()
}

// persistLocked writes the session's file; once the session is ending,
// as saveEndedLocked does.
func (m *Mux) persistLocked() {
	if m.ending {
		m.saveEndedLocked()
		return
	}
	m.saveLocked()
}

// retire records Run id on ep as a leftover, then terminates it
// (goEndLeftover).
func (m *Mux) retire(ep config.Endpoint, id string) {
	m.mu.Lock()
	m.recordLeftoverLocked(ep, id)
	m.mu.Unlock()
	m.goEndLeftover(ep, id)
}

// goEndLeftover terminates leftover Run id on ep in a goroutine of its
// own, outside m.wg: a lux that does not answer must not hold a detach up
// for terminate's retries, and the leftover is already in the file. It is
// forgotten once ended, kept (with a notice) otherwise; after the session
// stopped, the file is left to the next attach.
func (m *Mux) goEndLeftover(ep config.Endpoint, id string) {
	m.goSafe(func() {
		err := m.terminate(ep, id)
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.ctx.Err() != nil {
			return
		}
		if err != nil {
			m.unterminatedLocked(ep, id, err)
			return
		}
		// The session's end may have taken it already.
		if m.sess.Spares[leftoverPrefix+id] == id {
			m.sess.DropSpare(leftoverPrefix + id)
			m.persistLocked()
		}
	})
}

// terminateLeftovers terminates the leftovers the file recorded, each
// marked considered so the listing never adopts it as a window.
func (m *Mux) terminateLeftovers() {
	m.mu.Lock()
	left := map[string]config.Endpoint{}
	def := m.eps.Default()
	for slot, id := range m.sess.Spares {
		if strings.HasPrefix(slot, leftoverPrefix) {
			left[id] = m.sess.SpareEndpoint(slot, def)
			m.considerLocked(left[id], id)
		}
	}
	m.mu.Unlock()
	for id, ep := range left {
		m.goEndLeftover(ep, id)
	}
}

// relaunch replaces an ended window's Run with a fresh one of the same
// profile, in the same slot. The old Run is retired: recorded as a
// leftover in the same step that takes it off the window, then
// terminated, so one terminate cannot end (the key refused, lux failing)
// stays in the file. While the window's lux has no client (its key
// unreadable) no fresh Run could be made either: the window is kept as it
// is, with a notice.
func (m *Mux) relaunch(w *Window) {
	m.mu.Lock()
	if w.phase != phaseDead || w.closed {
		m.mu.Unlock()
		return
	}
	old := w.RunID
	m.mu.Unlock()
	// A client once made is kept, so the launch below gets this one.
	if old != "" {
		if _, err := m.eps.Client(w.Endpoint); err != nil {
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
	if old != "" {
		m.sess.SetSpare(leftoverPrefix+old, old, w.Endpoint)
	}
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
	m.saveLocked()
	m.mu.Unlock()
	if old != "" {
		m.goEndLeftover(w.Endpoint, old)
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
			m.sess.DropSpare(slot)
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
	lux, err := m.eps.Client(w.Endpoint)
	if err != nil {
		// Kept for when the key can be read again.
		m.mu.Lock()
		if _, taken := m.sess.Spares[slot]; taken || m.ending {
			m.keepLeftoverLocked(w.Endpoint, id, err)
		} else {
			m.sess.SetSpare(slot, id, w.Endpoint)
			m.saveLocked()
		}
		m.mu.Unlock()
		return ""
	}
	ctx, cancel := context.WithTimeout(m.ctx, 15*time.Second)
	run, err := lux.Get(ctx, id)
	cancel()
	if err != nil || !spareStates[run.State] {
		if err != nil || state.Live(run.State) {
			m.retire(w.Endpoint, id)
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
// warm_spare on, on the profile's endpoint: one goroutine each, so a lux
// that does not answer holds up only its own.
func (m *Mux) keepSpares() {
	defer m.wg.Done()
	cfg := m.config()
	for _, n := range cfg.ProfileNames() {
		if p := cfg.Profiles[n]; p.WarmSpare {
			m.wg.Add(1)
			m.goSafe(func() {
				defer m.wg.Done()
				m.ensureSpare(n, p)
			})
		}
	}
}

// ensureSpare submits a spare of profile name on its endpoint, in the
// slot of its spec there, unless there is one. It waits for the
// endpoint's first listing, which records a spare an earlier detach left
// unrecorded. A submit in flight when the session detaches is waited for
// up to spareDetachWait and its Run recorded; one cut off then leaves its
// slot free, and the next attach's listing finds the Run by its labels.
// One in flight when the session ends is terminated (or, failing that,
// kept as a leftover).
func (m *Mux) ensureSpare(name string, p config.Profile) {
	slot := m.spareSlotOf(p)
	if slot == "" {
		return
	}
	ep := m.eps.Of(p)
	if !m.waitListed(ep) {
		return
	}
	m.mu.Lock()
	if m.ending || m.ctx.Err() != nil {
		m.mu.Unlock()
		return
	}
	if _, ok := m.sess.Spares[slot]; ok {
		m.mu.Unlock()
		return
	}
	// Reserved while submitting; the endpoint is recorded for every slot,
	// the default lux's too, which may be another at the next attach.
	m.sess.SetSpare(slot, "", ep)
	m.mu.Unlock()
	lux, err := m.eps.Client(ep)
	var run *api.Run
	if err == nil {
		var secrets map[string]string
		secrets, err = p.ResolveSecrets(m.opt.Lookup)
		if err == nil {
			var sp api.RunSpec
			sp, err = config.RunSpec(p, config.Target{Session: m.label, Profile: name, Spare: true}, secrets)
			if err == nil {
				// Not cancelled by a detach at once: lux may make the Run,
				// and luxmux must learn its id to record it. A switch does
				// cancel it (stopSpares): the next attach's listing records
				// the Run lux made.
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
			m.terminateOrKeep(ep, run.ID)
		}
		return
	}
	defer m.mu.Unlock()
	if err != nil {
		m.sess.DropSpare(slot)
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
	lux, err := m.eps.Client(m.eps.Of(p))
	if err != nil {
		return nil, err
	}
	return lux.Validate(ctx, sp)
}

// SaveProfileFunc writes a profile into the configuration file; tests
// replace it.
var SaveProfile = func(name string, p config.Profile) error { return config.AppendProfile(name, p) }
