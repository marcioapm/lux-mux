package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/marcioapm/lux/api"

	"github.com/marcioapm/lux-mux/internal/config"
	"github.com/marcioapm/lux-mux/internal/fakelux"
	"github.com/marcioapm/lux-mux/internal/info"
	"github.com/marcioapm/lux-mux/internal/luxapi"
	"github.com/marcioapm/lux-mux/internal/state"
)

const infoSecretValue = "example-secret-value-cli-71"

// infoEnv is twoLuxEnv with session "work": window 0 "api" on a (the lux
// CLI configuration's), window 1 "web" on b (profile remote), each Run
// filled in as lux would report it.
func infoEnv(t *testing.T) (env Env, a, b *fakelux.Server, out, errb *bytes.Buffer, ids [2]string) {
	t.Helper()
	env, a, b, out, errb = twoLuxEnv(t)
	idA := submitInfo(t, a, cliKeyA, "work", "a")
	idB := submitInfo(t, b, cliKeyB, "work", "remote")
	since := time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC)
	for _, l := range []struct {
		s    *fakelux.Server
		id   string
		host string
	}{{a, idA, "host-a"}, {b, idB, "host-b"}} {
		l.s.Update(l.id, func(r *api.Run) {
			r.Host, r.Epoch, r.RuntimeSince, r.RuntimeSeconds = l.host, 2, &since, 4000
			r.Stage, r.StageSince = "running", since
		})
		l.s.SetCost(l.id, &luxapi.RunCost{Status: "complete", Totals: []api.CostTotal{{Currency: "USD", Amount: "0.75"}}})
		l.s.SetExecResponder(func(string, []string) (fakelux.ExecAnswer, bool) {
			return fakelux.ExecAnswer{Output: fakelux.ResourcesOutput("100000 100000", "1073741824", "536870912", "1000", 1024, 512)}, true
		})
	}
	bEP := config.Endpoint{URL: b.URL, Tenant: "example", KeyEnv: cliKeyEnv}
	if err := state.Save(env.StateDir, &state.Session{Name: "work", Windows: []state.Window{
		{RunID: idA, Name: "api", Profile: "a"}, {RunID: idB, Name: "web", Profile: "remote", Endpoint: &bEP}}}); err != nil {
		t.Fatal(err)
	}
	return env, a, b, out, errb, [2]string{idA, idB}
}

func submitInfo(t *testing.T, l *fakelux.Server, key, session, profile string) string {
	t.Helper()
	c, _ := luxapi.New(api.Config{URL: l.URL, APIKey: key})
	p := config.Profile{Image: "img", Secrets: []string{"EXAMPLE_TOKEN"}, Egress: []string{"example.com"}}
	sp, err := config.RunSpec(p, config.Target{Session: session, Profile: profile}, map[string]string{"EXAMPLE_TOKEN": infoSecretValue})
	if err != nil {
		t.Fatal(err)
	}
	r, err := c.Submit(context.Background(), sp)
	if err != nil {
		t.Fatal(err)
	}
	return r.ID
}

func decodeInfo(t *testing.T, b []byte) info.Output {
	t.Helper()
	var o info.Output
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&o); err != nil {
		t.Fatalf("%v:\n%s", err, b)
	}
	return o
}

func TestInfoAllWindowsText(t *testing.T) {
	env, a, b, out, errb, ids := infoEnv(t)
	if code := Main(env, []string{"info", "-t", "work"}); code != 0 {
		t.Fatalf("%d: %s", code, errb.String())
	}
	s := out.String()
	ia, ib := strings.Index(s, "0 api"), strings.Index(s, "1 web")
	if ia < 0 || ib < ia {
		t.Fatalf("windows out of order:\n%s", s)
	}
	for _, want := range []string{ids[0], ids[1], "host-a", "host-b", "2 (1 move since the first)", "0.75 USD",
		"only example.com", "EXAMPLE_TOKEN", "512Mi of 1Gi (50%)", "Placement", "Timing", "Access"} {
		if !strings.Contains(s, want) {
			t.Errorf("lacks %q", want)
		}
	}
	// Each window's Run was read on its own lux.
	if !strings.Contains(s[ia:ib], "host-a") || !strings.Contains(s[ib:], "host-b") || !strings.Contains(s[ib:], "tenant example") {
		t.Fatalf("a window read on another lux:\n%s", s)
	}
	for _, l := range []*fakelux.Server{a, b} {
		if n := len(l.Execs()); n != 1 {
			t.Errorf("%d execs on one lux", n)
		}
	}
	if strings.Contains(s, infoSecretValue) || strings.Contains(s, cliKeyEnv) {
		t.Fatalf("a secret or the key's variable is printed:\n%s", s)
	}
	noKeysIn(t, env.StateDir, s, errb.String())
}

func TestInfoJSONRoundTrip(t *testing.T) {
	env, _, _, out, errb, ids := infoEnv(t)
	if code := Main(env, []string{"info", "--json", "-t", "work"}); code != 0 {
		t.Fatalf("%d: %s", code, errb.String())
	}
	raw := out.Bytes()
	o := decodeInfo(t, raw)
	if o.SchemaVersion != info.SchemaVersion || o.Session != "work" || len(o.Windows) != 2 {
		t.Fatalf("%+v", o)
	}
	w := o.Windows[1]
	if w.Index != 1 || w.Name != "web" || w.RunID != ids[1] || w.Placement.Host != "host-b" || *w.Placement.Moves != 1 ||
		*w.Timing.RuntimeSeconds != 4000 || w.Cost.Totals[0].Amount != "0.75" || w.Egress.Policy != info.EgressAllowlist ||
		*w.Resources.MemoryBytes != 512<<20 || w.Timing.StageSince.Format(time.RFC3339) != "2026-01-02T03:00:00Z" {
		t.Fatalf("window 1: %+v", w)
	}
	// snake_case keys, numbers as numbers, times as RFC 3339.
	for _, want := range []string{`"schema_version": 1`, `"run_id": "` + ids[0], `"runtime_seconds": 4000`, `"amount": 0.75`,
		`"stage_since": "2026-01-02T03:00:00Z"`, `"memory_limit_bytes": 1073741824`} {
		if !bytes.Contains(raw, []byte(want)) {
			t.Errorf("json lacks %s", want)
		}
	}
	// Re-encoding the decoded struct gives the same document.
	again, _ := json.MarshalIndent(o, "", "  ")
	if strings.TrimSpace(string(raw)) != string(again) {
		t.Fatalf("round trip differs:\n%s\n---\n%s", raw, again)
	}
	if bytes.Contains(raw, []byte(infoSecretValue)) {
		t.Fatal("a secret value is in the JSON")
	}
}

func TestInfoSelectWindow(t *testing.T) {
	env, _, _, out, errb, ids := infoEnv(t)
	for _, args := range [][]string{{"info", "-t", "work", "1", "--json"}, {"info", "-t", "work", "web", "--json"}, {"info", "web", "--json", "-t", "work"}} {
		out.Reset()
		if code := Main(env, args); code != 0 {
			t.Fatalf("%v: %s", args, errb.String())
		}
		o := decodeInfo(t, out.Bytes())
		if len(o.Windows) != 1 || o.Windows[0].RunID != ids[1] {
			t.Fatalf("%v: %+v", args, o.Windows)
		}
	}
	for _, sel := range []string{"7", "nope"} {
		errb.Reset()
		if code := Main(env, []string{"info", "-t", "work", sel}); code == 0 || !strings.Contains(errb.String(), "no window") {
			t.Fatalf("%s: %d %q", sel, code, errb.String())
		}
	}
	// Without -t: the session of the window it runs in, by its label.
	env.Lookup = func(k string) (string, bool) {
		switch k {
		case config.EnvSession:
			return "work", true
		case cliKeyEnv:
			return cliKeyB, true
		}
		return "", false
	}
	out.Reset()
	if code := Main(env, []string{"info", "0", "--json"}); code != 0 {
		t.Fatal(errb.String())
	}
	if o := decodeInfo(t, out.Bytes()); o.Session != "work" || o.Windows[0].RunID != ids[0] {
		t.Fatalf("$LUXMUX_SESSION: %+v", o)
	}
	// A renamed session is found from its Runs' label.
	if err := state.SetRenamed(env.StateDir, "work", "job", "work"); err != nil {
		t.Fatal(err)
	}
	if s, err := infoSession(env, ""); err != nil || s != "job" {
		t.Fatalf("renamed: %q %v", s, err)
	}
}

// TestInfoOneLuxDown: a lux that does not answer marks its windows
// unreachable; the other's still print, and the exit is non-zero.
func TestInfoOneLuxDown(t *testing.T) {
	env, _, b, out, errb, ids := infoEnv(t)
	b.SetDown(true)
	if code := Main(env, []string{"info", "-t", "work", "--json"}); code == 0 || !strings.Contains(errb.String(), "1 of 2 window(s) could not be read") {
		t.Fatalf("%d: %s", code, errb.String())
	}
	o := decodeInfo(t, out.Bytes())
	if o.Windows[0].Problem != "" || o.Windows[0].Placement.Host != "host-a" {
		t.Fatalf("the healthy lux's window: %+v", o.Windows[0])
	}
	if w := o.Windows[1]; w.Problem != info.ProblemUnreachable || w.RunID != ids[1] || w.Placement != nil ||
		!strings.Contains(w.Error, "lux unreachable") || !strings.Contains(w.Error, "tenant example") {
		t.Fatalf("the down lux's window: %+v", w)
	}
	out.Reset()
	Main(env, []string{"info", "-t", "work"})
	if s := out.String(); !strings.Contains(s, "host-a") || !strings.Contains(s, "lux unreachable") {
		t.Fatalf("text:\n%s", s)
	}
}

// TestInfoHungLuxBounded: a lux that never answers costs info its own
// deadline, and the other lux's window still prints.
func TestInfoHungLuxBounded(t *testing.T) {
	old := infoTimeout
	infoTimeout = time.Second
	t.Cleanup(func() { infoTimeout = old })
	env, _, _, out, errb, _ := infoEnv(t)
	hung := config.Endpoint{URL: hangingLux(t), KeyEnv: cliKeyEnv}
	s, _, _ := state.Load(env.StateDir, "work")
	s.Windows[1].Endpoint = &hung
	if err := state.Save(env.StateDir, s); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	code := Main(env, []string{"info", "-t", "work", "--json"})
	if d := time.Since(start); d > 4*time.Second || code == 0 {
		t.Fatalf("%d after %v: %s", code, d, errb.String())
	}
	o := decodeInfo(t, out.Bytes())
	if o.Windows[0].Problem != "" || o.Windows[1].Problem != info.ProblemUnreachable {
		t.Fatalf("%+v", o.Windows)
	}
}

func TestInfoNoSession(t *testing.T) {
	env, _, _, _, errb, _ := infoEnv(t)
	if code := Main(env, []string{"info", "-t", "nope"}); code == 0 || !strings.Contains(errb.String(), "no session nope") {
		t.Fatalf("%d %q", code, errb.String())
	}
	errb.Reset()
	if code := Main(env, []string{"info", "-t", "work", "1", "2"}); code == 0 || !strings.Contains(errb.String(), "usage") {
		t.Fatalf("two windows: %d %q", code, errb.String())
	}
}
