package info

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marcioapm/lux/api"

	"github.com/marcioapm/lux-mux/internal/config"
	"github.com/marcioapm/lux-mux/internal/luxapi"
)

// secretValue is a value no output may ever hold.
const secretValue = "example-secret-value-5f3a"

var t0 = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

func ep() config.Endpoint {
	return config.Endpoint{URL: "https://lux.example.com", Tenant: "example", KeyEnv: "EXAMPLE_LUX_KEY"}
}

func target() Target {
	return Target{Index: 1, Name: "api", Profile: "dev", Session: "work", Label: "work", Endpoint: ep(), RunID: "run_1"}
}

// fullRun is a Run with every field info reads set.
func fullRun() *api.Run {
	snap := "snap_1"
	since := t0.Add(-90 * time.Minute)
	mem, disk := api.Bytes(8<<30), api.Bytes(40<<30)
	cpus := 2.0
	return &api.Run{
		ID: "run_1", Name: "luxmux-work", State: "running",
		Labels: map[string]string{config.LabelApp: config.AppName, config.LabelSession: "work", config.LabelProfile: "dev",
			config.LabelResources: "size=m-2-arm64", "team": "example"},
		Epoch: 3, Host: "host-3", Pool: "m",
		Spec: api.RunSpec{
			Image:    api.Image{Ref: "ghcr.io/example/dev:latest"},
			Workload: api.Workload{Workdir: "/workspace/repos/api"},
			Git: &api.Git{Repositories: []api.Repository{{Name: "api", URL: "https://user:tok@example.com/api.git", Ref: "main"},
				{Name: "web", URL: "https://example.com/web.git", Path: "/workspace/web"}}},
			Network: api.Network{Egress: []api.EgressRule{{Host: "example.com"}, {CIDR: "192.0.2.0/24"}}},
			Secrets: []api.Secret{{Name: "EXAMPLE_TOKEN", Value: secretValue}},
		},
		Image:                 &api.ImageResolution{ImageID: "sha256:abc123"},
		Secrets:               []api.SecretRef{{Name: "EXAMPLE_TOKEN", Fingerprint: "f00d"}},
		CreatedAt:             t0.Add(-3 * time.Hour),
		RuntimeSeconds:        7200,
		RuntimeSince:          &since,
		PlacementWaitSeconds:  12,
		PlacementStartSeconds: 8,
		Stage:                 "running",
		StageSince:            since,
		SubmittedBy:           &api.RunSubmitter{KeyName: "ci", Email: "dev@example.com"},
		Sizing: &api.RunSizing{Requested: &api.SizeRequest{Size: "m-2-arm64"},
			Resolved: &api.SizeResolved{Size: "m-2-arm64", CPUs: cpus, Memory: mem, Disk: disk}},
		Resume: &api.Resumability{Snapshot: &snap},
	}
}

func fullCost() *luxapi.RunCost {
	return &luxapi.RunCost{Status: "incomplete", Totals: []api.CostTotal{{Currency: "USD", Amount: "1.25", Final: "1", Estimate: "0.25"}}}
}

// fakeClient answers from fields; exec runs execFn.
type fakeClient struct {
	run    *api.Run
	getErr error
	cost   *luxapi.RunCost
	execFn func(ctx context.Context) ([]byte, int, error)
	gets   atomic.Int32
	execs  atomic.Int32
}

func (f *fakeClient) Get(ctx context.Context, id string) (*api.Run, error) {
	f.gets.Add(1)
	if f.getErr != nil {
		return nil, f.getErr
	}
	r := *f.run
	return &r, nil
}

func (f *fakeClient) Cost(context.Context, string) (*luxapi.RunCost, error) {
	if f.cost == nil {
		return nil, &api.APIError{Status: 404, Message: "no cost"}
	}
	return f.cost, nil
}

func (f *fakeClient) ExecOutput(ctx context.Context, id string, cmd []string, limit int) ([]byte, int, error) {
	f.execs.Add(1)
	if f.execFn == nil {
		return nil, 0, errors.New("exec refused")
	}
	return f.execFn(ctx)
}

func cgroupOut(usec int64) func(context.Context) ([]byte, int, error) {
	return func(context.Context) ([]byte, int, error) {
		return []byte("cpu.max 200000 100000\nmemory.max 8589934592\nmemory.current 1073741824\n" +
			"cpu.usage_usec " + itoa(usec) + "\ndf 41943040 10485760\n"), 0, nil
	}
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

func clock(t *time.Time) func() time.Time { return func() time.Time { return *t } }

func mustJSON(t *testing.T, w Window) string {
	t.Helper()
	b, err := json.Marshal(w)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestFromRunAllFields(t *testing.T) {
	now := t0
	c := &fakeClient{run: fullRun(), cost: fullCost(), execFn: cgroupOut(5_000_000)}
	w := WithResources(context.Background(), c, NewSampler(), target(), clock(&now))
	if w.Problem != "" {
		t.Fatal(w.Error)
	}
	text := Text(w, now)
	for _, want := range []string{
		"window", "1 api", "profile", "dev", "session", "work", "lux.example.com · tenant example", "run_1",
		"luxmux-work", "running", "key ci · dev@example.com", "team=example",
		"ghcr.io/example/dev:latest", "sha256:abc123",
		"pool", "m", "size", "m-2-arm64", "2 cpus, 8Gi memory, 40Gi disk", "host-3", "3 (2 moves since the first)",
		"stage", "running, since 2026-01-02 01:34:05Z", "1h 30m (this placement)", "2h 0m (all placements)",
		"12s (for a host)", "8s (on a host)",
		"https://example.com/api.git @ main", "https://example.com/web.git → /workspace/web", "/workspace/repos/api",
		"only example.com, 192.0.2.0/24", "EXAMPLE_TOKEN",
		"1.25 USD (final 1, estimated 0.25)", "incomplete",
		"— of 2 cpus", "1Gi of 8Gi (12%)", "10Gi of 40Gi (25%)",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("text lacks %q", want)
		}
	}
	if strings.Contains(text, "luxmux-session") || strings.Contains(text, "app=luxmux") || strings.Contains(text, "tok@") {
		t.Errorf("luxmux labels or URL credentials shown:\n%s", text)
	}
	if strings.Contains(text, "EXAMPLE_LUX_KEY") {
		t.Errorf("the key's variable is named:\n%s", text)
	}
	j := mustJSON(t, w)
	for _, want := range []string{`"run_id":"run_1"`, `"host":"host-3"`, `"epoch":3`, `"moves":2`, `"runtime_seconds":7200`,
		`"amount":1.25`, `"stage_since":"2026-01-02T01:34:05Z"`, `"policy":"allowlist"`, `"secrets":["EXAMPLE_TOKEN"]`,
		`"memory_limit_bytes":8589934592`, `"resolved":"sha256:abc123"`} {
		if !strings.Contains(j, want) {
			t.Errorf("json lacks %s", want)
		}
	}
	// The JSON reads back into the documented struct, the same.
	var back Window
	if err := json.Unmarshal([]byte(j), &back); err != nil {
		t.Fatal(err)
	}
	if mustJSON(t, back) != j {
		t.Fatalf("round trip differs:\n%s\n%s", j, mustJSON(t, back))
	}
}

// TestOptionalFieldsOmitted: each field lux leaves out has no line and no
// JSON key, and no zero value stands in for it.
func TestOptionalFieldsOmitted(t *testing.T) {
	cases := []struct {
		name   string
		change func(r *api.Run, c *fakeClient)
		gone   []string // in the text
		goneJ  []string // in the JSON
	}{
		{"host", func(r *api.Run, _ *fakeClient) { r.Host = "" }, []string{"host-3", "  host "}, []string{`"host"`}},
		{"epoch", func(r *api.Run, _ *fakeClient) { r.Epoch = 0 }, []string{"epoch", "move"}, []string{`"epoch"`, `"moves"`}},
		{"resolved image", func(r *api.Run, _ *fakeClient) { r.Image = nil }, []string{"resolved"}, []string{`"resolved"`}},
		{"sizing", func(r *api.Run, _ *fakeClient) { r.Sizing = nil; r.Labels[config.LabelResources] = "" },
			[]string{"m-2-arm64", "asked", "effective"}, []string{`"size"`, `"asked"`, `"effective"`}},
		{"runtime", func(r *api.Run, _ *fakeClient) { r.RuntimeSeconds = 0; r.RuntimeSince = nil },
			[]string{"runtime", "uptime", "0s"}, []string{`"runtime_seconds"`, `"uptime_seconds"`}},
		{"placement time", func(r *api.Run, _ *fakeClient) { r.PlacementWaitSeconds, r.PlacementStartSeconds = 0, 0 },
			[]string{"waiting", "starting", "0s"}, []string{`"waiting_seconds"`, `"starting_seconds"`}},
		{"cost", func(_ *api.Run, c *fakeClient) { c.cost = nil }, []string{"Cost", "so far", "USD"}, []string{`"cost"`}},
		{"cost pending", func(_ *api.Run, c *fakeClient) { c.cost = &luxapi.RunCost{Status: "pending"} }, []string{"Cost", "pending"}, []string{`"cost"`}},
		{"cost zero", func(_ *api.Run, c *fakeClient) {
			c.cost = &luxapi.RunCost{Status: "complete", Totals: []api.CostTotal{{Currency: "USD", Amount: "0"}}}
		}, []string{"Cost", "0 USD", "$0"}, []string{`"cost"`}},
		{"secrets", func(r *api.Run, _ *fakeClient) { r.Secrets = nil }, []string{"EXAMPLE_TOKEN"}, []string{`"secrets"`}},
		{"submitter", func(r *api.Run, _ *fakeClient) { r.SubmittedBy = nil }, []string{"submitted by"}, []string{`"submitted_by"`}},
		{"labels", func(r *api.Run, _ *fakeClient) { delete(r.Labels, "team") }, []string{"labels"}, []string{`"labels"`}},
		{"repositories", func(r *api.Run, _ *fakeClient) { r.Spec.Git = nil }, []string{"repository"}, []string{`"repositories"`}},
		{"resume", func(r *api.Run, _ *fakeClient) { r.Resume = nil }, []string{"Resume", "snapshot"}, []string{`"resume"`}},
		{"resources", func(_ *api.Run, c *fakeClient) { c.execFn = nil }, []string{"In use"}, []string{`"resources"`}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := fullRun()
			c := &fakeClient{run: r, cost: fullCost(), execFn: cgroupOut(1)}
			tc.change(r, c)
			now := t0
			w := WithResources(context.Background(), c, NewSampler(), target(), clock(&now))
			text, j := Text(w, now), mustJSON(t, w)
			for _, g := range tc.gone {
				if strings.Contains(text, g) {
					t.Errorf("text holds %q:\n%s", g, text)
				}
			}
			for _, g := range tc.goneJ {
				if strings.Contains(j, g) {
					t.Errorf("json holds %s:\n%s", g, j)
				}
			}
			// The rest is still there.
			if !strings.Contains(text, "ghcr.io/example/dev:latest") || !strings.Contains(text, "lux.example.com") {
				t.Errorf("other fields lost:\n%s", text)
			}
		})
	}
	// A Run lux reports nothing of: only what luxmux knows.
	now := t0
	w := Fetch(context.Background(), &fakeClient{run: &api.Run{ID: "run_1"}}, target(), clock(&now))
	text := Text(w, now)
	for _, g := range []string{"0s", "$0", " 0 ", "epoch", "Placement", "Timing", "Cost", "Image"} {
		if strings.Contains(text, g) {
			t.Errorf("empty Run shows %q:\n%s", g, text)
		}
	}
	if j := mustJSON(t, w); regexp.MustCompile(`:(0|""|false|\[\]|\{\})[,}]`).MatchString(j) {
		t.Errorf("empty Run's JSON holds a zero: %s", j)
	}
}

func TestStoppedRunResumability(t *testing.T) {
	r := fullRun()
	r.State, r.StateReason, r.Stage, r.Resumable = "stopped", "stop", "stopped", true
	r.RuntimeSince, r.Host = nil, ""
	snap := "snap_7"
	r.Resume = &api.Resumability{Snapshot: &snap, Uploaded: true, OnHosts: []string{"host-2"}, Secrets: []string{"EXAMPLE_TOKEN"},
		Blockers: []string{"the tenant's Run quota is reached"}}
	c := &fakeClient{run: r, execFn: cgroupOut(1)}
	now := t0
	w := WithResources(context.Background(), c, NewSampler(), target(), clock(&now))
	text := Text(w, now)
	for _, want := range []string{"stopped (stop)", "resumable", "yes", "snap_7 (uploaded; on host-2)",
		"EXAMPLE_TOKEN (lux no longer holds their values)", "blocker", "the tenant's Run quota is reached"} {
		if !strings.Contains(text, want) {
			t.Errorf("lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "uptime") || strings.Contains(text, "In use") || c.execs.Load() != 0 {
		t.Errorf("a stopped Run has no uptime nor resources (execs %d):\n%s", c.execs.Load(), text)
	}
	r.Resume = &api.Resumability{}
	r.Resumable = false
	w = Fetch(context.Background(), c, target(), clock(&now))
	if v := lineValue(w, now, "resumable"); v != "no" || lineValue(w, now, "snapshot") != "none: it starts from scratch" {
		t.Errorf("no snapshot:\n%s", Text(w, now))
	}
}

// lineValue is the value of w's line key ("" when it has none).
func lineValue(w Window, now time.Time, key string) string {
	for _, l := range Lines(w, now) {
		if l.Key == key {
			return l.Value
		}
	}
	return ""
}

// TestCPURateFromTwoSamples: CPU in use is "—" until a second reading of
// the same placement, then the usage between them over the time between.
func TestCPURateFromTwoSamples(t *testing.T) {
	now := t0
	c := &fakeClient{run: fullRun(), execFn: cgroupOut(10_000_000)}
	s := NewSampler()
	w := WithResources(context.Background(), c, s, target(), clock(&now))
	if w.Resources == nil || w.Resources.CPUs != nil {
		t.Fatalf("first reading: %+v", w.Resources)
	}
	if !strings.Contains(Text(w, now), "— of 2 cpus") {
		t.Fatalf("first reading's text:\n%s", Text(w, now))
	}
	now = now.Add(3 * time.Second)
	c.execFn = cgroupOut(13_000_000)
	w = WithResources(context.Background(), c, s, target(), clock(&now))
	if w.Resources == nil || w.Resources.CPUs == nil || *w.Resources.CPUs != 1 {
		t.Fatalf("second reading: %+v", w.Resources)
	}
	if !strings.Contains(Text(w, now), "1.00 of 2 cpus") {
		t.Fatalf("text:\n%s", Text(w, now))
	}
	// A new placement (epoch) is a new cgroup: no rate across it.
	r := fullRun()
	r.Epoch = 4
	c.run = r
	now = now.Add(3 * time.Second)
	c.execFn = cgroupOut(100)
	if w = WithResources(context.Background(), c, s, target(), clock(&now)); w.Resources.CPUs != nil {
		t.Fatalf("rate across placements: %v", *w.Resources.CPUs)
	}
}

// TestNoRateAcrossEpochWhenUsageGrows: the new placement's usage is above
// the old one's, so only the epoch in the Sampler's key keeps the two
// samples apart.
func TestNoRateAcrossEpochWhenUsageGrows(t *testing.T) {
	now := t0
	c := &fakeClient{run: fullRun(), execFn: cgroupOut(1_000_000)}
	s := NewSampler()
	WithResources(context.Background(), c, s, target(), clock(&now))
	r := fullRun()
	r.Epoch = 4
	c.run = r
	now = now.Add(3 * time.Second)
	c.execFn = cgroupOut(4_000_000)
	if w := WithResources(context.Background(), c, s, target(), clock(&now)); w.Resources == nil || w.Resources.CPUs != nil {
		t.Fatalf("rate across placements: %+v", w.Resources)
	}
	// The same epoch again does give a rate: the first check was not
	// vacuous.
	now = now.Add(3 * time.Second)
	c.execFn = cgroupOut(7_000_000)
	if w := WithResources(context.Background(), c, s, target(), clock(&now)); w.Resources == nil || w.Resources.CPUs == nil || *w.Resources.CPUs != 1 {
		t.Fatalf("same placement: %+v", w.Resources)
	}
}

// blockingClient's Get and Cost block until their context ends, each as
// told.
type blockingClient struct {
	fakeClient
	hangGet, hangCost bool
}

func (b *blockingClient) Get(ctx context.Context, id string) (*api.Run, error) {
	if b.hangGet {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return b.fakeClient.Get(ctx, id)
}

func (b *blockingClient) Cost(ctx context.Context, id string) (*luxapi.RunCost, error) {
	if b.hangCost {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return b.fakeClient.Cost(ctx, id)
}

// TestFetchTimeoutBoundsHungLux: a Run GET or a cost lux never answers
// ends at fetchTimeout, the context given having no deadline: a hung GET
// is unreachable, a hung cost leaves the cost out.
func TestFetchTimeoutBoundsHungLux(t *testing.T) {
	old := fetchTimeout
	fetchTimeout = 100 * time.Millisecond
	t.Cleanup(func() { fetchTimeout = old })
	fetch := func(c Client) Window {
		t.Helper()
		now := t0
		got := make(chan Window, 1)
		go func() { got <- Fetch(context.Background(), c, target(), clock(&now)) }()
		select {
		case w := <-got:
			return w
		case <-time.After(5 * time.Second):
			t.Fatal("Fetch outlived fetchTimeout")
			return Window{}
		}
	}
	if w := fetch(&blockingClient{fakeClient: fakeClient{run: fullRun()}, hangGet: true}); w.Problem != ProblemUnreachable {
		t.Fatalf("hung GET: %+v", w)
	}
	w := fetch(&blockingClient{fakeClient: fakeClient{run: fullRun(), cost: fullCost()}, hangCost: true})
	if w.Problem != "" || w.Cost != nil || w.Placement == nil || w.Placement.Host != "host-3" {
		t.Fatalf("hung cost: %+v", w)
	}
}

// TestEgressPolicies: unrestricted wins over rules; no rules is deny.
func TestEgressPolicies(t *testing.T) {
	now := t0
	r := fullRun()
	r.Spec.Network = api.Network{Unrestricted: true, Egress: []api.EgressRule{{Host: "example.com"}}}
	w := Fetch(context.Background(), &fakeClient{run: r}, target(), clock(&now))
	if w.Egress == nil || w.Egress.Policy != EgressUnrestricted || w.Egress.Allow != nil || lineValue(w, now, "egress") != "unrestricted" {
		t.Fatalf("unrestricted: %+v\n%s", w.Egress, Text(w, now))
	}
	r = fullRun()
	r.Spec.Network = api.Network{}
	w = Fetch(context.Background(), &fakeClient{run: r}, target(), clock(&now))
	if w.Egress == nil || w.Egress.Policy != EgressDeny || !strings.HasPrefix(lineValue(w, now, "egress"), "none") {
		t.Fatalf("deny: %+v\n%s", w.Egress, Text(w, now))
	}
}

// TestResourcesExecFailures: an exec that fails, exits non-zero, reports
// nothing or outlives ExecTimeout leaves the section out, and the rest of
// the window is there.
func TestResourcesExecFailures(t *testing.T) {
	for name, fn := range map[string]func(ctx context.Context) ([]byte, int, error){
		"error":    func(context.Context) ([]byte, int, error) { return nil, 0, errors.New("not running") },
		"status":   func(context.Context) ([]byte, int, error) { return []byte("cpu.max 1 1\n"), 2, nil },
		"no tools": func(context.Context) ([]byte, int, error) { return []byte("cpu.max \nmemory.max \n"), 0, nil },
		"hangs":    func(ctx context.Context) ([]byte, int, error) { <-ctx.Done(); return nil, 0, ctx.Err() },
	} {
		t.Run(name, func(t *testing.T) {
			now := t0
			c := &fakeClient{run: fullRun(), execFn: fn}
			start := time.Now()
			w := WithResources(context.Background(), c, NewSampler(), target(), clock(&now))
			if d := time.Since(start); d > ExecTimeout+2*time.Second {
				t.Fatalf("took %v", d)
			}
			if w.Resources != nil || strings.Contains(Text(w, now), "In use") {
				t.Fatalf("resources kept: %+v", w.Resources)
			}
			if w.Problem != "" || w.Placement == nil {
				t.Fatalf("the rest lost: %+v", w)
			}
		})
	}
}

// TestOneExecInFlight: a second Read of a window while one is in flight
// is refused at once, not queued.
func TestOneExecInFlight(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	var first atomic.Bool
	c := &fakeClient{run: fullRun(), execFn: func(ctx context.Context) ([]byte, int, error) {
		if first.CompareAndSwap(false, true) {
			close(started)
			<-release
		}
		return cgroupOut(1)(ctx)
	}}
	s := NewSampler()
	done := make(chan error, 1)
	go func() {
		_, err := s.Read(context.Background(), c, "k", "run_1", time.Now)
		done <- err
	}()
	<-started
	if _, err := s.Read(context.Background(), c, "k", "run_1", time.Now); !errors.Is(err, ErrBusy) {
		t.Fatalf("second Read: %v", err)
	}
	if _, err := s.Read(context.Background(), c, "other", "run_2", time.Now); err != nil {
		t.Fatalf("another window's Read: %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if n := c.execs.Load(); n != 2 {
		t.Fatalf("%d execs", n)
	}
}

// TestNoSecretValue: a secret value lux would never return, planted in
// every place a Run carries text, reaches neither the text nor the JSON.
func TestNoSecretValue(t *testing.T) {
	r := fullRun()
	r.Spec.Env = map[string]string{"EXAMPLE_TOKEN": secretValue}
	r.Spec.Workload.Command = []string{"sh", "-c", "echo " + secretValue}
	r.Secrets = []api.SecretRef{{Name: "EXAMPLE_TOKEN", Fingerprint: secretValue}}
	r.Resume.Secrets = []string{"EXAMPLE_TOKEN"}
	r.Spec.Git.Repositories[0].URL = "https://x:" + secretValue + "@example.com/api.git"
	now := t0
	w := WithResources(context.Background(), &fakeClient{run: r, cost: fullCost(), execFn: cgroupOut(1)}, NewSampler(), target(), clock(&now))
	out := Text(w, now) + mustJSON(t, w) + mustJSON(t, Window{})
	if strings.Contains(out, secretValue) {
		t.Fatalf("a secret value is in the output:\n%s", out)
	}
	if !strings.Contains(out, "EXAMPLE_TOKEN") {
		t.Fatal("the secret's name is missing")
	}
}

func TestFetchProblems(t *testing.T) {
	now := t0
	for _, tc := range []struct {
		err          error
		problem, txt string
	}{
		{&api.APIError{Status: 401, Message: "bad key"}, ProblemRefused, "lux refused the key (lux.example.com · tenant example)"},
		{&api.APIError{Status: 404, Message: "gone"}, ProblemNotFound, "lux has no such Run"},
		{errors.New("dial tcp: connection refused"), ProblemUnreachable, "lux unreachable (lux.example.com · tenant example)"},
	} {
		w := Fetch(context.Background(), &fakeClient{getErr: tc.err}, target(), clock(&now))
		if w.Problem != tc.problem || !strings.Contains(w.Error, tc.txt) || w.Run != nil {
			t.Errorf("%v: %+v", tc.err, w)
		}
		if strings.Contains(Text(w, now), "EXAMPLE_LUX_KEY") {
			t.Error("the key's source named")
		}
	}
}

// TestGatherOnWindowsEndpoint: each window is read on its own lux; the
// default lux, which has another Run under the same id, is never asked.
func TestGatherOnWindowsEndpoint(t *testing.T) {
	def := config.Endpoint{URL: "https://lux.example.com"}
	other := ep()
	wrong := fullRun()
	wrong.Host = "default-lux-host"
	clients := map[config.Endpoint]*fakeClient{def: {run: wrong}, other: {run: fullRun(), execFn: cgroupOut(1)}}
	dial := func(e config.Endpoint) (Client, error) {
		if c, ok := clients[e]; ok {
			return c, nil
		}
		return nil, errors.New("no key")
	}
	now := t0
	w := Gather(context.Background(), dial, NewSampler(), target(), clock(&now))
	if w.Placement == nil || w.Placement.Host != "host-3" || clients[def].gets.Load() != 0 || clients[other].execs.Load() != 1 {
		t.Fatalf("read on the wrong lux: %+v (default gets %d)", w.Placement, clients[def].gets.Load())
	}
	tg := target()
	tg.Endpoint = config.Endpoint{URL: "https://none.example.com", KeyEnv: "UNSET"}
	if w := Gather(context.Background(), dial, nil, tg, clock(&now)); w.Problem != ProblemNoKey || strings.Contains(w.Error, "UNSET") {
		t.Fatalf("no key: %+v", w)
	}
}

func TestParseReading(t *testing.T) {
	r := ParseReading([]byte("cpu.max max 100000\nmemory.max max\nmemory.current 42\ncpu.usage_usec \ndf 100 50\n"))
	if r.CPULimit != nil || r.MemoryLimit != nil || *r.Memory != 42 || r.UsageUsec != nil || *r.DiskSize != 102400 || *r.DiskUsed != 51200 {
		t.Fatalf("%+v", r)
	}
	r = ParseReading([]byte("cpu.max 50000 100000\ngarbage\ndf  \n"))
	if *r.CPULimit != 0.5 || r.DiskSize != nil {
		t.Fatalf("%+v", r)
	}
}

func TestFormat(t *testing.T) {
	for d, want := range map[time.Duration]string{0: "0s", 59 * time.Second: "59s", 61 * time.Second: "1m 1s",
		3*time.Hour + 5*time.Minute: "3h 5m", 50 * time.Hour: "2d 2h"} {
		if got := Duration(d); got != want {
			t.Errorf("%v: %q", d, got)
		}
	}
	for n, want := range map[int64]string{900: "900", 1536: "1.5Ki", 8 << 30: "8Gi"} {
		if got := Bytes(n); got != want {
			t.Errorf("%d: %q", n, got)
		}
	}
}
