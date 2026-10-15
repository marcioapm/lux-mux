package fakelux

import (
	"io"
	"os"
	"os/exec"
	"sync"

	"github.com/creack/pty"
	"github.com/marcioapm/lux/vterm"
)

// Terminal is a Run's terminal as lux-shim keeps it: a process's output
// fed to a vterm emulator and copied to every attached stream, under one
// lock so a restore snapshot and the live output after it never overlap.
type Terminal struct {
	mu      sync.Mutex
	in      io.Writer
	resize  func(rows, cols int)
	screen  *vterm.Terminal
	subs    map[*Sub]bool
	gone    chan struct{}
	once    sync.Once
	closer  func()
	resizes [][2]int
	code    int
}

func newTerminal(rows, cols int) *Terminal {
	return &Terminal{screen: vterm.New(rows, cols, 2000), subs: map[*Sub]bool{}, gone: make(chan struct{})}
}

// As lux-shim: a snapshot is at most SnapshotLimit bytes (history lines
// dropped first; a screen bigger than that alone gets no snapshot); a
// plain attach misses output past PlainQueue reads queued; a restore
// attach with more than SnapshotLimit bytes queued is ended (Behind).
const (
	SnapshotLimit = 128 * 32 << 10
	PlainQueue    = 256
)

// Sub is one attach's copy of the terminal's output.
type Sub struct {
	C      chan []byte
	strict bool
	queued int
	// Behind is closed when a restore attach fell behind and was ended.
	Behind chan struct{}
}

// taken records that the stream sent b.
func (s *Sub) taken(t *Terminal, b []byte) {
	t.mu.Lock()
	s.queued -= len(b)
	t.mu.Unlock()
}

// Join subscribes to the output; with restore it resizes to rows×cols
// first (when given) and returns the snapshot the output continues from
// (nil when the screen alone is over SnapshotLimit).
func (t *Terminal) Join(restore bool, rows, cols int) ([]byte, *Sub) {
	t.mu.Lock()
	defer t.mu.Unlock()
	var snap []byte
	if restore {
		if rows > 0 && cols > 0 {
			t.resizeLocked(min(rows, vterm.MaxRows), min(cols, vterm.MaxCols))
		}
		snap = t.screen.Snapshot(SnapshotLimit)
		if len(snap) > SnapshotLimit {
			snap = nil
		}
	}
	sub := &Sub{strict: snap != nil, Behind: make(chan struct{})}
	if sub.strict {
		sub.C = make(chan []byte, 1<<16)
	} else {
		sub.C = make(chan []byte, PlainQueue)
	}
	t.subs[sub] = true
	return snap, sub
}

// Leave unsubscribes sub.
func (t *Terminal) Leave(sub *Sub) {
	t.mu.Lock()
	if t.subs[sub] {
		delete(t.subs, sub)
		close(sub.C)
	}
	t.mu.Unlock()
}

// Output is what the process wrote: fed to the emulator and every stream.
func (t *Terminal) Output(p []byte) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.screen.Write(p)
	b := append([]byte{}, p...)
	for sub := range t.subs {
		if sub.strict && sub.queued+len(b) > SnapshotLimit {
			delete(t.subs, sub)
			close(sub.Behind)
			continue
		}
		select {
		case sub.C <- b:
			if sub.strict {
				sub.queued += len(b)
			}
		default:
			// A plain attach that cannot keep up misses output.
		}
	}
}

// Queued is the most output any restore attach has queued, not yet sent.
func (t *Terminal) Queued() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := 0
	for sub := range t.subs {
		n = max(n, sub.queued)
	}
	return n
}

// Input writes to the process.
func (t *Terminal) Input(p []byte) {
	if t.in != nil {
		_, _ = t.in.Write(p)
	}
}

// Resize resizes the process's terminal and the emulator.
func (t *Terminal) Resize(rows, cols int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.resizeLocked(rows, cols)
}

func (t *Terminal) resizeLocked(rows, cols int) {
	t.resizes = append(t.resizes, [2]int{rows, cols})
	t.screen.Resize(rows, cols)
	if t.resize != nil {
		t.resize(rows, cols)
	}
}

// Resizes are the sizes the terminal was given, in order.
func (t *Terminal) Resizes() [][2]int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([][2]int{}, t.resizes...)
}

// Gone is closed when the process has exited.
func (t *Terminal) Gone() <-chan struct{} { return t.gone }

// ExitWith ends the terminal as if its process exited with code.
func (t *Terminal) ExitWith(code int) {
	t.mu.Lock()
	t.code = code
	t.mu.Unlock()
	t.Exit()
}

// ExitCode is the code the process exited with.
func (t *Terminal) ExitCode() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.code
}

// Exit ends the terminal as if its process exited (with code 0, unless
// ExitWith set another).
func (t *Terminal) Exit() {
	t.once.Do(func() {
		close(t.gone)
		if t.closer != nil {
			t.closer()
		}
	})
}

// Screen runs fn with the emulator, under the terminal's lock.
func (t *Terminal) Screen(fn func(*vterm.Terminal)) {
	t.mu.Lock()
	defer t.mu.Unlock()
	fn(t.screen)
}

// EchoTerminal is a terminal whose "process" echoes its input, as a
// terminal in cooked mode would, until Exit. Tests write more output with
// Output.
func EchoTerminal(rows, cols int) *Terminal {
	t := newTerminal(rows, cols)
	t.in = writerFunc(func(p []byte) (int, error) {
		select {
		case <-t.gone:
			return 0, io.ErrClosedPipe
		default:
		}
		t.Output(p)
		return len(p), nil
	})
	return t
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// ShellTerminal runs argv on a pseudo-terminal.
func ShellTerminal(rows, cols int, argv []string, env []string) (*Terminal, error) {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = append(os.Environ(), env...)
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: uint16(rows), Cols: uint16(cols)})
	if err != nil {
		return nil, err
	}
	t := newTerminal(rows, cols)
	t.in = f
	t.resize = func(r, c int) { _ = pty.Setsize(f, &pty.Winsize{Rows: uint16(r), Cols: uint16(c)}) }
	t.closer = func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = f.Close()
	}
	go func() {
		buf := make([]byte, 32<<10)
		for {
			n, err := f.Read(buf)
			if n > 0 {
				t.Output(buf[:n])
			}
			if err != nil {
				break
			}
		}
		_ = cmd.Wait()
		if cmd.ProcessState != nil {
			t.ExitWith(max(cmd.ProcessState.ExitCode(), 0))
		} else {
			t.Exit()
		}
	}()
	return t, nil
}
