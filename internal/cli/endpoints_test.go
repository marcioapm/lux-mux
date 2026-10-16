package cli

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/marcioapm/lux-mux/internal/config"
	"github.com/marcioapm/lux-mux/internal/fakelux"
	"github.com/marcioapm/lux-mux/internal/luxapi"
	"github.com/marcioapm/lux-mux/internal/state"
)

// Obviously fake keys, one per fake luxd; each luxd refuses the other's.
const (
	cliKeyA   = "lux_fake_key_AAAA_cli_test"
	cliKeyB   = "lux_fake_key_BBBB_cli_test"
	cliKeyEnv = "LMX_TEST_REMOTE_KEY"
)

// twoLuxEnv is an Env whose lux CLI configuration is a, with profile
// "remote" on b (its key in cliKeyEnv) and "a" on a.
func twoLuxEnv(t *testing.T) (env Env, a, b *fakelux.Server, out, errb *bytes.Buffer) {
	t.Helper()
	a = fakelux.NewWithKey(cliKeyA)
	t.Cleanup(a.Close)
	b = fakelux.NewWithKey(cliKeyB)
	b.IDPrefix = "run_b"
	t.Cleanup(b.Close)
	env, out, errb = testEnv(t, a, "")
	env.Lookup = func(k string) (string, bool) {
		if k == cliKeyEnv {
			return cliKeyB, true
		}
		return "", false
	}
	writeConfig(t, env, twoProfiles+`[profiles.remote]
image = "img-remote"
lux_url = "`+b.URL+`"
tenant = "example"
api_key_env = "`+cliKeyEnv+`"
`)
	return env, a, b, out, errb
}

func submitTo(t *testing.T, l *fakelux.Server, session, profile string) string {
	t.Helper()
	c, _ := luxapi.New(l.Config())
	sp, err := config.RunSpec(config.Profile{Image: "img"}, config.Target{Session: session, Profile: profile}, nil)
	if err != nil {
		t.Fatal(err)
	}
	r, err := c.Submit(context.Background(), sp)
	if err != nil {
		t.Fatal(err)
	}
	return r.ID
}

// noKeysIn fails if either fake key is in dir's files or in outs.
func noKeysIn(t *testing.T, dir string, outs ...string) {
	t.Helper()
	bs := make([][]byte, len(outs))
	for i, o := range outs {
		bs[i] = []byte(o)
	}
	for _, l := range fakelux.KeyLeaks([]string{cliKeyA, cliKeyB}, dir, bs...) {
		t.Errorf("%s holds a key", l)
	}
}

// hangingLux is a lux that takes every request and never answers it.
func hangingLux(t *testing.T) string {
	t.Helper()
	h := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	t.Cleanup(func() { h.CloseClientConnections(); h.Close() })
	return h.URL
}

// shortTimeouts shortens the commands' timeouts for one test: the
// requests after a listing get less time than the listing may take, so
// one that shares the listing's deadline fails.
func shortTimeouts(t *testing.T) {
	l, o, b := listTimeout, opTimeout, bulkTimeout
	listTimeout, opTimeout, bulkTimeout = time.Second, 700*time.Millisecond, 700*time.Millisecond
	t.Cleanup(func() { listTimeout, opTimeout, bulkTimeout = l, o, b })
}

// TestHungLuxCostsOneListing: a configured lux that never answers costs
// new, new-window and kill-session one listing's timeout; what they then
// do on a healthy lux has its own time.
func TestHungLuxCostsOneListing(t *testing.T) {
	shortTimeouts(t)
	a := fakelux.NewWithKey(cliKeyA)
	t.Cleanup(a.Close)
	env, out, errb := testEnv(t, a, "")
	env.Lookup = func(k string) (string, bool) { return cliKeyB, k == cliKeyEnv }
	writeConfig(t, env, twoProfiles+"[profiles.remote]\nimage = \"x\"\nlux_url = \""+hangingLux(t)+"\"\napi_key_env = \""+cliKeyEnv+"\"\n")
	const bound = 5 * time.Second

	start := time.Now()
	code := Main(env, []string{"new", "-s", "fresh", "-p", "a"})
	if d := time.Since(start); d > bound || code == 0 || !strings.Contains(errb.String(), "needs a terminal") {
		t.Fatalf("new -p a: %d after %v: %q", code, d, errb.String())
	}

	submitTo(t, a, "work", "a")
	errb.Reset()
	start = time.Now()
	code = Main(env, []string{"new-window", "-t", "work", "-p", "a"})
	if d := time.Since(start); d > bound || code != 0 || len(a.Submits()) != 2 {
		t.Fatalf("new-window -p a: %d after %v, %d submits: %q", code, d, len(a.Submits()), errb.String())
	}

	// new-window recorded the hung lux as unlisted: the file is kept.
	id := strings.TrimSpace(out.String())
	errb.Reset()
	start = time.Now()
	code = Main(env, []string{"kill-session", "-t", "work", "-y"})
	if d := time.Since(start); d > bound || code == 0 || !strings.Contains(errb.String(), "session file kept") || !slices.Contains(a.Terminated(), id) {
		t.Fatalf("kill-session: %d after %v, a terminated %v: %q", code, d, a.Terminated(), errb.String())
	}
	noKeysIn(t, env.StateDir, out.String(), errb.String())
}

// muxSession makes the next runMux record the session it was given and
// return without a terminal.
func muxSession(t *testing.T) func() *state.Session {
	t.Helper()
	var got *state.Session
	prev := startMux
	startMux = func(_ Env, _ *luxapi.Endpoints, s *state.Session, _ string, _ func(), _ *config.Profile) error {
		got = s
		return nil
	}
	t.Cleanup(func() { startMux = prev })
	return func() *state.Session { return got }
}

// TestNewOverRunsOnDownLux: new -s work while the lux holding work's Runs
// is down makes the session with that lux unlisted, so its Runs join the
// session once it answers.
func TestNewOverRunsOnDownLux(t *testing.T) {
	env, _, b, _, errb := twoLuxEnv(t)
	submitTo(t, b, "work", "remote")
	b.SetDown(true)
	got := muxSession(t)
	if code := Main(env, []string{"new", "-s", "work", "-p", "a"}); code != 0 {
		t.Fatalf("%d %q", code, errb.String())
	}
	want := config.Endpoint{URL: b.URL, Tenant: "example", KeyEnv: cliKeyEnv}
	if s := got(); s == nil || !slices.Equal(s.Unlisted, []config.Endpoint{want}) {
		t.Fatalf("session %+v", s)
	}
}

// TestKillSessionNoFileOneDown: kill-session of a session with no file
// while one of its luxes is down terminates what it can and fails naming
// the lux it could not list.
func TestKillSessionNoFileOneDown(t *testing.T) {
	env, a, b, out, errb := twoLuxEnv(t)
	near := submitTo(t, a, "work", "a")
	submitTo(t, b, "work", "remote")
	b.SetDown(true)
	code := Main(env, []string{"kill-session", "-t", "work", "-y"})
	if code == 0 || !strings.Contains(errb.String(), "may still have Runs on "+strings.TrimPrefix(b.URL, "http://")) {
		t.Fatalf("%d %q", code, errb.String())
	}
	if !slices.Equal(a.Terminated(), []string{near}) || !strings.Contains(out.String(), "terminated "+near) {
		t.Fatalf("a terminated %v; stdout %q", a.Terminated(), out.String())
	}
	// Nothing listed anywhere, one lux down: not "no session".
	errb.Reset()
	if code := Main(env, []string{"kill-session", "-t", "work", "-y"}); code == 0 || !strings.Contains(errb.String(), "may still have Runs") {
		t.Fatalf("second: %d %q", code, errb.String())
	}
	noKeysIn(t, env.StateDir, out.String(), errb.String())
}

// TestWrongKeyRefused: a lux that refuses the profile's key is reported
// as refusing it, not as unreachable, by new-window and ls; nothing is
// submitted there and the key is shown nowhere.
func TestWrongKeyRefused(t *testing.T) {
	env, a, b, out, errb := twoLuxEnv(t)
	env.Lookup = func(k string) (string, bool) { return cliKeyA, k == cliKeyEnv }
	submitTo(t, a, "work", "a")
	if code := Main(env, []string{"new-window", "-t", "work", "-p", "remote"}); code == 0 ||
		!strings.Contains(errb.String(), "refused the key") || strings.Contains(errb.String(), "unreachable") ||
		!strings.Contains(errb.String(), "invalid API key") || !strings.Contains(errb.String(), "$"+cliKeyEnv) {
		t.Fatalf("new-window: %d %q", code, errb.String())
	}
	if len(b.Submits()) != 0 || b.Refused() == 0 {
		t.Fatalf("b: %d submits, %d refused", len(b.Submits()), b.Refused())
	}
	ep := config.Endpoint{URL: b.URL, Tenant: "example", KeyEnv: cliKeyEnv}
	if err := state.Save(env.StateDir, &state.Session{Name: "there", Windows: []state.Window{{RunID: "run_b0001", Endpoint: &ep}}}); err != nil {
		t.Fatal(err)
	}
	stderr1 := errb.String()
	errb.Reset()
	if code := Main(env, []string{"ls"}); code != 0 || !strings.Contains(errb.String(), "refused the key") || strings.Contains(errb.String(), "unreachable") {
		t.Fatalf("ls: %d %q", code, errb.String())
	}
	if l := lineOf(out.String(), "there"); !strings.Contains(l, "refused the key") {
		t.Fatalf("ls:\n%s", out.String())
	}
	noKeysIn(t, env.StateDir, out.String(), stderr1, errb.String())
}

// lineOf is the line of out that starts with prefix.
func lineOf(out, prefix string) string {
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, prefix) {
			return l
		}
	}
	return ""
}

// TestNewWindowOnProfileEndpoint: new-window -p remote submits on the
// remote profile's lux, with its key and tenant, and records that
// endpoint (never the key) for the window; its labels carry no key.
func TestNewWindowOnProfileEndpoint(t *testing.T) {
	env, a, b, out, errb := twoLuxEnv(t)
	first := submitTo(t, a, "work", "a")
	if err := state.Save(env.StateDir, &state.Session{Name: "work", Windows: []state.Window{{RunID: first, Name: "a", Profile: "a"}}}); err != nil {
		t.Fatal(err)
	}
	if code := Main(env, []string{"new-window", "-t", "work", "-p", "remote", "-n", "far"}); code != 0 {
		t.Fatal(errb.String())
	}
	id := strings.TrimSpace(out.String())
	if !slices.Contains(b.RunIDs(), id) || len(a.Submits()) != 1 {
		t.Fatalf("Run %s: b has %v; a had %d submits", id, b.RunIDs(), len(a.Submits()))
	}
	for _, tn := range b.Tenants() {
		if tn != "example" {
			t.Fatalf("request to b with tenant %q", tn)
		}
	}
	s, _, _ := state.Load(env.StateDir, "work")
	if len(s.Windows) != 2 || s.Windows[1].RunID != id || s.Windows[1].Endpoint == nil ||
		*s.Windows[1].Endpoint != (config.Endpoint{URL: b.URL, Tenant: "example", KeyEnv: cliKeyEnv}) {
		t.Fatalf("file %+v", s.Windows)
	}
	for k, v := range b.Run(id).Labels {
		if strings.Contains(k+v, "fake_key") {
			t.Fatalf("label %s=%s", k, v)
		}
	}
	if a.Refused() != 0 || b.Refused() != 0 {
		t.Fatalf("refused: a %d b %d", a.Refused(), b.Refused())
	}
	noKeysIn(t, env.StateDir, out.String(), errb.String())
}

// TestNewDryRunsOnProfileEndpoint: new -p remote checks the spec with the
// remote lux's dry run (and stops at the terminal check here).
func TestNewDryRunsOnProfileEndpoint(t *testing.T) {
	env, a, b, _, errb := twoLuxEnv(t)
	if code := Main(env, []string{"new", "-s", "fresh", "-p", "remote"}); code == 0 || !strings.Contains(errb.String(), "needs a terminal") {
		t.Fatalf("%d %q", code, errb.String())
	}
	posts := func(l *fakelux.Server) int {
		n := 0
		for _, r := range l.Requests() {
			if r == "POST /v1/runs" {
				n++
			}
		}
		return n
	}
	if posts(b) != 1 || posts(a) != 0 {
		t.Fatalf("dry runs: b %d, a %d", posts(b), posts(a))
	}
}

// TestLsAcrossEndpoints: ls lists the sessions on both luxes; with one
// down it lists the other's, names the one that failed (by URL, tenant
// and key source, never the key) and marks the sessions with windows
// there.
func TestLsAcrossEndpoints(t *testing.T) {
	env, a, b, out, errb := twoLuxEnv(t)
	here := submitTo(t, a, "here", "a")
	far := submitTo(t, b, "there", "remote")
	if code := Main(env, []string{"ls"}); code != 0 {
		t.Fatal(errb.String())
	}
	if !strings.Contains(out.String(), "here") || !strings.Contains(out.String(), "there") {
		t.Fatalf("ls:\n%s", out.String())
	}
	ep := config.Endpoint{URL: b.URL, Tenant: "example", KeyEnv: cliKeyEnv}
	if err := state.Save(env.StateDir, &state.Session{Name: "there", Windows: []state.Window{{RunID: far, Name: "remote", Profile: "remote", Endpoint: &ep}}}); err != nil {
		t.Fatal(err)
	}
	if err := state.Save(env.StateDir, &state.Session{Name: "here", Windows: []state.Window{{RunID: here, Name: "a", Profile: "a"}}}); err != nil {
		t.Fatal(err)
	}
	b.SetDown(true)
	out.Reset()
	errb.Reset()
	if code := Main(env, []string{"ls"}); code != 0 {
		t.Fatal(errb.String())
	}
	if !strings.Contains(out.String(), "here") || !strings.Contains(errb.String(), "endpoint unreachable") ||
		!strings.Contains(errb.String(), strings.TrimPrefix(b.URL, "http://")+" · tenant example") || !strings.Contains(errb.String(), "$"+cliKeyEnv) {
		t.Fatalf("ls with b down:\n%s\nstderr: %s", out.String(), errb.String())
	}
	if !strings.Contains(lineOf(out.String(), "there"), "endpoint unreachable") {
		t.Fatalf("session there not marked:\n%s", out.String())
	}
	if l := lineOf(out.String(), "here"); l == "" || strings.Contains(l, "endpoint") {
		t.Fatalf("session here, with no window on b, marked:\n%s", out.String())
	}
	noKeysIn(t, env.StateDir, out.String(), errb.String())
}

// TestKillSessionAcrossEndpoints: kill-session terminates the session's
// Runs on each lux through that lux's client.
func TestKillSessionAcrossEndpoints(t *testing.T) {
	env, a, b, _, errb := twoLuxEnv(t)
	near := submitTo(t, a, "work", "a")
	far := submitTo(t, b, "work", "remote")
	if code := Main(env, []string{"kill-session", "-t", "work", "-y"}); code != 0 {
		t.Fatal(errb.String())
	}
	if !slices.Equal(a.Terminated(), []string{near}) || !slices.Equal(b.Terminated(), []string{far}) {
		t.Fatalf("terminated: a %v, b %v", a.Terminated(), b.Terminated())
	}
	if a.Refused() != 0 || b.Refused() != 0 {
		t.Fatalf("refused: a %d b %d", a.Refused(), b.Refused())
	}

	// A file with a window on b while b is down is kept; a's Runs go.
	near2 := submitTo(t, a, "work2", "a")
	far2 := submitTo(t, b, "work2", "remote")
	ep := config.Endpoint{URL: b.URL, Tenant: "example", KeyEnv: cliKeyEnv}
	if err := state.Save(env.StateDir, &state.Session{Name: "work2", Windows: []state.Window{{RunID: far2, Endpoint: &ep}}}); err != nil {
		t.Fatal(err)
	}
	b.SetDown(true)
	if code := Main(env, []string{"kill-session", "-t", "work2", "-y"}); code == 0 || !strings.Contains(errb.String(), "session file kept") {
		t.Fatalf("%d %q", code, errb.String())
	}
	if _, ok, _ := state.Load(env.StateDir, "work2"); !ok || !slices.Contains(a.Terminated(), near2) {
		t.Fatalf("file kept %v, a terminated %v", ok, a.Terminated())
	}
	// A configured profile whose key variable is unset (here: b's, with
	// b back) does not keep the file of a session with nothing there.
	b.SetDown(false)
	env.Lookup = func(string) (string, bool) { return "", false }
	near3 := submitTo(t, a, "work3", "a")
	_ = state.Save(env.StateDir, &state.Session{Name: "work3", Windows: []state.Window{{RunID: near3}}})
	errb.Reset()
	if code := Main(env, []string{"kill-session", "-t", "work3", "-y"}); code != 0 {
		t.Fatalf("%d %q", code, errb.String())
	}
	if _, ok, _ := state.Load(env.StateDir, "work3"); ok {
		t.Fatal("work3's file kept")
	}
}

// TestAttachRecordsExternalOnB: a Run new-window -p remote made on b
// while the session was attached elsewhere is recorded by the next
// attach with b's endpoint, not the default lux's.
func TestAttachRecordsExternalOnB(t *testing.T) {
	env, a, b, out, errb := twoLuxEnv(t)
	near := submitTo(t, a, "work", "a")
	def := config.Endpoint{URL: a.URL}
	if err := state.Save(env.StateDir, &state.Session{Name: "work", Windows: []state.Window{{RunID: near, Profile: "a", Endpoint: &def}}}); err != nil {
		t.Fatal(err)
	}
	unlock, err := state.Lock(env.StateDir, "work")
	if err != nil {
		t.Fatal(err)
	}
	if code := Main(env, []string{"new-window", "-t", "work", "-p", "remote", "-n", "far"}); code != 0 {
		unlock()
		t.Fatalf("%d %q", code, errb.String())
	}
	unlock()
	far := strings.TrimSpace(out.String())
	got := muxSession(t)
	if code := Main(env, []string{"attach", "-t", "work"}); code != 0 {
		t.Fatalf("%d %q", code, errb.String())
	}
	want := config.Endpoint{URL: b.URL, Tenant: "example", KeyEnv: cliKeyEnv}
	s := got()
	if s == nil || len(s.Windows) != 2 || s.Windows[1].RunID != far || s.Windows[1].Endpoint == nil || *s.Windows[1].Endpoint != want {
		t.Fatalf("session %+v", s)
	}
}

// TestAttachSkipsRecordedLeftover: a new-window Run the file records as a
// leftover (replaced by r, not yet terminated) is not made a window again
// by attach, and stays recorded as the leftover.
func TestAttachSkipsRecordedLeftover(t *testing.T) {
	env, a, b, out, errb := twoLuxEnv(t)
	near := submitTo(t, a, "work", "a")
	def := config.Endpoint{URL: a.URL}
	if err := state.Save(env.StateDir, &state.Session{Name: "work", Windows: []state.Window{{RunID: near, Profile: "a", Endpoint: &def}}}); err != nil {
		t.Fatal(err)
	}
	if code := Main(env, []string{"new-window", "-t", "work", "-p", "remote", "-n", "far"}); code != 0 {
		t.Fatalf("%d %q", code, errb.String())
	}
	far := strings.TrimSpace(out.String())
	if far == "" || b.Run(far).Labels[config.LabelExternal] != "true" {
		t.Fatalf("new-window made %q on b", far)
	}
	ep := config.Endpoint{URL: b.URL, Tenant: "example", KeyEnv: cliKeyEnv}
	s, _, _ := state.Load(env.StateDir, "work")
	s.Windows = s.Windows[:1]
	s.SetSpare("leftover:"+far, far, ep)
	if err := state.Save(env.StateDir, s); err != nil {
		t.Fatal(err)
	}
	got := muxSession(t)
	if code := Main(env, []string{"attach", "-t", "work"}); code != 0 {
		t.Fatalf("%d %q", code, errb.String())
	}
	s = got()
	if s == nil || len(s.Windows) != 1 || s.Windows[0].RunID != near || s.Spares["leftover:"+far] != far {
		t.Fatalf("session %+v", s)
	}
}

// TestAttachWithEndpointDown: attach of a session whose file is gone and
// whose only Runs are on a lux that does not answer makes no window in
// their place, and says which lux failed.
func TestAttachWithEndpointDown(t *testing.T) {
	env, a, b, _, errb := twoLuxEnv(t)
	submitTo(t, b, "far", "remote")
	b.SetDown(true)
	if code := Main(env, []string{"attach", "-t", "far"}); code == 0 || !strings.Contains(errb.String(), "endpoint unreachable") ||
		!strings.Contains(errb.String(), "try again") {
		t.Fatalf("%d %q", code, errb.String())
	}
	if len(a.Submits()) != 0 {
		t.Fatal("a window made in place of the unreachable ones")
	}
}

// TestKeyFileModeRefusedByCLI: a profile whose api_key_file others may
// read is refused with the fix, the key not shown.
func TestKeyFileModeRefusedByCLI(t *testing.T) {
	env, a, b, out, errb := twoLuxEnv(t)
	key := filepath.Join(t.TempDir(), "remote.key")
	if err := os.WriteFile(key, []byte(cliKeyB+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(key, 0o644); err != nil {
		t.Fatal(err)
	}
	writeConfig(t, env, twoProfiles+`[profiles.remote]
image = "img-remote"
lux_url = "`+b.URL+`"
api_key_file = "`+key+`"
`)
	submitTo(t, a, "work", "a")
	if code := Main(env, []string{"new-window", "-t", "work", "-p", "remote"}); code == 0 || !strings.Contains(errb.String(), "chmod 600") {
		t.Fatalf("%d %q", code, errb.String())
	}
	if len(b.Submits()) != 0 {
		t.Fatal("submitted with a key file others can read")
	}
	if err := os.Chmod(key, 0o600); err != nil {
		t.Fatal(err)
	}
	errb.Reset()
	if code := Main(env, []string{"new-window", "-t", "work", "-p", "remote"}); code != 0 || len(b.Submits()) != 1 {
		t.Fatalf("0600: %d %q", code, errb.String())
	}
	noKeysIn(t, env.StateDir, out.String(), errb.String())
}
