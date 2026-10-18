package fakelux

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/marcioapm/lux-mux/internal/luxapi"
)

// TestRename: PATCH renames a Run in any state and answers it; names lux
// refuses are refused; FailRenames makes it answer as an old or a failing
// lux, or hang.
func TestRename(t *testing.T) {
	s := New()
	defer s.Close()
	c, id := submitted(t, s)
	ctx := context.Background()
	r, err := c.Rename(ctx, id, "s/api")
	if err != nil || r.Name != "s/api" || r.ID != id || s.Run(id).Name != "s/api" {
		t.Fatalf("rename: %+v %v", r, err)
	}
	s.SetState(id, "stopped", "stop")
	if r, err := c.Rename(ctx, id, ""); err != nil || r.Name != "" {
		t.Fatalf("clear a stopped Run's name: %+v %v", r, err)
	}
	// Over raw HTTP: the client reads a 422 as a lux that can't rename.
	for _, bad := range []string{strings.Repeat("x", 201), "a\nb", "a\x1bb"} {
		if code := patch(t, s, id, bad); code != 422 {
			t.Errorf("%q: %d", bad, code)
		}
	}
	if _, err := c.Rename(ctx, id, strings.Repeat("é", 100)); err != nil {
		t.Fatalf("200 bytes: %v", err)
	}
	if _, err := c.Rename(ctx, "run_none", "x"); !luxapi.IsStatus(err, 404) || errors.Is(err, luxapi.ErrRenameUnsupported) {
		t.Fatalf("no such Run: %v", err)
	}
	if got := s.Renames(); len(got) != 3 || got[0] != (RenameReq{id, "s/api"}) {
		t.Fatalf("renames %v", got)
	}

	s.FailRenames(http.StatusInternalServerError)
	if _, err := c.Rename(ctx, id, "y"); !luxapi.IsStatus(err, 500) || errors.Is(err, luxapi.ErrRenameUnsupported) {
		t.Fatalf("failing: %v", err)
	}
	s.FailRenames(RenameHang)
	hctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	if _, err := c.Rename(hctx, id, "y"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("hang: %v", err)
	}
	s.FailRenames(0)
	if _, err := c.Rename(ctx, id, "back"); err != nil || s.Run(id).Name != "back" {
		t.Fatalf("after failures: %v", err)
	}
}

// patch sends PATCH /v1/runs/{id} with name, returning the status.
func patch(t *testing.T, s *Server, id, name string) int {
	t.Helper()
	body := `{"name":"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\x1b", `\u001b`).Replace(name) + `"}`
	req, err := http.NewRequest(http.MethodPatch, s.URL+"/v1/runs/"+id, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+DefaultKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// TestRenameOldLux: a lux without the route (405, or 404 for a Run GET
// finds) can't rename; the client remembers it and asks no more.
func TestRenameOldLux(t *testing.T) {
	for _, code := range []int{http.StatusMethodNotAllowed, http.StatusNotFound, http.StatusUnprocessableEntity} {
		s := New()
		c, id := submitted(t, s)
		s.FailRenames(code)
		if _, err := c.Rename(context.Background(), id, "x"); !errors.Is(err, luxapi.ErrRenameUnsupported) {
			t.Fatalf("%d: %v", code, err)
		}
		n := len(s.Requests())
		if _, err := c.Rename(context.Background(), id, "y"); !errors.Is(err, luxapi.ErrRenameUnsupported) || len(s.Requests()) != n {
			t.Fatalf("%d: asked again: %v, %v", code, err, s.Requests()[n:])
		}
		fresh, _ := luxapi.New(s.Config())
		if _, err := fresh.Rename(context.Background(), id, "z"); !errors.Is(err, luxapi.ErrRenameUnsupported) || len(s.Requests()) != n {
			t.Fatalf("%d: a new client forgot the endpoint's capability: %v", code, err)
		}
		if !c.FirstRenameRefusal() || fresh.FirstRenameRefusal() {
			t.Fatal("rename refusal notice is not shared by endpoint")
		}
		s.Close()
	}
}
