package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marcioapm/lux/api"

	"github.com/marcioapm/lux-mux/internal/config"
	"github.com/marcioapm/lux-mux/internal/fakelux"
	"github.com/marcioapm/lux-mux/internal/luxapi"
	"github.com/marcioapm/lux-mux/internal/state"
)

func testEnv(t *testing.T, lux *fakelux.Server, stdin string) (Env, *bytes.Buffer, *bytes.Buffer) {
	var out, errb bytes.Buffer
	dir := t.TempDir()
	return Env{Stdin: strings.NewReader(stdin), Stdout: &out, Stderr: &errb,
		Lux:        lux.Config,
		ConfigPath: filepath.Join(dir, "cfg", "config.toml"), StateDir: filepath.Join(dir, "state"),
		Lookup: func(string) (string, bool) { return "", false }}, &out, &errb
}

func submit(t *testing.T, lux *fakelux.Server, session, profile string, spare bool) string {
	t.Helper()
	c, _ := luxapi.New(lux.Config())
	sp, err := config.RunSpec(config.Profile{Image: "alpine"}, config.Target{Session: session, Profile: profile, Spare: spare}, nil)
	if err != nil {
		t.Fatal(err)
	}
	r, err := c.Submit(context.Background(), sp)
	if err != nil {
		t.Fatal(err)
	}
	return r.ID
}

func TestConfigInit(t *testing.T) {
	lux := fakelux.New()
	defer lux.Close()
	env, out, _ := testEnv(t, lux, "")
	if code := Main(env, []string{"config", "init"}); code != 0 {
		t.Fatal(code)
	}
	if !strings.Contains(out.String(), "wrote") {
		t.Fatal(out.String())
	}
	c, err := config.Load(env.ConfigPath)
	if err != nil || c.DefaultProfile != "default" {
		t.Fatalf("written config: %v %+v", err, c)
	}
	if code := Main(env, []string{"config", "init"}); code == 0 {
		t.Fatal("overwrote without --force")
	}
	if code := Main(env, []string{"config", "init", "--force"}); code != 0 {
		t.Fatal("--force")
	}
}

func TestLsAndKillSession(t *testing.T) {
	lux := fakelux.New()
	defer lux.Close()
	a := submit(t, lux, "work", "default", false)
	b := submit(t, lux, "work", "default", false)
	sp := submit(t, lux, "work", "default", true)
	other := submit(t, lux, "play", "default", false)

	env, out, errb := testEnv(t, lux, "n\n")
	if code := Main(env, []string{"ls"}); code != 0 {
		t.Fatal(errb.String())
	}
	if !strings.Contains(out.String(), "work") || !strings.Contains(out.String(), "play") || !strings.Contains(out.String(), "rebuilds") {
		t.Fatalf("ls:\n%s", out.String())
	}

	if code := Main(env, []string{"kill-session", "-t", "work"}); code == 0 {
		t.Fatal("killed without confirmation")
	}
	if lux.Run(a).State == "terminated" {
		t.Fatal("answered n, still terminated")
	}
	_ = state.Save(env.StateDir, &state.Session{Name: "work", Windows: []state.Window{{RunID: a}}})
	env.Stdin = strings.NewReader("y\n")
	if code := Main(env, []string{"kill-session", "-t", "work"}); code != 0 {
		t.Fatal(errb.String())
	}
	for _, id := range []string{a, b, sp} {
		if lux.Run(id).State != "terminated" {
			t.Errorf("%s not terminated", id)
		}
	}
	if lux.Run(other).State == "terminated" {
		t.Fatal("another session's Run terminated")
	}
	if _, ok, _ := state.Load(env.StateDir, "work"); ok {
		t.Fatal("session file kept")
	}
	if code := Main(env, []string{"kill-session", "-t", "play", "-y"}); code != 0 || lux.Run(other).State != "terminated" {
		t.Fatal("-y")
	}
	if code := Main(env, []string{"kill-session", "-t", "nope", "-y"}); code == 0 {
		t.Fatal("unknown session")
	}
}

func TestSparesRm(t *testing.T) {
	lux := fakelux.New()
	defer lux.Close()
	claimed := submit(t, lux, "work", "app", true)
	waiting := submit(t, lux, "work", "app", true)
	win := submit(t, lux, "work", "app", false)
	env, out, _ := testEnv(t, lux, "")
	_ = state.Save(env.StateDir, &state.Session{Name: "work", Windows: []state.Window{{RunID: claimed}, {RunID: win}},
		Claimed: []string{claimed}, Spares: map[string]string{"app": waiting}})
	if code := Main(env, []string{"spares"}); code != 0 || !strings.Contains(out.String(), waiting) || strings.Contains(out.String(), claimed) {
		t.Fatalf("spares:\n%s", out.String())
	}
	if code := Main(env, []string{"spares", "--rm"}); code != 0 {
		t.Fatal(code)
	}
	if lux.Run(waiting).State != "terminated" || lux.Run(claimed).State == "terminated" || lux.Run(win).State == "terminated" {
		t.Fatal("spares --rm must terminate the waiting spare only")
	}
	s, _, _ := state.Load(env.StateDir, "work")
	if len(s.Spares) != 0 {
		t.Fatalf("spare kept in the file: %v", s.Spares)
	}
}

func TestNotSignedIn(t *testing.T) {
	lux := fakelux.New()
	defer lux.Close()
	env, _, errb := testEnv(t, lux, "")
	env.Lux = func() api.Config { return api.Config{} }
	if code := Main(env, []string{"ls"}); code == 0 || !strings.Contains(errb.String(), "not signed in") {
		t.Fatal(errb.String())
	}
	if code := Main(env, []string{"attach", "-t", "../x"}); code == 0 {
		t.Fatal("bad name accepted")
	}
	_ = os.Remove(env.ConfigPath)
}

// TestAttachRefusedWhileAttached: a session attached by another process
// (its lock held) is not attached a second time.
func TestAttachRefusedWhileAttached(t *testing.T) {
	lux := fakelux.New()
	defer lux.Close()
	submit(t, lux, "work", "app", false)
	env, _, errb := testEnv(t, lux, "")
	unlock, err := state.Lock(env.StateDir, "work")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	for _, args := range [][]string{{"attach", "-t", "work"}, {"new", "-s", "work"}} {
		errb.Reset()
		if code := Main(env, args); code == 0 || !strings.Contains(errb.String(), "session work: session is attached elsewhere (pid ") {
			t.Fatalf("%v: %d %q", args, code, errb.String())
		}
	}
}
