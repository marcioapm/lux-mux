package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/marcioapm/lux-mux/internal/config"
	"github.com/marcioapm/lux-mux/internal/fakelux"
	"github.com/marcioapm/lux-mux/internal/luxapi"
	"github.com/marcioapm/lux-mux/internal/state"
)

// submitNamed submits a window's Run of session, named name.
func submitNamed(t *testing.T, lux *fakelux.Server, session, profile, name string, spare bool) string {
	t.Helper()
	c, _ := luxapi.New(lux.Config())
	sp, err := config.RunSpec(config.Profile{Image: "alpine"}, config.Target{Session: session, Profile: profile, Name: name, Spare: spare}, nil)
	if err != nil {
		t.Fatal(err)
	}
	r, err := c.Submit(context.Background(), sp)
	if err != nil {
		t.Fatal(err)
	}
	return r.ID
}

// TestNewWindowRunName: new-window names its Run <session>/<window>: -n's
// name, else the profile's; for a renamed session, its name, not its
// Runs' label.
func TestNewWindowRunName(t *testing.T) {
	lux := fakelux.New()
	defer lux.Close()
	env, _, errb := testEnv(t, lux, "")
	writeConfig(t, env, twoProfiles)
	first := submit(t, lux, "work", "a", false)
	if err := state.Save(env.StateDir, &state.Session{Name: "job", Windows: []state.Window{{RunID: first, Name: "a", Profile: "a"}}}); err != nil {
		t.Fatal(err)
	}
	if err := state.SetRenamed(env.StateDir, "work", "job", "work"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"new-window", "-t", "job", "-n", "logs/tail"}, "job/logs/tail"},
		{[]string{"new-window", "-t", "job", "-p", "b"}, "job/b"},
	} {
		if code := Main(env, tc.args); code != 0 {
			t.Fatal(errb.String())
		}
		subs := lux.Submits()
		if sp := subs[len(subs)-1]; sp.Name != tc.want || sp.Labels[config.LabelSession] != "work" {
			t.Fatalf("%v: submitted name %q label %q, want %q", tc.args, sp.Name, sp.Labels[config.LabelSession], tc.want)
		}
	}
}

// TestSparesListsName: luxmux spares shows each waiting spare's Run name.
func TestSparesListsName(t *testing.T) {
	lux := fakelux.New()
	defer lux.Close()
	waiting := submitNamed(t, lux, "work", "app", config.RunName("work", config.SpareWindow), true)
	env, out, errb := testEnv(t, lux, "")
	if code := Main(env, []string{"spares"}); code != 0 {
		t.Fatal(errb.String())
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 || strings.Join(strings.Fields(lines[0]), " ") != "RUN NAME SESSION PROFILE STATE LUX" {
		t.Fatalf("spares:\n%s", out.String())
	}
	if f := strings.Fields(lines[1]); len(f) < 5 || f[0] != waiting || f[1] != "work/spare" || f[2] != "work" || f[3] != "app" {
		t.Fatalf("spare row %q", lines[1])
	}
}

// TestInfoRunName: info and info --json give the window's Run name as lux
// has it.
func TestInfoRunName(t *testing.T) {
	lux := fakelux.New()
	defer lux.Close()
	id := submitNamed(t, lux, "work", "a", "work/api", false)
	env, out, errb := testEnv(t, lux, "")
	if err := state.Save(env.StateDir, &state.Session{Name: "work", Windows: []state.Window{{RunID: id, Name: "api", Profile: "a"}}}); err != nil {
		t.Fatal(err)
	}
	if code := Main(env, []string{"info", "--json", "-t", "work"}); code != 0 {
		t.Fatal(errb.String())
	}
	o := decodeInfo(t, out.Bytes())
	if len(o.Windows) != 1 || o.Windows[0].Run == nil || o.Windows[0].Run.Name != "work/api" {
		t.Fatalf("%+v", o.Windows)
	}
	out.Reset()
	if code := Main(env, []string{"info", "-t", "work"}); code != 0 {
		t.Fatal(errb.String())
	}
	found := false
	for _, l := range strings.Split(out.String(), "\n") {
		if f := strings.Fields(l); len(f) == 2 && f[0] == "name" && f[1] == "work/api" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no name line:\n%s", out.String())
	}
}
