package cli

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/marcioapm/lux/api"

	"github.com/marcioapm/lux-mux/internal/config"
	"github.com/marcioapm/lux-mux/internal/fakelux"
	"github.com/marcioapm/lux-mux/internal/state"
)

// TestWindowFlagsToRunSpec: new's and new-window's flags, applied to a
// profile, give the RunSpec they say.
func TestWindowFlagsToRunSpec(t *testing.T) {
	base := config.Profile{Image: "alpine", Pool: "m", Size: "m-2", Repository: "https://example.com/base.git", Branch: "main",
		GitCredential: "TOKEN", Secrets: []string{"BASE"}}
	repo := func(name, url, ref, cred string) api.Repository {
		return api.Repository{Name: name, URL: url, Ref: ref, Credential: cred}
	}
	for _, c := range []struct {
		name  string
		args  []string
		check func(t *testing.T, sp api.RunSpec)
	}{
		{"none: the profile", nil, func(t *testing.T, sp api.RunSpec) {
			want := []api.Repository{repo("base", "https://example.com/base.git", "main", "TOKEN")}
			if sp.Image.Ref != "alpine" || sp.Resources.Size != "m-2" || !reflect.DeepEqual(sp.Git.Repositories, want) {
				t.Fatalf("%+v", sp)
			}
		}},
		{"two repos replace the profile's", []string{"--repo", "https://example.com/api.git#dev", "--repo", "https://example.com/web.git"}, func(t *testing.T, sp api.RunSpec) {
			want := []api.Repository{repo("api", "https://example.com/api.git", "dev", "TOKEN"), repo("web", "https://example.com/web.git", "", "TOKEN")}
			if !reflect.DeepEqual(sp.Git.Repositories, want) || sp.Workload.Workdir != "/workspace/repos/api" {
				t.Fatalf("%+v workdir %q", sp.Git.Repositories, sp.Workload.Workdir)
			}
		}},
		{"--branch on the profile's repository", []string{"--branch", "feature"}, func(t *testing.T, sp api.RunSpec) {
			if sp.Git.Repositories[0].Ref != "feature" || sp.Git.Repositories[0].URL != "https://example.com/base.git" {
				t.Fatalf("%+v", sp.Git.Repositories)
			}
		}},
		{"--ref on one --repo", []string{"--repo", "https://example.com/api.git", "--ref", "v1.0"}, func(t *testing.T, sp api.RunSpec) {
			if len(sp.Git.Repositories) != 1 || sp.Git.Repositories[0].Ref != "v1.0" {
				t.Fatalf("%+v", sp.Git.Repositories)
			}
		}},
		{"image pool size", []string{"--image", "debian:12", "--pool", "gpu", "--size", "g-1"}, func(t *testing.T, sp api.RunSpec) {
			if sp.Image.Ref != "debian:12" || sp.Placement.Pool != "gpu" || sp.Resources.Size != "g-1" {
				t.Fatalf("%+v", sp)
			}
		}},
		{"cpus memory disk replace the size", []string{"--pool", "plain", "--cpus", "3", "--memory", "6Gi", "--disk", "20Gi"}, func(t *testing.T, sp api.RunSpec) {
			if sp.Resources.Size != "" || sp.Resources.CPUs != 3 || sp.Resources.Memory != 6<<30 || sp.Resources.Disk != 20<<30 {
				t.Fatalf("%+v", sp.Resources)
			}
		}},
		{"secrets add to the profile's", []string{"--secret", "EXTRA", "--secret", "BASE"}, func(t *testing.T, sp api.RunSpec) {
			want := []api.Secret{{Name: "BASE", As: "env"}, {Name: "EXTRA", As: "env"}, {Name: "TOKEN"}}
			if !reflect.DeepEqual(sp.Secrets, want) {
				t.Fatalf("%+v", sp.Secrets)
			}
		}},
		{"workdir", []string{"--workdir", "/workspace"}, func(t *testing.T, sp api.RunSpec) {
			if sp.Workload.Workdir != "/workspace" {
				t.Fatal(sp.Workload.Workdir)
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			o, err := parseWindowFlags("new", c.args, io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			p, err := o.apply(base)
			if err != nil {
				t.Fatal(err)
			}
			sp, err := config.RunSpec(p, config.Target{Session: "s", Profile: "p"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			c.check(t, sp)
		})
	}
}

// TestWindowFlagsRefused: contradictory or impossible flags are errors
// naming the problem.
func TestWindowFlagsRefused(t *testing.T) {
	two := config.Profile{Image: "alpine", Repositories: []config.Repository{{URL: "https://example.com/a.git"}, {URL: "https://example.com/b.git"}}}
	none := config.Profile{Image: "alpine"}
	for _, c := range []struct {
		name string
		base config.Profile
		args []string
		want string
	}{
		{"branch and ref", none, []string{"--repo", "https://example.com/a.git", "--branch", "x", "--ref", "y"}, "give one"},
		{"branch with two repos", none, []string{"--repo", "https://example.com/a.git", "--repo", "https://example.com/b.git", "--branch", "x"}, "exactly one repository"},
		{"branch on a two-repo profile", two, []string{"--branch", "x"}, "this window has 2"},
		{"branch without a repo", none, []string{"--branch", "x"}, "this window has 0"},
		{"branch twice", none, []string{"--repo", "https://example.com/a.git#x", "--branch", "y"}, "not both"},
		{"size and cpus", none, []string{"--size", "m-1", "--cpus", "2"}, "--size cannot be combined"},
		{"cpus zero", none, []string{"--cpus", "0"}, "--cpus must be positive"},
		{"no url", none, []string{"--repo", "#main"}, "no URL"},
		{"bad memory", none, []string{"--memory", "lots"}, "memory"},
		{"relative workdir", none, []string{"--workdir", "src"}, "workdir must be absolute"},
		{"bad secret", none, []string{"--secret", "not-a-name"}, "invalid name"},
		{"empty image", none, []string{"--image", ""}, "image is required"},
	} {
		t.Run(c.name, func(t *testing.T) {
			o, err := parseWindowFlags("new", c.args, io.Discard)
			if err == nil {
				_, err = o.apply(c.base)
			}
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("%v, want %q", err, c.want)
			}
		})
	}
	for _, args := range [][]string{{"--nope"}, {"stray"}, {"-n", strings.Repeat("x", 65)}} {
		if _, err := parseWindowFlags("new-window", args, io.Discard); err == nil {
			t.Errorf("%q accepted", args)
		}
	}
	if _, err := parseWindowFlags("new", []string{"-n", "x"}, io.Discard); err == nil {
		t.Error("new took new-window's -n")
	}
}

// TestWindowProfileChoice: -p, else the session's default, else the
// configuration's default_profile.
func TestWindowProfileChoice(t *testing.T) {
	cfg, err := config.Parse([]byte("default_profile = \"a\"\n[profiles.a]\nimage = \"img-a\"\n[profiles.b]\nimage = \"img-b\"\n[profiles.c]\nimage = \"img-c\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ flag, session, want string }{
		{"", "", "a"}, {"", "b", "b"}, {"c", "b", "c"}, {"c", "", "c"},
	} {
		name, p, err := windowProfile(cfg, c.session, windowFlags{Profile: c.flag})
		if err != nil || name != c.want || p.Image != "img-"+c.want {
			t.Errorf("-p %q session %q: %q %q %v", c.flag, c.session, name, p.Image, err)
		}
	}
	if _, _, err := windowProfile(cfg, "", windowFlags{Profile: "zz"}); err == nil || !strings.Contains(err.Error(), `no profile named "zz"`) {
		t.Fatal(err)
	}
}

func writeConfig(t *testing.T, env Env, src string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(env.ConfigPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(env.ConfigPath, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
}

const twoProfiles = "default_profile = \"a\"\n[profiles.a]\nimage = \"img-a\"\n[profiles.b]\nimage = \"img-b\"\n"

// TestNewWindowDetached: with no luxmux attached, new-window submits the
// Run (labelled for adoption, with its name), takes the session's lock
// meanwhile and records the window, with the session's default profile.
func TestNewWindowDetached(t *testing.T) {
	lux := fakelux.New()
	defer lux.Close()
	env, out, errb := testEnv(t, lux, "")
	writeConfig(t, env, twoProfiles)
	first := submit(t, lux, "work", "b", false)
	if err := state.Save(env.StateDir, &state.Session{Name: "work", DefaultProfile: "b", Windows: []state.Window{{RunID: first, Name: "b", Profile: "b"}}}); err != nil {
		t.Fatal(err)
	}
	if code := Main(env, []string{"new-window", "-t", "work", "-n", "logs", "--repo", "https://example.com/api.git#dev"}); code != 0 {
		t.Fatal(errb.String())
	}
	subs := lux.Submits()
	sp := subs[len(subs)-1]
	if sp.Image.Ref != "img-b" || sp.Labels[config.LabelProfile] != "b" || sp.Labels[config.LabelExternal] != "true" ||
		sp.Labels[config.LabelWindow] != "logs" || sp.Labels[config.LabelSession] != "work" || sp.Git.Repositories[0].Ref != "dev" {
		t.Fatalf("submitted %+v", sp)
	}
	id := strings.TrimSpace(out.String())
	s, ok, err := state.Load(env.StateDir, "work")
	if err != nil || !ok || len(s.Windows) != 2 || s.Windows[1].RunID != id || s.Windows[1].Name != "logs" || s.Windows[1].Profile != "b" ||
		s.Windows[1].Spec == nil || s.Windows[1].Spec.Repositories[0].URL != "https://example.com/api.git" || s.DefaultProfile != "b" {
		t.Fatalf("state %+v %v", s, err)
	}
	// The lock was given back.
	unlock, err := state.Lock(env.StateDir, "work")
	if err != nil {
		t.Fatalf("lock kept: %v", err)
	}
	unlock()
}

// TestNewWindowAttached: while another process holds the session's lock,
// new-window submits the Run and leaves the file to that process.
func TestNewWindowAttached(t *testing.T) {
	lux := fakelux.New()
	defer lux.Close()
	env, _, errb := testEnv(t, lux, "")
	writeConfig(t, env, twoProfiles)
	first := submit(t, lux, "work", "a", false)
	sess := &state.Session{Name: "work", Windows: []state.Window{{RunID: first, Name: "a", Profile: "a"}}}
	if err := state.Save(env.StateDir, sess); err != nil {
		t.Fatal(err)
	}
	unlock, err := state.Lock(env.StateDir, "work")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if code := Main(env, []string{"new-window", "-t", "work", "-p", "b"}); code != 0 {
		t.Fatal(errb.String())
	}
	if n := len(lux.Submits()); n != 2 || lux.Submits()[1].Labels[config.LabelExternal] != "true" {
		t.Fatalf("submits %d", n)
	}
	s, _, _ := state.Load(env.StateDir, "work")
	if !reflect.DeepEqual(s, sess) {
		t.Fatalf("file written while attached elsewhere: %+v", s)
	}
}

// TestNewWindowRefusals: lux's refusal is printed and nothing is
// submitted; an unknown session or profile is an error too.
func TestNewWindowRefusals(t *testing.T) {
	lux := fakelux.New()
	defer lux.Close()
	env, _, errb := testEnv(t, lux, "")
	writeConfig(t, env, twoProfiles)
	if code := Main(env, []string{"new-window", "-t", "nosuch"}); code == 0 || !strings.Contains(errb.String(), "no session nosuch") {
		t.Fatalf("unknown session: %s", errb.String())
	}
	submit(t, lux, "work", "a", false)
	errb.Reset()
	if code := Main(env, []string{"new-window", "-t", "work", "-p", "zz"}); code == 0 || !strings.Contains(errb.String(), `no profile named "zz"`) {
		t.Fatalf("unknown profile: %s", errb.String())
	}
	lux.Refuse = "image.ref: no such image"
	errb.Reset()
	if code := Main(env, []string{"new-window", "-t", "work", "--image", "nope"}); code == 0 ||
		!strings.Contains(errb.String(), "lux refuses it: image.ref: no such image (invalid_spec)") {
		t.Fatalf("refusal: %d %s", code, errb.String())
	}
	errb.Reset()
	if code := Main(env, []string{"new", "-s", "fresh", "--image", "nope"}); code == 0 || !strings.Contains(errb.String(), "lux refuses it") {
		t.Fatalf("new refusal: %s", errb.String())
	}
	if n := len(lux.Submits()); n != 1 {
		t.Fatalf("%d submits after refusals", n)
	}
	if _, ok, _ := state.Load(env.StateDir, "work"); ok {
		t.Fatal("a refused window was recorded")
	}
}

// TestNewWindowWaitsForCapacity: a spec lux accepts but cannot start now
// is submitted, with a warning.
func TestNewWindowWaitsForCapacity(t *testing.T) {
	lux := fakelux.New()
	defer lux.Close()
	env, _, errb := testEnv(t, lux, "")
	writeConfig(t, env, twoProfiles)
	submit(t, lux, "work", "a", false)
	if code := Main(env, []string{"new-window", "-t", "work", "--pool", "busy"}); code != 0 {
		t.Fatal(errb.String())
	}
	if !strings.Contains(errb.String(), "would not start it now") || len(lux.Submits()) != 2 {
		t.Fatalf("%s, %d submits", errb.String(), len(lux.Submits()))
	}
}

// TestAttachRecordsExternalWindows: a Run new-window made that the file
// lacks (its record was lost to a process that detached after reading
// the file) becomes a window on attach; Rebuild names it from its label.
func TestAttachRecordsExternalWindows(t *testing.T) {
	lux := fakelux.New()
	defer lux.Close()
	env, _, _ := testEnv(t, lux, "")
	cfg, _ := config.Parse([]byte(twoProfiles))
	first := submit(t, lux, "work", "a", false)
	sp, err := config.RunSpec(config.Profile{Image: "img-b", Repositories: []config.Repository{{URL: "https://example.com/x.git", Branch: "dev"}}},
		config.Target{Session: "work", Profile: "b"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	sp.Labels[config.LabelExternal], sp.Labels[config.LabelWindow] = "true", "ext"
	c, _ := client(Env{Lux: lux.Config})
	ext, err := c.Submit(context.Background(), sp)
	if err != nil {
		t.Fatal(err)
	}
	runs, _ := c.ListSession(context.Background(), "work")
	s := &state.Session{Name: "work", Windows: []state.Window{{RunID: first, Name: "a", Profile: "a"}}}
	recordExternal(s, runs, cfg)
	recordExternal(s, runs, cfg)
	if len(s.Windows) != 2 || s.Windows[1].RunID != ext.ID || s.Windows[1].Name != "ext" || s.Windows[1].Profile != "b" {
		t.Fatalf("windows %+v", s.Windows)
	}
	if r := s.Windows[1].Spec.Repositories; len(r) != 1 || r[0].URL != "https://example.com/x.git" || r[0].Ref != "dev" {
		t.Fatalf("spec %+v", s.Windows[1].Spec)
	}
	_ = env
}
