package fakelux

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
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

// shellUser is the workload user the fake runs every command as when the
// spec names none: HOME and USER are set for it, as the shim sets them for
// the image's user.
const shellUser = "luxuser"

// RunShells makes every Run's terminal its spec's workload command, run by
// a real sh in a pty with the spec's env, its secret values and LUX_RUN_ID,
// LUX_EPOCH, HOME and USER, as the shim starts it. Each Run's state volume
// is a directory under dir that outlives its placements. The workload and
// exec run in the spec's workdir mapped under that Run's directory
// (ShellDir), as the shim runs them in workload.workdir. extra is more
// environment for every command (PS1, TERM); it cannot replace the
// variables above.
func (s *Server) RunShells(dir string, extra ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	env := func(run *api.Run) []string {
		ws := WorkspaceDir(dir, run.ID)
		home := HomeDir(dir, run.ID)
		_ = os.MkdirAll(ws, 0o755)
		_ = os.MkdirAll(home, 0o755)
		user := run.Spec.Workload.User
		if user == "" {
			user = shellUser
		}
		out := append(slices.Clone(extra), workspaceVar+"="+ws, cgroupVar+"="+CgroupDir(dir, run.ID),
			"HOME="+home, "USER="+user, "LUX_RUN_ID="+run.ID, "LUX_EPOCH="+strconv.Itoa(run.Epoch))
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
		t, err := ShellTerminalIn(24, 80, run.Spec.Workload.Command, env(run), ShellDir(dir, run.ID, run.Spec.Workload.Workdir))
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

// HomeDir is Run id's HOME under RunShells' dir.
func HomeDir(dir, id string) string { return filepath.Join(dir, id, "home") }

// RootDir is the container's / of Run id under RunShells' dir.
func RootDir(dir, id string) string { return filepath.Join(dir, id, "root") }

// ShellDir is where Run id's workload and exec commands run, as the shim
// picks it: workload.workdir, else HOME; one that does not exist is /.
// A workdir under /workspace is mapped into WorkspaceDir, created as lux's
// volume and checkouts would have it; any other into RootDir, not created.
func ShellDir(dir, id, workdir string) string {
	d := HomeDir(dir, id)
	if workdir != "" {
		rel, err := filepath.Rel(workspaceRoot, filepath.Clean(workdir))
		if err == nil && rel != ".." && !strings.HasPrefix(rel, "../") {
			d = filepath.Join(WorkspaceDir(dir, id), rel)
			_ = os.MkdirAll(d, 0o755)
		} else {
			d = filepath.Join(RootDir(dir, id), workdir)
		}
	}
	if _, err := os.Stat(d); err != nil {
		d = RootDir(dir, id)
		_ = os.MkdirAll(d, 0o755)
	}
	return d
}

// workspaceRoot is the state volume's path inside a Run.
const workspaceRoot = "/workspace"

// runsDirOf is RunShells' dir, read back from the workspace variable of an
// environment its ExecEnv made ("": another ExecEnv's).
func runsDirOf(env []string) string {
	for _, kv := range slices.Backward(env) {
		if v, ok := strings.CutPrefix(kv, workspaceVar+"="); ok {
			return filepath.Dir(filepath.Dir(v))
		}
	}
	return ""
}

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
	responder := s.execResponder
	var run api.Run
	if r != nil {
		run = *r.run
	}
	s.mu.Unlock()
	switch {
	case r == nil:
		writeErr(w, 404, "not_found", "no such run")
		return
	case run.State != "running" || (envOf == nil && responder == nil):
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
	if a, ok := s.execResponse(id, open.Command); ok {
		// A hang lasts until the client goes away, not the 10 s above.
		respondExec(req.Context(), ws, a)
		return
	}
	if envOf == nil {
		// The responder declined this one and no command runs here.
		_ = wsjson.Write(ctx, ws, api.StreamData{Error: "the workload has not started yet"})
		return
	}
	cmd := exec.CommandContext(ctx, open.Command[0], open.Command[1:]...)
	env := envOf(&run)
	cmd.Env = append(os.Environ(), env...)
	if d := runsDirOf(env); d != "" {
		cmd.Dir = ShellDir(d, run.ID, run.Spec.Workload.Workdir)
	}
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
