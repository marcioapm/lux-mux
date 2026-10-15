package cli

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/marcioapm/lux/api"

	"github.com/marcioapm/lux-mux/internal/config"
	"github.com/marcioapm/lux-mux/internal/luxapi"
	"github.com/marcioapm/lux-mux/internal/state"
)

// submitWindow checks p's spec with lux's dry run, then submits it as a
// window of session. external labels the Run as made outside the attached
// process (new-window), which is what an attached luxmux adopts.
func submitWindow(ctx context.Context, env Env, c *luxapi.Client, session, profile, name string, p config.Profile, external bool) (state.Window, error) {
	label := func(sp *api.RunSpec) {
		if external {
			sp.Labels[config.LabelExternal] = "true"
		}
		if name != "" {
			sp.Labels[config.LabelWindow] = name
		}
	}
	t := config.Target{Session: session, Profile: profile}
	sp, err := config.RunSpec(p, t, nil)
	if err != nil {
		return state.Window{}, err
	}
	label(&sp)
	if err := validate(ctx, env, c, sp); err != nil {
		return state.Window{}, err
	}
	values, err := p.ResolveSecrets(env.Lookup)
	if err != nil {
		return state.Window{}, err
	}
	if sp, err = config.RunSpec(p, t, values); err != nil {
		return state.Window{}, err
	}
	label(&sp)
	run, err := c.Submit(ctx, sp)
	if err != nil {
		return state.Window{}, fmt.Errorf("submit: %s", luxapi.Describe(err))
	}
	if name == "" {
		name = profile
	}
	return state.Window{RunID: run.ID, Name: name, Profile: profile, Spec: &p}, nil
}

// dryRun checks the spec a window of profile p would be submitted with.
func dryRun(ctx context.Context, env Env, c *luxapi.Client, session, profile string, p config.Profile) error {
	sp, err := config.RunSpec(p, config.Target{Session: session, Profile: profile}, nil)
	if err != nil {
		return err
	}
	return validate(ctx, env, c, sp)
}

// validate is lux's dry run of sp: its refusal is the error. A spec lux
// accepts but has no capacity for now is submitted, as the dialog's
// second Create does, after a warning on stderr: it waits in the queue.
func validate(ctx context.Context, env Env, c *luxapi.Client, sp api.RunSpec) error {
	ans, err := c.Validate(ctx, sp)
	if err != nil {
		return fmt.Errorf("lux refuses it: %s", luxapi.Describe(err))
	}
	if ans == nil || !ans.Valid {
		return errors.New("lux refuses it: its dry run did not find it valid")
	}
	if !ans.WouldStartNow {
		fmt.Fprintf(env.Stderr, "luxmux: lux would not start it now (it waits for capacity): %s\n", strings.Join(blockers(ans), "; "))
	}
	return nil
}

func blockers(a *api.SubmitAnswer) []string {
	var out []string
	for _, b := range a.Blockers {
		s := fmt.Sprintf("%s (%d hosts)", b.Code, b.Hosts)
		if b.Reason != "" {
			s += ": " + b.Reason
		}
		out = append(out, s)
	}
	return out
}

// windowProfile is the profile a new window is made from: -p, else the
// session's default, else the configuration's, with the overrides on it.
func windowProfile(cfg *config.Config, sessionDefault string, o windowFlags) (string, config.Profile, error) {
	name := o.Profile
	if name == "" {
		name = sessionDefault
	}
	if name == "" {
		name = cfg.DefaultProfile
	}
	p, ok := cfg.Profiles[name]
	if !ok {
		return "", config.Profile{}, fmt.Errorf("no profile named %q", name)
	}
	p, err := o.apply(p)
	return name, p, err
}

// newWindow adds a window to a session from outside it. An attached
// luxmux holds the session's lock and owns its file: it adopts the Run
// from lux's event feed. With none attached, the lock is taken for the
// time it takes to submit and record the window.
func newWindow(env Env, o windowFlags) error {
	if err := checkName(o.Session); err != nil {
		return err
	}
	unlock, err := state.Lock(env.StateDir, o.Session)
	attached := errors.Is(err, state.ErrLocked)
	if err != nil && !attached {
		return err
	}
	if unlock != nil {
		defer unlock()
	}
	c, err := client(env)
	if err != nil {
		return err
	}
	cfg, err := loadConfig(env)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	runs, err := c.ListSession(ctx, o.Session)
	if err != nil {
		return err
	}
	// Read-only while attached: the file is the attached process's.
	sess, ok, err := state.Load(env.StateDir, o.Session)
	if err != nil {
		return err
	}
	if !ok {
		sess = state.Rebuild(o.Session, runs, cfg.Profiles)
	}
	if !ok && len(sess.Windows) == 0 {
		return fmt.Errorf("no session %s: luxmux new -s %s", o.Session, o.Session)
	}
	profile, p, err := windowProfile(cfg, sess.DefaultProfile, o)
	if err != nil {
		return err
	}
	w, err := submitWindow(ctx, env, c, o.Session, profile, o.Name, p, true)
	if err != nil {
		return err
	}
	if !attached {
		sess.Windows = append(sess.Windows, w)
		if err := state.Save(env.StateDir, sess); err != nil {
			return err
		}
	}
	fmt.Fprintln(env.Stdout, w.RunID)
	return nil
}

// recordExternal appends to sess the windows new-window made while no
// luxmux had the session but whose record never landed (the file was
// written by a process that detached after new-window read it).
func recordExternal(sess *state.Session, runs []*api.Run, cfg *config.Config) {
	for _, r := range runs {
		if r.Labels[config.LabelExternal] != "true" || !state.Live(r.State) {
			continue
		}
		if slices.ContainsFunc(sess.Windows, func(w state.Window) bool { return w.RunID == r.ID }) {
			continue
		}
		sess.Windows = append(sess.Windows, state.WindowFromRun(r, cfg.Profiles[r.Labels[config.LabelProfile]]))
	}
}
