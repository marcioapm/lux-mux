package mux

import (
	"context"
	"fmt"
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
}

// ChooseResult is what the chooser did. Its renames and kills happen
// through the Mux as they are made; Select is the window to show (-1: none).
type ChooseResult struct {
	Select int
}

// newWindowFrom makes a window for a new Run of profile p and shows it at
// once; the Run is submitted (or a warm spare claimed) in the background,
// the window showing its progress. relaunch, when not nil, is replaced in
// its slot instead of a new window being added.
func (m *Mux) newWindowFrom(name string, p config.Profile, title string, _ bool) {
	if title == "" {
		title = name
	}
	m.mu.Lock()
	w := m.newWindow(title, name, p)
	w.steps.note("submitting")
	m.windows = append(m.windows, w)
	m.selectLocked(len(m.windows) - 1)
	m.mu.Unlock()
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		m.launch(w)
	}()
}

// launch gets w a Run: its profile's waiting spare, else a new one.
func (m *Mux) launch(w *Window) {
	if id := m.claimSpare(w); id != "" {
		m.mu.Lock()
		w.RunID = id
		m.byRun[id] = w
		w.steps.note("claimed warm spare " + id)
		m.saveLocked()
		if !w.closed {
			m.startSupervisor(w)
		}
		m.mu.Unlock()
		w.poke()
		return
	}
	secrets, err := w.Spec.ResolveSecrets(m.opt.Lookup)
	if err != nil {
		m.markDead(w, err.Error())
		return
	}
	sp, err := config.RunSpec(w.Spec, config.Target{Session: m.opt.Session, Profile: w.Profile}, secrets)
	if err != nil {
		m.markDead(w, err.Error())
		return
	}
	var run *api.Run
	bo := m.opt.Backoff
	for {
		ctx, cancel := context.WithTimeout(m.ctx, 30*time.Second)
		run, err = m.lux.Submit(ctx, sp)
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
	if m.ctx.Err() != nil {
		return
	}
	if err != nil {
		m.markDead(w, "submit: "+luxapi.Describe(err))
		return
	}
	m.mu.Lock()
	if w.closed {
		m.mu.Unlock()
		m.terminate(run.ID)
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
// profile, in the same slot.
func (m *Mux) relaunch(w *Window) {
	m.mu.Lock()
	if w.phase != phaseDead || w.closed {
		m.mu.Unlock()
		return
	}
	old := w.RunID
	if w.cancel != nil {
		w.cancel()
		w.cancel = nil
	}
	delete(m.byRun, old)
	w.RunID = ""
	w.phase = phaseStarting
	w.reason = ""
	w.status = ""
	w.hasScreen = false
	w.attachedOnce = false
	w.epoch = 0
	w.steps = progress{}
	w.steps.note("relaunching")
	w.feed <- feedItem{reset: true}
	m.repaintLocked()
	m.mu.Unlock()
	if old != "" {
		go m.terminate(old)
	}
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		m.launch(w)
	}()
}

// claimSpare takes the waiting spare of w's profile, if the profile keeps
// one and it is running or on its way; the Run keeps its spare label, so
// the claim is recorded in the session file.
func (m *Mux) claimSpare(w *Window) string {
	if !w.Spec.WarmSpare {
		return ""
	}
	m.mu.Lock()
	id := m.sess.Spares[w.Profile]
	if id != "" {
		delete(m.sess.Spares, w.Profile)
		m.sess.Claimed = append(m.sess.Claimed, id)
		m.saveLocked()
	}
	m.mu.Unlock()
	if id == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(m.ctx, 15*time.Second)
	run, err := m.lux.Get(ctx, id)
	cancel()
	if err != nil || !(run.State == "running" || run.State == "submitted" || run.State == "scheduled" ||
		run.State == "starting" || run.State == "provisioning") {
		return ""
	}
	go m.ensureSpare(w.Profile, w.Spec)
	return id
}

// keepSpares keeps one waiting spare of every configured profile with
// warm_spare on.
func (m *Mux) keepSpares() {
	defer m.wg.Done()
	for _, n := range m.opt.Config.ProfileNames() {
		if p := m.opt.Config.Profiles[n]; p.WarmSpare {
			m.ensureSpare(n, p)
		}
	}
}

// ensureSpare submits a spare for profile name unless it has one.
func (m *Mux) ensureSpare(name string, p config.Profile) {
	m.mu.Lock()
	if m.sess.Spares == nil {
		m.sess.Spares = map[string]string{}
	}
	if _, ok := m.sess.Spares[name]; ok || m.ctx.Err() != nil {
		m.mu.Unlock()
		return
	}
	m.sess.Spares[name] = "" // reserved while submitting
	m.mu.Unlock()
	secrets, err := p.ResolveSecrets(m.opt.Lookup)
	var run *api.Run
	if err == nil {
		var sp api.RunSpec
		sp, err = config.RunSpec(p, config.Target{Session: m.opt.Session, Profile: name, Spare: true}, secrets)
		if err == nil {
			ctx, cancel := context.WithTimeout(m.ctx, 30*time.Second)
			run, err = m.lux.Submit(ctx, sp)
			cancel()
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err != nil {
		delete(m.sess.Spares, name)
		if m.ctx.Err() == nil {
			m.setNoticeLocked(fmt.Sprintf("warm spare for %s: %s", name, luxapi.Describe(err)))
		}
		return
	}
	m.sess.Spares[name] = run.ID
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
			m.opt.Config.Profiles[req.SaveAs] = req.Profile
		}
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

// Recent are the images and repositories typed in the dialog lately.
func (m *Mux) Recent() (images, repos []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.sess.RecentImages), slices.Clone(m.sess.RecentRepos)
}

// Validate is lux's dry run of the spec a profile makes.
func (m *Mux) Validate(ctx context.Context, name string, p config.Profile) (*api.SubmitAnswer, error) {
	sp, err := config.RunSpec(p, config.Target{Session: m.opt.Session, Profile: name}, nil)
	if err != nil {
		return nil, err
	}
	return m.lux.Validate(ctx, sp)
}

// SaveProfileFunc writes a profile into the configuration file; tests
// replace it.
var SaveProfile = func(name string, p config.Profile) error { return config.AppendProfile(name, p) }
