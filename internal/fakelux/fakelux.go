// Package fakelux is a fake luxd for luxmux's tests: the Run endpoints
// luxmux uses, the event feed, and attach streams backed by in-process
// terminals that keep their screen as lux-shim does.
package fakelux

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/marcioapm/lux/api"
)

// SecretsLostReason is luxd's stateReason for a Run stopped because no
// luxd held its secret values any more.
const SecretsLostReason = "secrets must be supplied again: resume with them"

// AttachReq is one attach request as luxd received it.
type AttachReq struct {
	RunID      string
	Restore    bool
	Rows, Cols int
}

// ResumeReq is one resume request.
type ResumeReq struct {
	RunID   string
	Secrets map[string]string
}

// Server is the fake luxd.
type Server struct {
	*httptest.Server
	// NewTerminal makes a Run's terminal each time it starts running (a
	// first start and every new placement). Default: EchoTerminal.
	NewTerminal func(run *api.Run) *Terminal
	// NoRestore makes attach ignore restore, as a host whose lux-shim
	// predates it.
	NoRestore bool
	// NoSnapshot makes a restore attach send no snapshot, only the
	// Restored marker, as a host whose screen is over the snapshot limit.
	NoSnapshot bool
	// Hold keeps new Runs queued (submitted) until Start is called.
	Hold bool

	mu      sync.Mutex
	cond    *sync.Cond
	runs    map[string]*fakeRun
	order   []string
	events  []api.FeedEvent
	seq     int
	attachs []AttachReq
	resumes []ResumeReq
	submits []api.RunSpec
	terms   []string
	pools   []api.Pool
	// InputHook, if set, is called with each input message's data.
	InputHook func(runID string, data []byte)
	// SubmitHook, if set, runs before each submit is answered.
	SubmitHook func()
	// AttachHook, if set, runs before each attach to a running Run is
	// accepted: a slow attach.
	AttachHook func(runID string)
	// GetHook, if set (SetGetHook), runs before each GET of a Run is
	// answered.
	GetHook func(runID string)
	// ResumeHook, if set, runs before each resume request is handled.
	ResumeHook func(runID string)
	// TerminateHook, if set, runs before each terminate request is
	// handled.
	TerminateHook func(runID string)

	// failing: every attach to the Run is ended with this error as its
	// first frame (FailAttaches).
	failing map[string]string
	// reqs are the requests received, "METHOD path" as sent.
	reqs []string
}

// FailAttaches makes every attach to Run id end at once, its first frame
// the error msg, as luxd does for a host that went away between the
// upgrade and the stream; msg "" lets attaches through again.
func (s *Server) FailAttaches(id, msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failing == nil {
		s.failing = map[string]string{}
	}
	if msg == "" {
		delete(s.failing, id)
	} else {
		s.failing[id] = msg
	}
}

// Requests are the requests received, as "METHOD path" with the path as
// the client sent it.
func (s *Server) Requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.reqs)
}

// SetGetHook sets GetHook while the server is serving.
func (s *Server) SetGetHook(f func(runID string)) {
	s.mu.Lock()
	s.GetHook = f
	s.mu.Unlock()
}

type fakeRun struct {
	run     *api.Run
	term    *Terminal
	conns   map[*websocket.Conn]bool
	secrets map[string]string
	// snapshotRefused: the current placement's snapshot report was refused
	// and the Run has no snapshot (see RefuseSnapshot).
	snapshotRefused bool
}

// resumable is lux's Run.resumable: an end other than terminated, unless
// the spec's resumePolicy is never or the Run is left with only a refused
// snapshot report.
func resumable(r *fakeRun) bool {
	switch r.run.State {
	case "stopped", "lost", "failed", "succeeded":
		return r.run.Spec.ResumePolicy != "never" && !r.snapshotRefused
	}
	return false
}

// New starts a fake luxd.
func New() *Server {
	s := &Server{runs: map[string]*fakeRun{}}
	s.cond = sync.NewCond(&s.mu)
	s.NewTerminal = func(*api.Run) *Terminal { return EchoTerminal(24, 80) }
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/runs", s.submit)
	mux.HandleFunc("GET /v1/runs", s.list)
	mux.HandleFunc("GET /v1/runs/{id}", s.get)
	mux.HandleFunc("POST /v1/runs/{id}/terminate", s.terminate)
	mux.HandleFunc("POST /v1/runs/{id}/resume", s.resume)
	mux.HandleFunc("GET /v1/runs/{id}/attach", s.attach)
	mux.HandleFunc("GET /v1/events", s.feed)
	mux.HandleFunc("GET /v1/pools", s.listPools)
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		s.mu.Lock()
		s.reqs = append(s.reqs, req.Method+" "+req.URL.EscapedPath())
		s.mu.Unlock()
		mux.ServeHTTP(w, req)
	}))
	return s
}

// Close stops the server and every terminal.
func (s *Server) Close() {
	s.mu.Lock()
	for _, r := range s.runs {
		for c := range r.conns {
			c.CloseNow()
		}
		if r.term != nil {
			r.term.Exit()
		}
	}
	s.cond.Broadcast()
	s.mu.Unlock()
	s.Server.CloseClientConnections()
	s.Server.Close()
}

// Config is a lux client configuration for this server.
func (s *Server) Config() api.Config { return api.Config{URL: s.URL, APIKey: "lux_test"} }

// SetPools sets what GET /v1/pools answers.
func (s *Server) SetPools(p []api.Pool) {
	s.mu.Lock()
	s.pools = p
	s.mu.Unlock()
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": code, "message": msg}})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// event records an event of a Run, with the lock held.
func (s *Server) event(r *api.Run, typ string, data map[string]any) {
	id := r.ID
	ep := r.Epoch
	s.events = append(s.events, api.FeedEvent{Event: api.Event{ID: int64(len(s.events) + 1), Epoch: &ep, Type: typ, Data: data, Time: time.Now()}, RunID: &id})
	s.cond.Broadcast()
}

func (s *Server) setState(r *fakeRun, state, reason string) {
	r.run.State, r.run.StateReason = state, reason
	r.run.Resumable = resumable(r)
	d := map[string]any{"state": state}
	if reason != "" {
		d["reason"] = reason
	}
	s.event(r.run, "state", d)
}

func (s *Server) setStage(r *fakeRun, stage string) {
	r.run.Stage = stage
	s.event(r.run, "stage", map[string]any{"stage": stage, "epoch": r.run.Epoch})
}

func (s *Server) submit(w http.ResponseWriter, req *http.Request) {
	var sp api.RunSpec
	if err := json.NewDecoder(req.Body).Decode(&sp); err != nil {
		writeErr(w, 400, "bad_request", err.Error())
		return
	}
	if sp.Image.Ref == "" {
		writeErr(w, 422, "invalid_spec", "image.ref is required")
		return
	}
	if s.SubmitHook != nil && req.URL.Query().Get("validateOnly") != "true" {
		s.SubmitHook()
	}
	if req.URL.Query().Get("validateOnly") == "true" {
		ans := api.SubmitAnswer{Valid: true, WouldStartNow: true, EffectivePool: sp.Placement.Pool}
		if sp.Placement.Pool == "busy" {
			ans.WouldStartNow = false
			ans.Blockers = []api.AdmissionBlocker{{Code: "not_enough_spare", Hosts: 2}}
		}
		writeJSON(w, 200, ans)
		return
	}
	s.mu.Lock()
	s.seq++
	id := fmt.Sprintf("run_%04d", s.seq)
	run := &api.Run{ID: id, Name: sp.Name, Labels: sp.Labels, Spec: sp, CreatedAt: time.Now().Add(time.Duration(s.seq) * time.Millisecond), Pool: sp.Placement.Pool}
	for _, sec := range sp.Secrets {
		run.Secrets = append(run.Secrets, api.SecretRef{Name: sec.Name})
	}
	fr := &fakeRun{run: run, conns: map[*websocket.Conn]bool{}, secrets: map[string]string{}}
	for _, sec := range sp.Secrets {
		fr.secrets[sec.Name] = sec.Value
	}
	s.runs[id] = fr
	s.order = append(s.order, id)
	s.submits = append(s.submits, sp)
	s.setState(fr, "submitted", "")
	s.setStage(fr, "waiting")
	hold := s.Hold
	ans := *run
	s.mu.Unlock()
	writeJSON(w, 201, api.SubmitAnswer{Run: &ans})
	if !hold {
		s.Start(id)
	}
}

// Start takes a Run through its start stages to running, on a new
// placement with a new terminal.
func (s *Server) Start(id string) {
	s.mu.Lock()
	r := s.runs[id]
	if r == nil || r.run.State == "terminated" {
		s.mu.Unlock()
		return
	}
	r.run.Epoch++
	s.setState(r, "scheduled", "")
	for _, st := range []string{"image", "volumes", "repositories", "container"} {
		s.setStage(r, st)
	}
	run := *r.run
	s.mu.Unlock()
	t := s.NewTerminal(&run)
	s.mu.Lock()
	if r.term != nil {
		r.term.Exit()
	}
	r.term = t
	r.run.Host = "host-" + strconv.Itoa(r.run.Epoch)
	s.setStage(r, "running")
	s.setState(r, "running", "")
	s.mu.Unlock()
	go s.watchExit(id, t)
}

// watchExit ends the Run as lux does when its workload exits.
func (s *Server) watchExit(id string, t *Terminal) {
	<-t.Gone()
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.runs[id]
	if r == nil || r.term != t || r.run.State != "running" {
		return
	}
	code := t.ExitCode()
	r.run.ExitCode = &code
	if code == 0 {
		s.setState(r, "succeeded", "")
		s.setStage(r, "succeeded")
	} else {
		s.setState(r, "failed", fmt.Sprintf("exit code %d", code))
		s.setStage(r, "failed")
	}
}

func (s *Server) list(w http.ResponseWriter, req *http.Request) {
	labels := req.URL.Query()["label"]
	s.mu.Lock()
	var out api.RunList
	for i := len(s.order) - 1; i >= 0; i-- {
		r := s.runs[s.order[i]].run
		ok := true
		for _, l := range labels {
			k, v, _ := strings.Cut(l, "=")
			if r.Labels[k] != v {
				ok = false
			}
		}
		if ok {
			c := *r
			out.Runs = append(out.Runs, &c)
		}
	}
	s.mu.Unlock()
	writeJSON(w, 200, out)
}

func (s *Server) get(w http.ResponseWriter, req *http.Request) {
	s.mu.Lock()
	hook := s.GetHook
	s.mu.Unlock()
	if hook != nil {
		hook(req.PathValue("id"))
	}
	s.mu.Lock()
	r := s.runs[req.PathValue("id")]
	var c api.Run
	if r != nil {
		c = *r.run
		if c.Resumable {
			names := make([]string, 0, len(r.run.Secrets))
			for _, sr := range r.run.Secrets {
				names = append(names, sr.Name)
			}
			c.Resume = &api.Resumability{Secrets: names, SecretsHeld: c.StateReason != SecretsLostReason && len(names) > 0}
		}
	}
	s.mu.Unlock()
	if r == nil {
		writeErr(w, 404, "not_found", "no such run")
		return
	}
	writeJSON(w, 200, c)
}

func (s *Server) terminate(w http.ResponseWriter, req *http.Request) {
	id := req.PathValue("id")
	s.mu.Lock()
	thook := s.TerminateHook
	s.mu.Unlock()
	if thook != nil {
		thook(id)
	}
	s.mu.Lock()
	r := s.runs[id]
	if r == nil {
		s.mu.Unlock()
		writeErr(w, 404, "not_found", "no such run")
		return
	}
	s.terms = append(s.terms, id)
	if r.run.State != "terminated" {
		s.setState(r, "terminated", "terminate")
		s.setStage(r, "terminated")
	}
	for c := range r.conns {
		_ = wsjson.Write(context.Background(), c, api.StreamData{Error: "the Run was terminated"})
		c.CloseNow()
	}
	t := r.term
	c := *r.run
	s.mu.Unlock()
	if t != nil {
		t.Exit()
	}
	writeJSON(w, 202, c)
}

func (s *Server) resume(w http.ResponseWriter, req *http.Request) {
	id := req.PathValue("id")
	var body struct {
		Secrets []api.Secret `json:"secrets"`
	}
	_ = json.NewDecoder(req.Body).Decode(&body)
	vals := map[string]string{}
	for _, sec := range body.Secrets {
		vals[sec.Name] = sec.Value
	}
	s.mu.Lock()
	hook := s.ResumeHook
	s.mu.Unlock()
	if hook != nil {
		hook(id)
	}
	s.mu.Lock()
	r := s.runs[id]
	if r == nil {
		s.mu.Unlock()
		writeErr(w, 404, "not_found", "no such run")
		return
	}
	s.resumes = append(s.resumes, ResumeReq{RunID: id, Secrets: vals})
	if !r.run.Resumable {
		s.mu.Unlock()
		writeErr(w, 409, "not_resumable", "run is "+r.run.State)
		return
	}
	var missing []string
	for _, sr := range r.run.Secrets {
		if _, ok := vals[sr.Name]; !ok {
			missing = append(missing, sr.Name)
		}
	}
	if len(missing) > 0 {
		s.mu.Unlock()
		writeErr(w, 422, "secrets_required", "secrets required: "+strings.Join(missing, ", "))
		return
	}
	r.secrets = vals
	s.setState(r, "resuming", "")
	s.setStage(r, "waiting")
	hold := s.Hold
	s.mu.Unlock()
	writeJSON(w, 202, map[string]any{"id": id})
	if !hold {
		s.Start(id)
	}
}

func (s *Server) listPools(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	p := s.pools
	s.mu.Unlock()
	writeJSON(w, 200, api.PoolList{Pools: p})
}

func (s *Server) attach(w http.ResponseWriter, req *http.Request) {
	id := req.PathValue("id")
	q := req.URL.Query()
	rows, _ := strconv.Atoi(q.Get("rows"))
	cols, _ := strconv.Atoi(q.Get("cols"))
	restore := q.Get("restore") == "true"
	s.mu.Lock()
	r := s.runs[id]
	if r == nil {
		s.mu.Unlock()
		writeErr(w, 404, "not_found", "no such run")
		return
	}
	s.attachs = append(s.attachs, AttachReq{RunID: id, Restore: restore, Rows: rows, Cols: cols})
	s.cond.Broadcast()
	if rows > 200 || cols > 500 {
		s.mu.Unlock()
		writeErr(w, 422, "invalid_request", "rows and cols are at most 200 and 500")
		return
	}
	if r.run.State != "running" || r.term == nil {
		st := r.run.State
		s.mu.Unlock()
		writeErr(w, 409, "not_running", "run is "+st+": interactive access needs it running")
		return
	}
	t := r.term
	noRestore, noSnapshot := s.NoRestore, s.NoSnapshot
	hook := s.AttachHook
	fail, failing := s.failing[id]
	s.mu.Unlock()
	if hook != nil {
		hook(id)
	}

	ws, err := websocket.Accept(w, req, nil)
	if err != nil {
		return
	}
	ws.SetReadLimit(4 << 20)
	defer ws.CloseNow()
	if failing {
		_ = wsjson.Write(req.Context(), ws, api.StreamData{Error: fail})
		_ = ws.Close(websocket.StatusNormalClosure, "")
		return
	}
	s.mu.Lock()
	r.conns[ws] = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(r.conns, ws)
		s.mu.Unlock()
	}()
	snap, sub := t.Join(restore && !noRestore, rows, cols)
	defer t.Leave(sub)
	if noSnapshot {
		snap = nil
	}
	ctx, cancel := context.WithCancel(req.Context())
	defer cancel()
	go func() {
		defer cancel()
		for {
			var d api.StreamData
			if err := wsjson.Read(ctx, ws, &d); err != nil {
				return
			}
			switch {
			case d.Rows > 0:
				t.Resize(d.Rows, d.Cols)
			case len(d.Data) > 0:
				if s.InputHook != nil {
					s.InputHook(id, d.Data)
				}
				t.Input(d.Data)
			}
		}
	}()
	for len(snap) > 0 {
		n := min(len(snap), 32<<10)
		if wsjson.Write(ctx, ws, api.StreamData{Channel: "stdout", Data: snap[:n]}) != nil {
			return
		}
		snap = snap[n:]
	}
	if restore && !noRestore {
		// Where live output starts, with or without a snapshot.
		if wsjson.Write(ctx, ws, api.StreamData{Restored: true}) != nil {
			return
		}
	}
	send := func(b []byte) error {
		err := wsjson.Write(ctx, ws, api.StreamData{Channel: "stdout", Data: b})
		sub.taken(t, b)
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-sub.Behind:
			_ = wsjson.Write(ctx, ws, api.StreamData{Error: ErrAttachFellBehind})
			_ = ws.Close(websocket.StatusNormalClosure, "")
			return
		case <-t.Gone():
			// Drain what the process wrote last.
			for {
				select {
				case b := <-sub.C:
					_ = send(b)
					continue
				default:
				}
				break
			}
			_ = wsjson.Write(ctx, ws, api.StreamData{Error: ErrWorkloadExited})
			_ = ws.Close(websocket.StatusNormalClosure, "")
			return
		case b, ok := <-sub.C:
			if !ok {
				return
			}
			if send(b) != nil {
				return
			}
		}
	}
}

func (s *Server) feed(w http.ResponseWriter, req *http.Request) {
	s.mu.Lock()
	after := len(s.events)
	s.mu.Unlock()
	if a := req.URL.Query().Get("after"); a != "" {
		after, _ = strconv.Atoi(a)
	} else if a := req.Header.Get("Last-Event-ID"); a != "" {
		after, _ = strconv.Atoi(a)
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(200)
	fl, _ := w.(http.Flusher)
	if fl != nil {
		fl.Flush()
	}
	ctx := req.Context()
	stop := context.AfterFunc(ctx, func() {
		s.mu.Lock()
		s.cond.Broadcast()
		s.mu.Unlock()
	})
	defer stop()
	for {
		s.mu.Lock()
		for len(s.events) <= after && ctx.Err() == nil {
			s.cond.Wait()
		}
		if ctx.Err() != nil {
			s.mu.Unlock()
			return
		}
		evs := slices.Clone(s.events[after:])
		after = len(s.events)
		s.mu.Unlock()
		for _, e := range evs {
			b, _ := json.Marshal(e)
			if _, err := fmt.Fprintf(w, "id: %d\nevent: lux\ndata: %s\n\n", e.ID, b); err != nil {
				return
			}
		}
		if fl != nil {
			fl.Flush()
		}
	}
}
