package mux

import (
	"context"
	"time"

	"github.com/marcioapm/lux-mux/internal/info"
)

// openInfo runs the window-info dialog over the shown window.
func (m *Mux) openInfo() {
	if m.opt.UI == nil {
		m.Notice("no window info in this build")
		return
	}
	m.suspend(true)
	err := m.opt.UI.Info(m)
	m.suspend(false)
	if err != nil {
		m.Notice("window info: " + err.Error())
	}
}

// InfoTarget is the shown window as luxmux knows it, without asking lux;
// ok is false when the session has no window. It only reads under m.mu.
func (m *Mux) InfoTarget() (info.Target, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	w := m.activeLocked()
	if w == nil {
		return info.Target{}, false
	}
	return info.Target{Index: m.active, Name: w.Name, Profile: w.Profile, Session: m.name, Label: m.label,
		Endpoint: w.Endpoint, RunID: w.RunID}, true
}

// FetchInfo reads t's Run, cost and (while it runs) resources from t's
// own lux, within ctx and info's FetchTimeout and ExecTimeout. It holds
// no lock of the Mux while it waits on the network: call it from a
// goroutine of its own, never from the input loop.
func (m *Mux) FetchInfo(ctx context.Context, t info.Target) info.Window {
	return info.Gather(ctx, info.Endpoints(m.eps), m.infoSampler, t, time.Now)
}
