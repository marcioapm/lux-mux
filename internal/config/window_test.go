package config

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestWindowFacts(t *testing.T) {
	p := Profile{Image: "alpine", Pool: "plain", CPUs: 0.5, OnStart: "make deps", OnResume: "make warm", Workdir: "/workspace/tools", Repositories: []Repository{{URL: "https://example.com/api.git", Branch: "feature"}, {URL: "https://example.com/web.git", Path: "/workspace/web"}}}
	sp, err := RunSpec(p, Target{Session: "original", Profile: "app"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{EnvSession: "original", EnvProfile: "app", EnvImage: "alpine", EnvPool: "plain", EnvSize: "", EnvBranch: "feature", EnvRepoDir: "/workspace/repos/api", EnvWorkdir: "/workspace/tools", EnvRepositories: "api\thttps://example.com/api.git\tfeature\t/workspace/repos/api\nweb\thttps://example.com/web.git\t\t/workspace/web"} {
		if sp.Env[k] != want {
			t.Errorf("%s = %q, want %q", k, sp.Env[k], want)
		}
	}
	for _, n := range append(append([]string{}, StaticEnv...), ComputedEnv...) {
		q := Profile{Image: "alpine", Secrets: []string{n}}
		if _, err := RunSpec(q, Target{}, map[string]string{n: "test-secret"}); err == nil {
			t.Fatalf("reserved secret %s accepted", n)
		}
	}
	for _, n := range []string{"LUX_RUN_ID", "LUX_EPOCH", "LUX_INPUTS", "LUX_SERVICE_DB"} {
		if err := (Profile{Image: "alpine", Secrets: []string{n}}).Validate(); err == nil {
			t.Fatalf("lux-owned secret %s accepted", n)
		}
	}
}

// TestSecretNamespaceRefused: a secret named after a variable lux's shim
// sets or one luxmux sets is refused before a RunSpec exists, so its value
// is never submitted; other LUX_ names are accepted. A real wrapper run
// with what lux would export for such a secret (LUX_RUN_ID holding the
// value) still keeps the value out of the env file.
func TestSecretNamespaceRefused(t *testing.T) {
	const value = "secret-run-id-value"
	for _, n := range []string{"LUX_API_KEY", "LUX_TOKEN", "LUX_SERVICE", "LUX_RUN_ID_X"} {
		for name, p := range map[string]Profile{
			"secrets":        {Image: "alpine", Secrets: []string{n}},
			"git_credential": {Image: "alpine", Repository: "https://example.com/a.git", GitCredential: n},
			"repository":     {Image: "alpine", Repositories: []Repository{{URL: "https://example.com/a.git", GitCredential: n}}},
		} {
			if _, err := RunSpec(p, Target{Session: "s", Profile: "p"}, map[string]string{n: value}); err != nil {
				t.Fatalf("%s %s refused: %v", name, n, err)
			}
		}
		if _, err := Parse([]byte("[profiles.a]\nimage = \"x\"\nsecrets = [\"" + n + "\"]\n")); err != nil {
			t.Fatalf("config with secret %s refused: %v", n, err)
		}
	}
	for _, n := range []string{"LUX_RUN_ID", "LUX_EPOCH", "LUX_INPUTS", "LUX_SERVICE_DB", "LUX_SERVICE_DB_URL", "LUXMUX_RUN_ID", EnvWindow, EnvTestWorkspace} {
		for name, p := range map[string]Profile{
			"secrets":        {Image: "alpine", Secrets: []string{n}},
			"git_credential": {Image: "alpine", Repository: "https://example.com/a.git", GitCredential: n},
			"repository":     {Image: "alpine", Repositories: []Repository{{URL: "https://example.com/a.git", GitCredential: n}}},
		} {
			sp, err := RunSpec(p, Target{Session: "s", Profile: "p"}, map[string]string{n: value})
			if err == nil {
				t.Fatalf("%s %s accepted", name, n)
			}
			if b, _ := json.Marshal(sp); strings.Contains(string(b), value) {
				t.Fatalf("%s %s: refused spec carries the value: %s", name, n, b)
			}
		}
		if _, err := Parse([]byte("[profiles.a]\nimage = \"x\"\nsecrets = [\"" + n + "\"]\n")); err == nil {
			t.Fatalf("config with secret %s accepted", n)
		}
	}
	// An ordinary name is a lux env secret and stays out of the env file.
	p := Profile{Image: "alpine", Secrets: []string{"RUN_TOKEN"}}
	sp, err := RunSpec(p, Target{Session: "s", Profile: "p"}, map[string]string{"RUN_TOKEN": value})
	if err != nil || len(sp.Secrets) != 1 || sp.Secrets[0].As != "env" || sp.Secrets[0].Value != value {
		t.Fatalf("%+v %v", sp.Secrets, err)
	}
	f := newFixture(t, toolDir(t, baseTools, nil), p)
	f.env = append(f.env, "RUN_TOKEN="+value)
	f.mustWrapper()
	if b, err := os.ReadFile(filepath.Join(f.dir, ".luxmux", EnvFile)); err != nil || strings.Contains(string(b), value) {
		t.Fatalf("env file: %v %q", err, b)
	}
	// The overlay lux v0.1.30 applies for an env secret named LUX_RUN_ID,
	// had one been submitted: its value replaces the Run's id.
	for _, v := range []string{value, "run_x;" + value, "run_" + value} {
		g := newFixture(t, toolDir(t, baseTools, nil), Profile{Image: "alpine"})
		g.mustWrapper("LUX_RUN_ID=" + v)
		b, err := os.ReadFile(filepath.Join(g.dir, ".luxmux", EnvFile))
		if err != nil || strings.Contains(string(b), value) || !strings.Contains(string(b), "export LUXMUX_RUN_ID=''\n") {
			t.Fatalf("LUX_RUN_ID=%q: env file %v %q", v, err, b)
		}
	}
	g := newFixture(t, toolDir(t, baseTools, nil), Profile{Image: "alpine"})
	g.mustWrapper("LUX_RUN_ID=run_abc234xyz")
	if b, _ := os.ReadFile(filepath.Join(g.dir, ".luxmux", EnvFile)); !strings.Contains(string(b), "export LUXMUX_RUN_ID='run_abc234xyz'\n") {
		t.Fatalf("a lux Run id is not copied: %q", b)
	}
}

// TestRepositoriesEnvBound: LUXMUX_REPOSITORIES of exactly MaxEnvString
// bytes (NAME=value and NUL) is accepted and a process starts with the
// RunSpec's env; one byte more is refused. The kernel's own limit is
// demonstrated above the bound.
func TestRepositoriesEnvBound(t *testing.T) {
	if MaxEnvString >= 128<<10 {
		t.Fatalf("MaxEnvString %d is not below 128 KiB", MaxEnvString)
	}
	profile := func(branch int) Profile {
		return Profile{Image: "alpine", Repository: "https://example.com/a.git", Branch: strings.Repeat("b", branch)}
	}
	size := func(p Profile) int {
		return envStringSize(EnvRepositories, windowEnv(p, Target{}, Workspace)[EnvRepositories])
	}
	at := MaxEnvString - (size(profile(1)) - 1)
	if n := size(profile(at)); n != MaxEnvString {
		t.Fatalf("fixture size %d", n)
	}
	sp, err := RunSpec(profile(at), Target{Session: "s", Profile: "p"}, nil)
	if err != nil {
		t.Fatalf("at the bound: %v", err)
	}
	if err := profile(at - 1).Validate(); err != nil {
		t.Fatalf("below the bound: %v", err)
	}
	if err := profile(at + 1).Validate(); err == nil || !strings.Contains(err.Error(), EnvRepositories) {
		t.Fatalf("over the bound: %v", err)
	}
	if _, err := Parse([]byte("[profiles.a]\nimage = \"x\"\nrepository = \"https://example.com/a.git\"\nbranch = \"" + strings.Repeat("b", at+1) + "\"\n")); err == nil {
		t.Fatal("config over the bound accepted")
	}
	if runtime.GOOS != "linux" || os.Getpagesize() != 4096 {
		t.Skip("exec limit checks assume Linux with 4 KiB pages")
	}
	env := []string{"PATH=/usr/bin:/bin"}
	for k, v := range sp.Env {
		env = append(env, k+"="+v)
	}
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	cmd.Env = env
	if err := cmd.Run(); err != nil {
		t.Fatalf("exec with the bounded env: %v", err)
	}
	cmd = exec.Command("/bin/sh", "-c", "exit 0")
	cmd.Env = []string{"X=" + strings.Repeat("x", 128<<10-2)}
	if err := cmd.Run(); !errors.Is(err, syscall.E2BIG) {
		t.Fatalf("a 128 KiB + 1 string: %v, want E2BIG", err)
	}
}

func TestStartWrapper(t *testing.T) {
	// It runs the wrapper with the host's PATH and reads the env file,
	// which needs flock(1); fixtures with toolDir skip on their own.
	if _, err := exec.LookPath("flock"); err != nil {
		t.Skipf("the env file needs flock: %v", err)
	}
	shells := [][]string{{"sh"}}
	if b, err := exec.LookPath("busybox"); err == nil {
		shells = append(shells, []string{b, "sh"})
	} else {
		t.Log("busybox unavailable")
	}
	for _, shell := range shells {
		t.Run(filepath.Base(shell[0]), func(t *testing.T) {
			for _, tc := range []struct{ cpu, mem, want string }{{"max 100000", "max", ""}, {"200000 100000", "12345", "2"}, {"50000 100000", "max", "0.5"}, {"", "", ""}} {
				t.Run(tc.cpu, func(t *testing.T) {
					dir := t.TempDir()
					cg := filepath.Join(dir, "cg")
					if err := os.Mkdir(cg, 0700); err != nil {
						t.Fatal(err)
					}
					if tc.cpu != "" {
						os.WriteFile(filepath.Join(cg, "cpu.max"), []byte(tc.cpu+"\n"), 0600)
						os.WriteFile(filepath.Join(cg, "memory.max"), []byte(tc.mem+"\n"), 0600)
					}
					p := Profile{Image: "alpine", OnStart: `printf 'hook:%s:%s\n' "$LUXMUX_START" "$EXAMPLE_SECRET"; exit 7`, OnResume: `printf 'hook:%s\n' "$LUXMUX_START"`, Shell: []string{"/bin/sh", "-c", `printf 'shell:%s:%s:%s\n' "$LUXMUX_START" "$LUXMUX_CPU" "$LUXMUX_MEMORY"`}}
					sp, err := RunSpec(p, Target{Session: "s", Profile: "p"}, nil)
					if err != nil {
						t.Fatal(err)
					}
					sp.Env[EnvRepository] = "quote' $HOME `false`\nsecond line"
					run := func() string {
						ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
						defer cancel()
						args := append(append([]string{}, shell[1:]...), sp.Workload.Command[1:]...)
						cmd := exec.CommandContext(ctx, shell[0], args...)
						cmd.Env = append(os.Environ(), EnvTestWorkspace+"="+dir, EnvTestCgroup+"="+cg, "EXAMPLE_SECRET=test-secret-value", "LUX_RUN_ID=run_example")
						for k, v := range sp.Env {
							cmd.Env = append(cmd.Env, k+"="+v)
						}
						b, err := cmd.CombinedOutput()
						if err != nil {
							t.Fatalf("%v: %s", err, b)
						}
						return string(b)
					}
					first := run()
					if !strings.Contains(first, "hook:start:test-secret-value\n") || !strings.Contains(first, "on_start exited with status 7; starting the shell\n") || !strings.Contains(first, "shell:start:"+tc.want+":") {
						t.Fatal(first)
					}
					second := run()
					if !strings.Contains(second, "hook:resume\n") || strings.Contains(second, "hook:start") || !strings.Contains(second, "shell:resume:"+tc.want+":") {
						t.Fatal(second)
					}
					envfile := filepath.Join(dir, ".luxmux", "env")
					b, err := os.ReadFile(envfile)
					if err != nil {
						t.Fatal(err)
					}
					if strings.Contains(string(b), "test-secret") || strings.Contains(string(b), "EXAMPLE_SECRET") {
						t.Fatal("secret in facts file")
					}
					info, err := os.Stat(envfile)
					if err != nil || info.Mode().Perm() != 0644 {
						t.Fatalf("mode: %v %v", info, err)
					}
					cmd := exec.Command("sh", "-c", `. "$1"; printf '%s\000%s\000%s' "$LUXMUX_REPOSITORY" "$LUXMUX_MEMORY" "$LUXMUX_RUN_ID"`, "test", envfile)
					got, err := cmd.CombinedOutput()
					mem := tc.mem
					if mem == "max" {
						mem = ""
					}
					if err != nil || string(got) != sp.Env[EnvRepository]+"\x00"+mem+"\x00run_example" {
						t.Fatalf("source: %q %v", got, err)
					}
				})
			}
		})
	}
}

// TestWrapperQuiet: without commands the wrapper prints nothing before the
// shell, and a read-only workspace, an absent cgroup directory and no git
// on PATH change nothing but the facts.
func TestWrapperQuiet(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o755)
	sp, err := RunSpec(Profile{Image: "alpine", Shell: []string{"/bin/sh", "-c", `printf 'shell:%s:%s:%s'  "$LUXMUX_START" "$LUXMUX_BRANCH_CHECKED_OUT" "$$"`}}, Target{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", sp.Workload.Command[1:]...)
	cmd.Env = []string{"PATH=/nonexistent", EnvTestWorkspace + "=" + dir, EnvTestCgroup + "=" + filepath.Join(dir, "none"), EnvRepoDir + "=" + dir}
	var out strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	err = cmd.Wait()
	// The shell is exec'd: it is the wrapper's own process.
	if want := "shell:start::" + strconv.Itoa(cmd.Process.Pid); err != nil || out.String() != want {
		t.Fatalf("%q %v, want %q", out.String(), err, want)
	}
}

// TestWrapperText: the wrapper's text is one constant whatever the
// profile; commands are arguments only.
func TestWrapperText(t *testing.T) {
	cmd := `echo '"; exit 9; "' $(false)`
	a := WindowCommand(Profile{OnStart: cmd, OnResume: cmd})
	b := WindowCommand(Profile{})
	if a[2] != b[2] || a[2] != buildWrapper() || strings.Contains(a[2], cmd) || a[4] != cmd || a[5] != cmd {
		t.Fatal("the wrapper text depends on the profile")
	}
	shell, s, r, ok := ParseWindowCommand(WindowCommand(Profile{OnStart: "x", Shell: []string{"zsh", "-l"}}))
	if !ok || s != "x" || r != "" || len(shell) != 2 {
		t.Fatal(shell, s, r, ok)
	}
}

// baseTools are the external commands the wrapper and WindowNameScript
// use; fixtures give them a PATH holding these and nothing else.
var baseTools = []string{"sh", "cat", "mv", "chmod", "rm", "mkdir", "sleep", "flock", "timeout", "setsid", "id", "find"}

// realTool resolves a command, preferring the system's own binary over a
// PATH wrapper that a restricted PATH could not run.
func realTool(t *testing.T, name string) string {
	for _, d := range []string{"/usr/bin", "/bin"} {
		if p := filepath.Join(d, name); isExec(p) {
			return p
		}
	}
	p, err := exec.LookPath(name)
	if err != nil {
		t.Skipf("%s unavailable", name)
	}
	return p
}

func isExec(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0
}

// toolDir is a PATH directory with the real commands named and fake ones
// (sh script bodies; REAL_<name> in a body is that command's real path).
func toolDir(t *testing.T, real []string, fakes map[string]string) string {
	bin := filepath.Join(t.TempDir(), "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range real {
		if _, fake := fakes[n]; !fake {
			if err := os.Symlink(realTool(t, n), filepath.Join(bin, n)); err != nil {
				t.Fatal(err)
			}
		}
	}
	for n, body := range fakes {
		for _, r := range []string{"mv", "flock", "timeout", "df", "stat", "git"} {
			if strings.Contains(body, "REAL_"+r) {
				body = strings.ReplaceAll(body, "REAL_"+r, realTool(t, r))
			}
		}
		if err := os.WriteFile(filepath.Join(bin, n), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return bin
}

func toolsWithout(drop ...string) []string {
	return slices.DeleteFunc(slices.Clone(baseTools), func(n string) bool { return slices.Contains(drop, n) })
}

// fixture runs the wrapper and WindowNameScript against a workspace
// directory with a controlled PATH and environment.
type fixture struct {
	t     *testing.T
	dir   string
	bin   string
	state string
	shell []string
	cmd   []string
	env   []string
}

func newFixture(t *testing.T, bin string, p Profile) *fixture {
	if p.Shell == nil {
		p.Shell = []string{"/bin/sh", "-c", `printf 'shell:%s' "$LUXMUX_START"`}
	}
	sp, err := RunSpec(p, Target{Session: "s", Profile: "p"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, dir: t.TempDir(), bin: bin, shell: []string{"/bin/sh"}, cmd: sp.Workload.Command}
	f.state = filepath.Join(f.dir, "state")
	for _, d := range []string{"cg", "tmp", "state"} {
		if err := os.Mkdir(filepath.Join(f.dir, d), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	f.env = []string{"PATH=" + bin, "TMPDIR=" + filepath.Join(f.dir, "tmp"), "LM_STATE=" + f.state, "LUX_RUN_ID=run_example",
		EnvTestWorkspace + "=" + f.dir, EnvTestCgroup + "=" + filepath.Join(f.dir, "cg")}
	for k, v := range sp.Env {
		f.env = append(f.env, k+"="+v)
	}
	return f
}

// commandIn is f's shell running args in f's environment.
func commandIn(ctx context.Context, f *fixture, args []string, extra ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, f.shell[0], append(slices.Clone(f.shell[1:]), args...)...)
	cmd.Env = append(slices.Clone(f.env), extra...)
	return cmd
}

func (f *fixture) run(args []string, extra ...string) (string, time.Duration, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := commandIn(ctx, f, args, extra...)
	start := time.Now()
	b, err := cmd.CombinedOutput()
	return string(b), time.Since(start), err
}

func (f *fixture) wrapper(extra ...string) (string, time.Duration, error) {
	return f.run(f.cmd[1:], extra...)
}

func (f *fixture) mustWrapper(extra ...string) string {
	f.t.Helper()
	out, _, err := f.wrapper(extra...)
	if err != nil || !strings.Contains(out, "shell:") {
		f.t.Fatalf("wrapper: %v %q", err, out)
	}
	return out
}

func (f *fixture) name(name string, extra ...string) (time.Duration, error) {
	out, d, err := f.run(WindowNameCommand(name)[1:], extra...)
	if out != "" {
		f.t.Logf("name script output: %q", out)
	}
	return d, err
}

func (f *fixture) mustName(name string, extra ...string) {
	f.t.Helper()
	if _, err := f.name(name, extra...); err != nil {
		f.t.Fatalf("name %q: %v", name, err)
	}
}

func (f *fixture) path(name string) string { return filepath.Join(f.dir, ".luxmux", name) }

// probeDeadline is the watchdog's sleep in the wrapper text.
const probeDeadline = "sleep " + probeSeconds + ";"

// shortProbes gives f's wrapper a probe deadline of d seconds, so tests of
// what happens at the deadline do not each wait the production second;
// TestWrapperProbesBounded/hung keeps the production value.
func (f *fixture) shortProbes(d string) {
	f.t.Helper()
	if !strings.Contains(f.cmd[2], probeDeadline) {
		f.t.Fatal("the probe deadline is not in the wrapper")
	}
	f.cmd[2] = strings.Replace(f.cmd[2], probeDeadline, "sleep "+d+";", 1)
}

// dfFake prints df -Pk output for a 10-block (10240-byte) volume.
const dfFake = `printf 'Filesystem 1024-blocks Used Available Capacity Mounted on\nvolume 10 0 10 0%% workspace\n'`

func readPid(t *testing.T, file string) int {
	t.Helper()
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	return pid
}

// waitGone fails unless pid is gone within 2 s: SIGKILL is delivered at
// once, but the exit may lag a moment.
func waitGone(t *testing.T, pid int) {
	t.Helper()
	for end := time.Now().Add(2 * time.Second); syscall.Kill(pid, 0) == nil && time.Now().Before(end); {
		time.Sleep(10 * time.Millisecond)
	}
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		syscall.Kill(pid, syscall.SIGKILL)
		t.Fatalf("process %d remains: %v", pid, err)
	}
}

// source runs ". env" in a shell with an empty environment and returns
// every LUXMUX_* value it defines.
func (f *fixture) source() map[string]string {
	f.t.Helper()
	names := append(append(slices.Clone(StaticEnv), ComputedEnv...), EnvWindow)
	cmd := exec.Command("/bin/sh", "-c", `. "$1" || exit 9
shift
for n; do eval "[ -z \"\${$n+x}\" ] || printf '%s\000%s\000' \"\$n\" \"\$$n\""; done`, "source", f.path(EnvFile))
	cmd.Args = append(cmd.Args, names...)
	cmd.Env = []string{"PATH=" + f.bin}
	b, err := cmd.Output()
	if err != nil {
		f.t.Fatalf("sourcing the env file: %v", err)
	}
	got := map[string]string{}
	parts := strings.Split(string(b), "\x00")
	for i := 0; i+1 < len(parts); i += 2 {
		got[parts[i]] = parts[i+1]
	}
	return got
}

func testShells(t *testing.T) map[string][]string {
	shells := map[string][]string{"sh": {"/bin/sh"}}
	if b, err := exec.LookPath("busybox"); err == nil {
		shells["busybox"] = []string{b, "sh"}
	} else {
		t.Log("busybox unavailable: only /bin/sh runs")
	}
	return shells
}

// Interposers that run another writer in the middle of a writer's env
// replacement. mv starts $LM_NEXT (as role $LM_NEXT_ROLE) when a writer
// with role $LM_TRIGGER_ROLE is about to replace the env file, then waits
// until that writer is about to take the lock (flock marks it) or has
// finished. Both actual replacements and locks use the real commands.
var interposers = map[string]string{
	"mv": `case $# in 3) _d=$3 ;; *) _d= ;; esac
case $_d in
*/env)
	if [ -n "${LM_ROLE-}" ] && [ "$LM_ROLE" = "${LM_TRIGGER_ROLE-}" ] && mkdir "$LM_STATE/triggered" 2>/dev/null; then
		LM_ROLE=$LM_NEXT_ROLE sh -c "$LM_NEXT" >"$LM_STATE/next.out" 2>&1 </dev/null 9>&- &
		i=0
		while [ ! -e "$LM_STATE/$LM_NEXT_ROLE.locking" ] && [ ! -e "$LM_STATE/next.done" ] && [ $i -lt 200 ]; do sleep 0.05; i=$((i + 1)); done
	fi ;;
esac
exec REAL_mv "$@"`,
	"flock": `[ -n "${LM_ROLE-}" ] && : >"$LM_STATE/$LM_ROLE.locking"
exec REAL_flock "$@"`,
}

func waitDone(t *testing.T, state string) string {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(state, "next.done")); err == nil {
			b, _ := os.ReadFile(filepath.Join(state, "next.rc"))
			out, _ := os.ReadFile(filepath.Join(state, "next.out"))
			if strings.TrimSpace(string(b)) != "0" {
				t.Fatalf("interleaved writer failed: rc %q, output %q", b, out)
			}
			return string(out)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("interleaved writer never finished")
	return ""
}

// TestEnvWritersInterleaved: a rename that lands while a placement is
// replacing the env file, and a placement that lands while a rename is
// replacing it, both leave the latest name and the latest facts. It runs
// under /bin/sh only: BusyBox sh runs mv as a built-in applet, which a PATH
// interposer cannot intercept.
func TestEnvWritersInterleaved(t *testing.T) {
	shell := []string{"/bin/sh"}
	{
		{
			t.Run("rename during placement", func(t *testing.T) {
				f := newFixture(t, toolDir(t, baseTools, interposers), Profile{Image: "alpine"})
				f.shell = shell
				f.mustName("rename-1")
				f.mustWrapper("LM_ROLE=wrapper", "LM_TRIGGER_ROLE=wrapper", "LM_NEXT_ROLE=name",
					"LM_NAME="+WindowNameScript,
					`LM_NEXT=sh -c "$LM_NAME" luxmux-window rename-2; echo $? >"$LM_STATE/next.rc"; : >"$LM_STATE/next.done"`)
				if _, err := os.Stat(filepath.Join(f.state, "triggered")); err != nil {
					t.Fatal("the rename was not interleaved")
				}
				waitDone(t, f.state)
				if got := f.source(); got[EnvWindow] != "rename-2" || got[EnvStart] != "start" || got[EnvRunID] != "run_example" {
					t.Fatalf("env after interleaved rename: %q", got)
				}
			})
			t.Run("placement during rename", func(t *testing.T) {
				f := newFixture(t, toolDir(t, baseTools, interposers), Profile{Image: "alpine"})
				f.shell = shell
				f.mustWrapper()
				f.mustName("app")
				if got := f.source(); got[EnvStart] != "start" || got[EnvWindow] != "app" {
					t.Fatalf("before: %q", got)
				}
				f.mustName("renamed", "LM_ROLE=name", "LM_TRIGGER_ROLE=name", "LM_NEXT_ROLE=wrapper",
					"LM_WRAP="+StartWrapper,
					`LM_NEXT=sh -c "$LM_WRAP" luxmux-start '' '' /bin/sh -c 'printf shell:%s "$LUXMUX_START"'; echo $? >"$LM_STATE/next.rc"; : >"$LM_STATE/next.done"`)
				if _, err := os.Stat(filepath.Join(f.state, "triggered")); err != nil {
					t.Fatal("the placement was not interleaved")
				}
				if out := waitDone(t, f.state); out != "shell:resume" {
					t.Fatalf("interleaved placement output %q", out)
				}
				if got := f.source(); got[EnvStart] != "resume" || got[EnvWindow] != "renamed" {
					t.Fatalf("env after interleaved placement: %q", got)
				}
			})
		}
	}
}

// TestEnvFileMultiline: values spanning lines, including lines that look
// like the window's export line, survive every rewrite exactly.
func TestEnvFileMultiline(t *testing.T) {
	workdir := "/workspace/path\nexport LUXMUX_WINDOW=part-of-path\nlast"
	for label, shell := range testShells(t) {
		t.Run(label, func(t *testing.T) {
			f := newFixture(t, toolDir(t, baseTools, nil), Profile{Image: "alpine", Workdir: workdir})
			f.shell = shell
			f.mustWrapper()
			if got := f.source(); got[EnvWorkdir] != workdir {
				t.Fatalf("workdir %q", got[EnvWorkdir])
			} else if _, ok := got[EnvWindow]; ok {
				t.Fatalf("window before any name: %q", got[EnvWindow])
			}
			for _, name := range []string{"app", "first\nexport LUXMUX_WORKDIR=x\nsecond 'q' $HOME `false`", "plain"} {
				f.mustName(name)
				if got := f.source(); got[EnvWorkdir] != workdir || got[EnvWindow] != name {
					t.Fatalf("after naming %q: workdir %q window %q", name, got[EnvWorkdir], got[EnvWindow])
				}
			}
			f.mustWrapper()
			if got := f.source(); got[EnvWorkdir] != workdir || got[EnvWindow] != "plain" || got[EnvStart] != "resume" {
				t.Fatalf("after a placement: %q", got)
			}
		})
	}
}

// TestEnvFileReplacedAtomically: a reader holding the env file open keeps
// the complete old file, and the path names a new inode; the lock file
// keeps its inode across writers.
func TestEnvFileReplacedAtomically(t *testing.T) {
	f := newFixture(t, toolDir(t, baseTools, nil), Profile{Image: "alpine"})
	f.mustWrapper()
	f.mustName("old")
	old, err := os.ReadFile(f.path(EnvFile))
	if err != nil {
		t.Fatal(err)
	}
	r, err := os.Open(f.path(EnvFile))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	before, _ := r.Stat()
	lockBefore, err := os.Stat(f.path(LockFile))
	if err != nil {
		t.Fatal(err)
	}
	f.mustName("new")
	f.mustWrapper()
	held, err := readAll(r)
	if err != nil || held != string(old) {
		t.Fatalf("the open file changed: %v\n%q\nwant\n%q", err, held, old)
	}
	after, err := os.Stat(f.path(EnvFile))
	if err != nil || os.SameFile(before, after) {
		t.Fatalf("the env file was rewritten in place: %v", err)
	}
	if lockAfter, err := os.Stat(f.path(LockFile)); err != nil || !os.SameFile(lockBefore, lockAfter) {
		t.Fatalf("the lock file was replaced: %v", err)
	}
	if got := f.source(); got[EnvWindow] != "new" || got[EnvStart] != "resume" {
		t.Fatalf("%q", got)
	}
}

func readAll(r *os.File) (string, error) {
	if _, err := r.Seek(0, 0); err != nil {
		return "", err
	}
	var b strings.Builder
	_, err := bufio.NewReader(r).WriteTo(&b)
	return b.String(), err
}

// holdLock takes the env lock in another process, as a crashed or slow
// writer would hold it; kill releases it the way a crash does.
func holdLock(t *testing.T, f *fixture) (kill func()) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(f.path(LockFile)), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", "-c", `exec 9>>"$1"; flock 9 || exit 1; echo locked; exec sleep 60`, "hold", f.path(LockFile))
	cmd.Env = []string{"PATH=" + toolDir(t, baseTools, nil)}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if line, err := bufio.NewReader(out).ReadString('\n'); err != nil || line != "locked\n" {
		cmd.Process.Kill()
		t.Fatalf("holder: %q %v", line, err)
	}
	done := false
	kill = func() {
		if !done {
			done = true
			cmd.Process.Signal(syscall.SIGKILL)
			cmd.Wait()
		}
	}
	t.Cleanup(kill)
	return kill
}

// TestEnvLockHeldAndCrashed: while another process holds the lock, a
// rename fails within the lock bound and changes nothing, and a placement
// still reaches its shell; once the holder is killed (its lock released by
// the kernel) the next writer proceeds at once and removes temporary files
// a dead writer left. Both util-linux flock and one without -w/-E (BusyBox)
// are exercised.
func TestEnvLockHeldAndCrashed(t *testing.T) {
	for label, fakes := range map[string]map[string]string{
		"flock -w": nil,
		"flock -n only": {"flock": `for a; do case $a in -w|-E) exit 64 ;; esac; done
exec REAL_flock "$@"`},
	} {
		t.Run(label, func(t *testing.T) {
			f := newFixture(t, toolDir(t, baseTools, fakes), Profile{Image: "alpine"})
			// Only these scripts use the shorter test deadline; production stays 2 s.
			shortLock := func(s string) string {
				return strings.ReplaceAll(s, lockFunc, buildLockFunc("0.2", "2"))
			}
			f.cmd[2] = shortLock(f.cmd[2])
			f.mustWrapper()
			f.mustName("first")
			envBefore, _ := os.ReadFile(f.path(EnvFile))
			kill := holdLock(t, f)

			name := func(n string) (time.Duration, error) {
				_, d, err := f.run([]string{"-c", shortLock(WindowNameScript), "luxmux-window", n})
				return d, err
			}
			d, err := name("second")
			if err == nil {
				t.Fatal("a rename succeeded while another process held the lock")
			}
			if d < 150*time.Millisecond || d > 10*time.Second {
				t.Fatalf("rename gave up after %v", d)
			}
			out, d, err := f.wrapper()
			if err != nil || out != "shell:resume" || d > 10*time.Second {
				t.Fatalf("placement under a held lock: %q %v after %v", out, err, d)
			}
			if b, _ := os.ReadFile(f.path(EnvFile)); string(b) != string(envBefore) {
				t.Fatalf("env changed under a held lock:\n%s", b)
			}

			kill()
			stale := f.path(".lm-tmp.env.999999")
			if err := os.WriteFile(stale, []byte("partial"), 0o600); err != nil {
				t.Fatal(err)
			}
			if d, err := f.name("third"); err != nil || d > time.Second*3/2 {
				t.Fatalf("rename after the holder died: %v after %v", err, d)
			}
			if _, err := os.Stat(stale); !os.IsNotExist(err) {
				t.Fatalf("a dead writer's temporary file remains: %v", err)
			}
			// The placement that could not lock published nothing: its facts
			// appear with the next placement, which keeps the latest name.
			if got := f.source(); got[EnvWindow] != "third" || got[EnvStart] != "start" {
				t.Fatalf("%q", got)
			}
			f.mustWrapper()
			if got := f.source(); got[EnvWindow] != "third" || got[EnvStart] != "resume" {
				t.Fatalf("%q", got)
			}
		})
	}
}

// TestEnvWritersMissingTools: without flock nothing is published (no
// unserialized write) yet the shell starts and a rename reports failure;
// on a read-only .luxmux a rename fails and the env file is unchanged.
func TestEnvWritersMissingTools(t *testing.T) {
	t.Run("no flock", func(t *testing.T) {
		f := newFixture(t, toolDir(t, toolsWithout("flock"), nil), Profile{Image: "alpine"})
		if out := f.mustWrapper(); out != "shell:start" {
			t.Fatalf("%q", out)
		}
		if _, err := os.Stat(f.path(EnvFile)); !os.IsNotExist(err) {
			t.Fatalf("env written without a lock: %v", err)
		}
		if _, err := f.name("app"); err == nil {
			t.Fatal("rename without flock reported success")
		}
		if _, err := os.Stat(f.path(WindowFile)); !os.IsNotExist(err) {
			t.Fatalf("window written without a lock: %v", err)
		}
	})
	t.Run("read-only", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root ignores directory permissions")
		}
		f := newFixture(t, toolDir(t, baseTools, nil), Profile{Image: "alpine"})
		f.mustWrapper()
		f.mustName("app")
		before, _ := os.ReadFile(f.path(EnvFile))
		if err := os.Chmod(filepath.Join(f.dir, ".luxmux"), 0o555); err != nil {
			t.Fatal(err)
		}
		defer os.Chmod(filepath.Join(f.dir, ".luxmux"), 0o755)
		if _, err := f.name("other"); err == nil {
			t.Fatal("rename on a read-only volume reported success")
		}
		if out := f.mustWrapper(); out != "shell:resume" {
			t.Fatalf("%q", out)
		}
		if b, _ := os.ReadFile(f.path(EnvFile)); string(b) != string(before) {
			t.Fatalf("env changed on a read-only volume:\n%s", b)
		}
	})
}

// TestWrapperProbesBounded: df, stat and git that never answer (and ignore
// SIGTERM) delay the prompt only by the single probe deadline, and their
// facts are empty. The watchdog does not depend on timeout(1).
func TestWrapperProbesBounded(t *testing.T) {
	hang := `: >"$LM_STATE/${0##*/}.ran"
trap '' TERM
exec sleep 30`
	repo := func(f *fixture) string {
		d := filepath.Join(f.dir, "repo")
		if err := os.MkdirAll(filepath.Join(d, ".git"), 0o755); err != nil {
			t.Fatal(err)
		}
		return EnvRepoDir + "=" + d
	}
	t.Run("hung", func(t *testing.T) {
		f := newFixture(t, toolDir(t, baseTools, map[string]string{"df": hang, "stat": hang, "git": hang}), Profile{Image: "alpine"})
		out, d, err := f.wrapper(repo(f))
		if err != nil || out != "shell:start" {
			t.Fatalf("%q %v", out, err)
		}
		// One 1 s deadline for the group; 4 s leaves room for a loaded
		// host, as in TestWrapperProbeProcesses.
		if d > 4*time.Second {
			t.Fatalf("prompt after %v", d)
		}
		if _, err := os.Stat(filepath.Join(f.state, "df.ran")); err != nil {
			t.Fatalf("df never ran: the deadline was not exercised")
		}
		if _, err := os.Stat(filepath.Join(f.state, "stat.ran")); err == nil {
			t.Fatal("stat ran after df timed out")
		}
		if got := f.source(); got[EnvDisk] != "" || got[EnvBranchCheckedOut] != "" {
			t.Fatalf("%q", got)
		}
	})
	for label, fakes := range map[string]map[string]string{
		"no timeout": {"df": hang, "stat": hang, "git": hang},
		"timeout lacks -k": {"df": hang, "stat": hang, "git": hang, "timeout": `[ "$1" = -k ] && exit 125
exec REAL_timeout "$@"`},
	} {
		t.Run(label, func(t *testing.T) {
			tools := baseTools
			if _, ok := fakes["timeout"]; !ok {
				tools = toolsWithout("timeout")
			}
			f := newFixture(t, toolDir(t, tools, fakes), Profile{Image: "alpine"})
			f.shortProbes("0.3")
			out, d, err := f.wrapper(repo(f))
			if err != nil || out != "shell:start" || d > 3*time.Second {
				t.Fatalf("%q %v after %v", out, err, d)
			}
			if _, err := os.Stat(filepath.Join(f.state, "df.ran")); err != nil {
				t.Fatalf("df never ran: the deadline was not exercised: %v", err)
			}
			if got := f.source(); got[EnvDisk] != "" || got[EnvBranchCheckedOut] != "" {
				t.Fatalf("%q", got)
			}
		})
	}
	t.Run("stat fallback", func(t *testing.T) {
		f := newFixture(t, toolDir(t, baseTools, map[string]string{"df": "exit 1", "stat": `printf '4096 10\n'`}), Profile{Image: "alpine"})
		f.mustWrapper()
		if got := f.source(); got[EnvDisk] != "40960" {
			t.Fatalf("disk %q", got[EnvDisk])
		}
	})
}

func TestWrapperProbeCost(t *testing.T) {
	dash, err := exec.LookPath("dash")
	if err != nil {
		t.Skip("dash unavailable")
	}
	f := newFixture(t, toolDir(t, append(slices.Clone(baseTools), "df", "stat", "git"), nil), Profile{Image: "alpine"})
	f.shell = []string{dash}
	plain, placement := time.Hour, time.Hour
	for i := 0; i < 40; i++ {
		_, d, err := f.run([]string{"-c", ":"})
		if err != nil {
			t.Fatal(err)
		}
		if d < plain {
			plain = d
		}
		_, d, err = f.wrapper()
		if err != nil {
			t.Fatal(err)
		}
		if d < placement {
			placement = d
		}
		if i >= 7 && placement <= 60*time.Millisecond+12*plain {
			break
		}
	}
	// Minimums select an idle sample; the relative allowance handles slow hosts.
	bound := 60*time.Millisecond + 12*plain
	t.Logf("plain shell %v, placement %v, bound %v", plain, placement, bound)
	if placement > bound {
		t.Fatalf("placement overhead: %v > %v", placement, bound)
	}
}

func TestWrapperProbeProcesses(t *testing.T) {
	for label, shell := range testShells(t) {
		for _, hung := range []string{"", "df", "git"} {
			t.Run(label+"/"+hung, func(t *testing.T) {
				fakes := map[string]string{"df": "printf 'Filesystem 1024-blocks Used Available Capacity Mounted on\\nvolume 10 0 10 0%% workspace\\n'", "git": "printf 'main\\n'"}
				if hung != "" {
					fakes[hung] = `printf '%s' "$$" >"$LM_STATE/probe.pid"
trap '' TERM
exec sleep 30`
				}
				ps := realTool(t, "ps")
				f := newFixture(t, toolDir(t, baseTools, fakes), Profile{Image: "alpine", Shell: []string{ps, "-e", "-o", "pid=,pgid="}})
				f.shell = shell
				f.shortProbes("0.3")
				repo := filepath.Join(f.dir, "repo")
				if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o700); err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctx, shell[0], append(slices.Clone(shell[1:]), f.cmd[1:]...)...)
				cmd.Env = append(slices.Clone(f.env), EnvRepoDir+"="+repo)
				cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
				start := time.Now()
				out, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("%v: %s", err, out)
				}
				if time.Since(start) > 4*time.Second {
					t.Fatal("probe deadline exceeded")
				}
				var pids []string
				for _, line := range strings.Split(string(out), "\n") {
					fields := strings.Fields(line)
					if len(fields) == 2 && fields[1] == strconv.Itoa(cmd.Process.Pid) {
						pids = append(pids, fields[0])
					}
				}
				if len(pids) != 1 || pids[0] != strconv.Itoa(cmd.Process.Pid) {
					t.Fatalf("processes at shell exec: %v", pids)
				}
				if hung != "" {
					b, err := os.ReadFile(filepath.Join(f.state, "probe.pid"))
					if err != nil {
						t.Fatal(err)
					}
					pid, err := strconv.Atoi(string(b))
					if err != nil {
						t.Fatal(err)
					}
					if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
						t.Fatalf("probe %d remains: %v", pid, err)
					}
					if got := f.source(); got[EnvDisk] != "" || got[EnvBranchCheckedOut] != "" {
						t.Fatalf("late facts: %v", got)
					}
				}
			})
		}
	}
}

// TestWrapperProbeForks: a df that forks a grandchild (which ignores
// SIGTERM, records its pid and sleeps) and then hangs is cut off with its
// whole process group: after the prompt no grandchild remains.
func TestWrapperProbeForks(t *testing.T) {
	for label, shell := range testShells(t) {
		t.Run(label, func(t *testing.T) {
			fakes := map[string]string{"df": `( trap '' TERM; printf '%s' "$(exec sh -c 'echo $PPID')" >"$LM_STATE/grandchild.pid"; exec sleep 30 ) &
trap '' TERM
while [ ! -s "$LM_STATE/grandchild.pid" ]; do sleep 0.01; done
exec sleep 30`, "stat": "exit 1"}
			f := newFixture(t, toolDir(t, baseTools, fakes), Profile{Image: "alpine"})
			f.shell = shell
			f.shortProbes("0.3")
			out, d, err := f.wrapper()
			if err != nil || out != "shell:start" || d > 4*time.Second {
				t.Fatalf("%q %v after %v", out, err, d)
			}
			b, err := os.ReadFile(filepath.Join(f.state, "grandchild.pid"))
			if err != nil {
				t.Fatalf("the probe never forked: %v", err)
			}
			pid, err := strconv.Atoi(string(b))
			if err != nil {
				t.Fatal(err)
			}
			// SIGKILL is delivered at once, but the exit may lag a moment.
			deadline := time.Now().Add(2 * time.Second)
			for syscall.Kill(pid, 0) == nil && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
				syscall.Kill(pid, syscall.SIGKILL)
				t.Fatalf("grandchild %d remains: %v", pid, err)
			}
		})
	}
}

// TestWrapperProbeDeadlineSweep: df forks a child that ignores SIGTERM.
// When df then SIGKILLs its process group, a kill landing right after a
// fork, no child and no disk fact remain. When df prints and exits, the
// disk fact is exactly its value and the probe's closing group kill still
// reaches the child nothing recorded.
func TestWrapperProbeDeadlineSweep(t *testing.T) {
	fork := `( trap '' TERM; exec sleep 30 ) >/dev/null &
printf '%s' "$!" >"$LM_STATE/child.pid"
`
	for label, df := range map[string]string{"killed": fork + "kill -KILL 0", "done": fork + dfFake} {
		t.Run(label, func(t *testing.T) {
			f := newFixture(t, toolDir(t, baseTools, map[string]string{"df": df, "stat": "exit 1"}), Profile{Image: "alpine"})
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			cmd := commandIn(ctx, f, f.cmd[1:])
			// A probe outside its own group would kill this group, not the test's.
			cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
			start := time.Now()
			out, err := cmd.CombinedOutput()
			if d := time.Since(start); err != nil || string(out) != "shell:start" || d > 4*time.Second {
				t.Fatalf("%q %v after %v", out, err, d)
			}
			waitGone(t, readPid(t, filepath.Join(f.state, "child.pid")))
			want := "10240"
			if label == "killed" {
				want = ""
			}
			if got := f.source(); got[EnvDisk] != want {
				t.Fatalf("disk %q, want %q", got[EnvDisk], want)
			}
		})
	}
}

// TestWrapperSweepsProbeDirs: a placement removes the .luxmux-probe.<pid>
// directories an earlier wrapper killed mid-probe left in TMPDIR or on the
// state volume, and keeps one whose pid is alive, another user's, and any
// other name.
func TestWrapperSweepsProbeDirs(t *testing.T) {
	gone := exec.Command(realTool(t, "true"))
	if err := gone.Run(); err != nil {
		t.Fatal(err)
	}
	dead, live := strconv.Itoa(gone.Process.Pid), strconv.Itoa(os.Getpid())
	for label, shell := range testShells(t) {
		t.Run(label, func(t *testing.T) {
			f := newFixture(t, toolDir(t, baseTools, nil), Profile{Image: "alpine"})
			f.shell = shell
			tmp, state := filepath.Join(f.dir, "tmp"), filepath.Join(f.dir, ".luxmux")
			mk := func(dir, name string) string {
				p := filepath.Join(dir, name)
				if err := os.MkdirAll(p, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(p, "df"), []byte("x"), 0o600); err != nil {
					t.Fatal(err)
				}
				return p
			}
			stale := []string{mk(tmp, ".luxmux-probe."+dead), mk(state, ".luxmux-probe."+dead)}
			kept := []string{mk(tmp, ".luxmux-probe."+live), mk(state, ".luxmux-probe."+live), mk(tmp, ".luxmux-probe.x1"), mk(tmp, "luxmux-probe."+dead)}
			file := filepath.Join(state, ".luxmux-probe.1"+dead)
			if err := os.WriteFile(file, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			f.mustWrapper()
			for _, p := range stale {
				if _, err := os.Stat(p); !os.IsNotExist(err) {
					t.Errorf("stale %s remains: %v", p, err)
				}
			}
			for _, p := range kept {
				if _, err := os.Stat(filepath.Join(p, "df")); err != nil {
					t.Errorf("%s removed: %v", p, err)
				}
			}
			if _, err := os.Stat(file); err != nil {
				t.Errorf("a file named like a probe directory was removed: %v", err)
			}
			// Directories that are not this user's stay: with id reporting
			// another uid, the dead pid's directories are not this user's.
			// BusyBox sh runs id as an applet, past a PATH fake.
			if label != "sh" {
				return
			}
			g := newFixture(t, toolDir(t, baseTools, map[string]string{"id": "echo " + strconv.Itoa(os.Getuid()+1)}), Profile{Image: "alpine"})
			g.shell = shell
			other := mk(filepath.Join(g.dir, "tmp"), ".luxmux-probe."+dead)
			g.mustWrapper()
			if _, err := os.Stat(filepath.Join(other, "df")); err != nil {
				t.Errorf("another user's %s removed: %v", other, err)
			}
		})
	}
}

// TestWrapperProbeKillQuiet: the probe script's own group kill is a valid
// kill(1) in dash, bash and BusyBox sh. Its stderr is discarded in the
// wrapper, so a shell that rejected the syntax would fail silently.
func TestWrapperProbeKillQuiet(t *testing.T) {
	bin := toolDir(t, baseTools, map[string]string{"df": dfFake, "stat": "exit 1"})
	shells := map[string][]string{}
	for _, n := range []string{"dash", "bash"} {
		if p, err := exec.LookPath(n); err == nil {
			shells[n] = []string{p}
		}
	}
	if b, err := exec.LookPath("busybox"); err == nil {
		shells["busybox"] = []string{b, "sh"}
	}
	for label, shell := range shells {
		t.Run(label, func(t *testing.T) {
			cmd := exec.Command(shell[0], append(slices.Clone(shell[1:]), "-c", probeScript, "luxmux-probe", t.TempDir(), "")...)
			cmd.Env = []string{"PATH=" + bin}
			cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
			var stdout, stderr strings.Builder
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			_ = cmd.Run()
			if stdout.String() != "ok:10240::end" || stderr.String() != "" {
				t.Fatalf("stdout %q stderr %q", stdout.String(), stderr.String())
			}
		})
	}
}

// TestWrapperBusyBoxProbeShell: with BusyBox as PATH's sh and setsid, so
// the probe shell itself is BusyBox, a df that ignores SIGTERM is cut off
// at the deadline and leaves no process and no disk fact.
func TestWrapperBusyBoxProbeShell(t *testing.T) {
	bb, err := exec.LookPath("busybox")
	if err != nil {
		t.Skip("busybox unavailable")
	}
	bin := toolDir(t, toolsWithout("sh", "setsid"), map[string]string{"df": `printf '%s' "$$" >"$LM_STATE/df.pid"
trap '' TERM
exec sleep 30`, "stat": "exit 1"})
	for _, n := range []string{"sh", "setsid"} {
		if err := os.Symlink(bb, filepath.Join(bin, n)); err != nil {
			t.Fatal(err)
		}
	}
	f := newFixture(t, bin, Profile{Image: "alpine"})
	f.shell = []string{bb, "sh"}
	f.shortProbes("0.3")
	out, d, err := f.wrapper()
	if err != nil || out != "shell:start" || d > 4*time.Second {
		t.Fatalf("%q %v after %v", out, err, d)
	}
	waitGone(t, readPid(t, filepath.Join(f.state, "df.pid")))
	if got := f.source(); got[EnvDisk] != "" {
		t.Fatalf("disk %q", got[EnvDisk])
	}
}

// TestWrapperRealFacts: LUXMUX_DISK from the real df matches statfs of the
// workspace, and LUXMUX_BRANCH_CHECKED_OUT from the real git is the branch,
// then the short sha once detached.
func TestWrapperRealFacts(t *testing.T) {
	f := newFixture(t, toolDir(t, append(slices.Clone(baseTools), "df", "stat", "git"), nil), Profile{Image: "alpine"})
	want := statfsBytes(t, f.dir)
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command(realTool(t, "git"), append([]string{"-C", filepath.Join(f.dir, "repo")}, args...)...)
		cmd.Env = []string{"PATH=" + f.bin, "HOME=" + f.dir, "GIT_CONFIG_NOSYSTEM=1", "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com"}
		b, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, b)
		}
		return strings.TrimSpace(string(b))
	}
	if err := os.Mkdir(filepath.Join(f.dir, "repo"), 0o755); err != nil {
		t.Fatal(err)
	}
	git("init", "-q", "-b", "feature/x")
	git("commit", "-q", "--allow-empty", "-m", "one")
	repoEnv := EnvRepoDir + "=" + filepath.Join(f.dir, "repo")
	f.mustWrapper(repoEnv, "HOME="+f.dir)
	got := f.source()
	disk, err := strconv.ParseInt(got[EnvDisk], 10, 64)
	// df -Pk reports whole KiB; LUXMUX_DISK is that times 1024.
	if err != nil || disk <= 0 || disk%1024 != 0 || disk-want >= 1024 || want-disk >= 1024 {
		t.Fatalf("disk %q, statfs %d", got[EnvDisk], want)
	}
	if got[EnvBranchCheckedOut] != "feature/x" {
		t.Fatalf("branch %q", got[EnvBranchCheckedOut])
	}
	git("checkout", "-q", "--detach")
	sha := git("rev-parse", "--short", "HEAD")
	f.mustWrapper(repoEnv, "HOME="+f.dir)
	if got := f.source(); got[EnvBranchCheckedOut] != sha || sha == "" {
		t.Fatalf("detached: %q, want %q", got[EnvBranchCheckedOut], sha)
	}
}
