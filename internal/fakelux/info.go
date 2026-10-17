package fakelux

import (
	"context"
	"net/http"
	"slices"
	"strconv"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/marcioapm/lux/api"

	"github.com/marcioapm/lux-mux/internal/luxapi"
)

// ExecAnswer is what ExecResponder makes an exec stream answer: Output on
// stdout then exit code Code, or Error as the stream's error frame. Hang
// answers nothing until the client goes away.
type ExecAnswer struct {
	Output []byte
	Code   int
	Error  string
	Hang   bool
}

// SetExecResponder answers each exec of a running Run with f's answer,
// instead of running the command (ExecEnv); f returning ok false leaves
// that exec to ExecEnv. nil removes it.
func (s *Server) SetExecResponder(f func(runID string, command []string) (ExecAnswer, bool)) {
	s.mu.Lock()
	s.execResponder = f
	s.mu.Unlock()
}

// Update changes Run id as luxd would report it (its host, epoch, stage
// times, runtime, sizing, resolved image, spec…): what GET and the
// listings answer from now on. The Run's state machine is not run.
func (s *Server) Update(id string, f func(r *api.Run)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r := s.runs[id]; r != nil {
		f(r.run)
	}
}

// SetResume makes GET of Run id report rs as its resumability (nil: the
// fake's own, from its state and secrets).
func (s *Server) SetResume(id string, rs *api.Resumability) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.resumability == nil {
		s.resumability = map[string]*api.Resumability{}
	}
	if rs == nil {
		delete(s.resumability, id)
	} else {
		s.resumability[id] = rs
	}
}

// SetCost makes GET /v1/runs/{id}/cost answer c; without one it answers
// as luxd does before the first cost tick: pending, no totals.
func (s *Server) SetCost(id string, c *luxapi.RunCost) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.costs == nil {
		s.costs = map[string]*luxapi.RunCost{}
	}
	if c == nil {
		delete(s.costs, id)
	} else {
		s.costs[id] = c
	}
}

func (s *Server) cost(w http.ResponseWriter, req *http.Request) {
	id := req.PathValue("id")
	s.mu.Lock()
	r, c := s.runs[id], s.costs[id]
	s.mu.Unlock()
	if r == nil {
		writeErr(w, 404, "not_found", "no such run")
		return
	}
	if c == nil {
		c = &luxapi.RunCost{Status: "pending", Totals: []api.CostTotal{}}
	}
	writeJSON(w, 200, map[string]any{"runId": id, "status": c.Status, "final": c.Status == "final", "basis": "list",
		"totals": c.Totals, "byFamily": []api.CostTotal{}, "lines": []any{}, "sources": []any{}})
}

// ResourcesOutput is what luxmux's resources script prints for a cgroup
// with those values ("" for a file it cannot read, "max" for no limit)
// and df -Pk's size and used KiB (negative: no df line).
func ResourcesOutput(cpuMax, memMax, memCurrent, usageUsec string, dfKiB, dfUsedKiB int64) []byte {
	b := []byte("cpu.max " + cpuMax + "\nmemory.max " + memMax + "\nmemory.current " + memCurrent + "\ncpu.usage_usec " + usageUsec + "\n")
	if dfKiB >= 0 {
		b = append(b, "df "+strconv.FormatInt(dfKiB, 10)+" "+strconv.FormatInt(dfUsedKiB, 10)+"\n"...)
	}
	return b
}

// respondExec answers an accepted exec stream with a: its output, its
// exit code or its error, or nothing until the client goes away.
func respondExec(ctx context.Context, ws *websocket.Conn, a ExecAnswer) {
	if a.Hang {
		<-ctx.Done()
		return
	}
	if a.Error != "" {
		_ = wsjson.Write(ctx, ws, api.StreamData{Error: a.Error})
		_ = ws.Close(websocket.StatusNormalClosure, "")
		return
	}
	if len(a.Output) > 0 {
		_ = wsjson.Write(ctx, ws, api.StreamData{Channel: "stdout", Data: a.Output})
	}
	code := a.Code
	_ = wsjson.Write(ctx, ws, api.StreamData{ExitCode: &code})
	_ = ws.Close(websocket.StatusNormalClosure, "")
}

// execResponse is the responder's answer for an exec of command in Run
// id, if it takes it.
func (s *Server) execResponse(id string, command []string) (ExecAnswer, bool) {
	s.mu.Lock()
	f := s.execResponder
	s.mu.Unlock()
	if f == nil {
		return ExecAnswer{}, false
	}
	return f(id, slices.Clone(command))
}
