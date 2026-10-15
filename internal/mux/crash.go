package mux

import (
	"fmt"
	"os"
	"runtime/debug"

	"github.com/marcioapm/lux-mux/internal/screen"
)

// goSafe runs fn on a new goroutine whose panic ends luxmux through crash.
func (m *Mux) goSafe(fn func()) {
	go func() {
		defer m.recoverPanic()
		fn()
	}()
}

// RecoverPanic, deferred at the top of a goroutine that is not luxmux's
// own (a signal handler's), sends its panic to the same crash path.
func (m *Mux) RecoverPanic() {
	if v := recover(); v != nil {
		m.crash(v, debug.Stack())
	}
}

// recoverPanic, deferred at the top of a goroutine, sends its panic to
// crash.
func (m *Mux) recoverPanic() {
	if v := recover(); v != nil {
		m.crash(v, debug.Stack())
	}
}

// crash is the single way out after a panic: the terminal is given back
// as it was found (main screen, the modes a program set off, the cursor
// shown in its default shape), written past the queue in case the
// queue's writer is what failed, then Options.Crash reports the panic and
// exits non-zero.
func (m *Mux) crash(v any, stack []byte) {
	m.crashOnce.Do(func() {
		m.tw.mu.Lock()
		m.tw.closed = true
		m.tw.mu.Unlock()
		func() {
			defer func() { _ = recover() }()
			_, _ = m.opt.Out.Write(screen.Reset([]byte{0x18}))
		}()
		m.opt.Crash(fmt.Sprintf("luxmux: panic: %v\n\n%s", v, stack))
	})
}

// defaultCrash prints the panic and exits with status 2, as an unrecovered
// panic does.
func defaultCrash(msg string) {
	fmt.Fprint(os.Stderr, msg)
	os.Exit(2)
}
