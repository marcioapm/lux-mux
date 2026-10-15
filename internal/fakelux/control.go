package fakelux

import (
	"context"
	"slices"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/marcioapm/lux/api"
)

// Error strings luxd and lux-shim end an attach stream with.
const (
	// ErrWorkloadExited: the shim, when the workload's terminal closed
	// (its shell exited, or lux stopped it).
	ErrWorkloadExited = "the workload exited"
	// ErrStreamDropped and ErrHostDisconnected: luxd, when the stream's
	// reader fell behind its hub, or the Run's host went away.
	ErrStreamDropped    = "the stream fell behind and was dropped"
	ErrHostDisconnected = "the Run's host disconnected"
	// ErrAttachFellBehind: the shim, when a restore attach fell behind.
	ErrAttachFellBehind = "attach fell behind; attach again"
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

// DropStreams ends the Run's attach streams with msg, as luxd or the shim
// does (ErrStreamDropped, ErrHostDisconnected, ErrAttachFellBehind); the
// Run goes on running.
func (s *Server) DropStreams(id, msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for c := range s.runs[id].conns {
		_ = wsjson.Write(context.Background(), c, api.StreamData{Error: msg})
		_ = c.Close(websocket.StatusNormalClosure, "")
	}
}

// FellBehind ends the Run's attach streams as the shim ends a restore
// attach whose reader fell behind; the Run goes on running.
func (s *Server) FellBehind(id string) { s.DropStreams(id, ErrAttachFellBehind) }

// stopWorkload kills the Run's workload as lux does before any stop: the
// terminal exits, so each attach stream gets ErrWorkloadExited, and
// returns once they ended; the Run's state is the caller's to change.
func (s *Server) stopWorkload(id string) {
	s.mu.Lock()
	r := s.runs[id]
	t := r.term
	r.term = nil
	s.mu.Unlock()
	if t == nil {
		return
	}
	t.Exit()
	WaitFor(context.Background(), 2*time.Second, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return len(r.conns) == 0
	})
}

// Move stops the Run's placement and resumes it elsewhere, as a drain or
// a migration does with resumePolicy auto: the workload stops (the
// streams end with ErrWorkloadExited), then a new placement with a new
// terminal (a fresh shell). With hold, it stays waiting until Start.
func (s *Server) Move(id string, hold bool) {
	s.mu.Lock()
	r := s.runs[id]
	s.setState(r, "stopping", "")
	r.run.StageReason = "drain"
	s.setStage(r, "stopping")
	s.mu.Unlock()
	s.stopWorkload(id)
	s.mu.Lock()
	s.setState(r, "resuming", "auto-resume after drain")
	s.setStage(r, "waiting")
	s.mu.Unlock()
	if !hold {
		s.Start(id)
	}
}

// LoseSecrets stops the Run as luxd does when it no longer holds its
// secret values: stopped, resumable only with them.
func (s *Server) LoseSecrets(id string) { s.End(id, "stopped", SecretsLostReason) }

// End stops the Run's workload, then ends the Run in state with reason,
// as lux does: stopped ("stop" for lux stop), lost, failed ("timeout",
// "disk limit exceeded", "exit code N") or terminated.
func (s *Server) End(id, state, reason string) {
	s.stopWorkload(id)
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.runs[id]
	s.setState(r, state, reason)
	s.setStage(r, state)
}

// SetState changes the Run's state and reason and nothing else (no
// stream, no terminal is touched).
func (s *Server) SetState(id, state, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.runs[id]
	s.setState(r, state, reason)
	s.setStage(r, state)
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
