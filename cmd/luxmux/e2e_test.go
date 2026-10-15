package main

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/marcioapm/lux/api"
	"github.com/marcioapm/lux/vterm"
	"golang.org/x/sys/unix"

	"github.com/marcioapm/lux-mux/internal/config"
	"github.com/marcioapm/lux-mux/internal/fakelux"
	"github.com/marcioapm/lux-mux/internal/luxapi"
)

// The test binary is luxmux itself when LUXMUX_E2E_MAIN is set: the
// end-to-end test runs it in a pty as a user would.
func TestMain(m *testing.M) {
	if os.Getenv("LUXMUX_E2E_MAIN") == "1" {
		os.Args = append([]string{"luxmux"}, strings.Fields(os.Getenv("LUXMUX_E2E_ARGS"))...)
		if name := os.Getenv("LUXMUX_E2E_PANIC_ON"); name != "" {
			// A fault inside luxmux: looking this secret up panics.
			lookup = func(k string) (string, bool) {
				if k == name {
					if mark := os.Getenv("LUXMUX_E2E_PANIC_MARK"); mark != "" {
						_ = os.WriteFile(mark, nil, 0o600)
					}
					panic("injected: lookup " + k)
				}
				return os.LookupEnv(k)
			}
			// The exit after the panic report is held back, so anything
			// still written to the terminal after its Reset shows.
			stderr = lingerOnPanic{os.Stderr}
		}
		main()
		return
	}
	os.Exit(m.Run())
}

// lingerOnPanic waits a second after writing a panic report.
type lingerOnPanic struct{ w io.Writer }

func (l lingerOnPanic) Write(p []byte) (int, error) {
	n, err := l.w.Write(p)
	if strings.Contains(string(p), "luxmux: panic:") {
		time.Sleep(time.Second)
	}
	return n, err
}

// screenTerm is the person's terminal: luxmux's output read from the pty
// into an emulator.
type screenTerm struct {
	mu sync.Mutex
	vt *vterm.Terminal
}

func (s *screenTerm) text() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, cols := s.vt.Size()
	var lines []string
	for y := 0; y < rows; y++ {
		var b strings.Builder
		for x := 0; x < cols; x++ {
			b.WriteString(s.vt.Text(s.vt.Cell(x, y)))
		}
		lines = append(lines, strings.TrimRight(b.String(), " "))
	}
	return strings.Join(lines, "\n")
}

func (s *screenTerm) title() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.vt.Title()
}

type proc struct {
	t    *testing.T
	cmd  *exec.Cmd
	f    *os.File
	scr  *screenTerm
	done chan struct{}
	// raw is everything luxmux wrote.
	rawMu sync.Mutex
	raw   []byte
	// hold, while locked, stops the pty being read: a terminal that takes
	// nothing.
	hold sync.Mutex
}

// queries are what luxmux's UI library asks the terminal at start, and a
// terminal's answers.
var queries = []struct{ q, answer string }{
	{"\x1b]11;?", "\x1b]11;rgb:0000/0000/0000\x07"},
	{"\x1b[6n", "\x1b[1;1R"},
}

// answerer answers the queries, wherever the reads split them.
type answerer struct{ tail []byte }

// feed takes one read's bytes and returns the answers to the queries
// that end in it.
func (a *answerer) feed(b []byte) []string {
	buf := append(a.tail, b...)
	var out []string
	for _, q := range queries {
		// Only queries that end in this read; earlier ones were answered.
		for i := strings.Index(string(buf), q.q); i >= 0; {
			if i+len(q.q) > len(a.tail) {
				out = append(out, q.answer)
			}
			j := strings.Index(string(buf[i+1:]), q.q)
			if j < 0 {
				break
			}
			i += 1 + j
		}
	}
	keep := 16
	if len(buf) < keep {
		keep = len(buf)
	}
	a.tail = append([]byte{}, buf[len(buf)-keep:]...)
	return out
}

func startLuxmux(t *testing.T, env []string, args string) *proc {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(env, "LUXMUX_E2E_MAIN=1", "LUXMUX_E2E_ARGS="+args)
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 24, Cols: 80})
	if err != nil {
		t.Fatal(err)
	}
	p := &proc{t: t, cmd: cmd, f: f, scr: &screenTerm{vt: vterm.New(24, 80, 0)}, done: make(chan struct{})}
	go func() {
		defer close(p.done)
		var ans answerer
		buf := make([]byte, 32<<10)
		for {
			p.hold.Lock()
			p.hold.Unlock()
			n, err := f.Read(buf)
			if n > 0 {
				for _, a := range ans.feed(buf[:n]) {
					_, _ = f.Write([]byte(a))
				}
				p.rawMu.Lock()
				p.raw = append(p.raw, buf[:n]...)
				p.rawMu.Unlock()
				p.scr.mu.Lock()
				p.scr.vt.Write(buf[:n])
				p.scr.mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = f.Close()
	})
	return p
}

func (p *proc) rawLen() int {
	p.rawMu.Lock()
	defer p.rawMu.Unlock()
	return len(p.raw)
}

func (p *proc) rawSince(n int) string {
	p.rawMu.Lock()
	defer p.rawMu.Unlock()
	return string(p.raw[n:])
}

// TestAnswererSplitQuery: a query split over two reads is answered once.
func TestAnswererSplitQuery(t *testing.T) {
	var a answerer
	if got := a.feed([]byte("x\x1b]1")); len(got) != 0 {
		t.Fatalf("answered early: %q", got)
	}
	if got := a.feed([]byte("1;?\x1b[")); len(got) != 1 || got[0] != queries[0].answer {
		t.Fatalf("split OSC 11: %q", got)
	}
	if got := a.feed([]byte("6n")); len(got) != 1 || got[0] != queries[1].answer {
		t.Fatalf("split CSI 6n: %q", got)
	}
	if got := a.feed([]byte("more")); len(got) != 0 {
		t.Fatalf("answered twice: %q", got)
	}
	if got := a.feed([]byte("\x1b]11;?\x1b[6n")); len(got) != 2 {
		t.Fatalf("whole: %q", got)
	}
}

func (p *proc) typ(s string) {
	p.t.Helper()
	if _, err := p.f.Write([]byte(s)); err != nil {
		p.t.Fatal(err)
	}
}

func (p *proc) wait(what string, cond func(screen string) bool) {
	p.t.Helper()
	if !fakelux.WaitFor(context.Background(), 15*time.Second, func() bool { return cond(p.scr.text()) }) {
		p.t.Fatalf("waiting for %s; screen:\n%s", what, p.scr.text())
	}
}

func has(s string) func(string) bool {
	return func(scr string) bool { return strings.Contains(scr, s) }
}

// exited waits for luxmux to exit and checks its status.
func (p *proc) exited() {
	p.t.Helper()
	if code := p.exitCode(); code != 0 {
		p.t.Fatalf("luxmux exited with %d; screen:\n%s", code, p.scr.text())
	}
}

// exitCode waits for luxmux to exit and returns its status.
func (p *proc) exitCode() int {
	p.t.Helper()
	errc := make(chan error, 1)
	go func() { errc <- p.cmd.Wait() }()
	select {
	case <-errc:
		<-p.done
		return p.cmd.ProcessState.ExitCode()
	case <-time.After(15 * time.Second):
		p.t.Fatalf("luxmux did not exit; screen:\n%s", p.scr.text())
	}
	return -1
}

// waitModes waits until mouse tracking (1000) and bracketed paste (2004)
// are both on in the terminal.
func (p *proc) waitModes(what string) {
	p.t.Helper()
	ok := fakelux.WaitFor(context.Background(), 15*time.Second, func() bool {
		p.scr.mu.Lock()
		defer p.scr.mu.Unlock()
		return p.scr.vt.Mode(1000) && p.scr.vt.Mode(2004)
	})
	if !ok {
		p.t.Fatalf("waiting for %s; screen:\n%s", what, p.scr.text())
	}
}

// restored checks the terminal is as luxmux found it: main screen, the
// modes a program set off, the cursor shown, and the pty out of raw mode.
func (p *proc) restored() {
	p.t.Helper()
	p.scr.mu.Lock()
	alt, mouse, paste, cursor := p.scr.vt.AltScreen(), p.scr.vt.Mode(1000), p.scr.vt.Mode(2004), p.scr.vt.Mode(25)
	p.scr.mu.Unlock()
	if alt || mouse || paste || !cursor {
		p.t.Fatalf("terminal not restored: alt %v mouse %v paste %v cursor %v", alt, mouse, paste, cursor)
	}
	tio, err := unix.IoctlGetTermios(int(p.f.Fd()), unix.TCGETS)
	if err != nil {
		p.t.Fatal(err)
	}
	if tio.Lflag&unix.ICANON == 0 || tio.Lflag&unix.ECHO == 0 {
		p.t.Fatalf("pty left in raw mode: lflag %#x", tio.Lflag)
	}
}

// TestEndToEnd drives luxmux in a pty against a fake luxd whose Runs'
// terminals are local shells.
func TestEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	lux := fakelux.New()
	defer lux.Close()
	lux.NewTerminal = func(*api.Run) *fakelux.Terminal {
		term, err := fakelux.ShellTerminal(24, 80, []string{"sh"}, []string{"PS1=$ ", "ENV=", "TERM=xterm"})
		if err != nil {
			t.Error(err)
			return fakelux.EchoTerminal(24, 80)
		}
		return term
	}
	dir := t.TempDir()
	env := []string{
		"PATH=" + os.Getenv("PATH"), "TERM=xterm-256color", "HOME=" + dir,
		"XDG_CONFIG_HOME=" + filepath.Join(dir, "config"), "XDG_STATE_HOME=" + filepath.Join(dir, "state"),
		"LUX_URL=" + lux.URL, "LUX_API_KEY=lux_test",
	}

	p := startLuxmux(t, env, "new -s e2e")
	p.wait("first prompt", has("$"))
	p.typ("echo hi-1\r")
	p.wait("hi-1", has("\nhi-1"))
	if p.scr.title() != "luxmux:e2e [0] default" {
		t.Fatalf("title %q", p.scr.title())
	}

	// A second window, then back to the first: its screen is restored.
	p.typ("\x02c")
	p.wait("window 1", func(string) bool { return p.scr.title() == "luxmux:e2e [1] default" })
	p.wait("second prompt", func(s string) bool { return strings.Contains(s, "$") && !strings.Contains(s, "hi-1") })
	p.typ("echo hi-2\r")
	p.wait("hi-2", has("\nhi-2"))
	p.typ("\x020")
	p.wait("window 0 restored", func(s string) bool { return strings.Contains(s, "hi-1") && !strings.Contains(s, "hi-2") })

	// The chooser: from window 0, down to window 1, enter.
	p.typ("\x02w")
	p.wait("chooser", has("enter switch"))
	p.typ("\x1b[B")
	p.wait("preview of window 1", has("hi-2"))
	p.typ("\r")
	p.wait("window 1 via chooser", func(s string) bool {
		return p.scr.title() == "luxmux:e2e [1] default" && strings.Contains(s, "hi-2") && !strings.Contains(s, "enter switch")
	})

	// Detach: the Runs keep running.
	p.typ("\x02d")
	p.exited()
	ids := lux.RunIDs()
	if len(ids) != 2 {
		t.Fatalf("Runs %v", ids)
	}
	for _, id := range ids {
		if st := lux.Run(id).State; st != "running" {
			t.Fatalf("%s is %s after detach", id, st)
		}
	}

	// A new process on another machine (an empty state directory)
	// rebuilds the session from the Runs' labels and restores its screen.
	other := filepath.Join(dir, "state-elsewhere")
	envElsewhere := append(slices.Clone(env), "XDG_STATE_HOME="+other)
	q := startLuxmux(t, envElsewhere, "attach -t e2e")
	q.wait("rebuilt from labels", func(s string) bool { return strings.Contains(s, "hi-1") || strings.Contains(s, "hi-2") })
	q.wait("two windows", func(string) bool { return strings.HasPrefix(q.scr.title(), "luxmux:e2e [0]") })
	q.typ("\x021")
	q.wait("window 1 rebuilt", func(s string) bool { return q.scr.title() == "luxmux:e2e [1] default" && strings.Contains(s, "hi-2") })
	// A second process on the same session is refused while it is
	// attached.
	r := startLuxmux(t, envElsewhere, "attach -t e2e")
	if code := r.exitCode(); code == 0 || !strings.Contains(r.scr.text(), "attached elsewhere") {
		t.Fatalf("second attach: %d\n%s", code, r.scr.text())
	}
	// SIGTERM gives the terminal back and leaves the Runs running.
	q.typ("printf '\\033[?1000h\\033[?2004h'\r")
	q.waitModes("the program's modes on the terminal")
	if err := q.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	q.exited()
	q.restored()
	for _, id := range ids {
		if st := lux.Run(id).State; st != "running" {
			t.Fatalf("%s is %s after SIGTERM", id, st)
		}
	}

	// A new process attaches and restores the screen of the active window.
	p = startLuxmux(t, env, "attach -t e2e")
	p.wait("restored after reattach", has("hi-2"))
	p.typ("echo hi-3\r")
	p.wait("hi-3", has("\nhi-3"))

	// exit closes the window and terminates its Run.
	p.typ("exit\r")
	p.wait("window 1 gone", func(s string) bool { return p.scr.title() == "luxmux:e2e [0] default" && strings.Contains(s, "hi-1") })
	if !fakelux.WaitFor(context.Background(), 10*time.Second, func() bool { return lux.Run(ids[1]).State == "terminated" }) {
		t.Fatalf("Run %s not terminated: %s", ids[1], lux.Run(ids[1]).State)
	}
	if lux.Run(ids[0]).State != "running" {
		t.Fatal("the other window's Run must keep running")
	}
	p.typ("exit\r")
	p.exited()
	if st := lux.Run(ids[0]).State; st != "terminated" {
		t.Fatalf("last Run %s", st)
	}
}

// e2eEnv is a fake luxd whose Runs' terminals are local shells, and the
// environment luxmux runs in against it.
func e2eEnv(t *testing.T) (*fakelux.Server, []string) {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	lux := fakelux.New()
	t.Cleanup(lux.Close)
	lux.NewTerminal = func(*api.Run) *fakelux.Terminal {
		term, err := fakelux.ShellTerminal(24, 80, []string{"sh"}, []string{"PS1=$ ", "ENV=", "TERM=xterm"})
		if err != nil {
			t.Error(err)
			return fakelux.EchoTerminal(24, 80)
		}
		return term
	}
	dir := t.TempDir()
	return lux, []string{
		"PATH=" + os.Getenv("PATH"), "TERM=xterm-256color", "HOME=" + dir,
		"XDG_CONFIG_HOME=" + filepath.Join(dir, "config"), "XDG_STATE_HOME=" + filepath.Join(dir, "state"),
		"LUX_URL=" + lux.URL, "LUX_API_KEY=lux_test",
	}
}

// TestNewWindowDialog: C-b C opens the form; a repository, a branch and
// a size from the list are what the new window's Run is submitted with.
// Esc closes it with nothing created.
func TestNewWindowDialog(t *testing.T) {
	lux, env := e2eEnv(t)
	yes := true
	lux.SetPools([]api.Pool{{Name: "m", IsDefault: &yes, Sizes: []api.PoolSize{
		{Name: "m-1", CPUs: 1, Memory: 2 << 30, Disk: 10 << 30},
		{Name: "m-4", CPUs: 4, Memory: 8 << 30, Disk: 40 << 30},
	}}})
	p := startLuxmux(t, env, "new -s dlg")
	p.wait("first prompt", has("$"))
	p.typ("echo first-window\r")
	p.wait("first window's output", has("\nfirst-window"))

	// The Tabs, in the same write as C-b C, are the dialog's.
	p.typ("\x02C\t\t\t\t")
	p.wait("dialog with the pools loaded, over the window's screen", func(s string) bool {
		return strings.Contains(s, "New window") && strings.Contains(s, "(pool default)") && strings.Contains(s, "first-window")
	})
	p.wait("on Repository", has("▸ Repository"))
	p.typ("https://example.com/team/app.git")
	p.wait("repository typed", has("https://example.com/team/app.git"))
	p.typ("\t")
	p.wait("on Branch", has("▸ Branch"))
	p.typ("feature-x")
	p.wait("branch typed", has("feature-x"))
	p.typ("\x1b[Z\x1b[Z")
	p.wait("on Size", has("▸ Size"))
	p.typ("\r")
	p.wait("size list", has("filter:"))
	p.typ("m-4")
	p.wait("filtered", func(s string) bool { return strings.Contains(s, "filter: m-4") && !strings.Contains(s, "m-1  1 cpus") })
	p.typ("\r")
	p.wait("size picked", func(s string) bool {
		return strings.Contains(s, "m-4  4 cpus, 8Gi, 40Gi") && !strings.Contains(s, "filter:")
	})
	p.typ("\x13")
	if !fakelux.WaitFor(context.Background(), 15*time.Second, func() bool { return len(lux.Submits()) == 2 }) {
		t.Fatalf("no submit; screen:\n%s", p.scr.text())
	}
	sp := lux.Submits()[1]
	if sp.Git == nil || len(sp.Git.Repositories) != 1 || sp.Git.Repositories[0].URL != "https://example.com/team/app.git" ||
		sp.Git.Repositories[0].Ref != "feature-x" || sp.Resources.Size != "m-4" {
		t.Fatalf("submitted %+v", sp)
	}
	p.wait("window 1", func(string) bool { return strings.HasPrefix(p.scr.title(), "luxmux:dlg [1]") })
	p.wait("dialog closed", func(s string) bool { return !strings.Contains(s, "New window") })

	// Esc: nothing created, the window's shell is back and takes keys.
	p.typ("\x02C")
	p.wait("dialog again", func(s string) bool {
		return strings.Contains(s, "New window") && strings.Contains(s, "(pool default)")
	})
	p.typ("\x1b")
	p.wait("dialog closed by esc", func(s string) bool { return !strings.Contains(s, "New window") })
	p.typ("echo after-esc\r")
	p.wait("shell after esc", has("\nafter-esc"))
	if n := len(lux.Submits()); n != 2 {
		t.Fatalf("%d submits after esc", n)
	}
	if !strings.HasPrefix(p.scr.title(), "luxmux:dlg [1]") {
		t.Fatalf("title %q", p.scr.title())
	}
	p.typ("\x02d")
	p.exited()
}

// TestKeysAfterHelpCloseReachShell: keys in the same write as the key
// that closes help reach the shell.
func TestKeysAfterHelpCloseReachShell(t *testing.T) {
	_, env := e2eEnv(t)
	p := startLuxmux(t, env, "new -s handback")
	p.wait("prompt", has("$"))
	p.typ("\x02?")
	p.wait("help", has("window chooser"))
	p.typ("?\recho after-close\r")
	p.wait("command after closing key", func(s string) bool { return strings.Count(s, "after-close") >= 2 })
	p.typ("\x02d")
	p.exited()
}

// TestDialogsFollowResize: help and the new-window dialog re-center when
// the terminal grows, and say it is too small when it shrinks below them.
func TestDialogsFollowResize(t *testing.T) {
	for _, dialog := range []struct{ key, title string }{{"?", "Keys"}, {"C", "New window"}} {
		t.Run(dialog.title, func(t *testing.T) {
			_, env := e2eEnv(t)
			p := startLuxmux(t, env, "new -s resize")
			p.wait("prompt", has("$"))
			p.typ("\x02" + dialog.key)
			p.wait("dialog", has(dialog.title))
			resize := func(rows, cols uint16) {
				p.scr.mu.Lock()
				p.scr.vt.Resize(int(rows), int(cols))
				p.scr.mu.Unlock()
				if err := pty.Setsize(p.f, &pty.Winsize{Rows: rows, Cols: cols}); err != nil {
					t.Fatal(err)
				}
			}
			resize(40, 160)
			p.wait("recentered box", func(s string) bool {
				for _, line := range strings.Split(s, "\n") {
					if left := strings.IndexRune(line, '╭'); left >= 0 {
						runes := []rune(line)
						right := slices.Index(runes, '╮')
						return strings.Contains(s, dialog.title) && right >= 0 && left == 159-right && left > 20
					}
				}
				return false
			})
			resize(10, 30)
			p.wait("too small", has("too small"))
			p.typ("\x1b")
			p.wait("dialog closed", func(s string) bool { return !strings.Contains(s, "too small") })
			p.typ("\x02d")
			p.exited()
		})
	}
}

// TestHelpDialog: C-b ? shows the key help over the window; Esc closes it
// and the shell's screen is back, with what it printed meanwhile.
func TestHelpDialog(t *testing.T) {
	lux, env := e2eEnv(t)
	p := startLuxmux(t, env, "new -s hlp")
	p.wait("first prompt", has("$"))
	p.typ("echo before-help\r")
	p.wait("before-help", has("\nbefore-help"))
	p.typ("\x02?")
	p.wait("help", func(s string) bool {
		return strings.Contains(s, "Keys") && strings.Contains(s, "C-b w") && strings.Contains(s, "window chooser") &&
			strings.Contains(s, "before-help")
	})
	// Output while the help is open reaches the window and shows after.
	lux.Term(lux.RunIDs()[0]).Output([]byte("\r\nwhile-help\r\n"))
	p.typ("\x1b")
	p.wait("shell back", func(s string) bool {
		return !strings.Contains(s, "window chooser") && strings.Contains(s, "before-help") && strings.Contains(s, "while-help")
	})
	p.typ("echo after-help\r")
	p.wait("after-help", has("\nafter-help"))
	p.typ("\x02d")
	p.exited()
}

// TestPanicGivesTerminalBack: a panic inside luxmux (here in a window's
// supervisor, resolving a secret to resume its Run) restores the terminal
// before printing the panic, and luxmux exits non-zero.
func TestPanicGivesTerminalBack(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	lux := fakelux.New()
	defer lux.Close()
	dir := t.TempDir()
	// The injected fault creates mark just before it panics.
	mark := filepath.Join(dir, "panicked")
	cfgDir := filepath.Join(dir, "config", "luxmux")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "config.toml"), []byte("[profiles.default]\nimage = \"alpine\"\nsecrets = [\"TOKEN\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	env := []string{
		"PATH=" + os.Getenv("PATH"), "TERM=xterm-256color", "HOME=" + dir,
		"XDG_CONFIG_HOME=" + filepath.Join(dir, "config"), "XDG_STATE_HOME=" + filepath.Join(dir, "state"),
		"LUX_URL=" + lux.URL, "LUX_API_KEY=lux_test", "TOKEN=v", "LUXMUX_E2E_PANIC_ON=TOKEN",
		"LUXMUX_E2E_PANIC_MARK=" + mark,
	}
	// The submit resolves TOKEN too: the first window's Run is made
	// before luxmux starts.
	c, _ := luxapi.New(lux.Config())
	sp, err := config.RunSpec(config.Profile{Image: "alpine", Secrets: []string{"TOKEN"}}, config.Target{Session: "p", Profile: "default"}, map[string]string{"TOKEN": "v"})
	if err != nil {
		t.Fatal(err)
	}
	run, err := c.Submit(context.Background(), sp)
	if err != nil {
		t.Fatal(err)
	}
	fakelux.WaitFor(context.Background(), 5*time.Second, func() bool { return lux.Term(run.ID) != nil })
	p := startLuxmux(t, env, "attach -t p")
	p.wait("attached", func(string) bool { return lux.Streams(run.ID) > 0 })
	lux.Term(run.ID).Output([]byte("\x1b[?1049h\x1b[?1000h\x1b[?2004hFULL"))
	p.wait("program on screen", has("FULL"))
	// The terminal stops reading. Output past what the pty buffers blocks
	// luxmux's writer, so the program's modes set again, and the repaint
	// "resuming" queues (which sets them too), are still queued at the
	// panic; none of it may reach the terminal after its Reset.
	p.hold.Lock()
	held := true
	defer func() {
		if held {
			p.hold.Unlock()
		}
	}()
	term := lux.Term(run.ID)
	line := []byte(strings.Repeat(".", 79) + "\r\n")
	for range 256 << 10 / len(line) {
		term.Output(line)
	}
	term.Output([]byte("\x1b[?1049h\x1b[?1000h\x1b[?2004hFULL-AGAIN"))
	lux.LoseSecrets(run.ID)
	if !fakelux.WaitFor(context.Background(), 15*time.Second, func() bool { _, err := os.Stat(mark); return err == nil }) {
		t.Fatal("no panic")
	}
	p.hold.Unlock()
	held = false
	if code := p.exitCode(); code == 0 {
		t.Fatal("luxmux exited 0 after a panic")
	}
	p.restored()
	out := p.rawSince(0)
	if !strings.Contains(out, "panic: injected: lookup TOKEN") {
		t.Fatalf("panic not printed:\n%s", p.scr.text())
	}
	if i := strings.LastIndex(out, "\x1b[?1049l"); i < 0 || strings.Contains(out[i:], "\x1b[?1049h") {
		t.Fatalf("the alternate screen set again after the Reset: %q", out[i:])
	}
}
