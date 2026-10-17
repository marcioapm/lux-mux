package config

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// stuckTracer holds a process the way uninterruptible sleep does: seized
// with PTRACE_O_TRACEEXIT, a SIGKILLed tracee stops at its exit event and
// neither finishes exiting nor becomes reapable until the tracer lets it
// go. Every ptrace request runs on one locked OS thread, as ptrace needs.
type stuckTracer struct {
	req  chan func()
	done chan struct{}
}

func newStuckTracer() *stuckTracer {
	s := &stuckTracer{req: make(chan func()), done: make(chan struct{})}
	go func() {
		runtime.LockOSThread()
		defer close(s.done)
		for f := range s.req {
			f()
		}
	}()
	return s
}

func (s *stuckTracer) do(f func()) {
	ch := make(chan struct{})
	s.req <- func() { f(); close(ch) }
	<-ch
}

func (s *stuckTracer) seize(pid int) error {
	var err error
	s.do(func() {
		_, _, e := unix.RawSyscall6(unix.SYS_PTRACE, unix.PTRACE_SEIZE, uintptr(pid), 0, unix.PTRACE_O_TRACEEXIT, 0, 0)
		if e != 0 {
			err = e
		}
	})
	return err
}

// release kills the tracee if it still runs and detaches once it is in its
// exit stop, so it finishes exiting.
func (s *stuckTracer) release(pid int) {
	unix.Kill(pid, unix.SIGKILL)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var err error
		s.do(func() { err = unix.PtraceDetach(pid) })
		if err == nil || errors.Is(err, unix.ESRCH) && unix.Kill(pid, 0) != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	close(s.req)
	<-s.done
}

// TestWrapperProbeStuckAfterKill: a df that SIGKILL cannot end (held in
// its exit stop, as uninterruptible sleep on a dead mount would hold it)
// delays the prompt by the probe deadline only, and the facts are empty.
// The stuck process is left behind, as documented, until it can exit.
func TestWrapperProbeStuckAfterKill(t *testing.T) {
	for label, shell := range testShells(t) {
		t.Run(label, func(t *testing.T) {
			stuck := `printf '%s' "$$" >"$LM_STATE/df.tmp" && mv "$LM_STATE/df.tmp" "$LM_STATE/df.pid"
while [ ! -e "$LM_STATE/seized" ]; do sleep 0.01; done
exec sleep 30`
			f := newFixture(t, toolDir(t, baseTools, map[string]string{"df": stuck, "stat": "exit 1"}), Profile{Image: "alpine"})
			f.shell = shell
			const deadline = 500 * time.Millisecond
			f.shortProbes("0.5")
			tr := newStuckTracer()
			pid := 0
			defer func() {
				if pid != 0 {
					tr.release(pid)
				} else {
					close(tr.req)
				}
			}()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := commandIn(ctx, f, f.cmd[1:])
			cmd.WaitDelay = time.Second
			outc := make(chan []byte, 1)
			start := time.Now()
			go func() {
				b, _ := cmd.CombinedOutput()
				outc <- b
			}()
			file := filepath.Join(f.state, "df.pid")
			for pid == 0 && time.Since(start) < 5*time.Second {
				if _, err := os.Stat(file); err == nil {
					p := readPid(t, file)
					err := tr.seize(p)
					switch {
					case errors.Is(err, unix.EPERM):
						t.Skipf("ptrace not permitted here: %v", err)
					case errors.Is(err, unix.ESRCH) && time.Since(start) >= deadline:
						// The watchdog fired before this poll seized df: the
						// stuck state was never built, so there is nothing to test.
						t.Skipf("seize %d after the %v deadline: %v", p, deadline, err)
					case err != nil:
						t.Fatalf("seize %d: %v", p, err)
					}
					pid = p
					if err := os.WriteFile(filepath.Join(f.state, "seized"), nil, 0o600); err != nil {
						t.Fatal(err)
					}
					break
				}
				time.Sleep(2 * time.Millisecond)
			}
			if pid == 0 {
				t.Fatal("df never started")
			}
			out := <-outc
			d := time.Since(start)
			if string(out) != "shell:start" || d > 4*time.Second {
				t.Fatalf("prompt %q after %v", out, d)
			}
			// The simulation held: the killed df has not finished exiting.
			if err := unix.Kill(pid, 0); err != nil {
				t.Fatalf("the stuck probe exited: %v", err)
			}
			if got := f.source(); got[EnvDisk] != "" || got[EnvBranchCheckedOut] != "" {
				t.Fatalf("facts from a stuck probe: %q", got)
			}
		})
	}
}
