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
		n := o.Session
		if n == "" {
			var err error
			if n, err = freeName(env); err != nil {
				return err
			}
		}
		return newSessionWith(env, n, o)
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

func checkName(n string) error {
	if !config.NameRe.MatchString(n) {
		return fmt.Errorf("session name %q: letters, digits, '.', '_' and '-'", n)
	}
	return nil
}

// sessionRuns groups luxmux's live Runs by session.
func sessionRuns(ctx context.Context, c *luxapi.Client) (map[string][]*api.Run, error) {
	runs, err := c.ListAll(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string][]*api.Run{}
	for _, r := range runs {
		if s := r.Labels[config.LabelSession]; s != "" && state.Live(r.State) {
			out[s] = append(out[s], r)
		}
	}
	return out, nil
}

func freeName(env Env) (string, error) {
	names, err := state.List(env.StateDir)
	if err != nil {
		return "", err
	}
	used := map[string]bool{}
	for _, n := range names {
		used[n] = true
	}
	for i := 0; ; i++ {
		n := fmt.Sprint(i)
		if !used[n] {
			return n, nil
		}
	}
}

func newSession(env Env, name string) error {
	return newSessionWith(env, name, windowFlags{})
}

func newSessionWith(env Env, name string, o windowFlags) error {
	if err := checkName(name); err != nil {
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
	c, err := client(env)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	runs, err := c.ListSession(ctx, name)
	if err != nil {
		return err
	}
	for _, r := range runs {
		if state.Live(r.State) {
			return fmt.Errorf("session %s has Runs on lux: luxmux attach -t %s", name, name)
		}
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
	return runMuxFirst(env, c, &state.Session{Name: name, DefaultProfile: o.Profile}, &p)
}

// attach attaches name: its file, else rebuilt from its Runs' labels.
// create makes it when there is neither.
func attach(env Env, name string, create bool) error {
	if err := checkName(name); err != nil {
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
	runs, err := c.ListSession(ctx, name)
	if err != nil {
		return err
	}
	sess, ok, err := state.Load(env.StateDir, name)
	if err != nil {
		return err
	}
	cfg, err := loadConfig(env)
	if err != nil {
		return err
	}
	if ok {
		known := map[string]string{}
		for _, r := range runs {
			known[r.ID] = r.State
		}
		recordExternal(sess, runs, cfg)
		sess.Reconcile(known)
	} else {
		sess = state.Rebuild(name, runs, cfg.Profiles)
	}
	if len(sess.Windows) == 0 && !create && !ok {
		return fmt.Errorf("no session %s: luxmux new -s %s", name, name)
	}
	return runMux(env, c, sess)
}

// lockSession makes this process the session's only one until unlock.
func lockSession(env Env, name string) (func(), error) {
	unlock, err := state.Lock(env.StateDir, name)
	if errors.Is(err, state.ErrLocked) {
		return nil, fmt.Errorf("session %s: %w; detach it there first", name, err)
	}
	return unlock, err
}

// runMux puts the terminal in raw mode and runs the session.
func runMux(env Env, c *luxapi.Client, sess *state.Session) error {
	return runMuxFirst(env, c, sess, nil)
}

// runMuxFirst is runMux whose first window, in a session that has none,
// is made from first (nil: the session's default profile).
func runMuxFirst(env Env, c *luxapi.Client, sess *state.Session, first *config.Profile) error {
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
	m, err := mux.New(mux.Options{Session: sess.Name, Config: cfg, Lux: c, StateDir: env.StateDir,
		In: in, Out: env.Stdout, Size: size, UI: env.UI, Lookup: env.Lookup, Crash: crash, First: first}, sess)
	if err != nil {
		return err
	}
	defer m.RecoverPanic()
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)
	go func() {
		defer m.RecoverPanic()
		for range winch {
			m.Resize(size())
		}
	}()
	// SIGTERM and SIGHUP (the terminal closed) end the session as a
	// detach does: the terminal is given back, the Runs keep running.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	reason, err := m.Run(ctx)
	term.Restore(int(in.Fd()), old)
	if err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	switch reason {
	case "detached":
		fmt.Fprintf(env.Stderr, "[detached (from session %s)]\n", sess.Name)
	case "exited":
		fmt.Fprintf(env.Stderr, "[exited (session %s)]\n", sess.Name)
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
	byS, err := sessionRuns(ctx, c)
	if err != nil {
		return err
	}
	files, err := state.List(env.StateDir)
	if err != nil {
		return err
	}
	names := map[string]bool{}
	for _, f := range files {
		names[f] = true
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
		note := ""
		if s, ok, _ := state.Load(env.StateDir, n); ok {
			windows = len(s.Windows)
			spares = len(s.Spares)
		} else {
			note = "no local file: attach rebuilds it from lux"
			windows -= spares
		}
		when := "-"
		if !created.IsZero() {
			when = created.Local().Format("2006-01-02 15:04")
		}
		fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%s\t%s\n", n, windows, running, spares, when, note)
	}
	return tw.Flush()
}

func killSession(env Env, name string, yes bool) error {
	c, err := client(env)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	runs, err := c.ListSession(ctx, name)
	if err != nil {
		return err
	}
	var live []*api.Run
	for _, r := range runs {
		if state.Live(r.State) {
			live = append(live, r)
		}
	}
	_, hasFile, _ := state.Load(env.StateDir, name)
	if len(live) == 0 && !hasFile {
		return fmt.Errorf("no session %s", name)
	}
	if !yes && len(live) > 0 {
		fmt.Fprintf(env.Stderr, "terminate %d Run(s) of session %s (their shells and anything not saved go)? [y/N] ", len(live), name)
		line, _ := bufio.NewReader(env.Stdin).ReadString('\n')
		if a := strings.TrimSpace(strings.ToLower(line)); a != "y" && a != "yes" {
			return errors.New("not killed")
		}
	}
	var errs []error
	for _, r := range live {
		if err := c.Terminate(ctx, r.ID); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", r.ID, err))
		} else {
			fmt.Fprintln(env.Stdout, "terminated", r.ID)
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return state.Remove(env.StateDir, name)
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
