package fakelux

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/marcioapm/lux/api"

	"github.com/marcioapm/lux-mux/internal/luxapi"
)

// writerLoop is an sh script that records its pid in $W/<$1>.pid, then
// appends to $W/<$1>.log every 10 ms until killed or until the append
// fails (the test's directory was removed: a failing test leaks no
// loop). It ignores SIGHUP, as a nohup'd job does, so only a kill of its
// group ends it while the workspace exists.
const writerLoop = `trap '' HUP
echo $$ >"$` + workspaceVar + `/$1.pid"
while echo x 2>/dev/null >>"$` + workspaceVar + `/$1.log"; do sleep 0.01; done`

// forkWriter is an sh command line that starts writerLoop as name in the
// background, with no descriptor of its caller's output, and records the
// caller's own pid in name-leader.pid.
func forkWriter(name string) string {
	return `sh -c '` + strings.ReplaceAll(writerLoop, `'`, `'\''`) + `' sh ` + name + ` >/dev/null 2>&1 </dev/null &
echo $$ >"$` + workspaceVar + `/` + name + `-leader.pid"`
}

// pidsOf waits for the pid files of name's writer and its leader in Run
// id's workspace and returns both pids.
func pidsOf(t *testing.T, dir, id, name string) []int {
	t.Helper()
	var pids []int
	for _, f := range []string{name + ".pid", name + "-leader.pid"} {
		path := filepath.Join(WorkspaceDir(dir, id), f)
		var pid int
		if !WaitFor(context.Background(), 5*time.Second, func() bool {
			b, err := os.ReadFile(path)
			if err != nil {
				return false
			}
			pid, err = strconv.Atoi(strings.TrimSpace(string(b)))
			return err == nil
		}) {
			t.Fatalf("%s not written", path)
		}
		pids = append(pids, pid)
	}
	return pids
}

// writing waits until Run id's writer name has appended to its log.
func writing(t *testing.T, dir, id, name string) {
	t.Helper()
	if !WaitFor(context.Background(), 5*time.Second, func() bool { return logSize(dir, id, name) > 0 }) {
		t.Fatalf("writer %s never wrote", name)
	}
}

func logSize(dir, id, name string) int64 {
	fi, err := os.Stat(filepath.Join(WorkspaceDir(dir, id), name+".log"))
	if err != nil {
		return 0
	}
	return fi.Size()
}

// running is the pids that still exist (zombies included).
func running(pids []int) []int {
	var out []int
	for _, p := range pids {
		if err := syscall.Kill(p, 0); !errors.Is(err, syscall.ESRCH) {
			out = append(out, p)
		}
	}
	return out
}

// assertStopped fails unless no pid exists now and writer name's log
// does not grow over the next 200 ms.
func assertStopped(t *testing.T, dir, id, name string, pids []int) {
	t.Helper()
	if left := running(pids); len(left) > 0 {
		t.Fatalf("processes %v of %s still exist", left, name)
	}
	n := logSize(dir, id, name)
	time.Sleep(200 * time.Millisecond)
	if m := logSize(dir, id, name); m != n {
		t.Fatalf("%s's log grew from %d to %d bytes after its processes were stopped", name, n, m)
	}
}

func waitSpec(script string) api.RunSpec {
	sp := shellSpec()
	sp.Workload.Command = []string{"sh", "-c", script + "\nexec sleep 60"}
	return sp
}

// TestCloseKillsShellChildren: a Run shell's background child that
// ignores SIGHUP is gone, and writes nothing more, once Close returns.
func TestCloseKillsShellChildren(t *testing.T) {
	s, _, dir, id := shellRun(t, waitSpec(forkWriter("shell")))
	pids := pidsOf(t, dir, id, "shell")
	writing(t, dir, id, "shell")
	s.Close()
	assertStopped(t, dir, id, "shell", pids)
}

// TestCloseKillsExecChildren: an exec whose command forks a writer and
// exits leaves it running in the Run, as in a container; Close kills it
// and returns once it is gone.
func TestCloseKillsExecChildren(t *testing.T) {
	s, c, dir, id := shellRun(t, waitSpec(""))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if code, err := c.Exec(ctx, id, []string{"sh", "-c", forkWriter("exec")}); err != nil || code != 0 {
		t.Fatalf("exec: %d %v", code, err)
	}
	pids := pidsOf(t, dir, id, "exec")
	writing(t, dir, id, "exec")
	s.Close()
	assertStopped(t, dir, id, "exec", pids)
}

// TestEndKillsShellAndExecChildren: End stops the workload as a container
// stop does: the shell's children and those an exec left die with it,
// while the server stays up.
func TestEndKillsShellAndExecChildren(t *testing.T) {
	s, c, dir, id := shellRun(t, waitSpec(forkWriter("shell")))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if code, err := c.Exec(ctx, id, []string{"sh", "-c", forkWriter("exec")}); err != nil || code != 0 {
		t.Fatalf("exec: %d %v", code, err)
	}
	pids := append(pidsOf(t, dir, id, "shell"), pidsOf(t, dir, id, "exec")...)
	writing(t, dir, id, "shell")
	writing(t, dir, id, "exec")
	s.End(id, "stopped", "stop")
	if !WaitFor(context.Background(), 2*time.Second, func() bool { return len(running(pids)) == 0 }) {
		t.Fatalf("processes %v still exist after End", running(pids))
	}
	assertStopped(t, dir, id, "shell", pids)
	assertStopped(t, dir, id, "exec", pids)
}

// TestExecClientGoneKillsGroup: when an exec's client goes away while its
// command runs, the command and the child it forked die promptly, not
// at the exec's 10 s bound.
func TestExecClientGoneKillsGroup(t *testing.T) {
	_, c, dir, id := shellRun(t, waitSpec(""))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := c.Exec(ctx, id, []string{"sh", "-c", forkWriter("exec") + "\nexec sleep 60"})
		done <- err
	}()
	defer cancel()
	pids := pidsOf(t, dir, id, "exec")
	writing(t, dir, id, "exec")
	cancel()
	execReturned(t, done)
	if !WaitFor(context.Background(), 2*time.Second, func() bool { return len(running(pids)) == 0 }) {
		t.Fatalf("processes %v still exist 2 s after the exec's client went away", running(pids))
	}
	assertStopped(t, dir, id, "exec", pids)
}

// TestCloseAfterExecClientLeft is the shape of a mux test's teardown: the
// client of a slow exec (luxmux's window-name write) goes away, Close
// runs, then the test's TempDir is removed. Once Close returned, the
// exec must not create anything under the removed directory.
func TestCloseAfterExecClientLeft(t *testing.T) {
	s, c, dir, id := shellRun(t, waitSpec(""))
	sub := filepath.Join(WorkspaceDir(dir, id), ".luxmux")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := c.Exec(ctx, id, []string{"sh", "-c", `echo $$ >"$1/started"; sleep 0.3; mkdir -p "$1"; echo late >"$1/window"`, "sh", sub})
		done <- err
	}()
	if !WaitFor(context.Background(), 5*time.Second, func() bool {
		_, err := os.Stat(filepath.Join(sub, "started"))
		return err == nil
	}) {
		t.Fatal("the exec never started")
	}
	cancel()
	execReturned(t, done)
	s.Close()
	if err := os.RemoveAll(sub); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	if _, err := os.Stat(sub); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%s was recreated after Close returned: %v", sub, err)
	}
}

// execReturned fails the test when an exec whose client went away does
// not return within 5 s, instead of hanging the package.
func execReturned(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the exec did not return after its client went away")
	}
}

// gatedStart submits a Run whose shell has spawned (and forked a writer)
// while Start is held before it hands the terminal back, as a Close that
// races a submit (a spare made at teardown) finds it.
func gatedStart(t *testing.T) (*Server, string, string, func()) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "runs")
	s := New()
	t.Cleanup(s.Close)
	s.RunShells(dir, "PS1=$ ", "ENV=")
	inner := s.NewTerminal
	gate := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(gate) }) }
	// Registered after Close, so it runs first: a failing test never
	// leaves Start, and Close behind it, held.
	t.Cleanup(release)
	spawned := make(chan struct{}, 1)
	s.NewTerminal = func(run *api.Run) *Terminal {
		tm := inner(run)
		spawned <- struct{}{}
		<-gate
		return tm
	}
	c, err := luxapi.New(s.Config())
	if err != nil {
		t.Fatal(err)
	}
	go func() { _, _ = c.Submit(context.Background(), waitSpec(forkWriter("shell"))) }()
	select {
	case <-spawned:
	case <-time.After(5 * time.Second):
		t.Fatal("the Run's shell never spawned")
	}
	s.mu.Lock()
	id := s.order[0]
	s.mu.Unlock()
	return s, dir, id, release
}

// TestCloseWaitsInflightStart: Close's process stop during a Start under
// way does not return while that Start's processes run. It calls
// stopProcs, not Close: closing the HTTP server would itself wait for the
// held Submit and hide an early return.
func TestCloseWaitsInflightStart(t *testing.T) {
	s, dir, id, release := gatedStart(t)
	pids := pidsOf(t, dir, id, "shell")
	stopped := make(chan struct{})
	go func() { s.stopProcs(); close(stopped) }()
	select {
	case <-stopped:
		t.Fatalf("stopProcs returned while a Start was under way (its processes %v)", running(pids))
	case <-time.After(500 * time.Millisecond):
	}
	release()
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("stopProcs did not return once the Start finished")
	}
	assertStopped(t, dir, id, "shell", pids)
}

// TestLateStartKilled: a Start that finishes after Close stopped waiting
// for it (procWait) has its processes killed as it registers them.
func TestLateStartKilled(t *testing.T) {
	old := procWait
	procWait = 300 * time.Millisecond
	t.Cleanup(func() { procWait = old })
	s, dir, id, release := gatedStart(t)
	pids := pidsOf(t, dir, id, "shell")
	stopped := make(chan struct{})
	go func() { s.stopProcs(); close(stopped) }()
	// The Start registers its group only after stopProcs gave up on it.
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("stopProcs did not give up on the held Start after procWait")
	}
	release()
	if !WaitFor(context.Background(), 2*time.Second, func() bool { return len(running(pids)) == 0 }) {
		t.Fatalf("processes %v of a Start finished after Close still exist", running(pids))
	}
}

// TestExecLeavingChildReportsItsCode: an exec whose command exits while a
// child it left holds the output reports the command's exit code, as lux
// does, not a failure.
func TestExecLeavingChildReportsItsCode(t *testing.T) {
	_, c, _, id := shellRun(t, waitSpec(""))
	for want, script := range map[int]string{0: "echo hi; sleep 3 & exit 0", 3: "echo hi; sleep 3 & exit 3"} {
		_, code, err := c.ExecOutput(context.Background(), id, []string{"sh", "-c", script}, 4096)
		if err != nil {
			t.Fatal(err)
		}
		if code != want {
			t.Errorf("%q: exit %d, want %d", script, code, want)
		}
	}
}
