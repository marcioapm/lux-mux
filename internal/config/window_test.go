package config

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
}

func TestStartWrapper(t *testing.T) {
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
					p := Profile{Image: "alpine", OnStart: `printf 'hook:%s:%s\n' "$LUXMUX_START" "$EXAMPLE_SECRET"; exit 7`, OnResume: `printf 'hook:%s\n' "$LUXMUX_START"`, Shell: []string{"sh", "-c", `printf 'shell:%s:%s:%s\n' "$LUXMUX_START" "$LUXMUX_CPU" "$LUXMUX_MEMORY"`}}
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
