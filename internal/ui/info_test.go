package ui

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/marcioapm/lux-mux/internal/config"
	"github.com/marcioapm/lux-mux/internal/info"
)

// fakeInfoHost shows target; each FetchInfo records its context and
// answers what the test sends on answer (or its context's end).
type fakeInfoHost struct {
	mu      sync.Mutex
	target  info.Target
	ok      bool
	calls   []context.Context
	targets []info.Target
	answer  chan info.Window
}

func newInfoHost() *fakeInfoHost {
	return &fakeInfoHost{ok: true, answer: make(chan info.Window, 4), target: info.Target{Index: 2, Name: "api", Profile: "dev",
		Session: "work", Label: "work", RunID: "run_1",
		Endpoint: config.Endpoint{URL: "https://lux.example.com", Tenant: "example", KeyEnv: "EXAMPLE_LUX_KEY"}}}
}

func (f *fakeInfoHost) InfoTarget() (info.Target, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.target, f.ok
}

func (f *fakeInfoHost) FetchInfo(ctx context.Context, t info.Target) info.Window {
	f.mu.Lock()
	f.calls = append(f.calls, ctx)
	f.targets = append(f.targets, t)
	f.mu.Unlock()
	select {
	case w := <-f.answer:
		return w
	case <-ctx.Done():
		w := info.Base(t)
		w.Problem, w.Error = info.ProblemUnreachable, "cancelled"
		return w
	}
}

func (f *fakeInfoHost) nCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeInfoHost) ctx(i int) context.Context {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[i]
}

func (f *fakeInfoHost) setTarget(t info.Target) {
	f.mu.Lock()
	f.target = t
	f.mu.Unlock()
}

// scheduled is a tick the fake clock holds until the test fires it.
type scheduled struct {
	d   time.Duration
	msg tea.Msg
}

// infoRig runs an InfoDialog without a terminal: a fake clock for its
// ticks and its "now", and its commands run as bubbletea runs them, each
// in a goroutine of its own, their messages collected.
type infoRig struct {
	t    *testing.T
	d    *InfoDialog
	host *fakeInfoHost
	now  time.Time
	msgs chan tea.Msg
	// detach cancels the dialog's parent context, as a detach does.
	detach context.CancelFunc
	// ticks are the ticks scheduled and not yet fired.
	ticks []scheduled
}

func newInfoRig(t *testing.T, rows, cols int) *infoRig {
	r := &infoRig{t: t, host: newInfoHost(), now: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), msgs: make(chan tea.Msg, 64)}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	r.detach = cancel
	r.d = NewInfoDialog(ctx, r.host, "ctrl+b", func(int, int) []string { return []string{"$ shell behind"} }, rows, cols)
	r.d.now = func() time.Time { return r.now }
	r.d.after = func(d time.Duration, msg tea.Msg) tea.Cmd {
		return func() tea.Msg { return scheduled{d, msg} }
	}
	r.run(r.d.Init())
	return r
}

// run runs cmd's leaves in goroutines; their messages arrive on msgs.
func (r *infoRig) run(cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	go func() {
		msg := cmd()
		if b, ok := msg.(tea.BatchMsg); ok {
			for _, c := range b {
				r.run(c)
			}
			return
		}
		if msg != nil {
			if _, quit := msg.(tea.QuitMsg); quit {
				return
			}
			r.msgs <- msg
		}
	}()
}

// settle takes the commands' messages until none comes for a moment: a
// scheduled tick is kept, anything else is given to the dialog.
func (r *infoRig) settle() {
	r.t.Helper()
	for {
		select {
		case m := <-r.msgs:
			if s, ok := m.(scheduled); ok {
				r.ticks = append(r.ticks, s)
				continue
			}
			r.update(m)
		case <-time.After(50 * time.Millisecond):
			return
		}
	}
}

func (r *infoRig) update(m tea.Msg) {
	_, cmd := r.d.Update(m)
	r.run(cmd)
}

// advance moves the fake clock by d and fires the ticks due.
func (r *infoRig) advance(d time.Duration) {
	r.t.Helper()
	r.settle()
	r.now = r.now.Add(d)
	due := r.ticks
	r.ticks = nil
	for _, s := range due {
		if s.d != InfoEvery {
			r.t.Fatalf("a tick of %v, want %v", s.d, InfoEvery)
		}
		r.update(s.msg)
	}
	r.settle()
}

func (r *infoRig) answer(w info.Window) {
	r.host.answer <- w
	r.settle()
}

func (r *infoRig) view() string { return plain(r.d.View()) }

func (r *infoRig) waitCalls(n int) {
	r.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for r.host.nCalls() < n {
		if time.Now().After(deadline) {
			r.t.Fatalf("%d reads, want %d", r.host.nCalls(), n)
		}
		time.Sleep(time.Millisecond)
	}
}

// goodWindow is what lux answers for the target, as of the rig's now.
func (r *infoRig) goodWindow(host string) info.Window {
	w := info.Base(r.host.target)
	at := r.now
	moves := 1
	w.FetchedAt = &at
	w.Run = &info.Run{State: "running"}
	w.Image = &info.Image{Ref: "ghcr.io/example/dev:latest"}
	w.Placement = &info.Placement{Pool: "m", Host: host, Epoch: 2, Moves: &moves}
	w.Secrets = []string{"EXAMPLE_TOKEN"}
	return w
}

// TestInfoFirstFrame: before lux answers, the dialog shows what luxmux
// knows (window, profile, Run, lux) and that it is reading, not the key.
func TestInfoFirstFrame(t *testing.T) {
	r := newInfoRig(t, 30, 100)
	v := r.view()
	for k, want := range map[string]string{"window": "2 api", "profile": "dev", "session": "work", "run": "run_1",
		"lux": "lux.example.com · tenant example"} {
		if got := lineOf(v, k); got != want {
			t.Errorf("first frame's %s: %q, want %q:\n%s", k, got, want, v)
		}
	}
	for _, want := range []string{"Window 2 · api", "reading from lux…", "shell behind"} {
		if !strings.Contains(v, want) {
			t.Errorf("first frame lacks %q:\n%s", want, v)
		}
	}
	if strings.Contains(v, "EXAMPLE_LUX_KEY") || strings.Contains(v, "Placement") {
		t.Fatalf("first frame:\n%s", v)
	}
	r.waitCalls(1)
}

// TestInfoFieldsShownAndOmitted: an answer's fields show; those it lacks
// have no line.
func TestInfoFieldsShownAndOmitted(t *testing.T) {
	r := newInfoRig(t, 40, 100)
	r.waitCalls(1)
	r.answer(r.goodWindow("host-7"))
	v := r.view()
	for k, want := range map[string]string{"host": "host-7", "epoch": "2 (1 move since the first)", "ref": "ghcr.io/example/dev:latest",
		"secrets": "EXAMPLE_TOKEN", "pool": "m"} {
		if got := lineOf(v, k); got != want {
			t.Errorf("%s: %q, want %q:\n%s", k, got, want, v)
		}
	}
	for _, want := range []string{"Placement", "read 0s ago"} {
		if !strings.Contains(v, want) {
			t.Errorf("lacks %q:\n%s", want, v)
		}
	}
	for _, gone := range []string{"Cost", "Timing", "Resume", "In use", "uptime", "$0", "0s (", "reading from lux"} {
		if strings.Contains(v, gone) {
			t.Errorf("shows %q:\n%s", gone, v)
		}
	}
	w := r.goodWindow("")
	w.Placement.Host = ""
	r.advance(InfoEvery)
	r.answer(w)
	if v := r.view(); strings.Contains(v, "host-7") || lineOf(v, "host") != "" {
		t.Fatalf("a host lux no longer reports is still shown:\n%s", v)
	}
}

// TestInfoRefreshEvery3s: a read every InfoEvery of the fake clock, never
// two at once: a tick while one is in flight starts none.
func TestInfoRefreshEvery3s(t *testing.T) {
	r := newInfoRig(t, 30, 100)
	r.waitCalls(1)
	r.settle()
	if len(r.ticks) != 1 {
		t.Fatalf("%d ticks after Init", len(r.ticks))
	}
	// The first read still in flight: the tick starts no second one.
	r.advance(InfoEvery)
	if n := r.host.nCalls(); n != 1 {
		t.Fatalf("%d reads with one in flight", n)
	}
	if len(r.ticks) != 1 {
		t.Fatalf("the tick did not schedule the next: %d", len(r.ticks))
	}
	r.answer(r.goodWindow("h1"))
	for i := 2; i <= 4; i++ {
		r.advance(InfoEvery)
		r.waitCalls(i)
		r.answer(r.goodWindow("h1"))
		if n := r.host.nCalls(); n != i {
			t.Fatalf("after %d ticks: %d reads", i, n)
		}
	}
	// No time passes: no tick fires, no read starts.
	r.settle()
	if n := r.host.nCalls(); n != 4 {
		t.Fatalf("%d reads without the clock moving", n)
	}
}

// TestInfoCloseCancelsAndDropsLate: closing cancels the read in flight,
// its late answer is dropped, and nothing is read or scheduled again.
func TestInfoCloseCancelsAndDropsLate(t *testing.T) {
	r := newInfoRig(t, 30, 100)
	r.waitCalls(1)
	r.answer(r.goodWindow("before-close"))
	r.advance(InfoEvery)
	r.waitCalls(2)
	ctx := r.host.ctx(1)
	_, cmd := r.d.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if !r.d.Done || cmd == nil {
		t.Fatal("esc did not close")
	}
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("the read in flight was not cancelled by the close")
	}
	// The late answer (the cancelled read's, or one lux sent anyway).
	r.d.Update(infoResultMsg{gen: r.d.gen, w: r.goodWindow("late")})
	r.settle()
	// A closed dialog renders nothing; reopen its view to see its state.
	r.d.Done = false
	v := r.view()
	r.d.Done = true
	if lineOf(v, "host") != "before-close" {
		t.Fatalf("a late answer was applied after the close:\n%s", v)
	}
	before := r.host.nCalls()
	r.advance(InfoEvery)
	r.advance(InfoEvery)
	if n := r.host.nCalls(); n != before || len(r.ticks) != 0 {
		t.Fatalf("reads %d→%d, %d ticks after the close", before, n, len(r.ticks))
	}
}

// TestInfoWindowSwitchDropsLate: another window shown cancels the read
// of the previous one, whose answer is then dropped; the next read is of
// the window now shown.
func TestInfoWindowSwitchDropsLate(t *testing.T) {
	r := newInfoRig(t, 30, 100)
	r.waitCalls(1)
	r.answer(r.goodWindow("old-host"))
	r.advance(InfoEvery)
	r.waitCalls(2)
	ctx := r.host.ctx(1)
	oldGen := r.d.gen
	next := r.host.target
	next.Index, next.Name, next.RunID = 0, "web", "run_9"
	r.host.setTarget(next)
	r.advance(InfoEvery)
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("the previous window's read was not cancelled")
	}
	r.waitCalls(3)
	if tg := r.host.targets[2]; tg.RunID != "run_9" {
		t.Fatalf("read %+v", tg)
	}
	v := r.view()
	if strings.Contains(v, "old-host") || lineOf(v, "window") != "0 web" || lineOf(v, "run") != "run_9" || lineOf(v, "host") != "" {
		t.Fatalf("after the switch:\n%s", v)
	}
	r.d.Update(infoResultMsg{gen: oldGen, w: r.goodWindow("old-late")})
	if v := r.view(); strings.Contains(v, "old-late") || lineOf(v, "host") != "" {
		t.Fatalf("the previous window's late answer applied:\n%s", v)
	}
	w := r.goodWindow("new-host")
	r.answer(w)
	if v := r.view(); lineOf(v, "host") != "new-host" {
		t.Fatalf("the new window's answer:\n%s", v)
	}
}

// TestInfoSwitchSeenByResultKeepsRefreshing: a switch noticed when a
// read's answer arrives (not on a tick) still leaves the dialog reading
// the new window every InfoEvery.
func TestInfoSwitchSeenByResultKeepsRefreshing(t *testing.T) {
	r := newInfoRig(t, 30, 100)
	r.waitCalls(1)
	next := r.host.target
	next.Index, next.Name, next.RunID = 0, "web", "run_9"
	r.host.setTarget(next)
	r.answer(r.goodWindow("old-host"))
	r.waitCalls(2)
	r.answer(r.goodWindow("new-host"))
	for i := 3; i <= 6; i++ {
		r.advance(InfoEvery)
		r.waitCalls(i)
		r.answer(r.goodWindow("new-host"))
	}
	if tg := r.host.targets[5]; tg.RunID != "run_9" {
		t.Fatalf("read %+v", tg)
	}
	if v := r.view(); lineOf(v, "host") != "new-host" {
		t.Fatalf("after the switch:\n%s", v)
	}
}

// TestInfoEndpointChangeIsASwitch: the shown window keeping its index and
// Run id but on another lux (a Run id is only unique on its lux) is
// another window: the read in flight is cancelled, its answer dropped,
// and the next read asks the new lux.
func TestInfoEndpointChangeIsASwitch(t *testing.T) {
	r := newInfoRig(t, 30, 100)
	r.waitCalls(1)
	r.answer(r.goodWindow("old-lux-host"))
	r.advance(InfoEvery)
	r.waitCalls(2)
	ctx := r.host.ctx(1)
	oldGen := r.d.gen
	next := r.host.target
	next.Endpoint.URL = "https://other.example.com"
	r.host.setTarget(next)
	r.advance(InfoEvery)
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("the read on the previous lux was not cancelled")
	}
	r.waitCalls(3)
	if tg := r.host.targets[2]; tg.Endpoint.URL != "https://other.example.com" {
		t.Fatalf("read %+v", tg)
	}
	r.d.Update(infoResultMsg{gen: oldGen, w: r.goodWindow("old-late")})
	if v := r.view(); lineOf(v, "host") != "" || lineOf(v, "lux") != "other.example.com · tenant example" {
		t.Fatalf("after the lux changed:\n%s", v)
	}
}

// lineOf is the value of the view's body line keyed k ("" if none).
func lineOf(view, k string) string {
	for _, l := range strings.Split(view, "\n") {
		l = strings.Trim(l, "│ ")
		if key, v, ok := strings.Cut(l, "  "); ok && key == k {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// TestInfoDetachCancelsRead: the session ending (the dialog's parent
// context cancelled) cancels the read in flight, and no tick starts
// another.
func TestInfoDetachCancelsRead(t *testing.T) {
	r := newInfoRig(t, 30, 100)
	r.waitCalls(1)
	r.detach()
	select {
	case <-r.host.ctx(0).Done():
	case <-time.After(time.Second):
		t.Fatal("the read in flight outlived the session")
	}
	r.settle()
	r.advance(InfoEvery)
	r.advance(InfoEvery)
	if n := r.host.nCalls(); n != 1 {
		t.Fatalf("%d reads after the session ended", n)
	}
}

// TestInfoErrorKeepsLastValues: a failed read says why in one line and
// keeps the last good values, with their age.
func TestInfoErrorKeepsLastValues(t *testing.T) {
	r := newInfoRig(t, 30, 100)
	r.waitCalls(1)
	r.answer(r.goodWindow("kept-host"))
	r.advance(InfoEvery)
	r.waitCalls(2)
	bad := info.Base(r.host.target)
	bad.Problem, bad.Error = info.ProblemUnreachable, "lux unreachable (lux.example.com · tenant example)"
	r.answer(bad)
	v := r.view()
	if lineOf(v, "host") != "kept-host" || !strings.Contains(v, "lux unreachable (lux.example.com · tenant example)") || !strings.Contains(v, "shown as read 3s ago") {
		t.Fatalf("after a failed read:\n%s", v)
	}
	r.advance(InfoEvery)
	r.waitCalls(3)
	bad.Problem, bad.Error = info.ProblemRefused, "lux refused the key (lux.example.com · tenant example)"
	r.answer(bad)
	if v := r.view(); !strings.Contains(v, "refused the key") || strings.Contains(v, "unreachable") || lineOf(v, "host") != "kept-host" || !strings.Contains(v, "6s ago") {
		t.Fatalf("refused:\n%s", v)
	}
	r.advance(InfoEvery)
	r.waitCalls(4)
	r.answer(r.goodWindow("fresh-host"))
	if v := r.view(); strings.Contains(v, "refused") || lineOf(v, "host") != "fresh-host" {
		t.Fatalf("recovered:\n%s", v)
	}
	if strings.Contains(v, "EXAMPLE_LUX_KEY") {
		t.Fatal("the key's variable is shown")
	}
}

// TestInfoSmallTerminal: on a small terminal every row fits its width,
// the body scrolls instead of wrapping, and a resize lays it out again.
func TestInfoSmallTerminal(t *testing.T) {
	r := newInfoRig(t, 12, 40)
	r.waitCalls(1)
	w := r.goodWindow(strings.Repeat("very-long-host-name-", 6))
	w.Workload = &info.Workload{Workdir: "/workspace/" + strings.Repeat("deep/", 20)}
	r.answer(w)
	check := func(rows, cols int) {
		t.Helper()
		lines := strings.Split(r.d.View(), "\n")
		if len(lines) != rows {
			t.Fatalf("%dx%d: %d rows", cols, rows, len(lines))
		}
		for _, l := range lines {
			if n := ansi.StringWidth(l); n > cols {
				t.Fatalf("%dx%d: a row %d wide: %q", cols, rows, n, ansi.Strip(l))
			}
		}
	}
	check(12, 40)
	if !strings.Contains(r.view(), "of ") || r.d.maxScroll() == 0 {
		t.Fatalf("no scrolling on 40x12:\n%s", r.view())
	}
	r.update(tea.KeyMsg{Type: tea.KeyPgDown})
	if r.d.scroll == 0 {
		t.Fatal("pgdown did not scroll")
	}
	check(12, 40)
	r.update(tea.WindowSizeMsg{Width: 30, Height: 10})
	if !strings.Contains(r.view(), "too small") {
		t.Fatalf("30x10:\n%s", r.view())
	}
	r.update(tea.WindowSizeMsg{Width: 120, Height: 50})
	check(50, 120)
	if col, _ := boxLeft(t, r.d.View()); col != (120-boxWidth(120))/2 {
		t.Fatalf("not centred after resize: %d", col)
	}
	if r.d.scroll != 0 {
		t.Fatalf("scroll %d where all fits", r.d.scroll)
	}
}

func TestInfoCloseKeys(t *testing.T) {
	for _, keys := range [][]tea.KeyMsg{{{Type: tea.KeyEsc}}, {runes("q")}, {{Type: tea.KeyCtrlB}, runes("i")}} {
		r := newInfoRig(t, 30, 100)
		for _, k := range keys {
			r.update(k)
		}
		if !r.d.Done {
			t.Fatalf("%v did not close", keys)
		}
	}
	r := newInfoRig(t, 30, 100)
	for _, k := range []tea.KeyMsg{runes("i"), runes("x"), {Type: tea.KeyEnter}, {Type: tea.KeyCtrlB}, runes("j")} {
		r.update(k)
	}
	if r.d.Done {
		t.Fatal("closed by another key")
	}
}

// TestInfoKeysWhileReadHangs: the dialog takes keys while its read hangs
// in lux (the read is a command of its own goroutine), and Esc closes it
// at once, the read cancelled.
func TestInfoKeysWhileReadHangs(t *testing.T) {
	host := newInfoHost()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := NewInfoDialog(ctx, host, "ctrl+b", nil, 30, 100)
	pr, pw := io.Pipe()
	p := tea.NewProgram(d, tea.WithInput(pr), tea.WithOutput(io.Discard), tea.WithoutSignalHandler(), tea.WithoutRenderer())
	done := make(chan struct{})
	go func() { _, _ = p.Run(); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for host.nCalls() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	start := time.Now()
	_, _ = pw.Write([]byte("jjk"))
	_, _ = pw.Write([]byte("\x1b"))
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the dialog did not close while its read hung")
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("closing took %v", d)
	}
	select {
	case <-host.ctx(0).Done():
	case <-time.After(time.Second):
		t.Fatal("the hung read was not cancelled")
	}
}
