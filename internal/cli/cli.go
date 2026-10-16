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

func client(env Env) (*luxapi.Client, error) { return luxapi.New(env.Lux()) }

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
	c, err := client(env)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := sessions.CheckFree(ctx, c, env.StateDir, name, "", true); errors.Is(err, sessions.ErrTaken) {
		return fmt.Errorf("%w: luxmux attach -t %s", err, name)
	} else if err != nil {
		return err
	}
	cfg, err := loadConfig(env)
	if err != nil {
		return err
	}
	profile, p, err := windowProfile(cfg, "", o, env.Stderr)
	if err != nil {
		return err
	}
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
	return runMux(env, c, &state.Session{Name: name, DefaultProfile: o.Profile}, name, unlock, &p)
}

// attach attaches name: its file, else rebuilt from its Runs' labels.
// create makes it when there is neither.
func attach(env Env, name string, create bool) error {
	if err := sessions.CheckName(name); err != nil {
		return err
	}
	unlock, err := lockSession(env, name)
	if err != nil {
		return err
	}
	defer unlock()
	c, err := client(env)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cfg, err := loadConfig(env)
	if err != nil {
		return err
	}
	sess, label, existed, err := sessions.Open(ctx, c, env.StateDir, cfg, name)
	if err != nil {
		return err
	}
	if !existed && !create {
		return fmt.Errorf("no session %s: luxmux new -s %s", name, name)
	}
	return runMux(env, c, sess, label, unlock, nil)
}

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
func runMux(env Env, c *luxapi.Client, sess *state.Session, label string, unlock func(), first *config.Profile) error {
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
		Config: cfg, Lux: c, StateDir: env.StateDir, In: in, Out: env.Stdout, Size: size, UI: env.UI,
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

func list(env Env) error {
	c, err := client(env)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	all, err := sessions.List(ctx, c, env.StateDir)
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
		var notes []string
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

// killSession terminates session name's Runs and forgets it; a rename
// entry whose Runs all ended, with no file left, is dropped.
func killSession(env Env, name string, yes bool) error {
	c, err := client(env)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	live, err := sessions.LiveRuns(ctx, c, env.StateDir, name)
	if err != nil {
		return err
	}
	_, hasFile, _ := state.Load(env.StateDir, name)
	renames, err := state.Renames(env.StateDir)
	if err != nil {
		return err
	}
	_, mapped := renames[name]
	if len(live) == 0 && !hasFile && !mapped {
		return fmt.Errorf("no session %s", name)
	}
	if !yes && len(live) > 0 {
		fmt.Fprintf(env.Stderr, "terminate %d Run(s) of session %s (their shells and anything not saved go)? [y/N] ", len(live), name)
		line, _ := bufio.NewReader(env.Stdin).ReadString('\n')
		if a := strings.TrimSpace(strings.ToLower(line)); a != "y" && a != "yes" {
			return errors.New("not killed")
		}
	}
	return sessions.Kill(ctx, c, env.StateDir, name, func(id string) { fmt.Fprintln(env.Stdout, "terminated", id) })
}

func spares(env Env, rm bool) error {
	c, err := client(env)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	runs, err := c.ListAll(ctx)
	if err != nil {
		return err
	}
	// A spare label stays on a claimed spare: the session files say
	// which were claimed.
	claimed := map[string]bool{}
	files, _ := state.List(env.StateDir)
	for _, f := range files {
		if s, ok, _ := state.Load(env.StateDir, f); ok {
			for _, id := range s.Claimed {
				claimed[id] = true
			}
			for _, w := range s.Windows {
				claimed[w.RunID] = true
			}
		}
	}
	var out []*api.Run
	for _, r := range runs {
		if r.Labels[config.LabelSpare] == "true" && state.Live(r.State) && !claimed[r.ID] {
			out = append(out, r)
		}
	}
	if !rm {
		tw := tabwriter.NewWriter(env.Stdout, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "RUN\tSESSION\tPROFILE\tSTATE")
		for _, r := range out {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", r.ID, r.Labels[config.LabelSession], r.Labels[config.LabelProfile], r.State)
		}
		return tw.Flush()
	}
	var errs []error
	for _, r := range out {
		if err := c.Terminate(ctx, r.ID); err != nil {
			errs = append(errs, err)
			continue
		}
		fmt.Fprintln(env.Stdout, "terminated", r.ID)
		sn := r.Labels[config.LabelSession]
		if s, ok, _ := state.Load(env.StateDir, sn); ok {
			for p, id := range s.Spares {
				if id == r.ID {
					delete(s.Spares, p)
				}
			}
			_ = state.Save(env.StateDir, s)
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
