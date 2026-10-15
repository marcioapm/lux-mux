package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/marcioapm/lux/api"
	"github.com/marcioapm/lux/vterm"

	"github.com/marcioapm/lux-mux/internal/fakelux"
)

// The test binary is luxmux itself when LUXMUX_E2E_MAIN is set: the
// end-to-end test runs it in a pty as a user would.
func TestMain(m *testing.M) {
	if os.Getenv("LUXMUX_E2E_MAIN") == "1" {
		os.Args = append([]string{"luxmux"}, strings.Fields(os.Getenv("LUXMUX_E2E_ARGS"))...)
		main()
		return
	}
	os.Exit(m.Run())
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
}

func startLuxmux(t *testing.T, env []string, args string) *proc {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(env, "LUXMUX_E2E_MAIN=1", "LUXMUX_E2E_ARGS="+args)
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 24, Cols: 80})
	if err != nil {
		t.Fatal(err)
	}
	p := &proc{t: t, cmd: cmd, f: f, scr: &screenTerm{vt: vterm.New(24, 80, 1000)}, done: make(chan struct{})}
	go func() {
		defer close(p.done)
		buf := make([]byte, 32<<10)
		for {
			n, err := f.Read(buf)
			if n > 0 {
				// Answer the colour and cursor-position queries luxmux's
				// UI library sends at start, as a terminal does.
				if strings.Contains(string(buf[:n]), "\x1b]11;?") {
					_, _ = f.Write([]byte("\x1b]11;rgb:0000/0000/0000\x07"))
				}
				if strings.Contains(string(buf[:n]), "\x1b[6n") {
					_, _ = f.Write([]byte("\x1b[1;1R"))
				}
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
	errc := make(chan error, 1)
	go func() { errc <- p.cmd.Wait() }()
	select {
	case err := <-errc:
		if err != nil {
			p.t.Fatalf("luxmux exited with %v; screen:\n%s", err, p.scr.text())
		}
	case <-time.After(15 * time.Second):
		p.t.Fatalf("luxmux did not exit; screen:\n%s", p.scr.text())
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
