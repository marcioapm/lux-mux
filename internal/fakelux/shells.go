package fakelux

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
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

// WrapperUnsupported is why the start wrapper cannot run for real here, or
// "": tests of its output expect Linux's sh, and the env file and facts
// need flock(1) and setsid(1).
func WrapperUnsupported() string {
	if runtime.GOOS != "linux" {
		return "the start wrapper's tests expect a Linux sh, not " + runtime.GOOS
	}
	for _, tool := range []string{"sh", "flock", "setsid"} {
		if _, err := exec.LookPath(tool); err != nil {
			return "the start wrapper needs " + tool + ": " + err.Error()
		}
	}
	return ""
}

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
	// The hijacked connection does not cancel req's context: conn ends
	// once the client closes or drops the stream (the reader below), ctx
	// also after 10 s.
	conn, gone := context.WithCancel(req.Context())
	defer gone()
	ctx, cancel := context.WithTimeout(conn, 10*time.Second)
	defer cancel()
	var open api.StreamOpen
	if wsjson.Read(ctx, ws, &open) != nil || len(open.Command) == 0 {
		_ = wsjson.Write(ctx, ws, api.StreamData{Error: "no command"})
		return
	}
	// What the client sends after StreamOpen (stdin, EOF) is not used.
	go func() {
		defer gone()
		for {
			if _, _, err := ws.Read(conn); err != nil {
				return
			}
		}
	}()
	s.mu.Lock()
	s.execs = append(s.execs, slices.Clone(open.Command))
	s.mu.Unlock()
	if a, ok := s.execResponse(id, open.Command); ok {
		// A hang lasts until the client goes away, not the 10 s above.
		respondExec(conn, ws, a)
		return
	}
	if envOf == nil {
		// The responder declined this one and no command runs here.
		_ = wsjson.Write(ctx, ws, api.StreamData{Error: "the workload has not started yet"})
		return
	}
	out, code := s.runExec(ctx, id, &run, envOf, open.Command)
	if len(out) > 0 {
		_ = wsjson.Write(ctx, ws, api.StreamData{Channel: "stdout", Data: out})
	}
	_ = wsjson.Write(ctx, ws, api.StreamData{ExitCode: &code})
	_ = ws.Close(websocket.StatusNormalClosure, "")
}

// runExec runs argv in Run id as lux's shim runs an exec: in its own
// process group, all of which is killed when ctx ends (the client went
// away, or the 10 s bound) and when the Run's workload stops. It returns
// the combined output and the exit code.
func (s *Server) runExec(ctx context.Context, id string, run *api.Run, envOf func(*api.Run) []string, argv []string) ([]byte, int) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	env := envOf(run)
	cmd.Env = append(os.Environ(), env...)
	if d := runsDirOf(env); d != "" {
		cmd.Dir = ShellDir(d, run.ID, run.Spec.Workload.Workdir)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Cancel runs before Wait reaps the leader, so its pid still names
	// the group.
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	// A child that escaped the group may hold the output pipe open.
	cmd.WaitDelay = time.Second
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if !s.beginProcs() {
		return out.Bytes(), 1
	}
	err := cmd.Start()
	var g *group
	if err == nil {
		g = &group{pgid: cmd.Process.Pid}
		s.track(g)
		s.mu.Lock()
		var t *Terminal
		if r := s.runs[id]; r != nil {
			t = r.term
		}
		s.mu.Unlock()
		if t != nil {
			t.addExec(g)
		}
	}
	s.inflight.Done()
	if err != nil {
		return out.Bytes(), 1
	}
	err = cmd.Wait()
	// Marks a group left empty gone before its number can be reused.
	g.alive()
	// WaitDelay only cuts the output copy short; report the command's
	// own exit code, as lux does.
	if errors.Is(err, exec.ErrWaitDelay) && (cmd.ProcessState == nil || cmd.ProcessState.Success()) {
		err = nil
	}
	if err == nil {
		return out.Bytes(), 0
	}
	if cmd.ProcessState != nil {
		return out.Bytes(), max(cmd.ProcessState.ExitCode(), 1)
	}
	return out.Bytes(), 1
}
