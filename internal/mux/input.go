package mux

import (
	"bytes"
	"io"
	"strconv"
	"sync"
	"unicode/utf8"

	"github.com/marcioapm/lux-mux/internal/config"
)

var (
	pasteStart = []byte("\x1b[200~")
	pasteEnd   = []byte("\x1b[201~")
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
	in        *input
	done      chan struct{}
	once      sync.Once
	cur       []byte
	closed    bool
	mu        sync.Mutex
	ack       chan struct{}
	waiting   bool
	oneKey    bool
	remaining int
}

// KeyboardReader gives the keyboard to a dialog until the Reader is closed.
func (m *Mux) KeyboardReader() *Reader {
	return &Reader{in: m.in, done: make(chan struct{}), ack: make(chan struct{}, 1)}
}

// OneKeyAtATime makes each Read return at most one key, and the next Read
// wait until the dialog calls KeyHandled for it: the dialog then never
// reads past the key that closes it, and Close hands the rest back.
func (r *Reader) OneKeyAtATime() { r.oneKey = true }

// KeyHandled reports that the dialog took the key the last Read returned.
func (r *Reader) KeyHandled() {
	select {
	case r.ack <- struct{}{}:
	default:
	}
}

// keyLen is the length of the first key in b, as bubbletea parses keys: a
// bracketed paste whole (to its end marker), an X10 mouse report (6
// bytes), a CSI or SS3 sequence to its final byte, Alt with the rune
// after ESC, else one UTF-8 rune. complete is false when b ends inside
// the key: bubbletea then reads on without delivering anything.
func keyLen(b []byte) (n int, complete bool) {
	if b[0] != 0x1b || len(b) == 1 {
		_, n := utf8.DecodeRune(b)
		return n, utf8.FullRune(b)
	}
	if bytes.HasPrefix(b, pasteStart) {
		if i := bytes.Index(b, pasteEnd); i >= 0 {
			return i + len(pasteEnd), true
		}
		return len(b), false
	}
	switch b[1] {
	case '[':
		if len(b) >= 3 && b[2] == 'M' {
			return min(6, len(b)), len(b) >= 6
		}
		for i := 2; i < len(b); i++ {
			if b[i] >= 0x40 && b[i] <= 0x7e {
				return i + 1, true
			}
		}
		return len(b), false
	case 'O':
		return min(3, len(b)), len(b) >= 3
	}
	_, n = utf8.DecodeRune(b[1:])
	return 1 + n, utf8.FullRune(b[1:])
}

func (r *Reader) Read(p []byte) (int, error) {
	r.mu.Lock()
	wait := r.oneKey && r.waiting
	r.mu.Unlock()
	if wait {
		select {
		case <-r.ack:
		case <-r.done:
			return 0, io.EOF
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return 0, io.EOF
	}
	if len(r.cur) == 0 {
		r.mu.Unlock()
		b, ok := r.in.next(r.done)
		r.mu.Lock()
		if !ok {
			return 0, io.EOF
		}
		if r.closed {
			// Closed while this read waited: the chunk is the input loop's.
			r.in.unreadFirst(b)
			return 0, io.EOF
		}
		r.cur = b
	}
	if len(p) == 0 {
		return 0, nil
	}
	if r.oneKey && r.remaining == 0 {
		// Bubbletea emits nothing for a partial event, so it is collected
		// whole before the first byte is handed out.
		for owned := false; ; owned = true {
			k, complete := keyLen(r.cur)
			if complete {
				r.remaining = k
				break
			}
			// Close must not hand back a prefix while its tail is in flight.
			pending := r.cur
			if !owned {
				// Appending must not write into a chunk's spare capacity.
				pending = bytes.Clone(pending)
			}
			r.cur = nil
			r.mu.Unlock()
			b, ok := r.in.next(r.done)
			r.mu.Lock()
			pending = append(pending, b...)
			// done closes before Close takes mu: either means closing.
			closing := r.closed
			select {
			case <-r.done:
				closing = true
			default:
			}
			if closing {
				r.in.unreadFirst(pending)
				return 0, io.EOF
			}
			r.cur = pending
			if !ok {
				// The keyboard is gone: what was read is all there is.
				r.remaining = len(r.cur)
				break
			}
		}
	}
	b := r.cur
	if r.oneKey {
		b = b[:r.remaining]
	}
	n := copy(p, b)
	r.cur = r.cur[n:]
	if r.oneKey {
		r.remaining -= n
	}
	r.waiting = r.oneKey && r.remaining == 0
	if r.waiting {
		// An ack left from a key bubbletea read in parts is not this one's.
		select {
		case <-r.ack:
		default:
		}
	}
	return n, nil
}

// Close hands the keyboard back.
func (r *Reader) Close() error {
	r.once.Do(func() {
		close(r.done)
		r.mu.Lock()
		r.closed = true
		if len(r.cur) > 0 {
			r.in.unreadFirst(r.cur)
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
		if m.ctx.Err() != nil {
			// Switched to another session (or detached) by a key before
			// these: they are the next session's.
			m.in.unreadFirst(b)
			return
		}
		m.mu.Lock()
		if b = m.keys.SkipReleases(b); len(b) == 0 {
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
			case a.IsCmd() && opensDialog(a.Cmd.Op):
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

// opensDialog reports whether op's command takes the keyboard, so what
// was typed after its key is handed to it.
func opensDialog(op string) bool {
	switch op {
	case config.OpChoose, config.OpNewDialog, config.OpHelp, config.OpChooseSession:
		return true
	}
	return false
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
		name := m.DefaultProfile()
		m.newWindowFrom(name, cfg.Profiles[name], "", false)
	case config.OpNewDialog:
		m.openNewDialog()
	case config.OpChoose:
		m.openChooser()
	case config.OpHelp:
		m.openHelp()
	case config.OpChooseSession:
		m.openSessionChooser()
	case config.OpNextSession:
		m.stepSession(1)
	case config.OpPrevSession:
		m.stepSession(-1)
	case config.OpRenameSession:
		m.mu.Lock()
		m.askRenameSessionLocked()
		m.mu.Unlock()
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
				m.sendNameLocked(w)
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
	}
}
