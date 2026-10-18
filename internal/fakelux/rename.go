package fakelux

import (
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"unicode"
	"unicode/utf8"
)

// RenameReq is one rename lux took: the Run and its new name.
type RenameReq struct {
	RunID string
	Name  string
}

// RenameHang, given to FailRenames, makes every rename answer nothing
// until the client goes away.
const RenameHang = -1

// FailRenames answers every PATCH /v1/runs/{id} with HTTP status code,
// leaving the Run's name as it is: 405 or 404 as a luxd from before the
// route, 500 as a failing one, RenameHang as one that does not answer; 0
// renames again.
func (s *Server) FailRenames(code int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failRenames = code
}

// Renames are the renames lux took, in order.
func (s *Server) Renames() []RenameReq {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.renames)
}

// rename is luxd's PATCH /v1/runs/{id}: a new name for a Run in any
// state, at most 200 bytes of UTF-8 without control characters ("" clears
// it); the answer is the Run, as GET's.
func (s *Server) rename(w http.ResponseWriter, req *http.Request) {
	id := req.PathValue("id")
	var body struct {
		Name *string `json:"name"`
	}
	// Read first: the request's context ends with the client only once
	// its body was read.
	raw, err := io.ReadAll(io.LimitReader(req.Body, 1<<20))
	if err == nil && !utf8.Valid(raw) {
		writeErr(w, 422, "invalid_name", "name must be UTF-8")
		return
	}
	if err == nil {
		err = json.Unmarshal(raw, &body)
	}
	s.mu.Lock()
	fail := s.failRenames
	s.mu.Unlock()
	switch fail {
	case 0:
	case RenameHang:
		<-req.Context().Done()
		return
	default:
		writeErr(w, fail, "failed", "rename failed")
		return
	}
	if err != nil || body.Name == nil {
		writeErr(w, 400, "bad_request", "name is required")
		return
	}
	name := *body.Name
	if len(name) > 200 || !utf8.ValidString(name) || slices.ContainsFunc([]rune(name), unicode.IsControl) {
		writeErr(w, 422, "invalid_name", "name: at most 200 bytes of UTF-8, no control characters")
		return
	}
	s.mu.Lock()
	r := s.runs[id]
	if r == nil {
		s.mu.Unlock()
		writeErr(w, 404, "not_found", "no such run")
		return
	}
	r.run.Name = name
	s.renames = append(s.renames, RenameReq{id, name})
	c := *r.run
	s.mu.Unlock()
	writeJSON(w, 200, c)
}
