package fakelux

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/marcioapm/lux/api"

	"github.com/marcioapm/lux-mux/internal/luxapi"
)

// shellRun submits sp to a RunShells server over dir and waits for it to
// run.
func shellRun(t *testing.T, sp api.RunSpec) (*Server, *luxapi.Client, string, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "runs")
	s := New()
	t.Cleanup(s.Close)
	s.RunShells(dir, "PS1=$ ", "ENV=")
	c, err := luxapi.New(s.Config())
	if err != nil {
		t.Fatal(err)
	}
	r, err := c.Submit(context.Background(), sp)
	if err != nil {
		t.Fatal(err)
	}
	if !WaitFor(context.Background(), 5*time.Second, func() bool { return s.Run(r.ID).State == "running" }) {
		t.Fatalf("run %s is %s", r.ID, s.Run(r.ID).State)
	}
	return s, c, dir, r.ID
}

// probeSpec is a spec whose workload writes its cwd, HOME and USER into
// the state volume, then waits.
func probeSpec(workdir, user string) api.RunSpec {
	sp := shellSpec()
	sp.Workload.Workdir, sp.Workload.User = workdir, user
	sp.Workload.Command = []string{"sh", "-c", `printf '%s\n%s\n%s\n' "$(pwd -P)" "$HOME" "$USER" >"$` + workspaceVar + `/probe"; exec sleep 60`}
	return sp
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	var b []byte
	if !WaitFor(context.Background(), 5*time.Second, func() bool {
		var err error
		b, err = os.ReadFile(path)
		return err == nil && strings.Count(string(b), "\n") >= 3
	}) {
		t.Fatalf("%s not written: %q", path, b)
	}
	return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
}

// TestShellsWorkdirHomeUser: as the shim, the workload and exec run in
// workload.workdir (here under the state volume), else HOME, else /
// when the workdir does not exist; HOME and USER are set for the
// workload user, not inherited from the test process.
func TestShellsWorkdirHomeUser(t *testing.T) {
	for _, tc := range []struct {
		name, workdir, user string
		dir                 func(dir, id string) string
		wantUser            string
	}{
		{"workdir on the volume", "/workspace/repos/app", "", func(d, id string) string { return filepath.Join(WorkspaceDir(d, id), "repos", "app") }, shellUser},
		{"no workdir: HOME", "", "dev", HomeDir, "dev"},
		{"missing workdir off the volume: /", "/opt/none", "", RootDir, shellUser},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, c, dir, id := shellRun(t, probeSpec(tc.workdir, tc.user))
			got := readLines(t, filepath.Join(WorkspaceDir(dir, id), "probe"))
			cwd, err := filepath.EvalSymlinks(tc.dir(dir, id))
			if err != nil {
				t.Fatal(err)
			}
			want := []string{cwd, HomeDir(dir, id), tc.wantUser}
			if strings.Join(got, "|") != strings.Join(want, "|") {
				t.Fatalf("workload cwd|HOME|USER %q, want %q", got, want)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			code, err := c.Exec(ctx, id, []string{"sh", "-c", `printf '%s\n%s\n%s\n' "$(pwd -P)" "$HOME" "$USER" >"$` + workspaceVar + `/exec"`})
			if err != nil || code != 0 {
				t.Fatalf("exec: %d %v", code, err)
			}
			if got := readLines(t, filepath.Join(WorkspaceDir(dir, id), "exec")); strings.Join(got, "|") != strings.Join(want, "|") {
				t.Fatalf("exec cwd|HOME|USER %q, want %q", got, want)
			}
		})
	}
}
