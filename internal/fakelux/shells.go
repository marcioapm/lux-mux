package fakelux

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/marcioapm/lux/api"
)

// The variables a Run's real shell gets from the fake: its workspace (the
// state volume, kept across placements) and its cgroup directory. They are
// the start wrapper's test-only overrides.
const (
	workspaceVar = "_LUXMUX_TEST_WORKSPACE"
	cgroupVar    = "_LUXMUX_TEST_CGROUP"
)

// RunShells makes every Run's terminal its spec's workload command, run by
// a real sh in a pty with the spec's env, its secret values and LUX_RUN_ID,
// LUX_EPOCH, as the shim starts it. Each Run's state volume is a directory
// under dir that outlives its placements; exec runs there too. extra is
// more environment for every command (PS1, TERM).
func (s *Server) RunShells(dir string, extra ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	env := func(run *api.Run) []string {
		ws := filepath.Join(dir, run.ID, "workspace")
		_ = os.MkdirAll(ws, 0o755)
		out := append(slices.Clone(extra), workspaceVar+"="+ws, cgroupVar+"="+filepath.Join(dir, run.ID, "cgroup"),
			"LUX_RUN_ID="+run.ID, "LUX_EPOCH="+strconv.Itoa(run.Epoch))
		for k, v := range run.Spec.Env {
			out = append(out, k+"="+v)
		}
		s.mu.Lock()
		vals := s.runs[run.ID].secrets
		s.mu.Unlock()
		for _, sec := range run.Spec.Secrets {
			if sec.As == "env" {
				out = append(out, sec.Name+"="+vals[sec.Name])
			}
		}
		return out
	}
	s.ExecEnv = env
	s.NewTerminal = func(run *api.Run) *Terminal {
		t, err := ShellTerminal(24, 80, run.Spec.Workload.Command, env(run))
		if err != nil {
			return EchoTerminal(24, 80)
		}
		return t
	}
}

// CgroupDir is where RunShells looks for Run id's cpu.max and memory.max.
func CgroupDir(dir, id string) string { return filepath.Join(dir, id, "cgroup") }

// WorkspaceDir is Run id's state volume under RunShells' dir.
func WorkspaceDir(dir, id string) string { return filepath.Join(dir, id, "workspace") }

// Execs are the commands exec streams ran, in order.
func (s *Server) Execs() [][]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.execs)
}

// exec is GET /v1/runs/{id}/exec without a TTY: the client's StreamOpen,
// then the command's output and its exit code.
func (s *Server) exec(w http.ResponseWriter, req *http.Request) {
	id := req.PathValue("id")
	s.mu.Lock()
	r := s.runs[id]
	envOf := s.ExecEnv
	var run api.Run
	if r != nil {
		run = *r.run
	}
	s.mu.Unlock()
	switch {
	case r == nil:
		writeErr(w, 404, "not_found", "no such run")
		return
	case run.State != "running" || envOf == nil:
		writeErr(w, 409, "not_running", "run is "+run.State+": interactive access needs it running")
		return
	}
	ws, err := websocket.Accept(w, req, nil)
	if err != nil {
		return
	}
	defer ws.CloseNow()
	ctx, cancel := context.WithTimeout(req.Context(), 10*time.Second)
	defer cancel()
	var open api.StreamOpen
	if wsjson.Read(ctx, ws, &open) != nil || len(open.Command) == 0 {
		_ = wsjson.Write(ctx, ws, api.StreamData{Error: "no command"})
		return
	}
	s.mu.Lock()
	s.execs = append(s.execs, slices.Clone(open.Command))
	s.mu.Unlock()
	cmd := exec.CommandContext(ctx, open.Command[0], open.Command[1:]...)
	cmd.Env = append(os.Environ(), envOf(&run)...)
	out, err := cmd.CombinedOutput()
	if len(out) > 0 {
		_ = wsjson.Write(ctx, ws, api.StreamData{Channel: "stdout", Data: out})
	}
	code := 0
	if err != nil {
		code = 1
		if cmd.ProcessState != nil {
			code = max(cmd.ProcessState.ExitCode(), 1)
		}
	}
	_ = wsjson.Write(ctx, ws, api.StreamData{ExitCode: &code})
	_ = ws.Close(websocket.StatusNormalClosure, "")
}
