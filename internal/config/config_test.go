package config

import (
	"encoding/json"
	"reflect"
	"regexp"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/marcioapm/lux/api"
)

// TestRepositoriesMapping: both forms map to git.repositories in order,
// with each repository's ref, name, path and credential (its own, else
// the profile's), the shell starting in the first checkout.
func TestRepositoriesMapping(t *testing.T) {
	for _, c := range []struct {
		name, src string
		repos     []api.Repository
		workdir   string
		secrets   []api.Secret
	}{
		{"shorthand", `repository = "https://example.com/team/API.git"
branch = "main"
git_credential = "TOKEN"`,
			[]api.Repository{{Name: "api", URL: "https://example.com/team/API.git", Ref: "main", Credential: "TOKEN"}},
			"/workspace/repos/api", []api.Secret{{Name: "TOKEN"}}},
		{"shorthand ref", `repository = "https://example.com/api.git"
ref = "v1.2"`,
			[]api.Repository{{Name: "api", URL: "https://example.com/api.git", Ref: "v1.2"}},
			"/workspace/repos/api", nil},
		{"list", `git_credential = "TOKEN"
secrets = ["NPM"]
repositories = [
  { url = "https://example.com/api.git", branch = "main" },
  { url = "https://example.com/web.git", ref = "v2", name = "client", path = "/workspace/client", git_credential = "WEB_TOKEN" },
  { url = "https://example.com/lib.git" },
]`,
			[]api.Repository{
				{Name: "api", URL: "https://example.com/api.git", Ref: "main", Credential: "TOKEN"},
				{Name: "client", URL: "https://example.com/web.git", Ref: "v2", Path: "/workspace/client", Credential: "WEB_TOKEN"},
				{Name: "lib", URL: "https://example.com/lib.git", Credential: "TOKEN"},
			},
			"/workspace/repos/api", []api.Secret{{Name: "NPM", As: "env"}, {Name: "TOKEN"}, {Name: "WEB_TOKEN"}}},
		{"list first has a path", `repositories = [{ url = "https://example.com/api.git", path = "/workspace/src/api" }, { url = "https://example.com/web.git" }]`,
			[]api.Repository{{Name: "api", URL: "https://example.com/api.git", Path: "/workspace/src/api"}, {Name: "web", URL: "https://example.com/web.git"}},
			"/workspace/src/api", nil},
		{"workdir set", `workdir = "/workspace"
repositories = [{ url = "https://example.com/api.git" }]`,
			[]api.Repository{{Name: "api", URL: "https://example.com/api.git"}}, "/workspace", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg, err := Parse([]byte("[profiles.p]\nimage = \"alpine\"\n" + c.src))
			if err != nil {
				t.Fatal(err)
			}
			sp, err := RunSpec(cfg.Profiles["p"], Target{Session: "s", Profile: "p"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if sp.Git == nil || !reflect.DeepEqual(sp.Git.Repositories, c.repos) {
				t.Fatalf("repositories %+v\nwant %+v", sp.Git, c.repos)
			}
			if sp.Workload.Workdir != c.workdir || !reflect.DeepEqual(sp.Secrets, c.secrets) {
				t.Fatalf("workdir %q secrets %+v", sp.Workload.Workdir, sp.Secrets)
			}
		})
	}
}

// TestRepositoriesInvalid: both forms together, and what lux would refuse
// in a list, are configuration errors that say what to change.
func TestRepositoriesInvalid(t *testing.T) {
	for name, c := range map[string]struct{ src, want string }{
		"both forms": {`repository = "https://example.com/a.git"
repositories = [{ url = "https://example.com/b.git" }]`, "set either repositories or the repository/branch/ref shorthand, not both"},
		"list and branch": {`branch = "main"
repositories = [{ url = "https://example.com/b.git" }]`, "not both"},
		"no url":         {`repositories = [{ branch = "main" }]`, "repositories[0]: url is required"},
		"duplicate name": {`repositories = [{ url = "https://example.com/a/app.git" }, { url = "https://example.com/b/app.git" }]`, `repositories[1]: name "app" is taken`},
		"bad name":       {`repositories = [{ url = "https://example.com/a.git", name = "Bad Name" }]`, `repositories[0]: name "Bad Name"`},
		"path outside":   {`repositories = [{ url = "https://example.com/a.git", path = "/tmp/a" }]`, "repositories[0]: path must be under /workspace"},
		"path escapes":   {`repositories = [{ url = "https://example.com/a.git", path = "/workspace/../etc" }]`, "path must be under /workspace"},
		"branch and ref": {`repositories = [{ url = "https://example.com/a.git", branch = "a", ref = "b" }]`, "repositories[0]: branch and ref"},
		"credential":     {`repositories = [{ url = "https://example.com/a.git", git_credential = "no-dash" }]`, `invalid name "no-dash"`},
		"unknown key":    {`repositories = [{ url = "https://example.com/a.git", push = false }]`, "push"},
	} {
		_, err := Parse([]byte("[profiles.p]\nimage = \"alpine\"\n" + c.src))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
	}
}

func TestParseDefaults(t *testing.T) {
	c, err := Parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.Prefix != "C-b" || c.DefaultProfile != "default" || c.Profiles["default"].Image != DefaultImage {
		t.Fatalf("defaults: %+v", c)
	}
	if len(c.Keys.Direct) != 0 {
		t.Fatalf("direct bindings must be off by default: %v", c.Keys.Direct)
	}
}

func TestImageForVersion(t *testing.T) {
	for version, want := range map[string]string{
		"v0.2.0":                         "ghcr.io/marcioapm/luxmux:0.2.0",
		"v1.10.3":                        "ghcr.io/marcioapm/luxmux:1.10.3",
		"(devel)":                        "ghcr.io/marcioapm/luxmux:latest",
		"v0.2.0-0.20261015000000-abcdef": "ghcr.io/marcioapm/luxmux:latest",
		"v0.1.1-0.20261015000000-abcdef": "ghcr.io/marcioapm/luxmux:latest",
		"v0.2.0-rc.1":                    "ghcr.io/marcioapm/luxmux:latest",
		"v2.0.0+incompatible":            "ghcr.io/marcioapm/luxmux:latest",
		"foo-v0.2.0":                     "ghcr.io/marcioapm/luxmux:latest",
		"":                               "ghcr.io/marcioapm/luxmux:latest",
	} {
		if got := imageForVersion(version); got != want {
			t.Errorf("imageForVersion(%q) = %q, want %q", version, got, want)
		}
	}
}

func TestImageFromBuildInfo(t *testing.T) {
	read := func(version string, ok bool) func() (*debug.BuildInfo, bool) {
		return func() (*debug.BuildInfo, bool) {
			return &debug.BuildInfo{Main: debug.Module{Path: "github.com/marcioapm/lux-mux", Version: version}}, ok
		}
	}
	for name, c := range map[string]struct {
		read func() (*debug.BuildInfo, bool)
		want string
	}{
		"release":       {read("v0.2.0", true), "ghcr.io/marcioapm/luxmux:0.2.0"},
		"other release": {read("v3.4.5", true), "ghcr.io/marcioapm/luxmux:3.4.5"},
		"devel":         {read("(devel)", true), "ghcr.io/marcioapm/luxmux:latest"},
		"no build info": {read("v0.2.0", false), "ghcr.io/marcioapm/luxmux:latest"},
	} {
		if got := imageFromBuildInfo(c.read); got != c.want {
			t.Errorf("%s: %q, want %q", name, got, c.want)
		}
	}
}

func TestParseProfiles(t *testing.T) {
	c, err := Parse([]byte(`
prefix = "C-a"
default_profile = "app"
[keys.direct]
"alt-1" = "select 1"
"alt-n" = "next"
[profiles.app]
image = "ghcr.io/example/dev:1"
pool = "m"
size = "m-2-arm64"
repository = "https://github.com/example/App.git"
branch = "main"
git_credential = "GH_TOKEN"
shell = ["zsh", "-l"]
env = { EDITOR = "vim" }
labels = { team = "x" }
secrets = ["NPM_TOKEN"]
workdir = "/workspace/repos/app"
warm_spare = true
[profiles.small]
image = "alpine"
cpus = 1
memory = "2Gi"
disk = "10Gi"
`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Prefix != "C-a" || c.DefaultProfile != "app" {
		t.Fatalf("%+v", c)
	}
	p := c.Profiles["app"]
	if p.Size != "m-2-arm64" || !p.WarmSpare || p.GitRef() != "main" || !reflect.DeepEqual(p.SecretNames(), []string{"NPM_TOKEN", "GH_TOKEN"}) {
		t.Fatalf("app: %+v", p)
	}
	if got := c.ProfileNames(); !reflect.DeepEqual(got, []string{"app", "small"}) {
		t.Fatalf("names %v", got)
	}
}

func TestParseRejects(t *testing.T) {
	for name, src := range map[string]string{
		"unknown key":        "bogus = 1",
		"bad prefix":         `prefix = "x"`,
		"escape prefix":      `prefix = "C-["`,
		"missing default":    "default_profile = \"nope\"\n[profiles.a]\nimage = \"x\"",
		"no image":           "[profiles.a]\npool = \"p\"",
		"size and cpus":      "[profiles.a]\nimage = \"x\"\nsize = \"m\"\ncpus = 2",
		"bad memory":         "[profiles.a]\nimage = \"x\"\nmemory = \"lots\"",
		"branch no repo":     "[profiles.a]\nimage = \"x\"\nbranch = \"main\"",
		"relative workdir":   "[profiles.a]\nimage = \"x\"\nworkdir = \"src\"",
		"reserved label":     "[profiles.a]\nimage = \"x\"\nlabels = { luxmux-session = \"s\" }",
		"bad direct binding": "[keys.direct]\n\"alt-1\" = \"explode\"",
		"bad direct key":     "[keys.direct]\n\"hyper-1\" = \"next\"",
		"LUX_ env":           "[profiles.a]\nimage = \"x\"\nenv = { LUX_X = \"1\" }",
	} {
		if _, err := Parse([]byte(src)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestExampleParses(t *testing.T) {
	c, err := Parse([]byte(Example))
	if err != nil {
		t.Fatal(err)
	}
	if c.Prefix != "C-b" || c.DefaultProfile != "default" || c.Profiles["default"].WarmSpare {
		t.Fatalf("example: %+v", c)
	}
	if got := c.Profiles["default"].Image; got != DefaultImage {
		t.Fatalf("example image = %q, want DefaultImage %q", got, DefaultImage)
	}
	// Uncommenting every commented setting must still parse.
	setting := regexp.MustCompile(`^# (\[|"?[a-z_0-9-]+"? = )`)
	var b strings.Builder
	for _, l := range strings.Split(Example, "\n") {
		if setting.MatchString(l) {
			l = strings.TrimPrefix(l, "# ")
		}
		b.WriteString(l + "\n")
	}
	full, err := Parse([]byte(b.String()))
	if err != nil {
		t.Fatalf("uncommented example: %v\n%s", err, b.String())
	}
	if full.Profiles["app"].Repository == "" || len(full.Profiles["stack"].Repositories) != 2 || len(full.Keys.Direct) != 4 {
		t.Fatalf("uncommented example: %+v", full)
	}
}

func TestKeys(t *testing.T) {
	for name, want := range map[string]string{"C-b": "\x02", "ctrl-a": "\x01", "C-Space": "", "M-n": "\x1bn", "alt-1": "\x1b1", "x": "x", "C-]": "\x1d"} {
		got, err := ParseKey(name)
		if want == "" {
			if err == nil {
				t.Errorf("%s: accepted", name)
			}
			continue
		}
		if err != nil || string(got) != want {
			t.Errorf("%s: %q %v, want %q", name, got, err, want)
		}
	}
	if c, err := ParseCommand("select 3"); err != nil || c != (Command{Op: OpSelect, Arg: 3}) {
		t.Fatalf("select: %v %v", c, err)
	}
	for _, bad := range []string{"", "select", "select x", "fly"} {
		if _, err := ParseCommand(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestRunSpec(t *testing.T) {
	p := Profile{
		Image: "ghcr.io/example/dev:1", Pool: "m", Size: "m-2-arm64",
		Repository: "git@github.com:example/My.App.git", Branch: "dev", GitCredential: "GH_TOKEN",
		Env: map[string]string{"A": "1"}, Labels: map[string]string{"team": "x"}, Secrets: []string{"NPM_TOKEN"},
		Egress: []string{"github.com", "10.0.0.0/8"},
	}
	sp, err := RunSpec(p, Target{Session: "work", Profile: "app"}, map[string]string{"GH_TOKEN": "g", "NPM_TOKEN": "n"})
	if err != nil {
		t.Fatal(err)
	}
	want := api.RunSpec{
		Name:   "luxmux-work",
		Labels: map[string]string{"app": "luxmux", "luxmux-session": "work", "luxmux-profile": "app", "team": "x"},
		Image:  api.Image{Ref: "ghcr.io/example/dev:1"},
		Workload: api.Workload{Adapter: "generic", TTY: true, Command: DefaultShell,
			Workdir: "/workspace/repos/my-app"},
		Env:     map[string]string{"A": "1"},
		Secrets: []api.Secret{{Name: "NPM_TOKEN", Value: "n", As: "env"}, {Name: "GH_TOKEN", Value: "g"}},
		Git: &api.Git{Repositories: []api.Repository{{Name: "my-app", URL: "git@github.com:example/My.App.git",
			Ref: "dev", Credential: "GH_TOKEN"}}},
		Volumes:   []api.Volume{{Name: "workspace", Path: "/workspace", Kind: "state"}},
		Resources: api.Resources{Size: "m-2-arm64"},
		Placement: api.Placement{Pool: "m"},
		Network:   api.Network{Egress: []api.EgressRule{{Host: "github.com"}, {CIDR: "10.0.0.0/8"}}},
	}
	if !reflect.DeepEqual(sp, want) {
		a, _ := json.MarshalIndent(sp, "", " ")
		b, _ := json.MarshalIndent(want, "", " ")
		t.Fatalf("got\n%s\nwant\n%s", a, b)
	}
	if sp.Spot || sp.ResumePolicy != "" {
		t.Fatal("never spot; resumePolicy left to lux's default (auto)")
	}

	sp, err = RunSpec(Profile{Image: "alpine", CPUs: 1, Memory: "2Gi", Shell: []string{"zsh"}}, Target{Session: "s", Profile: "p", Spare: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if sp.Resources.CPUs != 1 || sp.Resources.Memory != 2<<30 || sp.Workload.Workdir != "/workspace" ||
		!reflect.DeepEqual(sp.Workload.Command, []string{"zsh"}) || sp.Labels[LabelSpare] != "true" || sp.Git != nil {
		t.Fatalf("%+v", sp)
	}
}

// TestHomeVolume: the default image's Runs, windows and spares alike, keep
// /home/agent on a state volume; other images get it only with
// persist_home = true, and persist_home = false turns it off.
func TestHomeVolume(t *testing.T) {
	home := api.Volume{Name: "home", Path: "/home/agent", Kind: "state"}
	work := api.Volume{Name: "workspace", Path: "/workspace", Kind: "state"}
	withHome := []api.Volume{work, home}
	without := []api.Volume{work}
	for name, c := range map[string]struct {
		p    Profile
		want []api.Volume
	}{
		"default image":             {Profile{Image: DefaultImage}, withHome},
		"default image, version":    {Profile{Image: "ghcr.io/marcioapm/luxmux:0.2.0"}, withHome},
		"default image, digest":     {Profile{Image: "ghcr.io/marcioapm/luxmux@sha256:abcd"}, withHome},
		"default image, opted out":  {Profile{Image: DefaultImage, PersistHome: boolPtr(false)}, without},
		"custom image":              {Profile{Image: "ubuntu:24.04"}, without},
		"similar name":              {Profile{Image: "ghcr.io/marcioapm/luxmux-dev:1"}, without},
		"other registry, same path": {Profile{Image: "example.com/marcioapm/luxmux:1"}, without},
		"custom image, opted in":    {Profile{Image: "ubuntu:24.04", PersistHome: boolPtr(true)}, withHome},
	} {
		for _, spare := range []bool{false, true} {
			sp, err := RunSpec(c.p, Target{Session: "s", Profile: "p", Spare: spare}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(sp.Volumes, c.want) {
				t.Errorf("%s (spare %v): volumes %+v, want %+v", name, spare, sp.Volumes, c.want)
			}
		}
	}
}

func TestPersistHomeParses(t *testing.T) {
	c, err := Parse([]byte("[profiles.a]\nimage = \"ubuntu:24.04\"\npersist_home = true\n[profiles.b]\nimage = \"" + DefaultImage + "\"\npersist_home = false\n[profiles.c]\nimage = \"" + DefaultImage + "\""))
	if err != nil {
		t.Fatal(err)
	}
	if !c.Profiles["a"].HomeVolume() || c.Profiles["b"].HomeVolume() || !c.Profiles["c"].HomeVolume() {
		t.Fatalf("persist_home: %+v", c.Profiles)
	}
}

func TestResolveSecrets(t *testing.T) {
	dir := t.TempDir()
	env := dir + "/x.env"
	writeFile(t, env, "# c\nexport GH_TOKEN=\"from-file\"\n\nOTHER=1\n")
	p := Profile{Image: "x", Secrets: []string{"NPM_TOKEN"}, GitCredential: "GH_TOKEN", Repository: "https://e.com/a.git", SecretsFrom: env}
	lookup := func(n string) (string, bool) {
		if n == "NPM_TOKEN" || n == "GH_TOKEN" {
			return "from-env-" + n, true
		}
		return "", false
	}
	got, err := p.ResolveSecrets(lookup)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, map[string]string{"GH_TOKEN": "from-file", "NPM_TOKEN": "from-env-NPM_TOKEN"}) {
		t.Fatalf("%v", got)
	}
	p.Secrets = append(p.Secrets, "MISSING")
	_, err = p.ResolveSecrets(lookup)
	if err == nil || !strings.Contains(err.Error(), "MISSING") || strings.Contains(err.Error(), "from-") {
		t.Fatalf("missing secret: %v", err)
	}
}
