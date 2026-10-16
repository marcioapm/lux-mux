// Package cli is luxmux's command line.
package cli

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"os/signal"
	"slices"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/marcioapm/lux/api"
	"golang.org/x/term"

	"github.com/marcioapm/lux-mux/internal/config"
	"github.com/marcioapm/lux-mux/internal/luxapi"
	"github.com/marcioapm/lux-mux/internal/mux"
	"github.com/marcioapm/lux-mux/internal/sessions"
	"github.com/marcioapm/lux-mux/internal/state"
)

// DefaultSession is the session plain `luxmux` attaches to or creates.
const DefaultSession = "main"

// Env is what the commands read and write besides the network.
type Env struct {
	Stdin          io.Reader
	Stdout, Stderr io.Writer
	// Lux is the lux client configuration (api.LoadConfig()).
	Lux func() api.Config
	// ConfigPath and StateDir locate luxmux's own files.
	ConfigPath string
	StateDir   string
	Lookup     func(string) (string, bool)
	// UI runs the dialogs (nil: none).
	UI mux.UI
}

const usage = `luxmux: a terminal multiplexer whose windows are shells on lux Runs.

  luxmux                          attach the default session (main), creating it
  luxmux new [-s NAME] [-p PROFILE] [OVERRIDES]
                                  create a session and attach it; -p is its default profile
  luxmux new-window [-t NAME] [-n WINDOW] [-p PROFILE] [OVERRIDES]
                                  add a window to a session, attached or not
  luxmux attach [-t NAME]         attach a session (rebuilt from lux if its file is gone)
  luxmux ls                       list sessions
  luxmux kill-session -t NAME [-y]  terminate a session's Runs and forget it
  luxmux spares [--rm]            list warm spares; --rm terminates them
  luxmux config init [--force]    write a commented example configuration

OVERRIDES change the profile for this window:
  --repo URL[#BRANCH]             a repository (repeatable; replaces the profile's)
  --branch B | --ref R            the branch or ref of the only repository
  --image I  --pool P  --size S | --cpus N --memory M --disk D
  --secret NAME                   one more secret (repeatable)
  --workdir W                     the shell's working directory

Inside a session the prefix is C-b by default; C-b ? lists the keys.
`

// Main runs luxmux with args and returns its exit code.
func Main(env Env, args []string) int {
	if err := run(env, args); err != nil {
		fmt.Fprintln(env.Stderr, "luxmux:", err)
		return 1
	}
	return 0
}

func run(env Env, args []string) error {
	if len(args) == 0 {
		return attach(env, DefaultSession, true)
	}
	switch args[0] {
	case "-h", "--help", "help":
		fmt.Fprint(env.Stdout, usage)
		return nil
	case "new", "new-session":
		o, err := parseWindowFlags("new", args[1:], env.Stderr)
		if err != nil {
			return err
		}
		if o.Session == "" {
			if o.Session, err = freeName(env); err != nil {
				return err
			}
		}
		return newSession(env, o)
	case "new-window":
		o, err := parseWindowFlags("new-window", args[1:], env.Stderr)
		if err != nil {
			return err
		}
		return newWindow(env, o)
	case "attach", "attach-session", "a":
		fs := flag.NewFlagSet("attach", flag.ContinueOnError)
		name := fs.String("t", DefaultSession, "session name")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		return attach(env, *name, false)
	case "ls", "list-sessions":
		return list(env)
	case "kill-session":
		fs := flag.NewFlagSet("kill-session", flag.ContinueOnError)
		name := fs.String("t", "", "session name")
		yes := fs.Bool("y", false, "do not ask")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *name == "" {
			return errors.New("kill-session: -t NAME is required")
		}
		return killSession(env, *name, *yes)
	case "spares":
		fs := flag.NewFlagSet("spares", flag.ContinueOnError)
		rm := fs.Bool("rm", false, "terminate every warm spare")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		return spares(env, *rm)
	case "config":
		if len(args) >= 2 && args[1] == "init" {
			fs := flag.NewFlagSet("config init", flag.ContinueOnError)
			force := fs.Bool("force", false, "overwrite an existing file")
			if err := fs.Parse(args[2:]); err != nil {
				return err
			}
			return configInit(env, *force)
		}
		return errors.New("usage: luxmux config init [--force]")
	}
	return fmt.Errorf("unknown command %q\n\n%s", args[0], usage)
}

func loadConfig(env Env) (*config.Config, error) { return config.Load(env.ConfigPath) }

// loadSessions are the session files in the state directory.
func loadSessions(env Env) []*state.Session {
	files, _ := state.List(env.StateDir)
	var out []*state.Session
	for _, f := range files {
		if s, ok, _ := state.Load(env.StateDir, f); ok {
			out = append(out, s)
		}
	}
	return out
}

func freeName(env Env) (string, error) {
	names, err := state.List(env.StateDir)
	if err != nil {
		return "", err
	}
	renames, err := state.Renames(env.StateDir)
	if err != nil {
		return "", err
	}
	used := map[string]bool{}
	for _, n := range names {
		used[n] = true
	}
	for n, l := range renames {
		used[n], used[l] = true, true
	}
	for i := 0; ; i++ {
		n := fmt.Sprint(i)
		if !used[n] {
			return n, nil
		}
	}
}

// newSession creates session o.Session with a first window of o's
// profile and overrides, and attaches it.
func newSession(env Env, o windowFlags) error {
	name := o.Session
	if err := sessions.CheckName(name); err != nil {
		return err
	}
	unlock, err := lockSession(env, name)
	if err != nil {
		return err
	}
	defer unlock()
	if _, ok, err := state.Load(env.StateDir, name); err != nil {
		return err
	} else if ok {
		return fmt.Errorf("session %s exists: luxmux attach -t %s", name, name)
	}
	cfg, err := loadConfig(env)
	if err != nil {
		return err
	}
	profile, p, err := windowProfile(cfg, "", o, env.Stderr)
	if err != nil {
		return err
	}
	eps := endpoints(env)
	ep := eps.Of(p)
	c, err := eps.Client(ep)
	if err != nil {
		return err
	}
	ls := listSession(eps, knownEndpoints(eps, cfg), name)
	// Runs of name on a lux that did not answer are added to the session
	// once it does, as a rebuild's are.
	var failed []config.Endpoint
	for _, l := range ls {
		if l.Endpoint == ep && l.Err != nil {
			return l.Err
		}
		if l.Err != nil {
			failed = append(failed, l.Endpoint)
		}
		for _, r := range l.Runs {
			if state.Live(r.State) {
				return fmt.Errorf("session %s has Runs on lux %s: luxmux attach -t %s", name, l.Endpoint.Label(), name)
			}
		}
	}
	if err := reportFailed(env.Stderr, ls); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	if err := dryRun(ctx, env, c, name, profile, p); err != nil {
		return err
	}
	// As new-window: values not found are refused before the session
	// exists, not left to a dead first window.
	if _, err := p.ResolveSecrets(env.Lookup); err != nil {
		return err
	}
	// Only -p pins the session's default; without it C-b c follows
	// default_profile.
	return startMux(env, eps, &state.Session{Name: name, DefaultProfile: o.Profile, Unlisted: failed}, name, unlock, &p)
}

// attach attaches name: its file, else rebuilt from its Runs' labels on
// every lux the configuration and the file name. create makes it when
// there is neither. A lux that cannot be listed leaves its windows as the
// file has them (they show it unreachable) or, rebuilding, adds them once
// it answers.
func attach(env Env, name string, create bool) error {
	if err := sessions.CheckName(name); err != nil {
		return err
	}
	unlock, err := lockSession(env, name)
	if err != nil {
		return err
	}
	defer unlock()
	sess, ok, err := state.Load(env.StateDir, name)
	if err != nil {
		return err
	}
	cfg, err := loadConfig(env)
	if err != nil {
		return err
	}
	eps := endpoints(env)
	ls := listSession(eps, knownEndpoints(eps, cfg, sess), name)
	if err := reportFailed(env.Stderr, ls); err != nil {
		return err
	}
	if ok {
		recordExternal(sess, ls, cfg)
		sess.ReconcileUp(known(ls), eps.Default(), downOf(ls))
	} else {
		sess = state.RebuildFrom(name, ls, eps.Default(), cfg.Profiles)
	}
	if len(sess.Windows) == 0 && !create && !ok {
		if len(sess.Unlisted) > 0 {
			// Its windows may be on a lux that did not answer: none is
			// made in their place.
			return fmt.Errorf("no session %s on the luxes that answered; try again when the others do", name)
		}
		return fmt.Errorf("no session %s: luxmux new -s %s", name, name)
	}
	return startMux(env, eps, sess, name, unlock, nil)
}

// startMux is runMux; tests see the session it is given without a
// terminal.
var startMux = runMux

// lockSession makes this process the session's only one until unlock.
func lockSession(env Env, name string) (func(), error) {
	unlock, err := state.Lock(env.StateDir, name)
	if errors.Is(err, state.ErrLocked) {
		return nil, fmt.Errorf("session %s: %w; detach it there first", name, err)
	}
	return unlock, err
}

// runMux puts the terminal in raw mode and runs the session (label: its
// Runs' label; unlock: its lock, which mux releases on a switch), then
// each session switched to from inside it. Its first window, in a
// session that has none, is made from first (nil: the session's default
// profile).
func runMux(env Env, eps *luxapi.Endpoints, sess *state.Session, label string, unlock func(), first *config.Profile) error {
	cfg, err := loadConfig(env)
	if err != nil {
		return err
	}
	in, ok := env.Stdin.(*os.File)
	if !ok || !term.IsTerminal(int(in.Fd())) {
		return errors.New("luxmux needs a terminal")
	}
	outFd := int(os.Stdout.Fd())
	if f, ok := env.Stdout.(*os.File); ok {
		outFd = int(f.Fd())
	}
	size := func() (int, int) {
		cols, rows, err := term.GetSize(outFd)
		if err != nil || rows <= 0 || cols <= 0 {
			return 24, 80
		}
		return rows, cols
	}
	old, err := term.MakeRaw(int(in.Fd()))
	if err != nil {
		return err
	}
	defer term.Restore(int(in.Fd()), old)
	// After a panic the terminal is given back (raw mode off here, the
	// screen and modes by mux) before the panic is printed.
	crash := func(msg string) {
		_ = term.Restore(int(in.Fd()), old)
		fmt.Fprint(env.Stderr, msg)
		os.Exit(2)
	}
	con := mux.NewConsole(in, env.Stdout)
	defer con.RecoverPanic()
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)
	go func() {
		defer con.RecoverPanic()
		for range winch {
			con.Resize(size())
		}
	}()
	// SIGTERM and SIGHUP (the terminal closed) end the session as a
	// detach does: the terminal is given back, the Runs keep running.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	reason, name, err := mux.Attach(ctx, mux.Options{Session: sess.Name, Label: label, Unlock: unlock, Console: con,
		Config: cfg, Endpoints: eps, StateDir: env.StateDir, In: in, Out: env.Stdout, Size: size, UI: env.UI,
		Lookup: env.Lookup, Crash: crash, First: first}, sess)
	con.Close(2 * time.Second)
	term.Restore(int(in.Fd()), old)
	if err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	switch reason {
	case "detached":
		fmt.Fprintf(env.Stderr, "[detached (from session %s)]\n", name)
	case "exited":
		fmt.Fprintf(env.Stderr, "[exited (session %s)]\n", name)
	}
	return nil
}

// list lists the sessions across every lux the configuration and the
// session files name. A lux that cannot be listed is said on stderr and
// in the NOTE of the sessions whose files have windows there.
func list(env Env) error {
	cfg, err := loadConfig(env)
	if err != nil {
		return err
	}
	files := loadSessions(env)
	eps := endpoints(env)
	ls := listAll(eps, knownEndpoints(eps, cfg, files...))
	if err := reportFailed(env.Stderr, ls); err != nil {
		return err
	}
	failedOn := map[config.Endpoint]error{}
	for _, l := range ls {
		if l.Err != nil {
			failedOn[l.Endpoint] = l.Err
		}
	}
	byS := map[string][]*api.Run{}
	for _, t := range liveRuns(ls, func(r *api.Run) bool { return r.Labels[config.LabelSession] != "" }) {
		s := t.r.Labels[config.LabelSession]
		byS[s] = append(byS[s], t.r)
	}
	names := map[string]bool{}
	fileOf := map[string]*state.Session{}
	for _, f := range files {
		names[f.Name] = true
		fileOf[f.Name] = f
	}
	for n := range byS {
		names[n] = true
	}
	sorted := make([]string, 0, len(names))
	for n := range names {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted)
	if len(sorted) == 0 {
		fmt.Fprintln(env.Stderr, "no sessions")
		return nil
	}
	tw := tabwriter.NewWriter(env.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "SESSION\tWINDOWS\tRUNNING\tSPARES\tCREATED\tNOTE")
	for _, n := range sorted {
		var windows, running, spares int
		var created time.Time
		for _, r := range byS[n] {
			if r.Labels[config.LabelSpare] == "true" {
				spares++
			}
			windows++
			if r.State == "running" {
				running++
			}
			if created.IsZero() || r.CreatedAt.Before(created) {
				created = r.CreatedAt
			}
		}
		var notes []string
		if s := fileOf[n]; s != nil {
			windows = len(s.Windows)
			spares = len(s.Spares)
			unreachable := map[string]bool{}
			for _, w := range s.Windows {
				ep := w.EndpointOr(eps.Default())
				if _, down := failedOn[ep]; down {
					unreachable["endpoint unreachable: "+ep.Label()] = true
				}
			}
			for _, l := range slices.Sorted(maps.Keys(unreachable)) {
				notes = append(notes, l)
			}
		} else {
			notes = append(notes, "no local file: attach rebuilds it from lux")
			windows -= spares
		}
		when := "-"
		if !created.IsZero() {
			when = created.Local().Format("2006-01-02 15:04")
		}
		fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%s\t%s\n", n, windows, running, spares, when, strings.Join(notes, "; "))
	}
	return tw.Flush()
}

// killSession terminates the session's Runs on every lux it has them on.
func killSession(env Env, name string, yes bool) error {
	cfg, err := loadConfig(env)
	if err != nil {
		return err
	}
	file, hasFile, _ := state.Load(env.StateDir, name)
	eps := endpoints(env)
	ls := listSession(eps, knownEndpoints(eps, cfg, file), name)
	if err := reportFailed(env.Stderr, ls); err != nil {
		return err
	}
	live := liveRuns(ls, func(*api.Run) bool { return true })
	// Without a file, a lux that did not answer may hold any of the
	// session's Runs.
	var unlisted []string
	for _, l := range ls {
		if l.Err != nil {
			unlisted = append(unlisted, l.Endpoint.Label())
		}
	}
	mayRemain := func() error {
		return fmt.Errorf("session %s may still have Runs on %s: run kill-session again when it answers", name, strings.Join(unlisted, ", "))
	}
	if len(live) == 0 && !hasFile {
		if len(unlisted) > 0 {
			return mayRemain()
		}
		return fmt.Errorf("no session %s", name)
	}
	if !yes && len(live) > 0 {
		fmt.Fprintf(env.Stderr, "terminate %d Run(s) of session %s (their shells and anything not saved go)? [y/N] ", len(live), name)
		line, _ := bufio.NewReader(env.Stdin).ReadString('\n')
		if a := strings.TrimSpace(strings.ToLower(line)); a != "y" && a != "yes" {
			return errors.New("not killed")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), bulkTimeout)
	defer cancel()
	var errs []error
	for _, t := range live {
		c, err := eps.Client(t.ep)
		if err == nil {
			err = c.Terminate(ctx, t.r.ID)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", t.r.ID, err))
		} else {
			fmt.Fprintln(env.Stdout, "terminated", t.r.ID)
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	if hasFile && slices.ContainsFunc(knownEndpoints(eps, &config.Config{}, file), downOf(ls)) {
		// Its Runs on a lux that did not answer may still be there.
		return errors.New("session file kept: a lux it has windows on did not answer")
	}
	if !hasFile && len(unlisted) > 0 {
		return mayRemain()
	}
	return state.Remove(env.StateDir, name)
}

// spares lists (or with rm terminates) the warm spares no session has
// claimed, on every lux.
func spares(env Env, rm bool) error {
	cfg, err := loadConfig(env)
	if err != nil {
		return err
	}
	files := loadSessions(env)
	eps := endpoints(env)
	ls := listAll(eps, knownEndpoints(eps, cfg, files...))
	if err := reportFailed(env.Stderr, ls); err != nil {
		return err
	}
	// A spare label stays on a claimed spare: the session files say
	// which were claimed.
	claimed := map[string]bool{}
	for _, s := range files {
		for _, id := range s.Claimed {
			claimed[id] = true
		}
		for _, w := range s.Windows {
			claimed[w.RunID] = true
		}
	}
	out := liveRuns(ls, func(r *api.Run) bool { return r.Labels[config.LabelSpare] == "true" && !claimed[r.ID] })
	if !rm {
		tw := tabwriter.NewWriter(env.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "RUN\tSESSION\tPROFILE\tSTATE\tLUX")
		for _, s := range out {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", s.r.ID, s.r.Labels[config.LabelSession], s.r.Labels[config.LabelProfile], s.r.State, s.ep.Label())
		}
		return tw.Flush()
	}
	ctx, cancel := context.WithTimeout(context.Background(), bulkTimeout)
	defer cancel()
	var errs []error
	for _, s := range out {
		c, err := eps.Client(s.ep)
		if err == nil {
			err = c.Terminate(ctx, s.r.ID)
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		fmt.Fprintln(env.Stdout, "terminated", s.r.ID)
		sn := s.r.Labels[config.LabelSession]
		if sf, ok, _ := state.Load(env.StateDir, sn); ok {
			for slot, id := range sf.Spares {
				if id == s.r.ID {
					delete(sf.Spares, slot)
					delete(sf.SpareEndpoints, slot)
				}
			}
			_ = state.Save(env.StateDir, sf)
		}
	}
	return errors.Join(errs...)
}

func configInit(env Env, force bool) error {
	if _, err := os.Stat(env.ConfigPath); err == nil && !force {
		return fmt.Errorf("%s exists (--force overwrites it)", env.ConfigPath)
	}
	if err := os.MkdirAll(dirOf(env.ConfigPath), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(env.ConfigPath, []byte(config.Example), 0o600); err != nil {
		return err
	}
	fmt.Fprintln(env.Stdout, "wrote", env.ConfigPath)
	return nil
}

func dirOf(p string) string {
	if i := strings.LastIndex(p, "/"); i > 0 {
		return p[:i]
	}
	return "."
}
