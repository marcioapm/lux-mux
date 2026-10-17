package fakelux

import (
	"context"
	"sync"
	"syscall"
	"time"
)

// procWait bounds how long Close waits for the processes the fake started
// to be gone.
var procWait = 5 * time.Second

// group is a process group the fake started, as a container holds every
// process of a Run: a Run's shell (its pty session) or one exec command.
// Its pgid is its leader's pid.
type group struct {
	pgid int
	mu   sync.Mutex
	gone bool
}

// alive reports whether any process (a zombie included) is left in g.
// Once g was seen empty it is never signalled again: the kernel keeps a
// pgid from reuse only while some process is in the group.
func (g *group) alive() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.aliveLocked()
}

func (g *group) aliveLocked() bool {
	if !g.gone && syscall.Kill(-g.pgid, 0) != nil {
		g.gone = true
	}
	return !g.gone
}

// kill sends SIGKILL to every process in g, as a container stop does.
func (g *group) kill() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.aliveLocked() {
		_ = syscall.Kill(-g.pgid, syscall.SIGKILL)
	}
}

// beginProcs reserves the start of a Run shell or an exec command; false
// once Close began. endProcs releases it.
func (s *Server) beginProcs() bool {
	s.procMu.Lock()
	defer s.procMu.Unlock()
	if s.procsClosed {
		return false
	}
	s.inflight.Add(1)
	return true
}

func (s *Server) endProcs() { s.inflight.Done() }

// track records g for Close; a group started while Close runs is killed
// at once.
func (s *Server) track(g *group) {
	if g == nil {
		return
	}
	s.procMu.Lock()
	s.groups = append(s.groups, g)
	closed := s.procsClosed
	s.procMu.Unlock()
	if closed {
		g.kill()
	}
}

// stopProcs kills every group the fake started and returns once all are
// gone or procWait passed. Starts already under way finish first, so the
// groups they make are seen too.
func (s *Server) stopProcs() {
	ctx, cancel := context.WithTimeout(context.Background(), procWait)
	defer cancel()
	s.procMu.Lock()
	s.procsClosed = true
	s.procMu.Unlock()
	s.killGroups()
	started := make(chan struct{})
	go func() {
		s.inflight.Wait()
		close(started)
	}()
	select {
	case <-started:
	case <-ctx.Done():
	}
	gs := s.killGroups()
	WaitFor(ctx, procWait, func() bool {
		for _, g := range gs {
			if g.alive() {
				return false
			}
		}
		return true
	})
}

func (s *Server) killGroups() []*group {
	s.procMu.Lock()
	gs := append([]*group{}, s.groups...)
	s.procMu.Unlock()
	for _, g := range gs {
		g.kill()
	}
	return gs
}
