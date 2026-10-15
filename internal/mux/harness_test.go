package mux

import (
	"bytes"
	"context"
	"io"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/marcioapm/lux/api"
	"github.com/marcioapm/lux/vterm"

	"github.com/marcioapm/lux-mux/internal/config"
	"github.com/marcioapm/lux-mux/internal/fakelux"
	"github.com/marcioapm/lux-mux/internal/luxapi"
	"github.com/marcioapm/lux-mux/internal/screen"
	"github.com/marcioapm/lux-mux/internal/state"
)

// clientTerm is the user's terminal in tests: what luxmux writes to it is
// kept as bytes and fed to an emulator, so tests read the screen a person
// would see.
type clientTerm struct {
	mu  sync.Mutex
	raw bytes.Buffer
	vt  *vterm.Terminal
	// keys follows the keyboard modes the terminal was set to, which vt
	// does not keep.
	keys screen.KeyTracker
	// stall, while set, blocks every Write until it is closed: a terminal
	// (or link) that takes nothing.
	stallMu sync.Mutex
	stall   chan struct{}
}

// setStall stalls the terminal (on) or lets it go again.
func (c *clientTerm) setStall(on bool) {
	c.stallMu.Lock()
	defer c.stallMu.Unlock()
	switch {
	case on && c.stall == nil:
		c.stall = make(chan struct{})
	case !on && c.stall != nil:
		close(c.stall)
		c.stall = nil
	}
}

func newClientTerm(rows, cols int) *clientTerm {
	return &clientTerm{vt: vterm.New(rows, cols, 0)}
}

func (c *clientTerm) Write(p []byte) (int, error) {
	c.stallMu.Lock()
	st := c.stall
	c.stallMu.Unlock()
	if st != nil {
		<-st
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.raw.Write(p)
	c.vt.Write(p)
	c.keys.Write(p)
	return len(p), nil
}

func (c *clientTerm) keyModes() screen.KeyModes {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.keys.Modes()
}

// screen is the visible screen, rows joined by newlines, trailing blanks cut.
func (c *clientTerm) screen() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	rows, cols := c.vt.Size()
	var lines []string
	for y := 0; y < rows; y++ {
		var b strings.Builder
		for x := 0; x < cols; x++ {
			b.WriteString(c.vt.Text(c.vt.Cell(x, y)))
		}
		lines = append(lines, strings.TrimRight(b.String(), " "))
	}
	return strings.Join(lines, "\n")
}

func (c *clientTerm) mode(n int) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.vt.Mode(n)
}

func (c *clientTerm) keypadShape() (bool, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return screen.KeypadAndShape(c.vt)
}

func (c *clientTerm) altScreen() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.vt.AltScreen()
}

func (c *clientTerm) title() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.vt.Title()
}

func (c *clientTerm) rawBytes() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]byte{}, c.raw.Bytes()...)
}

func (c *clientTerm) cursor() (int, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	x, y, _ := c.vt.Cursor()
	return x, y
}

type harness struct {
	t     *testing.T
	lux   *fakelux.Server
	term  *clientTerm
	keys  *io.PipeWriter
	m     *Mux
	done  chan string
	dir   string
	env   map[string]string
	envMu sync.Mutex
	// panicOn makes Lookup panic for that name; crashed gets what
	// Options.Crash was given.
	panicOn string
	crashed chan string
	cfg     *config.Config
	rows    int
	cols    int
}

func newHarness(t *testing.T, cfgSrc string, setup func(*fakelux.Server)) *harness {
	return newHarnessEnv(t, cfgSrc, nil, setup)
}

// newHarnessEnv is newHarness with variables Lookup finds from the start.
func newHarnessEnv(t *testing.T, cfgSrc string, env map[string]string, setup func(*fakelux.Server)) *harness {
	t.Helper()
	h := &harness{t: t, lux: fakelux.New(), rows: 24, cols: 80, dir: t.TempDir(), env: map[string]string{}, crashed: make(chan string, 1)}
	for k, v := range env {
		h.env[k] = v
	}
	t.Cleanup(h.lux.Close)
	if setup != nil {
		setup(h.lux)
	}
	cfg, err := config.Parse([]byte(cfgSrc))
	if err != nil {
		t.Fatal(err)
	}
	h.cfg = cfg
	h.start(nil)
	return h
}

// start runs a Mux on the session file in h.dir (a new process attaching).
func (h *harness) start(sess *state.Session) {
	h.t.Helper()
	if sess == nil {
		s, ok, err := state.Load(h.dir, "s")
		if err != nil {
			h.t.Fatal(err)
		}
		if !ok {
			s = &state.Session{Name: "s"}
		}
		sess = s
	}
	cl, _ := luxapi.New(h.lux.Config())
	pr, pw := io.Pipe()
	h.keys = pw
	h.term = newClientTerm(h.rows, h.cols)
	m, err := New(Options{
		Session: "s", Config: h.cfg, Lux: cl, StateDir: h.dir,
		In: pr, Out: h.term, Size: func() (int, int) { return h.rows, h.cols },
		Lookup: func(k string) (string, bool) {
			h.envMu.Lock()
			defer h.envMu.Unlock()
			if k == h.panicOn {
				panic("injected: lookup " + k)
			}
			v, ok := h.env[k]
			return v, ok
		},
		Crash: func(msg string) {
			h.crashed <- msg
			// As os.Exit: the goroutine that crashed never returns.
			runtime.Goexit()
		},
		Backoff: 5 * time.Millisecond, MaxBackoff: 50 * time.Millisecond,
		NoticeFor: 50 * time.Millisecond, RestoreWait: 300 * time.Millisecond, Poll: 50 * time.Millisecond,
	}, sess)
	if err != nil {
		h.t.Fatal(err)
	}
	h.m = m
	h.done = make(chan string, 1)
	go func() {
		r, _ := m.Run(context.Background())
		h.done <- r
	}()
}

// detach detaches and waits for Run to return.
func (h *harness) detach() {
	h.t.Helper()
	h.type_("\x02d")
	select {
	case r := <-h.done:
		if r != "detached" {
			h.t.Fatalf("Run returned %q", r)
		}
	case <-time.After(5 * time.Second):
		h.t.Fatal("detach did not return")
	}
}

func (h *harness) type_(s string) {
	h.t.Helper()
	if _, err := h.keys.Write([]byte(s)); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) waitScreen(what string, cond func(string) bool) {
	h.t.Helper()
	if !fakelux.WaitFor(context.Background(), 5*time.Second, func() bool { return cond(h.term.screen()) }) {
		h.t.Fatalf("waiting for %s; screen:\n%s", what, h.term.screen())
	}
}

func (h *harness) waitFor(what string, cond func() bool) {
	h.t.Helper()
	if !fakelux.WaitFor(context.Background(), 5*time.Second, cond) {
		h.t.Fatalf("waiting for %s; screen:\n%s", what, h.term.screen())
	}
}

func (h *harness) contains(s string) func(string) bool {
	return func(scr string) bool { return strings.Contains(scr, s) }
}

// window is the Run id of window i.
func (h *harness) window(i int) string {
	h.m.mu.Lock()
	defer h.m.mu.Unlock()
	if i >= len(h.m.windows) {
		return ""
	}
	return h.m.windows[i].RunID
}

func (h *harness) nwindows() int {
	h.m.mu.Lock()
	defer h.m.mu.Unlock()
	return len(h.m.windows)
}

// live waits until window i is attached.
func (h *harness) live(i int) string {
	h.t.Helper()
	h.waitFor("window live", func() bool {
		h.m.mu.Lock()
		defer h.m.mu.Unlock()
		return i < len(h.m.windows) && h.m.windows[i].phase == phaseLive && h.m.windows[i].getConn() != nil
	})
	id := h.window(i)
	h.waitFor("stream open", func() bool { return h.lux.Streams(id) > 0 })
	return id
}

func (h *harness) phase(i int) phase {
	h.m.mu.Lock()
	defer h.m.mu.Unlock()
	return h.m.windows[i].phase
}

func (h *harness) reason(i int) string {
	h.m.mu.Lock()
	defer h.m.mu.Unlock()
	return h.m.windows[i].reason
}

var _ = api.Run{}

func timeout() <-chan time.Time { return time.After(5 * time.Second) }

// setEnv sets (or with ok false, unsets) a variable Lookup finds.
func (h *harness) setEnv(k, v string, ok bool) {
	h.envMu.Lock()
	defer h.envMu.Unlock()
	if ok {
		h.env[k] = v
	} else {
		delete(h.env, k)
	}
}
