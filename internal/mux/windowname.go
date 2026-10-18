package mux

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/marcioapm/lux-mux/internal/config"
	"github.com/marcioapm/lux-mux/internal/luxapi"
)

// nameWriteFor bounds one write of a window's name into its Run, and one
// rename of its Run on lux.
const nameWriteFor = 5 * time.Second

// sendNameLocked writes w's name into its Run's .luxmux files (lux's exec
// API) unless that name is there already or a write is in flight; the
// write that ends checks again, so the last rename wins. It runs only
// once the window is live: a spare's Run has no window name until a
// window takes it, and exec needs the workload started. A write that
// fails is retried at the next attach.
func (m *Mux) sendNameLocked(w *Window) {
	if w.closed || w.RunID == "" || w.phase != phaseLive || w.nameSending || w.nameSent == w.Name {
		return
	}
	id, name := w.RunID, w.Name
	w.nameSending = true
	m.goSafe(func() {
		err := m.execName(w.Endpoint, id, name)
		m.mu.Lock()
		defer m.mu.Unlock()
		w.nameSending = false
		if err == nil && w.RunID == id {
			w.nameSent = name
		}
		if err == nil && m.ctx.Err() == nil {
			m.sendNameLocked(w)
		}
	})
}

func (m *Mux) execName(ep config.Endpoint, id, name string) error {
	lux, err := m.eps.Client(ep)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(m.ctx, nameWriteFor)
	defer cancel()
	code, err := lux.Exec(ctx, id, config.WindowNameCommand(envName(name)))
	if err == nil && code != 0 {
		return errExecFailed
	}
	return err
}

// envName is name as LUXMUX_WINDOW carries it: each control character
// (a newline, ESC, DEL) replaced by a space, so the value is one printable
// line whoever sources or prints it.
func envName(name string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, name)
}

var errExecFailed = errors.New("writing the window name failed")

// runNameLocked is the name w's Run is given on lux: the session's name
// now and the window's.
func (m *Mux) runNameLocked(w *Window) string { return config.RunName(m.name, w.Name) }

// renameRunLocked marks w's Run as due a rename to runNameLocked and sends
// it. byUser: the user renamed the window or the session, so a lux that
// cannot rename is told of once (sendRunNameLocked).
func (m *Mux) renameRunLocked(w *Window, byUser bool) {
	w.runNamePending = true
	w.runNameByUser = w.runNameByUser || byUser
	m.sendRunNameLocked(w)
}

// sendRunNameLocked renames w's Run on lux, off the lock, while a rename
// is due and none is in flight; the rename that ends checks again, so the
// last rename wins. One that fails stays due, in the session file too, and
// is sent at the next attach (beginAttach). A lux without the rename API
// ends it: luxapi.Client remembers that per endpoint and asks no more, and
// the first rename the user made there gets one notice in this process;
// renames luxmux makes by itself (a claimed spare) get none.
func (m *Mux) sendRunNameLocked(w *Window) {
	if w.closed || w.RunID == "" || w.runNameSending || !w.runNamePending {
		return
	}
	id, ep, name, byUser := w.RunID, w.Endpoint, m.runNameLocked(w), w.runNameByUser
	w.runNameSending, w.runNameByUser = true, false
	m.goSafe(func() {
		err := m.renameRun(ep, id, name)
		unsupported := errors.Is(err, luxapi.ErrRenameUnsupported)
		m.mu.Lock()
		defer m.mu.Unlock()
		w.runNameSending = false
		live := m.ctx.Err() == nil && !m.ending
		if w.RunID != id {
			// Relaunched meanwhile: the new Run's rename may be due.
			if live {
				m.sendRunNameLocked(w)
			}
			return
		}
		switch {
		case unsupported:
			w.runNamePending = false
			if byUser && live {
				if lux, cerr := m.eps.Client(ep); cerr == nil && lux.FirstRenameRefusal() {
					m.setNoticeLocked(fmt.Sprintf("%s cannot rename Runs: they keep the names they were made with", ep.Label()))
				}
			}
		case err != nil:
			w.runNameByUser = w.runNameByUser || byUser
			return
		case name == m.runNameLocked(w):
			w.runNamePending = false
		}
		if live {
			m.saveLocked()
			m.sendRunNameLocked(w)
		}
	})
}

func (m *Mux) renameRun(ep config.Endpoint, id, name string) error {
	lux, err := m.eps.Client(ep)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(m.ctx, nameWriteFor)
	defer cancel()
	_, err = lux.Rename(ctx, id, name)
	return err
}
