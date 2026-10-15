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
	mu     sync.Mutex
	in     io.Writer
	resize func(rows, cols int)
	screen *vterm.Terminal
	subs   map[chan []byte]bool
	gone   chan struct{}
	once   sync.Once
	closer func()
}

func newTerminal(rows, cols int) *Terminal {
	return &Terminal{screen: vterm.New(rows, cols, 2000), subs: map[chan []byte]bool{}, gone: make(chan struct{})}
}

// Join subscribes to the output; with restore it resizes to rows×cols
// first (when given) and returns the snapshot the output continues from.
func (t *Terminal) Join(restore bool, rows, cols int) ([]byte, chan []byte) {
	ch := make(chan []byte, 4096)
	t.mu.Lock()
	defer t.mu.Unlock()
	var snap []byte
	if restore {
		if rows > 0 && cols > 0 {
			t.resizeLocked(rows, cols)
		}
		snap = t.screen.Snapshot(128 * 32 << 10)
	}
	t.subs[ch] = true
	return snap, ch
}

// Leave unsubscribes ch.
func (t *Terminal) Leave(ch chan []byte) {
	t.mu.Lock()
	if t.subs[ch] {
		delete(t.subs, ch)
		close(ch)
	}
	t.mu.Unlock()
}

// Output is what the process wrote: fed to the emulator and every stream.
func (t *Terminal) Output(p []byte) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.screen.Write(p)
	b := append([]byte{}, p...)
	for ch := range t.subs {
		// As lux-shim: a stream that cannot keep up misses output.
		select {
		case ch <- b:
		default:
		}
	}
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
	t.screen.Resize(rows, cols)
	if t.resize != nil {
		t.resize(rows, cols)
	}
}

// Gone is closed when the process has exited.
func (t *Terminal) Gone() <-chan struct{} { return t.gone }

// Exit ends the terminal as if its process exited.
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
		t.Exit()
	}()
	return t, nil
}
