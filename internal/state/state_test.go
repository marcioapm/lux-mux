package state

import (
	"reflect"
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
		Name: "work",
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

func TestRebuild(t *testing.T) {
	runs := []*api.Run{
		run("run_3", "work", "app", "running", 30, false),
		run("run_1", "work", "default", "running", 10, false),
		run("run_x", "other", "default", "running", 5, false),
		run("run_t", "work", "default", "terminated", 1, false),
		run("run_2", "work", "app", "stopped", 20, true), // an older spare: claimed
		run("run_4", "work", "app", "running", 40, true), // the newest spare: waiting
	}
	s := Rebuild("work", runs)
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

func TestReconcile(t *testing.T) {
	s := &Session{Windows: []Window{{RunID: "a"}, {RunID: "b"}, {RunID: "c"}}, Active: 2, Last: 1,
		Spares: map[string]string{"p": "s1", "q": "s2"}}
	s.Reconcile(map[string]string{"a": "running", "b": "terminated", "c": "stopped", "s1": "running"})
	if len(s.Windows) != 2 || s.Windows[0].RunID != "a" || s.Windows[1].RunID != "c" {
		t.Fatalf("%+v", s.Windows)
	}
	if s.Active != 1 || s.Last != 0 {
		t.Fatalf("active %d last %d", s.Active, s.Last)
	}
	if !reflect.DeepEqual(s.Spares, map[string]string{"p": "s1"}) {
		t.Fatalf("spares %v", s.Spares)
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
