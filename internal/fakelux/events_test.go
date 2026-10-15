package fakelux

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/marcioapm/lux/api"
)

// luxd v0.1.30's submitted event, copied from internal/server/api.go
// (submitRun): addEvent(…, "submitted", ev) with ev = {by, pool, poolFrom}
// plus poolOwner for a pool with an id and size on a shaped pool; poolFrom
// is one of the poolFrom* constants there.
var (
	luxdSubmittedRequired = []string{"by", "pool", "poolFrom"}
	luxdSubmittedOptional = []string{"poolOwner", "size"}
	luxdPoolFrom          = []string{"spec", "tenant-default", "platform-default", "fallback"}
)

// feedEvents reads the feed from its start until n events came.
func feedEvents(t *testing.T, s *Server, n int) []api.FeedEvent {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, s.URL+"/v1/events?after=0", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out []api.FeedEvent
	sc := bufio.NewScanner(resp.Body)
	for len(out) < n && sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data: ")
		if !ok {
			continue
		}
		var e api.FeedEvent
		if err := json.Unmarshal([]byte(data), &e); err != nil {
			t.Fatal(err)
		}
		out = append(out, e)
	}
	if len(out) < n {
		t.Fatalf("%d events, want %d: %v", len(out), n, sc.Err())
	}
	return out
}

// TestSubmitEventsAsLuxd: a submit is announced as luxd announces it: an
// event of type submitted with luxd's data, no epoch, and no state event;
// then the waiting stage. The Run answered is already submitted.
func TestSubmitEventsAsLuxd(t *testing.T) {
	yes := true
	for _, c := range []struct {
		name, pool, wantPool, wantFrom string
		owner                          bool
	}{
		{"no pool, none default", "", "default", "fallback", false},
		{"named", "gpu", "gpu", "spec", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := New()
			defer s.Close()
			s.Hold = true
			s.SetPools([]api.Pool{{Name: "gpu"}})
			body := `{"image":{"ref":"alpine"},"placement":{"pool":"` + c.pool + `"}}`
			resp, err := http.Post(s.URL+"/v1/runs", "application/json", strings.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			var ans api.SubmitAnswer
			_ = json.NewDecoder(resp.Body).Decode(&ans)
			resp.Body.Close()
			if resp.StatusCode != 201 || ans.Run == nil || ans.Run.State != "submitted" || ans.Run.Pool != c.wantPool {
				t.Fatalf("answer %d %+v", resp.StatusCode, ans.Run)
			}
			evs := feedEvents(t, s, 2)
			sub, stage := evs[0], evs[1]
			if sub.Type != "submitted" || sub.RunID == nil || *sub.RunID != ans.Run.ID || sub.Epoch != nil {
				t.Fatalf("first event %+v", sub)
			}
			for _, k := range luxdSubmittedRequired {
				if _, ok := sub.Data[k]; !ok {
					t.Errorf("submitted lacks %q: %v", k, sub.Data)
				}
			}
			for k := range sub.Data {
				if !slices.Contains(luxdSubmittedRequired, k) && !slices.Contains(luxdSubmittedOptional, k) {
					t.Errorf("submitted has %q, which luxd never sends", k)
				}
			}
			if from, _ := sub.Data["poolFrom"].(string); from != c.wantFrom || !slices.Contains(luxdPoolFrom, from) {
				t.Errorf("poolFrom %v", sub.Data["poolFrom"])
			}
			if sub.Data["pool"] != c.wantPool {
				t.Errorf("pool %v", sub.Data["pool"])
			}
			if _, ok := sub.Data["poolOwner"]; ok != c.owner {
				t.Errorf("poolOwner %v", sub.Data)
			}
			if stage.Type != "stage" || stage.Data["stage"] != "waiting" {
				t.Fatalf("second event %+v", stage)
			}
		})
	}
	// The tenant's default pool.
	s := New()
	defer s.Close()
	s.Hold = true
	s.SetPools([]api.Pool{{Name: "m", IsDefault: &yes}})
	resp, err := http.Post(s.URL+"/v1/runs", "application/json", strings.NewReader(`{"image":{"ref":"alpine"}}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if e := feedEvents(t, s, 1)[0]; e.Data["pool"] != "m" || e.Data["poolFrom"] != "tenant-default" {
		t.Fatalf("default pool: %v", e.Data)
	}
}
