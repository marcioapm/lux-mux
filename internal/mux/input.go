package mux

import (
	"io"
	"strconv"
	"sync"

	"github.com/marcioapm/lux-mux/internal/config"
	"github.com/marcioapm/lux-mux/internal/screen"
)

// input reads the keyboard in one goroutine and hands chunks to whoever
// owns the keyboard: the input loop, or a dialog while it is open.
type input struct {
	r     io.Reader
	spawn func(func())
	once  sync.Once
	ch    chan []byte
	// back holds a chunk a closed dialog read but did not use.
	mu   sync.Mutex
	back [][]byte
}

func (in *input) start() {
	in.once.Do(func() {
		in.ch = make(chan []byte, 64)
		in.spawn(func() {
			defer close(in.ch)
			for {
				buf := make([]byte, 4096)
				n, err := in.r.Read(buf)
				if n > 0 {
					in.ch <- buf[:n]
				}
				if err != nil {
					return
				}
			}
		})
	})
}

// next is the next chunk; ok is false when the keyboard is gone.
func (in *input) next(done <-chan struct{}) ([]byte, bool) {
	in.mu.Lock()
	if len(in.back) > 0 {
		b := in.back[0]
		in.back = in.back[1:]
		in.mu.Unlock()
		return b, true
	}
	in.mu.Unlock()
	select {
	case b, ok := <-in.ch:
		return b, ok
	case <-done:
		return nil, false
	}
}

func (in *input) unread(b []byte) {
	in.mu.Lock()
	in.back = append(in.back, b)
	in.mu.Unlock()
}

// unreadFirst puts b back to be read before anything else.
func (in *input) unreadFirst(b []byte) {
	if len(b) == 0 {
		return
	}
	in.mu.Lock()
	in.back = append([][]byte{b}, in.back...)
	in.mu.Unlock()
}

// Reader is the keyboard for a dialog, until Close; what it read and
// did not hand out by then goes back to the input loop.
type Reader struct {
	in     *input
	done   chan struct{}
	once   sync.Once
	cur    []byte
	closed bool
	mu     sync.Mutex
}

// KeyboardReader gives the keyboard to a dialog until the Reader is closed.
func (m *Mux) KeyboardReader() *Reader {
	return &Reader{in: m.in, done: make(chan struct{})}
}

func (r *Reader) Read(p []byte) (int, error) {
	r.mu.Lock()
	if len(r.cur) > 0 {
		n := copy(p, r.cur)
		r.cur = r.cur[n:]
		r.mu.Unlock()
		return n, nil
	}
	r.mu.Unlock()
	b, ok := r.in.next(r.done)
	if !ok {
		return 0, io.EOF
	}
	select {
	case <-r.done:
		// Closed while this read waited: the chunk is the input loop's.
		r.in.unread(b)
		return 0, io.EOF
	default:
	}
	n := copy(p, b)
	r.mu.Lock()
	r.cur = b[n:]
	r.mu.Unlock()
	return n, nil
}

// Close hands the keyboard back.
func (r *Reader) Close() error {
	r.once.Do(func() {
		close(r.done)
		r.mu.Lock()
		if len(r.cur) > 0 {
			r.in.unread(r.cur)
			r.cur = nil
		}
		r.mu.Unlock()
	})
	return nil
}

// inputLoop takes keystrokes until the session ends.
func (m *Mux) inputLoop() {
	m.in.start()
	for {
		b, ok := m.in.next(m.ctx.Done())
		if !ok {
			if m.ctx.Err() == nil {
				// The keyboard is gone (the terminal closed): detach.
				m.finish("detached")
			}
			return
		}
		m.handleInput(b)
	}
}

// handleInput routes one chunk of keyboard input.
func (m *Mux) handleInput(b []byte) {
	for len(b) > 0 {
		m.mu.Lock()
		if b = m.keys.SkipReleases(b); len(b) == 0 {
			m.mu.Unlock()
			return
		}
		if m.help {
			// Any key closes the help; the key itself is used up.
			m.help = false
			m.repaintLocked()
			m.mu.Unlock()
			return
		}
		if m.promptState != nil {
			b = m.promptKeysLocked(b)
			m.mu.Unlock()
			continue
		}
		w := m.activeLocked()
		if w != nil && w.phase == phaseDead && !m.keys.AfterPrefix() {
			// An ended window takes r and & without the prefix.
			switch b[0] {
			case 'r':
				m.mu.Unlock()
				m.relaunchOrResume(w)
				b = b[1:]
				continue
			case '&':
				m.mu.Unlock()
				m.command(config.Command{Op: config.OpKill})
				b = b[1:]
				continue
			}
		}
		acts, rest := m.keys.Feed(b)
		// An ended window's Run takes no keys: they would reach a resumed
		// shell later, out of context.
		dead := w != nil && w.phase == phaseDead
		m.mu.Unlock()
		for _, a := range acts {
			switch {
			case a.IsCmd() && (a.Cmd.Op == config.OpChoose || a.Cmd.Op == config.OpNewDialog):
				// What was typed after the key is the dialog's: Feed
				// stops at a command, so this is the last action.
				m.in.unreadFirst(rest)
				m.command(a.Cmd)
				return
			case a.IsCmd():
				m.command(a.Cmd)
			case w != nil && !dead:
				m.send(w, a.Send)
			}
		}
		b = rest
	}
}

// relaunchOrResume is r on an ended window: a stopped Run is resumed, any
// other is replaced by a fresh one.
func (m *Mux) relaunchOrResume(w *Window) {
	m.mu.Lock()
	resumable := w.resumable
	m.mu.Unlock()
	if resumable {
		m.resume(w)
		return
	}
	m.relaunch(w)
}

// command runs a key's command.
func (m *Mux) command(c config.Command) {
	switch c.Op {
	case config.OpDetach:
		m.finish("detached")
	case config.OpNew:
		cfg := m.config()
		m.newWindowFrom(cfg.DefaultProfile, cfg.Profiles[cfg.DefaultProfile], "", false)
	case config.OpNewDialog:
		m.openNewDialog()
	case config.OpChoose:
		m.openChooser()
	case config.OpRelaunch:
		m.mu.Lock()
		w := m.activeLocked()
		m.mu.Unlock()
		if w != nil {
			m.relaunchOrResume(w)
		}
	default:
		m.mu.Lock()
		m.commandLocked(c)
		m.mu.Unlock()
	}
}

func (m *Mux) commandLocked(c config.Command) {
	n := len(m.windows)
	if n == 0 {
		return
	}
	switch c.Op {
	case config.OpSelect:
		if c.Arg < n {
			m.selectLocked(c.Arg)
		} else {
			m.setNoticeLocked("no window " + strconv.Itoa(c.Arg))
		}
	case config.OpNext:
		m.selectLocked((m.active + 1) % n)
	case config.OpPrev:
		m.selectLocked((m.active + n - 1) % n)
	case config.OpLast:
		m.selectLocked(m.last)
	case config.OpPrompt:
		m.askLocked("index: ", "", false, func(s string, ok bool) {
			i, err := strconv.Atoi(s)
			switch {
			case !ok || s == "":
			case err != nil || i < 0 || i >= len(m.windows):
				m.setNoticeLocked("no window " + s)
			default:
				m.selectLocked(i)
			}
		})
	case config.OpRename:
		w := m.activeLocked()
		m.askLocked("rename window: ", w.Name, false, func(s string, ok bool) {
			if ok && s != "" && m.indexLocked(w) >= 0 {
				w.Name = s
				m.saveLocked()
				m.repaintLocked()
			}
		})
	case config.OpKill:
		w := m.activeLocked()
		m.askLocked("kill window "+strconv.Itoa(m.active)+" ("+w.Name+")? (y/N) ", "", true, func(_ string, yes bool) {
			if yes {
				// Terminating waits on the network: off the lock.
				m.goSafe(func() { m.closeWindow(w, true, "") })
			}
		})
	case config.OpHelp:
		// While help is shown no output reaches the terminal (the
		// emulators still take it); closing it repaints the window.
		m.help = true
		b := append([]byte{0x18}, screen.Text(nil, m.helpLines(), m.rows, m.cols)...)
		m.tw.enqueue(b)
	}
}
