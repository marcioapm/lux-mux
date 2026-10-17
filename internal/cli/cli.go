// Package cli is luxmux's command line.
package cli

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"slices"
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
  luxmux info [-t NAME] [WINDOW] [--json]
                                  a session's windows (WINDOW: an index or a name) as
                                  their Runs are: image, placement, times, cost, use;
                                  -t defaults to the session it runs in, else main
  luxmux kill-session -t NAME [-y]  terminate a session's Runs and forget it
  luxmux spares [--rm]            list warm spares; --rm terminates them
  luxmux config init [--force]    write a commented example configuration

OVERRIDES change the profile for this window:
  --repo URL[#BRANCH]             a repository (repeatable; replaces the profile's)
  --branch B | --ref R            the branch or ref of the only repository
  --image I  --pool P  --size S | --cpus N --memory M --disk D
  --secret NAME                   one more secret (repeatable)
  --workdir W                     the shell's working directory
  --on-start CMD | --on-resume CMD  run in the window before its prompt: on the
                                  Run's first start, on each later placement

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
	case "info":
		return infoCmd(env, args[1:])
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
// profile and overrides, and attaches it. Its name must be free on every
// lux (sessions.CheckFree); the lux the first window goes to must answer.
// Runs of the name on another lux that did not answer join the session
// once it does, as a rebuild's do.
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
	ls, err := sessions.CheckFree(context.Background(), luxes(env, eps, cfg), env.StateDir, name, "", true)
	if errors.Is(err, sessions.ErrTaken) {
		return fmt.Errorf("%w: luxmux attach -t %s", err, name)
	} else if err != nil {
		return err
	}
	if err := ls.ErrOn(ep); err != nil {
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
	return startMux(env, eps, &state.Session{Name: name, DefaultProfile: o.Profile, Unlisted: ls.Unlisted()}, name, unlock, &p)
}

// attach attaches name: its file, else rebuilt from its Runs' labels on
// every lux the configuration and the file name (sessions.Open). create
// makes it when there is neither. A lux that cannot be listed leaves its
// windows as the file has them (they show it unreachable) or, rebuilding,
// adds them once it answers.
func attach(env Env, name string, create bool) error {
	if err := sessions.CheckName(name); err != nil {
		return err
	}
	unlock, err := lockSession(env, name)
	if err != nil {
		return err
	}
	defer unlock()
	cfg, err := loadConfig(env)
	if err != nil {
		return err
	}
	eps := endpoints(env)
	sess, label, existed, err := sessions.Open(context.Background(), luxes(env, eps, cfg), env.StateDir, name)
	if err != nil {
		return err
	}
	if !existed && !create {
		if len(sess.Unlisted) > 0 {
			// Its windows may be on a lux that did not answer: none is
			// made in their place.
			return fmt.Errorf("no session %s on the luxes that answered; try again when the others do", name)
		}
		return fmt.Errorf("no session %s: luxmux new -s %s", name, name)
	}
	return startMux(env, eps, sess, label, unlock, nil)
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
		// The session that exited is the last one the console showed.
		if m := con.Current(); m != nil {
			if left := m.Unterminated(); len(left) > 0 {
				fmt.Fprintf(env.Stderr, "session file kept: Runs not terminated:\n  %s\n", strings.Join(left, "\n  "))
			}
		}
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
	all, err := sessions.List(context.Background(), luxes(env, endpoints(env), cfg), env.StateDir)
	if err != nil {
		return err
	}
	if len(all) == 0 {
		fmt.Fprintln(env.Stderr, "no sessions")
		return nil
	}
	tw := tabwriter.NewWriter(env.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "SESSION\tWINDOWS\tRUNNING\tSPARES\tCREATED\tNOTE")
	for _, s := range all {
		notes := slices.Clone(s.Unreachable)
		if !s.HasFile {
			notes = append(notes, "no local file: attach rebuilds it from lux")
		}
		if s.Label != s.Name {
			notes = append(notes, "Runs labelled "+s.Label)
		}
		when := "-"
		if !s.Created.IsZero() {
			when = s.Created.Local().Format("2006-01-02 15:04")
		}
		fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%s\t%s\n", s.Name, s.Windows, s.Running, s.Spares, when, strings.Join(notes, "; "))
	}
	return tw.Flush()
}

// killSession terminates session name's Runs on every lux it has them
// on, each on its own, and forgets it (sessions.Killing): its file and
// rename entry are kept while a terminate failed or a lux it has windows
// on did not answer.
func killSession(env Env, name string, yes bool) error {
	cfg, err := loadConfig(env)
	if err != nil {
		return err
	}
	x := luxes(env, endpoints(env), cfg)
	k, err := sessions.PlanKill(context.Background(), x, env.StateDir, name)
	if err != nil {
		return err
	}
	if !yes && len(k.Live) > 0 {
		fmt.Fprintf(env.Stderr, "terminate %d Run(s) of session %s (their shells and anything not saved go)? [y/N] ", len(k.Live), name)
		line, _ := bufio.NewReader(env.Stdin).ReadString('\n')
		if a := strings.TrimSpace(strings.ToLower(line)); a != "y" && a != "yes" {
			return errors.New("not killed")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), bulkTimeout)
	defer cancel()
	return k.Do(ctx, x, env.StateDir, func(id string) { fmt.Fprintln(env.Stdout, "terminated", id) })
}

// spares lists (or with rm terminates) the warm spares no session has
// claimed, on every lux.
func spares(env Env, rm bool) error {
	cfg, err := loadConfig(env)
	if err != nil {
		return err
	}
	files := sessions.Files(env.StateDir)
	eps := endpoints(env)
	ls := luxes(env, eps, cfg).ListAll(context.Background(), files...)
	if err := ls.Report(env.Stderr); err != nil {
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
	out := ls.Live(func(r *api.Run) bool { return r.Labels[config.LabelSpare] == "true" && !claimed[r.ID] })
	if !rm {
		tw := tabwriter.NewWriter(env.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "RUN\tSESSION\tPROFILE\tSTATE\tLUX")
		for _, s := range out {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", s.Run.ID, s.Run.Labels[config.LabelSession], s.Run.Labels[config.LabelProfile], s.Run.State, s.Endpoint.Label())
		}
		return tw.Flush()
	}
	ctx, cancel := context.WithTimeout(context.Background(), bulkTimeout)
	defer cancel()
	var errs []error
	for _, s := range out {
		if err := s.Terminate(ctx, eps); err != nil {
			errs = append(errs, err)
			continue
		}
		fmt.Fprintln(env.Stdout, "terminated", s.Run.ID)
		sn := s.Run.Labels[config.LabelSession]
		if sf, ok, _ := state.Load(env.StateDir, sn); ok {
			for slot, id := range sf.Spares {
				if id == s.Run.ID {
					sf.DropSpare(slot)
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
