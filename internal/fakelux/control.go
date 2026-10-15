package fakelux

import (
	"context"
	"slices"
	"time"

	"github.com/marcioapm/lux/api"
)

// Blip drops the Run's attach streams, as a network blip or a laptop
// sleeping does; the Run goes on running with the same terminal.
func (s *Server) Blip(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for c := range s.runs[id].conns {
		c.CloseNow()
	}
}

// Move stops the Run's placement and resumes it elsewhere, as a drain or
// a migration does with resumePolicy auto: a new placement, a new
// terminal (a fresh shell). With hold, it stays waiting until Start.
func (s *Server) Move(id string, hold bool) {
	s.mu.Lock()
	r := s.runs[id]
	s.setState(r, "stopping", "")
	r.run.StageReason = "drain"
	s.setStage(r, "stopping")
	for c := range r.conns {
		c.CloseNow()
	}
	t := r.term
	r.term = nil
	s.setState(r, "resuming", "auto-resume after drain")
	s.setStage(r, "waiting")
	s.mu.Unlock()
	if t != nil {
		t.Exit()
	}
	if !hold {
		s.Start(id)
	}
}

// LoseSecrets stops the Run as luxd does when it no longer holds its
// secret values: stopped, resumable only with them.
func (s *Server) LoseSecrets(id string) {
	s.mu.Lock()
	r := s.runs[id]
	for c := range r.conns {
		c.CloseNow()
	}
	t := r.term
	r.term = nil
	s.setState(r, "stopped", SecretsLostReason)
	s.setStage(r, "stopped")
	s.mu.Unlock()
	if t != nil {
		t.Exit()
	}
}

// End ends the Run in state (terminated or failed) with reason.
func (s *Server) End(id, state, reason string) {
	s.mu.Lock()
	r := s.runs[id]
	for c := range r.conns {
		c.CloseNow()
	}
	t := r.term
	r.term = nil
	s.setState(r, state, reason)
	s.setStage(r, state)
	s.mu.Unlock()
	if t != nil {
		t.Exit()
	}
}

// Term is the Run's current terminal (nil when it has none).
func (s *Server) Term(id string) *Terminal {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r := s.runs[id]; r != nil {
		return r.term
	}
	return nil
}

// Run is a copy of the Run.
func (s *Server) Run(id string) api.Run {
	s.mu.Lock()
	defer s.mu.Unlock()
	return *s.runs[id].run
}

// Secrets are the values the Run was last given (submit or resume).
func (s *Server) Secrets(id string) map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.runs[id].secrets
}

// RunIDs are every Run's id, oldest first.
func (s *Server) RunIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.order)
}

// Attaches are the attach requests received.
func (s *Server) Attaches() []AttachReq {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.attachs)
}

// Resumes are the resume requests received.
func (s *Server) Resumes() []ResumeReq {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.resumes)
}

// Submits are the specs submitted (validateOnly excluded).
func (s *Server) Submits() []api.RunSpec {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.submits)
}

// Terminated are the Runs a terminate was asked for, in order.
func (s *Server) Terminated() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.terms)
}

// Streams is how many attach streams of the Run are open.
func (s *Server) Streams(id string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.runs[id].conns)
}

// WaitFor polls cond until it holds or the deadline passes; it reports
// whether it held. Tests state what they wait for instead of sleeping.
func WaitFor(ctx context.Context, d time.Duration, cond func() bool) bool {
	ctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	tk := time.NewTicker(2 * time.Millisecond)
	defer tk.Stop()
	for {
		if cond() {
			return true
		}
		select {
		case <-ctx.Done():
			return cond()
		case <-tk.C:
		}
	}
}
