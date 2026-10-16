package fakelux

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/marcioapm/lux/api"

	"github.com/marcioapm/lux-mux/internal/config"
	"github.com/marcioapm/lux-mux/internal/luxapi"
)

// minimalSpec is the smallest spec luxd accepts: an image and a command.
const minimalSpec = `{"image":{"ref":"alpine"},"workload":{"command":["sh"]}}`

// shellSpec is a spec as luxmux makes one, with a repository list.
func shellSpec(repos ...api.Repository) api.RunSpec {
	sp := api.RunSpec{
		Image:    api.Image{Ref: "alpine"},
		Workload: api.Workload{Adapter: "generic", TTY: true, Command: []string{"sh"}},
		Volumes:  []api.Volume{{Name: "workspace", Path: "/workspace", Kind: "state"}},
	}
	if len(repos) > 0 {
		sp.Git = &api.Git{Repositories: repos}
	}
	return sp
}

// TestSubmitValidatesAsLuxd: the dry run and the submit refuse what luxd
// refuses, with luxd's status, code and words, and make no Run; what it
// accepts goes through. A submit also needs every secret's value, which
// the dry run does not.
func TestSubmitValidatesAsLuxd(t *testing.T) {
	yes := true
	s := New()
	defer s.Close()
	s.SetPools([]api.Pool{{Name: "m", IsDefault: &yes, Sizes: []api.PoolSize{
		{Name: "m-1", CPUs: 1, Memory: 2 << 30, Disk: 10 << 30},
		{Name: "m-2", CPUs: 2, Memory: 4 << 30, Disk: 20 << 30, Default: true},
	}}, {Name: "plain"}})
	c, _ := luxapi.New(s.Config())
	repo := func(name, url, path string) api.Repository { return api.Repository{Name: name, URL: url, Path: path} }
	for _, tc := range []struct {
		name string
		sp   func() api.RunSpec
		code string
		want string
	}{
		{"no image", func() api.RunSpec { sp := shellSpec(); sp.Image.Ref = ""; return sp }, "invalid_spec", "image: exactly one of ref or build is required"},
		{"no command", func() api.RunSpec { sp := shellSpec(); sp.Workload.Command = nil; return sp }, "invalid_spec", "workload.command is required"},
		{"duplicate repository names", func() api.RunSpec {
			return shellSpec(repo("a", "https://example.com/a.git", ""), repo("a", "https://example.com/b.git", ""))
		}, "invalid_spec", `git.repositories[1]: invalid or duplicate name "a"`},
		{"bad repository name", func() api.RunSpec { return shellSpec(repo("A b", "https://example.com/a.git", "")) }, "invalid_spec", "invalid or duplicate name"},
		{"credentials in the url", func() api.RunSpec { return shellSpec(repo("a", "https://user:tok@example.com/a.git", "")) }, "invalid_spec", "must not carry credentials"},
		{"no url", func() api.RunSpec { return shellSpec(repo("a", "", "")) }, "invalid_spec", "git.repositories[0].url is required"},
		{"path off the state volume", func() api.RunSpec { return shellSpec(repo("a", "https://example.com/a.git", "/tmp/a")) }, "invalid_spec", "is not on a state volume"},
		{"credential not a secret", func() api.RunSpec {
			sp := shellSpec(repo("a", "https://example.com/a.git", ""))
			sp.Git.Repositories[0].Credential = "TOKEN"
			return sp
		}, "invalid_spec", `credential: no secret named "TOKEN"`},
		{"negative cpus", func() api.RunSpec { sp := shellSpec(); sp.Placement.Pool = "plain"; sp.Resources.CPUs = -1; return sp }, "invalid_spec", "resources must not be negative"},
		{"size on an unshaped pool", func() api.RunSpec {
			sp := shellSpec()
			sp.Placement.Pool = "plain"
			sp.Resources.Size = "m-1"
			return sp
		}, "size_unsupported", "pool plain has no shape"},
		{"unknown size", func() api.RunSpec { sp := shellSpec(); sp.Resources.Size = "m-9"; return sp }, "unknown_size", `unknown size "m-9"`},
		{"size and cpus", func() api.RunSpec { sp := shellSpec(); sp.Resources.Size = "m-1"; sp.Resources.CPUs = 1; return sp }, "size_conflict", "cannot be combined"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := len(s.RunIDs())
			for _, dry := range []bool{true, false} {
				var err error
				if dry {
					_, err = c.Validate(context.Background(), tc.sp())
				} else {
					_, err = c.Submit(context.Background(), tc.sp())
				}
				if !luxapi.IsStatus(err, 422) || luxapi.Code(err) != tc.code || !strings.Contains(luxapi.Describe(err), tc.want) {
					t.Errorf("dry run %v: %v, want 422 %s %q", dry, err, tc.code, tc.want)
				}
			}
			if n := len(s.RunIDs()); n != before {
				t.Fatalf("a refused spec made a Run")
			}
		})
	}

	// A secret without its value: the dry run takes it, the submit not.
	sp := shellSpec()
	sp.Secrets = []api.Secret{{Name: "TOKEN", As: "env"}}
	if _, err := c.Validate(context.Background(), sp); err != nil {
		t.Fatalf("dry run without values: %v", err)
	}
	if _, err := c.Submit(context.Background(), sp); luxapi.Code(err) != "secrets_required" || !strings.Contains(luxapi.Describe(err), "TOKEN") {
		t.Fatalf("submit without values: %v", err)
	}

	// Accepted: a credential that is a secret, two repositories, a size.
	ok := shellSpec(repo("a", "https://example.com/a.git", ""), repo("b", "https://example.com/b.git", "/workspace/b"))
	ok.Git.Repositories[0].Credential = "TOKEN"
	ok.Secrets = []api.Secret{{Name: "TOKEN", Value: "v"}}
	ok.Resources.Size = "m-1"
	if _, err := c.Validate(context.Background(), ok); err != nil {
		t.Fatalf("dry run of a valid spec: %v", err)
	}
	r, err := c.Submit(context.Background(), ok)
	if err != nil {
		t.Fatalf("submit of a valid spec: %v", err)
	}
	// As luxd: the size is resolved into the spec's resources, and what
	// was asked is in sizing.requested.
	got, err := c.Get(context.Background(), r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Spec.Resources.Size != "" || got.Spec.Resources.CPUs != 1 || got.Spec.Resources.Memory != 2<<30 ||
		got.Sizing == nil || got.Sizing.Requested == nil || got.Sizing.Requested.Size != "m-1" ||
		got.Sizing.Resolved == nil || got.Sizing.Resolved.Size != "m-1" {
		t.Fatalf("resources %+v sizing %+v", got.Spec.Resources, got.Sizing)
	}
	// cpus on a shaped pool are rounded up to a size; none asked is the
	// default size; an unshaped pool has no sizing.
	for _, tc := range []struct {
		pool    string
		cpus    float64
		size    string
		sizing  bool
		reqCPUs bool
	}{{"", 0.5, "m-1", true, true}, {"", 1.5, "m-2", true, true}, {"", 0, "m-2", true, false}, {"plain", 3, "", false, false}} {
		sp := shellSpec()
		sp.Placement.Pool, sp.Resources.CPUs = tc.pool, tc.cpus
		r, err := c.Submit(context.Background(), sp)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := c.Get(context.Background(), r.ID)
		if (got.Sizing != nil) != tc.sizing || (tc.sizing && (got.Sizing.Resolved.Size != tc.size || (got.Sizing.Requested.CPUs != nil) != tc.reqCPUs)) {
			t.Errorf("%+v: sizing %+v", tc, got.Sizing)
		}
	}
}

// TestStoresNormalizedSpec: GET and the listing return the spec as luxd
// stores it: normalized with luxd's built-in defaults (resources, a
// repository's ref and path, a credential secret runner-only and not in
// the environment), without secret values.
func TestStoresNormalizedSpec(t *testing.T) {
	s := New()
	defer s.Close()
	c, _ := luxapi.New(s.Config())
	sp := shellSpec(api.Repository{Name: "a", URL: "https://example.com/a.git", Credential: "TOKEN"})
	sp.Secrets = []api.Secret{{Name: "TOKEN", Value: "v"}}
	sp.Labels = map[string]string{config.LabelApp: config.AppName}
	r, err := c.Submit(context.Background(), sp)
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.Get(context.Background(), r.ID)
	if err != nil {
		t.Fatal(err)
	}
	listed, err := c.ListAll(context.Background())
	if err != nil || len(listed) != 1 {
		t.Fatalf("listing %v %v", listed, err)
	}
	wantRes := api.Resources{CPUs: 2, Memory: 8 << 30, Disk: 20 << 30, Pids: 4096, DiskGrace: api.OptionalDuration{Set: true}}
	wantRepo := api.Repository{Name: "a", URL: "https://example.com/a.git", Ref: "HEAD", Path: "/workspace/repos/a", Credential: "TOKEN"}
	wantSecrets := []api.Secret{{Name: "TOKEN", As: "none", RunnerOnly: true}}
	for i, run := range []*api.Run{got, listed[0]} {
		if !reflect.DeepEqual(run.Spec.Resources, wantRes) {
			t.Errorf("run %d: resources %+v, want luxd's defaults %+v", i, run.Spec.Resources, wantRes)
		}
		if run.Spec.Git == nil || !reflect.DeepEqual(run.Spec.Git.Repositories, []api.Repository{wantRepo}) {
			t.Errorf("run %d: git %+v, want %+v", i, run.Spec.Git, wantRepo)
		}
		if !reflect.DeepEqual(run.Spec.Secrets, wantSecrets) {
			t.Errorf("run %d: secrets %+v, want %+v (no value)", i, run.Spec.Secrets, wantSecrets)
		}
		if run.Sizing != nil {
			t.Errorf("run %d: sizing %+v on an unshaped pool", i, run.Sizing)
		}
	}
}
