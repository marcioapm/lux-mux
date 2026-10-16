package mux

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode"

	"github.com/marcioapm/lux-mux/internal/config"
)

// nameWriteFor bounds one write of a window's name into its Run.
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
