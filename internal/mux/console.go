package mux

import (
	"io"
	"runtime/debug"
	"sync/atomic"
	"time"
)

// Console is the user's terminal and keyboard, kept across the sessions
// one process shows: the terminal's only writer, and the one goroutine
// that reads the keyboard (a read cannot be interrupted, so it outlives
// any session).
type Console struct {
	tw  *termWriter
	in  *input
	cur atomic.Pointer[Mux]
}

// NewConsole starts the writer of out; in is read once the first session
// takes keys.
func NewConsole(in io.Reader, out io.Writer) *Console {
	c := &Console{tw: newTermWriter(out)}
	c.in = &input{r: in, spawn: c.goSafe}
	c.goSafe(c.tw.run)
	return c
}

// goSafe runs fn on a goroutine whose panic goes to the current
// session's crash path.
func (c *Console) goSafe(fn func()) {
	go func() {
		defer c.RecoverPanic()
		fn()
	}()
}

// RecoverPanic, deferred at the top of a goroutine, sends its panic to
// the current session's crash path.
func (c *Console) RecoverPanic() {
	v := recover()
	if v == nil {
		return
	}
	if m := c.cur.Load(); m != nil {
		m.crash(v, debug.Stack())
		return
	}
	panic(v)
}

// Resize takes the terminal's new size for the session shown.
func (c *Console) Resize(rows, cols int) {
	if m := c.cur.Load(); m != nil {
		m.Resize(rows, cols)
	}
}

// Current is the session shown (nil before the first).
func (c *Console) Current() *Mux { return c.cur.Load() }

// Close writes what is queued, waiting at most d for the terminal, and
// stops the writer.
func (c *Console) Close(d time.Duration) { c.tw.close(d) }
