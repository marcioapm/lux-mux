package state

import (
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/marcioapm/lux/api"

	"github.com/marcioapm/lux-mux/internal/config"
)

func TestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if _, ok, err := Load(dir, "work"); ok || err != nil {
		t.Fatalf("missing file: ok=%v err=%v", ok, err)
	}
	s := &Session{
		Name: "work", DefaultProfile: "app",
		Windows: []Window{{RunID: "run_a", Name: "editor", Profile: "default"},
			{RunID: "run_b", Name: "logs", Profile: "app", Spec: &config.Profile{Image: "alpine", Secrets: []string{"X"}}}},
		Active: 1, Last: 0,
		Claimed:      []string{"run_b"},
		Spares:       map[string]string{"app": "run_c"},
		RecentImages: []string{"alpine"},
	}
	if err := Save(dir, s); err != nil {
		t.Fatal(err)
	}
	got, ok, err := Load(dir, "work")
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	if !reflect.DeepEqual(got, s) {
		t.Fatalf("got %+v\nwant %+v", got, s)
	}
	names, err := List(dir)
	if err != nil || !reflect.DeepEqual(names, []string{"work"}) {
		t.Fatalf("list %v %v", names, err)
	}
	if err := Remove(dir, "work"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := Load(dir, "work"); ok {
		t.Fatal("removed file still loads")
	}
}

func run(id, session, profile, state string, at int, spare bool) *api.Run {
	r := &api.Run{ID: id, State: state, CreatedAt: time.Unix(int64(at), 0),
		Labels: map[string]string{config.LabelSession: session, config.LabelProfile: profile}}
	if spare {
		r.Labels[config.LabelSpare] = "true"
	}
	return r
}

// testEP is the one endpoint of rebuildOne.
var testEP = config.Endpoint{URL: "https://lux.example.com"}

// rebuildOne is RebuildFrom over one endpoint's listing of runs.
func rebuildOne(session string, runs []*api.Run, profiles map[string]config.Profile) *Session {
	return RebuildFrom(session, []Listing{{Endpoint: testEP, Runs: runs}}, testEP, profiles)
}

// TestSessionDefaultProfile: a session's default profile is kept in its
// file, and a file from before it has none.
func TestSessionDefaultProfile(t *testing.T) {
	dir := t.TempDir()
	if err := Save(dir, &Session{Name: "a", DefaultProfile: "dev"}); err != nil {
		t.Fatal(err)
	}
	if s, ok, err := Load(dir, "a"); err != nil || !ok || s.DefaultProfile != "dev" {
		t.Fatalf("%+v %v", s, err)
	}
	if err := os.WriteFile(Path(dir, "old"), []byte(`{"name":"old","windows":[],"active":0,"last":0}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if s, ok, err := Load(dir, "old"); err != nil || !ok || s.DefaultProfile != "" {
		t.Fatalf("%+v %v", s, err)
	}
}

// TestRebuildSpecFromRun: a rebuilt window's spec is its Run's, over the
// configured profile it names: every repository, in order, with lux's
// defaults (name, path, ref HEAD) taken back out so its SpecKey is the
// profile's; a window name new-window gave is kept.
func TestRebuildSpecFromRun(t *testing.T) {
	prof := config.Profile{Image: "alpine", GitCredential: "TOKEN", SecretsFrom: "/somewhere.env", Labels: map[string]string{"team": "x"},
		Repositories: []config.Repository{{URL: "https://example.com/api.git", Branch: "main"}, {URL: "https://example.com/web.git", Name: "client", Path: "/workspace/web"}}}
	sp, err := config.RunSpec(prof, config.Target{Session: "work", Profile: "dev"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// As luxd stores it.
	for i := range sp.Git.Repositories {
		r := &sp.Git.Repositories[i]
		if r.Ref == "" {
			r.Ref = "HEAD"
		}
		if r.Path == "" {
			r.Path = "/workspace/repos/" + r.Name
		}
	}
	r := run("run_1", "work", "dev", "running", 1, false)
	r.Spec = sp
	r.Labels[config.LabelWindow] = "named"
	s := rebuildOne("work", []*api.Run{r}, map[string]config.Profile{"dev": prof})
	if len(s.Windows) != 1 || s.Windows[0].Name != "named" || s.Windows[0].Profile != "dev" || s.Windows[0].Spec == nil {
		t.Fatalf("%+v", s.Windows)
	}
	got := *s.Windows[0].Spec
	if !reflect.DeepEqual(got.Repositories, []config.Repository{{URL: "https://example.com/api.git", Ref: "main"},
		{URL: "https://example.com/web.git", Name: "client", Path: "/workspace/web"}}) {
		t.Fatalf("repositories %+v", got.Repositories)
	}
	if config.SpecKey(got) != config.SpecKey(prof) || got.SecretsFrom != prof.SecretsFrom {
		t.Fatalf("rebuilt spec %+v\nprofile %+v", got, prof)
	}
	// A Run listed without its spec keeps the profile name only.
	bare := run("run_2", "work", "dev", "running", 2, false)
	if s := rebuildOne("work", []*api.Run{bare}, nil); s.Windows[0].Spec != nil || s.Windows[0].Name != "dev" {
		t.Fatalf("%+v", s.Windows[0])
	}
}

// TestRebuildSizedRun: a Run of a sized profile comes back from luxd with
// the size resolved into cpus, memory and disk and no size in its spec;
// what was asked is in sizing.requested. The window's spec is the
// profile's again, size and not resources, so its SpecKey matches.
func TestRebuildSizedRun(t *testing.T) {
	prof := config.Profile{Image: "alpine", Pool: "m", Size: "m-2"}
	sp, err := config.RunSpec(prof, config.Target{Session: "work", Profile: "dev"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	sp.Resources = api.Resources{CPUs: 2, Memory: 4 << 30, Disk: 20 << 30}
	r := run("run_1", "work", "dev", "running", 1, false)
	r.Spec = sp
	r.Sizing = &api.RunSizing{Requested: &api.SizeRequest{Size: "m-2"}, Resolved: &api.SizeResolved{Size: "m-2", CPUs: 2, Memory: 4 << 30, Disk: 20 << 30}}
	got := WindowFromRun(r, prof).Spec
	if got.Size != "m-2" || got.CPUs != 0 || got.Memory != "" || got.Disk != "" || config.SpecKey(*got) != config.SpecKey(prof) {
		t.Fatalf("rebuilt %+v", got)
	}
}

// TestRebuildDefaultedResources: luxd stores resources for every Run,
// its defaults where the submit set none, so they are never read back.
// What the window asked comes from sizing.requested (shaped pools), else
// the luxmux-resources label, else the profile's own values.
func TestRebuildDefaultedResources(t *testing.T) {
	cpus, mem := 4.0, api.Bytes(16<<30)
	defaulted := api.Resources{CPUs: 2, Memory: 8 << 30, Disk: 20 << 30, Pids: 4096}
	with := func(f func(*api.Resources)) api.Resources { r := defaulted; f(&r); return r }
	cpus4 := with(func(r *api.Resources) { r.CPUs = 4 })
	unsized := config.Profile{Image: "alpine"}
	for _, tc := range []struct {
		name   string
		prof   config.Profile
		stored api.Resources
		label  string // "": none
		sizing *api.RunSizing
		want   config.Profile
	}{
		{"unshaped, unsized", unsized, defaulted, "", nil, unsized},
		{"unshaped, sized in config", config.Profile{Image: "alpine", CPUs: 4}, cpus4, "cpus=4", nil, config.Profile{Image: "alpine", CPUs: 4}},
		{"unshaped, unsized profile, cpus asked by the window", unsized, cpus4, "cpus=4", nil, config.Profile{Image: "alpine", CPUs: 4}},
		{"unshaped, label with every resource", config.Profile{Image: "alpine", CPUs: 1}, api.Resources{CPUs: 0.5, Memory: 1 << 30, Disk: 40 << 30, Pids: 4096},
			"cpus=0.5,memory=1Gi,disk=40Gi", nil, config.Profile{Image: "alpine", CPUs: 0.5, Memory: "1Gi", Disk: "40Gi"}},
		{"no label, no sizing: the profile's, not the stored", unsized, cpus4, "", nil, unsized},
		{"no label, no sizing, sized profile", config.Profile{Image: "alpine", CPUs: 4, Memory: "1Gi"}, defaulted, "", nil,
			config.Profile{Image: "alpine", CPUs: 4, Memory: "1Gi"}},
		{"shaped, default size", unsized, defaulted, "", &api.RunSizing{Requested: &api.SizeRequest{}}, unsized},
		{"shaped, cpus and memory asked", unsized, defaulted, "",
			&api.RunSizing{Requested: &api.SizeRequest{CPUs: &cpus, Memory: &mem}}, config.Profile{Image: "alpine", CPUs: 4, Memory: "17179869184"}},
		{"shaped: sizing.requested over the label", unsized, defaulted, "size=large",
			&api.RunSizing{Requested: &api.SizeRequest{CPUs: &cpus}}, config.Profile{Image: "alpine", CPUs: 4}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sp, err := config.RunSpec(tc.prof, config.Target{Session: "work", Profile: "dev"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			sp.Resources = tc.stored
			r := run("run_1", "work", "dev", "running", 1, false)
			if tc.label != "" {
				r.Labels[config.LabelResources] = tc.label
			}
			r.Spec, r.Sizing = sp, tc.sizing
			got := *WindowFromRun(r, tc.prof).Spec
			if got.Size != tc.want.Size {
				t.Errorf("size %q, want %q", got.Size, tc.want.Size)
			}
			if got.CPUs != tc.want.CPUs {
				t.Errorf("cpus %v, want %v", got.CPUs, tc.want.CPUs)
			}
			if got.Memory != tc.want.Memory {
				t.Errorf("memory %q, want %q", got.Memory, tc.want.Memory)
			}
			if got.Disk != tc.want.Disk {
				t.Errorf("disk %q, want %q", got.Disk, tc.want.Disk)
			}
			if config.SpecKey(got) != config.SpecKey(tc.want) {
				t.Errorf("SpecKey of %+v differs from %+v's", got, tc.want)
			}
		})
	}
}

func TestRebuild(t *testing.T) {
	runs := []*api.Run{
		run("run_3", "work", "app", "running", 30, false),
		run("run_1", "work", "default", "running", 10, false),
		run("run_x", "other", "default", "running", 5, false),
		run("run_t", "work", "default", "terminated", 1, false),
		run("run_2", "work", "app", "stopped", 20, true), // an older spare: claimed
		run("run_4", "work", "app", "running", 40, true), // the newest spare: waiting
	}
	s := rebuildOne("work", runs, nil)
	var ids []string
	for _, w := range s.Windows {
		ids = append(ids, w.RunID)
		if w.Name != w.Profile {
			t.Errorf("%s: name %q, want its profile %q", w.RunID, w.Name, w.Profile)
		}
	}
	if !reflect.DeepEqual(ids, []string{"run_1", "run_2", "run_3"}) {
		t.Fatalf("windows %v", ids)
	}
	if !reflect.DeepEqual(s.Spares, map[string]string{"app": "run_4"}) || !reflect.DeepEqual(s.Claimed, []string{"run_2"}) {
		t.Fatalf("spares %v claimed %v", s.Spares, s.Claimed)
	}
}

// TestRebuildBySpec: spares are keyed by their luxmux-spec label, so two
// profiles' spares stay apart and a spare is claimed by spec, not name; a
// spare from before the label is keyed by its profile.
func TestRebuildBySpec(t *testing.T) {
	spec := func(r *api.Run, key string) *api.Run { r.Labels[config.LabelSpec] = key; return r }
	runs := []*api.Run{
		spec(run("run_a", "work", "app", "running", 10, true), "k1"),
		spec(run("run_b", "work", "app", "running", 20, true), "k2"),     // same profile, other spec
		spec(run("run_c", "work", "other", "running", 30, true), "k3"),   // older spare of k3: claimed
		spec(run("run_d", "work", "renamed", "running", 40, true), "k3"), // newest of k3: waiting
		run("run_e", "work", "legacy", "running", 50, true),
	}
	s := rebuildOne("work", runs, nil)
	want := map[string]string{"k1": "run_a", "k2": "run_b", "k3": "run_d", "legacy": "run_e"}
	if !reflect.DeepEqual(s.Spares, want) {
		t.Fatalf("spares %v, want %v", s.Spares, want)
	}
	if !reflect.DeepEqual(s.Claimed, []string{"run_c"}) || len(s.Windows) != 1 || s.Windows[0].RunID != "run_c" {
		t.Fatalf("claimed %v windows %+v", s.Claimed, s.Windows)
	}
}

func TestReconcile(t *testing.T) {
	s := &Session{Windows: []Window{{RunID: "a"}, {RunID: "b"}, {RunID: "c"}}, Active: 2, Last: 1,
		Spares:  map[string]string{"p": "s1", "q": "s2"},
		Claimed: []string{"a", "b", "gone"}}
	s.ReconcileUp(map[string]string{"a": "running", "b": "terminated", "c": "stopped", "s1": "running"}, testEP, nil)
	if len(s.Windows) != 2 || s.Windows[0].RunID != "a" || s.Windows[1].RunID != "c" {
		t.Fatalf("%+v", s.Windows)
	}
	if s.Active != 1 || s.Last != 0 {
		t.Fatalf("active %d last %d", s.Active, s.Last)
	}
	if !reflect.DeepEqual(s.Spares, map[string]string{"p": "s1"}) {
		t.Fatalf("spares %v", s.Spares)
	}
	if !reflect.DeepEqual(s.Claimed, []string{"a"}) {
		t.Fatalf("claimed %v", s.Claimed)
	}
}

func TestAddRecent(t *testing.T) {
	l := AddRecent([]string{"a", "b"}, "b")
	if !reflect.DeepEqual(l, []string{"b", "a"}) {
		t.Fatal(l)
	}
	for i := range 20 {
		l = AddRecent(l, string(rune('c'+i)))
	}
	if len(l) != 10 {
		t.Fatal(len(l))
	}
}

// TestLock: one holder of a session's lock at a time; another session is
// independent; unlock frees it.
func TestLock(t *testing.T) {
	dir := t.TempDir()
	unlock, err := Lock(dir, "s")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Lock(dir, "s"); !errors.Is(err, ErrLocked) || !strings.Contains(err.Error(), fmt.Sprint(os.Getpid())) {
		t.Fatalf("second lock: %v", err)
	}
	u2, err := Lock(dir, "other")
	if err != nil {
		t.Fatal(err)
	}
	u2()
	unlock()
	u3, err := Lock(dir, "s")
	if err != nil {
		t.Fatalf("after unlock: %v", err)
	}
	u3()
	if names, _ := List(dir); len(names) != 0 {
		t.Fatalf("lock files listed as sessions: %v", names)
	}
}

// TestRecordSpares: lux's waiting spares of the session fill only its
// empty slots, the newest Run per slot; a filled slot keeps its spare,
// and a spare being terminated, ended, of another label, without a spec
// key, or already a window's Run (claimed or not) is skipped.
func TestRecordSpares(t *testing.T) {
	spare := func(id, label, key, st string, at int) *api.Run {
		r := run(id, label, "dev", st, at, true)
		r.Labels[config.LabelSpec] = key
		return r
	}
	stopping := spare("run_stop", "work", "k3", "running", 9)
	stopping.StateReason = "terminate"
	runs := []*api.Run{
		spare("run_old", "work", "k1", "running", 1),
		spare("run_new", "work", "k1", "starting", 2),
		spare("run_lux", "work", "k2", "running", 3),
		stopping,
		spare("run_k3", "work", "k3", "running", 4),
		spare("run_end", "work", "k4", "terminated", 5),
		spare("run_other", "play", "k5", "running", 6),
		spare("run_cl", "work", "k6", "running", 10),
		spare("run_win", "work", "k7", "running", 11),
		spare("run_nokey", "work", "", "running", 12),
	}
	s := &Session{Name: "work", Spares: map[string]string{"k1": "", "k2": "run_kept"},
		Claimed: []string{"run_cl"}, Windows: []Window{{RunID: "run_win"}}}
	RecordSpares(s, "work", runs)
	want := map[string]string{"k1": "run_new", "k2": "run_kept", "k3": "run_k3"}
	if !reflect.DeepEqual(s.Spares, want) {
		t.Fatalf("spares %v, want %v", s.Spares, want)
	}
}

// TestRebuildFromEndpoints: windows rebuilt from two endpoints' listings
// record the endpoint each Run was listed on; an endpoint that could not
// be listed adds nothing; a spare off the default endpoint gets a slot of
// its own; every spare records its endpoint, the default's too.
func TestRebuildFromEndpoints(t *testing.T) {
	def := config.Endpoint{URL: "https://lux.example.com"}
	other := config.Endpoint{URL: "https://lux2.example.com", Tenant: "example", KeyEnv: "EXAMPLE_LUX_API_KEY"}
	down := config.Endpoint{URL: "https://down.example.com"}
	spec := func(r *api.Run) *api.Run { r.Labels[config.LabelSpec] = "k"; return r }
	ls := []Listing{
		{Endpoint: def, Runs: []*api.Run{run("run_1", "work", "a", "running", 10, false), spec(run("run_s1", "work", "a", "running", 15, true))}},
		{Endpoint: other, Runs: []*api.Run{run("run_2", "work", "b", "running", 20, false), spec(run("run_s2", "work", "b", "running", 25, true))}},
		{Endpoint: down, Err: errors.New("unreachable")},
	}
	s := RebuildFrom("work", ls, def, nil)
	if len(s.Windows) != 2 || *s.Windows[0].Endpoint != def || *s.Windows[1].Endpoint != other {
		t.Fatalf("windows %+v", s.Windows)
	}
	slot := config.SpareSlot("k", other, def)
	if !reflect.DeepEqual(s.Spares, map[string]string{"k": "run_s1", slot: "run_s2"}) ||
		!reflect.DeepEqual(s.SpareEndpoints, map[string]config.Endpoint{"k": def, slot: other}) {
		t.Fatalf("spares %v endpoints %v", s.Spares, s.SpareEndpoints)
	}
}

// TestReconcileKeepsDownEndpoint: windows and spares on an endpoint that
// could not be listed are kept; those on the endpoints that answered are
// reconciled.
func TestReconcileKeepsDownEndpoint(t *testing.T) {
	def := config.Endpoint{URL: "https://lux.example.com"}
	down := config.Endpoint{URL: "https://down.example.com"}
	s := &Session{Windows: []Window{{RunID: "a"}, {RunID: "b", Endpoint: &down}, {RunID: "gone"}},
		Spares: map[string]string{"k": "s1", "k@x": "s2"}, SpareEndpoints: map[string]config.Endpoint{"k@x": down},
		Claimed: []string{"b"}}
	s.ReconcileUp(map[string]string{"a": "running"}, def, func(e config.Endpoint) bool { return e == down })
	if len(s.Windows) != 2 || s.Windows[0].RunID != "a" || s.Windows[1].RunID != "b" {
		t.Fatalf("windows %+v", s.Windows)
	}
	if !reflect.DeepEqual(s.Spares, map[string]string{"k@x": "s2"}) || !reflect.DeepEqual(s.Claimed, []string{"b"}) {
		t.Fatalf("spares %v claimed %v", s.Spares, s.Claimed)
	}
}
