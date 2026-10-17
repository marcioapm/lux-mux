package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/marcioapm/lux-mux/internal/config"
	"github.com/marcioapm/lux-mux/internal/info"
	"github.com/marcioapm/lux-mux/internal/sessions"
	"github.com/marcioapm/lux-mux/internal/state"
)

// infoTimeout bounds luxmux info's requests, all windows together. A
// variable: tests shorten it.
var infoTimeout = 10 * time.Second

// infoCmd is luxmux info [-t SESSION] [WINDOW] [--json]: each window's
// info, read from its own lux. Flags may follow WINDOW.
func infoCmd(env Env, args []string) error {
	fs := flag.NewFlagSet("info", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	name := fs.String("t", "", "session name (default: the session of the window this runs in, else main)")
	asJSON := fs.Bool("json", false, "print JSON")
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return err
		}
		if fs.NArg() == 0 {
			break
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
	if len(pos) > 1 {
		return errors.New("usage: luxmux info [-t SESSION] [WINDOW] [--json]")
	}
	cfg, err := loadConfig(env)
	if err != nil {
		return err
	}
	session, err := infoSession(env, *name)
	if err != nil {
		return err
	}
	eps := endpoints(env)
	x := luxes(env, eps, cfg)
	targets, err := infoTargets(env, x, session)
	if err != nil {
		return err
	}
	if len(pos) == 1 {
		if targets, err = pickWindow(targets, pos[0]); err != nil {
			return fmt.Errorf("session %s: %w", session, err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), infoTimeout)
	defer cancel()
	dial := info.Endpoints(eps)
	sampler := info.NewSampler()
	out := info.Output{SchemaVersion: info.SchemaVersion, Session: session, Windows: make([]info.Window, len(targets))}
	var wg sync.WaitGroup
	for i, t := range targets {
		wg.Go(func() { out.Windows[i] = info.Gather(ctx, dial, sampler, t, time.Now) })
	}
	wg.Wait()
	if *asJSON {
		b, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintln(env.Stdout, string(b))
	} else {
		for i, w := range out.Windows {
			if i > 0 {
				fmt.Fprintln(env.Stdout)
			}
			fmt.Fprint(env.Stdout, info.Text(w, time.Now()))
		}
	}
	failed := 0
	for _, w := range out.Windows {
		if w.Problem != "" {
			failed++
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d window(s) could not be read", failed, len(out.Windows))
	}
	return nil
}

// infoSession is -t, else the session of the window luxmux info runs in
// ($LUXMUX_SESSION holds its Runs' label: a renamed session is found by
// its rename entry), else DefaultSession.
func infoSession(env Env, name string) (string, error) {
	if name != "" {
		return name, sessions.CheckName(name)
	}
	label, ok := env.Lookup(config.EnvSession)
	if !ok || label == "" {
		return DefaultSession, nil
	}
	if err := sessions.CheckName(label); err != nil {
		return "", fmt.Errorf("%s: %w", config.EnvSession, err)
	}
	renames, err := state.Renames(env.StateDir)
	if err != nil {
		return "", err
	}
	for n, l := range renames {
		if l == label {
			return n, nil
		}
	}
	return label, nil
}

// infoTargets are session's windows in window order: its file's, else
// rebuilt from its Runs on every lux, as new-window rebuilds it.
func infoTargets(env Env, x sessions.Luxes, session string) ([]info.Target, error) {
	label, err := sessions.Label(env.StateDir, session)
	if err != nil {
		return nil, err
	}
	sess, ok, err := state.Load(env.StateDir, session)
	if err != nil {
		return nil, err
	}
	if !ok {
		ctx, cancel := context.WithTimeout(context.Background(), infoTimeout)
		defer cancel()
		ls := x.ListSession(ctx, label)
		if err := ls.Report(env.Stderr); err != nil {
			return nil, err
		}
		sess = state.RebuildLabelledFrom(session, label, ls, x.Eps.Default(), x.Cfg.Profiles)
	}
	if len(sess.Windows) == 0 {
		return nil, fmt.Errorf("no session %s", session)
	}
	def := x.Eps.Default()
	out := make([]info.Target, len(sess.Windows))
	for i, w := range sess.Windows {
		out[i] = info.Target{Index: i, Name: w.Name, Profile: w.Profile, Session: session, Label: label,
			Endpoint: w.EndpointOr(def), RunID: w.RunID}
	}
	return out, nil
}

// pickWindow is the window whose index is sel, else those named sel.
func pickWindow(ts []info.Target, sel string) ([]info.Target, error) {
	if i, err := strconv.Atoi(sel); err == nil {
		if i < 0 || i >= len(ts) {
			return nil, fmt.Errorf("no window %d (it has %d)", i, len(ts))
		}
		return ts[i : i+1], nil
	}
	var out []info.Target
	for _, t := range ts {
		if t.Name == sel {
			out = append(out, t)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no window named %q", sel)
	}
	return out, nil
}
