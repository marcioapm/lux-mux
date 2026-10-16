package mux

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/marcioapm/lux/api"

	"github.com/marcioapm/lux-mux/internal/config"
	"github.com/marcioapm/lux-mux/internal/fakelux"
	"github.com/marcioapm/lux-mux/internal/state"
)

const hooksConfig = `default_profile = "app"
[profiles.app]
image = "alpine"
repository = "https://example.com/app.git"
branch = "feature-x"
secrets = ["EXAMPLE_TOKEN"]
on_start = 'printf "START-HOOK %s %s %s\n" "$LUXMUX_START" "$LUXMUX_BRANCH" "$LUXMUX_CPU" | tee -a "$_LUXMUX_TEST_WORKSPACE/hooks.log"'
on_resume = 'printf "RESUME-HOOK %s %s\n" "$LUXMUX_START" "$LUXMUX_CPU" | tee -a "$_LUXMUX_TEST_WORKSPACE/hooks.log"'
shell = ["sh"]
`

const exampleSecret = "example-secret-value-1234"

// shellHarness is a harness whose Runs are real sh processes started from
// their spec's workload command, with a state volume per Run.
func shellHarness(t *testing.T, cfg string) (*harness, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "runs")
	h := newHarnessEnv(t, cfg, map[string]string{"EXAMPLE_TOKEN": exampleSecret}, func(s *fakelux.Server) {
		s.RunShells(dir, "PS1=PROMPT> ", "ENV=", "TERM=xterm")
	})
	return h, dir
}

func writeCgroup(t *testing.T, dir, id, cpu string) {
	t.Helper()
	cg := fakelux.CgroupDir(dir, id)
	if err := os.MkdirAll(cg, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cg, "cpu.max"), []byte(cpu+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// hookRuns is what the hooks of Run id logged, one line per run.
func hookRuns(dir, id string) string {
	b, _ := os.ReadFile(filepath.Join(fakelux.WorkspaceDir(dir, id), "hooks.log"))
	return string(b)
}

func envFile(t *testing.T, dir, id string) string {
	b, _ := os.ReadFile(filepath.Join(fakelux.WorkspaceDir(dir, id), ".luxmux", "env"))
	return string(b)
}

// TestHooksStartMoveReattach: the first start runs on_start only, before
// the prompt; a move (new placement, state volume kept) runs on_resume
// only with recomputed facts; a reattach of the same shell runs neither.
// The window's name reaches the env file, and no secret value does.
func TestHooksStartMoveReattach(t *testing.T) {
	h, dir := shellHarness(t, hooksConfig)
	id := h.live(0)
	h.waitScreen("prompt after on_start", func(s string) bool {
		i, j := strings.Index(s, "START-HOOK start feature-x"), strings.Index(s, "PROMPT>")
		return i >= 0 && j > i
	})
	if strings.Contains(h.term.screen(), "RESUME-HOOK") {
		t.Fatal("on_resume ran on the first start")
	}
	h.waitFor("window name written over exec", func() bool { return len(h.lux.Execs()) > 0 })
	h.waitFor("window name in the env file", func() bool { return strings.Contains(envFile(t, dir, id), "export LUXMUX_WINDOW='app'") })
	f := envFile(t, dir, id)
	for _, want := range []string{"export LUXMUX_START='start'", "export LUXMUX_SESSION='s'", "export LUXMUX_RUN_ID='" + id + "'"} {
		if !strings.Contains(f, want) {
			t.Fatalf("env file lacks %q:\n%s", want, f)
		}
	}

	writeCgroup(t, dir, id, "150000 100000")
	h.lux.Move(id, false)
	h.waitScreen("on_resume in the fresh shell", h.contains("RESUME-HOOK resume 1.5"))
	h.waitFor("env file recomputed", func() bool {
		f := envFile(t, dir, id)
		return strings.Contains(f, "export LUXMUX_START='resume'") && strings.Contains(f, "export LUXMUX_CPU='1.5'")
	})
	h.type_("echo AFTER-$LUXMUX_START\r")
	h.waitScreen("variables in the shell", h.contains("AFTER-resume"))

	// A dropped stream: the same shell, no hook runs again.
	if got := hookRuns(dir, id); got != "START-HOOK start feature-x \nRESUME-HOOK resume 1.5\n" {
		t.Fatalf("hook runs before the reattach: %q", got)
	}
	attaches := len(h.lux.Attaches())
	h.lux.Blip(id)
	h.waitFor("attached again", func() bool { return len(h.lux.Attaches()) > attaches && h.lux.Streams(id) > 0 })
	h.live(0)
	h.type_("echo SAME-SHELL\r")
	h.waitScreen("reattached", h.contains("SAME-SHELL"))
	if got := hookRuns(dir, id); got != "START-HOOK start feature-x \nRESUME-HOOK resume 1.5\n" {
		t.Fatalf("a reattach ran a hook: %q", got)
	}

	h.m.Rename(0, "renamed it's")
	h.waitFor("rename in the env file", func() bool {
		return strings.Contains(envFile(t, dir, id), `export LUXMUX_WINDOW='renamed it'\''s'`)
	})
	// Sourced back exactly: $, a backtick and spaces stay literal; a
	// control character becomes a space, so the value is one line.
	h.m.Rename(0, "a $HOME `id`  b\nc\x1b")
	const want = "a $HOME `id`  b c "
	h.waitFor("odd name sourced back", func() bool {
		cmd := exec.Command("sh", "-c", `. "$1" && printf %s "$LUXMUX_WINDOW"`, "t", filepath.Join(fakelux.WorkspaceDir(dir, id), ".luxmux", "env"))
		b, err := cmd.Output()
		return err == nil && string(b) == want
	})
	if leaks := fakelux.KeyLeaks([]string{exampleSecret}, filepath.Join(fakelux.WorkspaceDir(dir, id), ".luxmux")); len(leaks) > 0 {
		t.Fatalf("secret in %v", leaks)
	}
	for k, v := range h.lux.Run(id).Spec.Env {
		if strings.Contains(v, exampleSecret) || k == "EXAMPLE_TOKEN" {
			t.Fatalf("secret in the RunSpec env: %s", k)
		}
	}
	h.detach()
}

// TestHooksWarmSpare: a spare's Run runs on_start before any window takes
// it; the window that claims it does not run it again, and the window's
// name is written only once it is claimed.
func TestHooksWarmSpare(t *testing.T) {
	h, dir := shellHarness(t, strings.Replace(hooksConfig, "shell = [\"sh\"]", "shell = [\"sh\"]\nwarm_spare = true", 1))
	h.live(0)
	key := config.SpecKey(h.cfg.Profiles["app"])
	var spare string
	h.waitFor("spare", func() bool { spare = h.spares()[key]; return spare != "" })
	h.waitFor("spare's on_start", func() bool {
		f := envFile(t, dir, spare)
		return strings.Contains(f, "export LUXMUX_START='start'")
	})
	if f := envFile(t, dir, spare); strings.Contains(f, "LUXMUX_WINDOW") {
		t.Fatalf("an unclaimed spare names a window:\n%s", f)
	}
	h.type_("\x02c")
	h.waitFor("claimed", func() bool { return h.window(1) == spare })
	h.live(1)
	h.waitScreen("the spare's on_start output", h.contains("START-HOOK start feature-x"))
	h.waitFor("claimed window's name", func() bool { return strings.Contains(envFile(t, dir, spare), "export LUXMUX_WINDOW='app'") })
	h.type_("echo CLAIMED\r")
	h.waitScreen("claimed shell", h.contains("CLAIMED"))
	if got := hookRuns(dir, spare); got != "START-HOOK start feature-x \n" {
		t.Fatalf("the claimed spare's hook runs: %q", got)
	}
	h.detach()
}

// rebuildFromLux detaches h and starts it again on its session rebuilt
// from lux (the session file lost), over the configured profiles.
func rebuildFromLux(h *harness) {
	h.t.Helper()
	h.detach()
	var runs []*api.Run
	for _, id := range h.lux.RunIDs() {
		r := h.lux.Run(id)
		runs = append(runs, &r)
	}
	def := config.DefaultEndpoint(h.lux.Config())
	h.start(state.RebuildFrom("s", []state.Listing{{Endpoint: def, Runs: runs}}, def, h.cfg.Profiles))
}

// TestHooksRebuiltSessionClaimsSpare: in a session rebuilt from lux, a
// window's spec keeps its Run's hooks, so its SpecKey is its profile's:
// r on its ended Run takes the waiting spare, whose on_start already ran
// and does not run again.
func TestHooksRebuiltSessionClaimsSpare(t *testing.T) {
	h, dir := shellHarness(t, strings.Replace(hooksConfig, "shell = [\"sh\"]", "shell = [\"sh\"]\nwarm_spare = true", 1))
	first := h.live(0)
	key := config.SpecKey(h.cfg.Profiles["app"])
	var spare string
	h.waitFor("spare", func() bool { spare = h.spares()[key]; return spare != "" })
	h.waitFor("spare's on_start", func() bool { return hookRuns(dir, spare) != "" })
	rebuildFromLux(h)
	if got := h.spares()[key]; got != spare {
		t.Fatalf("rebuilt spares %v, want %s in %s", h.spares(), spare, key)
	}
	h.live(0)
	h.lux.End(first, "failed", "timeout")
	h.waitFor("dead", func() bool { return h.phase(0) == phaseDead })
	h.type_("r")
	h.waitFor("relaunched", func() bool { return h.window(0) != first && h.window(0) != "" })
	if got := h.window(0); got != spare {
		t.Fatalf("relaunched on %s, want the waiting spare %s", got, spare)
	}
	h.live(0)
	h.waitScreen("the spare's on_start output", h.contains("START-HOOK start feature-x"))
	h.type_("echo CLAIMED\r")
	h.waitScreen("claimed shell", h.contains("CLAIMED"))
	if got := hookRuns(dir, spare); got != "START-HOOK start feature-x \n" {
		t.Fatalf("the claimed spare's hook runs: %q", got)
	}
	h.detach()
}

// TestHooksRebuiltSessionRelaunch: r on an ended window of a session
// rebuilt from lux starts a Run with the same hooks: its on_start runs
// before the prompt.
func TestHooksRebuiltSessionRelaunch(t *testing.T) {
	h, dir := shellHarness(t, hooksConfig)
	first := h.live(0)
	h.waitScreen("first on_start", h.contains("START-HOOK start feature-x"))
	rebuildFromLux(h)
	h.live(0)
	h.lux.End(first, "failed", "timeout")
	h.waitFor("dead", func() bool { return h.phase(0) == phaseDead })
	h.type_("r")
	var id string
	h.waitFor("relaunched", func() bool { id = h.window(0); return id != first && id != "" })
	h.live(0)
	h.waitFor("the relaunched Run's on_start", func() bool { return hookRuns(dir, id) == "START-HOOK start feature-x \n" })
	h.type_("echo RELAUNCHED-$LUXMUX_START\r")
	h.waitScreen("relaunched shell", h.contains("RELAUNCHED-start"))
	if got, want := h.lux.Run(id).Spec.Workload.Command, h.lux.Run(first).Spec.Workload.Command; !slices.Equal(got, want) {
		t.Fatalf("relaunched command %q, want %q", got, want)
	}
	h.detach()
}

// TestRenamedSessionKeepsLabel: windows of a renamed session are still
// made under its label, which LUXMUX_SESSION names.
func TestRenamedSessionKeepsLabel(t *testing.T) {
	h, _ := shellHarness(t, hooksConfig)
	h.live(0)
	if err := h.m.RenameSession("s", "renamed"); err != nil {
		t.Fatal(err)
	}
	h.type_("\x02c")
	id := h.live(1)
	if got := h.lux.Run(id).Spec.Env[config.EnvSession]; got != "s" {
		t.Fatalf("LUXMUX_SESSION %q, want the label s", got)
	}
	h.type_("echo SESSION-$LUXMUX_SESSION\r")
	h.waitScreen("label in the shell", h.contains("SESSION-s"))
	h.detach()
}
