package mux

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/marcioapm/lux/api"

	"github.com/marcioapm/lux-mux/internal/config"
	"github.com/marcioapm/lux-mux/internal/luxapi"
	"github.com/marcioapm/lux-mux/internal/state"
)

// followEndpointLocked follows ep's event feed and lists the session's
// Runs on it, once per endpoint for the session's life: every window on
// ep shares that one stream.
func (m *Mux) followEndpointLocked(ep config.Endpoint) {
	if m.feeds[ep] || m.ctx.Err() != nil {
		return
	}
	m.feeds[ep] = true
	m.listedLocked(ep)
	m.wg.Add(2)
	m.goSafe(func() { m.followEvents(ep) })
	m.goSafe(func() { m.discoverWindows(ep) })
}

// listedLocked is the channel closed once ep's first listing was tried.
func (m *Mux) listedLocked(ep config.Endpoint) chan struct{} {
	c := m.listed[ep]
	if c == nil {
		c = make(chan struct{})
		m.listed[ep] = c
	}
	return c
}

// waitListed waits for ep's first listing (ensuring it is followed),
// false if the session ended first.
func (m *Mux) waitListed(ep config.Endpoint) bool {
	m.mu.Lock()
	m.followEndpointLocked(ep)
	c := m.listedLocked(ep)
	m.mu.Unlock()
	select {
	case <-c:
		return true
	case <-m.ctx.Done():
		return false
	}
}

// followEvents follows ep's tenant's event feed and routes each event of
// a window's Run on ep to it: progress while it starts, a wake-up for its
// supervisor whenever its state or stage changes.
func (m *Mux) followEvents(ep config.Endpoint) {
	defer m.wg.Done()
	var after int64
	bo := m.opt.Backoff
	for m.ctx.Err() == nil {
		start := time.Now()
		if lux, err := m.eps.Client(ep); err == nil {
			_ = lux.Feed(m.ctx, after, func(e api.FeedEvent) {
				after = e.ID
				if e.RunID == nil {
					return
				}
				// The feed is the tenant's and carries no labels: only a
				// Run just submitted may be new-window's (discoverWindows
				// finds those the feed missed). luxd announces a submit
				// with an event of type submitted, never a state event.
				if e.Type == luxapi.EventSubmitted {
					m.considerRun(ep, lux, *e.RunID)
				}
				m.event(ep, *e.RunID, e.Event)
			})
		}
		if m.ctx.Err() != nil {
			return
		}
		if time.Since(start) > time.Minute {
			bo = m.opt.Backoff
		}
		select {
		case <-m.ctx.Done():
		case <-time.After(bo):
		}
		bo = min(2*bo, m.opt.MaxBackoff)
	}
}

// discoverWindows lists the session's Runs on ep at the start and every
// discoverEvery: the feed starts from when it connects, and is down
// between reconnects, so a Run new-window made then is found here. The
// listing carries each Run's labels, state and spec, so a Run is adopted
// from it without reading it again. The first listing of an endpoint the
// session's rebuild could not list adds every window it has there.
func (m *Mux) discoverWindows(ep config.Endpoint) {
	defer m.wg.Done()
	// An endpoint the rebuild could not list is retried with the backoff,
	// not every discoverEvery: its windows are missing until it answers.
	bo := m.opt.Backoff
	first, adopted := true, false
	for {
		var runs []*api.Run
		lux, err := m.eps.Client(ep)
		if err == nil {
			ctx, cancel := context.WithTimeout(m.ctx, 30*time.Second)
			runs, err = lux.ListSession(ctx, m.label)
			cancel()
		}
		if err == nil {
			m.addUnlisted(ep, runs)
			if !adopted {
				adopted = true
				m.adoptSpares(ep, runs)
			}
			m.pruneConsidered(ep, runs)
			for _, r := range runs {
				if state.Adoptable(r, m.label) && m.markConsidered(ep, r.ID) {
					m.adoptRun(ep, r)
				}
			}
		}
		if first {
			first = false
			m.mu.Lock()
			close(m.listedLocked(ep))
			m.mu.Unlock()
		}
		wait := m.opt.discoverEvery
		m.mu.Lock()
		if slices.Contains(m.sess.Unlisted, ep) {
			wait = min(bo, wait)
			bo = min(2*bo, m.opt.MaxBackoff)
		}
		m.mu.Unlock()
		select {
		case <-m.ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// addUnlisted adds the windows and spares runs (ep's listing) hold when ep
// is one the session's rebuild could not list, as the rebuild would have.
func (m *Mux) addUnlisted(ep config.Endpoint, runs []*api.Run) {
	m.mu.Lock()
	unlisted := slices.Contains(m.sess.Unlisted, ep)
	m.mu.Unlock()
	if !unlisted {
		return
	}
	cfg := m.config()
	rb := state.RebuildLabelledFrom(m.Session(), m.label, []state.Listing{{Endpoint: ep, Runs: runs}}, m.eps.Default(), cfg.Profiles)
	m.mu.Lock()
	defer m.mu.Unlock()
	if !slices.Contains(m.sess.Unlisted, ep) || m.ending || m.ctx.Err() != nil {
		return
	}
	m.sess.Unlisted = slices.DeleteFunc(m.sess.Unlisted, func(e config.Endpoint) bool { return e == ep })
	if len(m.sess.Unlisted) == 0 {
		m.sess.Unlisted = nil
	}
	added := 0
	for _, sw := range rb.Windows {
		if m.byRun[sw.RunID] != nil || m.considered[sw.RunID] {
			continue
		}
		m.startSupervisor(m.addWindowLocked(sw, cfg.Profiles, ep))
		added++
	}
	m.sess.Claimed = append(m.sess.Claimed, rb.Claimed...)
	for slot, id := range rb.Spares {
		if _, ok := m.sess.Spares[slot]; !ok {
			m.sess.SetSpare(slot, id, ep)
		}
	}
	m.saveLocked()
	m.setNoticeLocked(fmt.Sprintf("lux %s answered: %d window(s) added", ep.Label(), added))
}

// adoptSpares records the waiting spares of this session ep's first
// successful listing has that the session does not: one whose submit an
// earlier detach stopped waiting for, made by lux anyway. Each takes its
// slot if that is free; any other is retired (recorded, then terminated),
// so none runs unrecorded.
// A spare is submitted on ep only after the first listing was tried
// (waitListed), so this normally runs first.
func (m *Mux) adoptSpares(ep config.Endpoint, runs []*api.Run) {
	m.mu.Lock()
	if m.ending || m.ctx.Err() != nil {
		m.mu.Unlock()
		return
	}
	known := map[string]bool{}
	for _, id := range m.sess.Spares {
		known[id] = true
	}
	for _, id := range m.sess.Claimed {
		known[id] = true
	}
	var found []*api.Run
	for _, r := range runs {
		if r.Labels[config.LabelSession] == m.label && r.Labels[config.LabelSpare] == "true" &&
			state.Live(r.State) && !state.Terminating(r) && !known[r.ID] && m.byRun[r.ID] == nil && !m.considered[r.ID] {
			found = append(found, r)
		}
	}
	// The newest is the one a slot keeps.
	slices.SortStableFunc(found, func(a, b *api.Run) int { return b.CreatedAt.Compare(a.CreatedAt) })
	var extra []string
	for _, r := range found {
		key := r.Labels[config.LabelSpec]
		if key == "" {
			key = r.Labels[config.LabelProfile]
		}
		slot := config.SpareSlot(key, ep, m.eps.Default())
		if _, taken := m.sess.Spares[slot]; taken {
			extra = append(extra, r.ID)
			m.considerLocked(ep, r.ID)
			continue
		}
		m.sess.SetSpare(slot, r.ID, ep)
	}
	if len(found) > len(extra) {
		m.saveLocked()
	}
	m.mu.Unlock()
	for _, id := range extra {
		m.retire(ep, id)
	}
}

// pruneConsidered keeps only the considered ids of ep that ep's listing
// of the session still has: a Run lux no longer lists cannot be adopted,
// and an id from the feed that is not this session's need not be
// remembered. Ids seen on other endpoints are theirs to prune.
func (m *Mux) pruneConsidered(ep config.Endpoint, runs []*api.Run) {
	listed := make(map[string]bool, len(runs))
	for _, r := range runs {
		listed[r.ID] = true
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for id := range m.considered {
		if m.consideredOn[id] == ep && !listed[id] {
			m.unconsiderLocked(id)
		}
	}
}

// considerLocked marks id, a Run on ep, as never to be adopted (again).
func (m *Mux) considerLocked(ep config.Endpoint, id string) {
	m.considered[id] = true
	m.consideredOn[id] = ep
}

func (m *Mux) unconsiderLocked(id string) {
	delete(m.considered, id)
	delete(m.consideredOn, id)
}

// markConsidered marks a Run on ep no window has as looked at; false when
// a window has it or it was looked at already.
func (m *Mux) markConsidered(ep config.Endpoint, id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.byRun[id] != nil || m.considered[id] || m.ctx.Err() != nil {
		return false
	}
	m.considerLocked(ep, id)
	return true
}

// considerRun reads a Run ep's feed says was just submitted, once, and
// adopts it when it is new-window's for this session. Labels never
// change, so a Run found to be anything else is not read again; one whose
// read fails is left to the listing.
func (m *Mux) considerRun(ep config.Endpoint, lux *luxapi.Client, id string) {
	if !m.markConsidered(ep, id) {
		return
	}
	m.wg.Add(1)
	m.goSafe(func() {
		defer m.wg.Done()
		ctx, cancel := context.WithTimeout(m.ctx, 15*time.Second)
		r, err := lux.Get(ctx, id)
		cancel()
		if err != nil {
			m.mu.Lock()
			m.unconsiderLocked(id)
			m.mu.Unlock()
			return
		}
		if state.Adoptable(r, m.label) {
			m.adoptRun(ep, r)
		}
	})
}

// adoptRun appends a window for r, a Run on ep new-window made for this
// session, with a notice; the window shown does not change. A Run the
// session records as a spare or leftover is never a window.
func (m *Mux) adoptRun(ep config.Endpoint, r *api.Run) {
	cfg := m.config()
	sw := state.WindowFromRun(r, cfg.Profiles[r.Labels[config.LabelProfile]])
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.byRun[r.ID] != nil || m.ending || m.ctx.Err() != nil {
		return
	}
	if m.sess.RecordsSpare(r.ID) {
		return
	}
	w := m.addWindowLocked(sw, cfg.Profiles, ep)
	m.startSupervisor(w)
	m.saveLocked()
	m.setNoticeLocked(fmt.Sprintf("window %d added: %s", len(m.windows)-1, w.Name))
}

// event routes an event of ep's feed to the window of its Run on ep.
func (m *Mux) event(ep config.Endpoint, runID string, e api.Event) {
	if e.Type != "state" && e.Type != "stage" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	w := m.byRun[runID]
	if w == nil || w.Endpoint != ep {
		return
	}
	str := func(k string) string { s, _ := e.Data[k].(string); return s }
	if w.phase == phaseStarting {
		if e.Type == "stage" {
			w.steps.note(stageOrState("", str("stage")))
		} else {
			w.steps.note(str("state"))
			w.status = str("state")
		}
		if m.activeLocked() == w {
			m.repaintLocked()
		}
	} else if w.phase == phaseReconnecting && e.Type == "stage" {
		w.reason = stageOrState("", str("stage"))
		if m.activeLocked() == w {
			m.repaintLocked()
		}
	}
	w.poke()
}
