package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/marcioapm/lux/api"

	"github.com/marcioapm/lux-mux/internal/config"
	"github.com/marcioapm/lux-mux/internal/luxapi"
	"github.com/marcioapm/lux-mux/internal/sessions"
	"github.com/marcioapm/lux-mux/internal/state"
)

// submitWindow checks p's spec with lux's dry run, then submits it as a
// window of the session whose Runs are labelled label, which an attached
// luxmux adopts.
func submitWindow(ctx context.Context, env Env, c *luxapi.Client, label, profile, name string, p config.Profile) (state.Window, error) {
	t := config.Target{Session: label, Profile: profile}
	sp, err := externalSpec(p, t, name, nil)
	if err != nil {
		return state.Window{}, err
	}
	if err := validate(ctx, env, c, sp); err != nil {
		return state.Window{}, err
	}
	values, err := p.ResolveSecrets(env.Lookup)
	if err != nil {
		return state.Window{}, err
	}
	if sp, err = externalSpec(p, t, name, values); err != nil {
		return state.Window{}, err
	}
	run, err := c.Submit(ctx, sp)
	if err != nil {
		return state.Window{}, fmt.Errorf("submit: %s", luxapi.Describe(err))
	}
	if name == "" {
		name = profile
	}
	return state.Window{RunID: run.ID, Name: name, Profile: profile, Spec: &p}, nil
}

// externalSpec is RunSpec labelled as made outside the attached process,
// which is what an attached luxmux adopts, and with -n's window name.
func externalSpec(p config.Profile, t config.Target, name string, secrets map[string]string) (api.RunSpec, error) {
	sp, err := config.RunSpec(p, t, secrets)
	if err != nil {
		return sp, err
	}
	sp.Labels[config.LabelExternal] = "true"
	if name != "" {
		sp.Labels[config.LabelWindow] = name
	}
	return sp, nil
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
// session's default while the configuration has it, else the
// configuration's (as C-b c), with the overrides on it. A session default
// that is gone is said on warn.
func windowProfile(cfg *config.Config, sessionDefault string, o windowFlags, warn io.Writer) (string, config.Profile, error) {
	name := o.Profile
	if name == "" && sessionDefault != "" {
		if _, ok := cfg.Profiles[sessionDefault]; ok {
			name = sessionDefault
		} else {
			fmt.Fprintf(warn, "luxmux: the session's default profile %q is not in the configuration; using %q\n", sessionDefault, cfg.DefaultProfile)
		}
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
	label, err := sessions.LiveLabel(ctx, c, env.StateDir, o.Session)
	if err != nil {
		return err
	}
	runs, err := c.ListSession(ctx, label)
	if err != nil {
		return err
	}
	// Read-only while attached: the file is the attached process's.
	sess, ok, err := state.Load(env.StateDir, o.Session)
	if err != nil {
		return err
	}
	if !ok {
		sess = state.RebuildLabelled(o.Session, label, runs, cfg.Profiles)
	}
	if !ok && len(sess.Windows) == 0 {
		return fmt.Errorf("no session %s: luxmux new -s %s", o.Session, o.Session)
	}
	profile, p, err := windowProfile(cfg, sess.DefaultProfile, o, env.Stderr)
	if err != nil {
		return err
	}
	w, err := submitWindow(ctx, env, c, label, profile, o.Name, p)
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
