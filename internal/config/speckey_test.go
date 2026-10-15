package config

import (
	"testing"
)

// TestSpecKeyFields: SpecKey differs whenever the Run a profile makes
// differs, and only then.
func TestSpecKeyRepositories(t *testing.T) {
	p := Profile{Image: "alpine", Repositories: []Repository{{URL: "https://example.com/api.git"}, {URL: "https://example.com/web.git"}}}
	key := SpecKey(p)
	for name, change := range map[string]func(*Profile){
		"url":        func(p *Profile) { p.Repositories[1].URL = "https://example.com/other.git" },
		"branch":     func(p *Profile) { p.Repositories[1].Branch = "dev" },
		"path":       func(p *Profile) { p.Repositories[1].Path = "/workspace/web" },
		"name":       func(p *Profile) { p.Repositories[1].Name = "client" },
		"credential": func(p *Profile) { p.Repositories[1].GitCredential = "TOKEN" },
		"order":      func(p *Profile) { p.Repositories[0], p.Repositories[1] = p.Repositories[1], p.Repositories[0] },
	} {
		t.Run(name, func(t *testing.T) {
			q := p
			q.Repositories = append([]Repository{}, p.Repositories...)
			change(&q)
			if got := SpecKey(q); got == "" || got == key {
				t.Fatalf("key %q", got)
			}
		})
	}
}

func TestSpecKeyFields(t *testing.T) {
	base := Profile{
		Image: "alpine", Pool: "p1", CPUs: 2, Memory: "1GiB", Disk: "10GiB",
		Repository: "https://example.com/a/repo.git", Branch: "main", GitCredential: "GIT_TOKEN",
		Shell: []string{"bash"}, Env: map[string]string{"A": "1"}, Labels: map[string]string{"team": "x"},
		Secrets: []string{"S1"}, Workdir: "/w", Egress: []string{"example.com"},
	}
	key := SpecKey(base)
	if key == "" {
		t.Fatal("no key")
	}
	clone := func() Profile {
		p := base
		p.Shell = append([]string{}, base.Shell...)
		p.Env = map[string]string{"A": "1"}
		p.Labels = map[string]string{"team": "x"}
		p.Secrets = append([]string{}, base.Secrets...)
		p.Egress = append([]string{}, base.Egress...)
		return p
	}
	differ := map[string]func(*Profile){
		"image":       func(p *Profile) { p.Image = "debian" },
		"pool":        func(p *Profile) { p.Pool = "p2" },
		"cpus":        func(p *Profile) { p.CPUs = 4 },
		"memory":      func(p *Profile) { p.Memory = "2GiB" },
		"disk":        func(p *Profile) { p.Disk = "20GiB" },
		"repository":  func(p *Profile) { p.Repository = "https://example.com/a/other.git" },
		"branch":      func(p *Profile) { p.Branch = "dev" },
		"credential":  func(p *Profile) { p.GitCredential = "OTHER_TOKEN" },
		"shell":       func(p *Profile) { p.Shell = []string{"zsh"} },
		"env value":   func(p *Profile) { p.Env["A"] = "2" },
		"env name":    func(p *Profile) { p.Env["B"] = "1" },
		"label":       func(p *Profile) { p.Labels["team"] = "y" },
		"secret name": func(p *Profile) { p.Secrets = append(p.Secrets, "S2") },
		"workdir":     func(p *Profile) { p.Workdir = "/v" },
		"egress":      func(p *Profile) { p.Egress = []string{"example.org"} },
		"egress cidr": func(p *Profile) { p.Egress = append(p.Egress, "10.0.0.0/8") },
		"unrestricted": func(p *Profile) {
			p.UnrestrictedNetwork = true
		},
		"persist home": func(p *Profile) { p.PersistHome = boolPtr(true) },
	}
	for name, f := range differ {
		p := clone()
		f(&p)
		if k := SpecKey(p); k == "" || k == key {
			t.Errorf("%s: key %q, the base has %q", name, k, key)
		}
	}
	// A shaped pool's size, compared with a profile that has none.
	sized := Profile{Image: "alpine", Size: "small"}
	if SpecKey(sized) == SpecKey(Profile{Image: "alpine"}) || SpecKey(sized) == SpecKey(Profile{Image: "alpine", Size: "large"}) {
		t.Error("size: same key")
	}
	same := map[string]func(*Profile){
		"warm spare":   func(p *Profile) { p.WarmSpare = true },
		"secrets from": func(p *Profile) { p.SecretsFrom = "somewhere" },
		"ref alias":    func(p *Profile) { p.Ref, p.Branch = p.Branch, "" },
		"home off":     func(p *Profile) { p.PersistHome = boolPtr(false) },
		"nothing":      func(*Profile) {},
	}
	for name, f := range same {
		p := clone()
		f(&p)
		if k := SpecKey(p); k != key {
			t.Errorf("%s: key %q, the base has %q", name, k, key)
		}
	}
	if SpecKey(Profile{}) != "" {
		t.Error("a profile without an image has a key")
	}
}

// TestSpecKeyStableForCustomImages: a custom-image profile gets no home
// volume, so its key is the one luxmux 0.1.0 computed and warm spares made
// by that version are still claimed. The constants were printed by 79e22db.
func TestSpecKeyStableForCustomImages(t *testing.T) {
	for want, p := range map[string]Profile{
		"76d9621a3223b5c5": {Image: "alpine", CPUs: 1},
		"6d2da9ae44885338": {Image: "ghcr.io/example/dev:1", Repository: "https://example.com/a.git", Egress: []string{"github.com"}},
	} {
		if got := SpecKey(p); got != want {
			t.Errorf("SpecKey(%+v) = %s, want the 0.1.0 key %s", p, got, want)
		}
	}
}

// TestSpareLabelIsSpecKey: a spare carries its profile's SpecKey whatever
// session or profile name it is made for, and whatever the secret values.
func TestSpareLabelIsSpecKey(t *testing.T) {
	p := Profile{Image: "alpine", Secrets: []string{"S1"}}
	sp, err := RunSpec(p, Target{Session: "s", Profile: "named", Spare: true}, map[string]string{"S1": "value"})
	if err != nil {
		t.Fatal(err)
	}
	if sp.Labels[LabelSpec] != SpecKey(p) || sp.Labels[LabelSpare] != "true" {
		t.Fatalf("labels %v, key %q", sp.Labels, SpecKey(p))
	}
	if sp, _ := RunSpec(p, Target{Session: "s", Profile: "named"}, nil); sp.Labels[LabelSpec] != "" {
		t.Fatalf("a window's Run labelled as a spare: %v", sp.Labels)
	}
}
