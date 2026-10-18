// Package luxapi is what luxmux asks of luxd, over lux's public api client.
package luxapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/marcioapm/lux/api"

	"github.com/marcioapm/lux-mux/internal/config"
)

// Client is a luxd client.
type Client struct{ C *api.Client }

// renameSupport is what is known of one endpoint's rename API, shared by
// every Client of that endpoint (URL and tenant).
type renameSupport struct {
	mu sync.Mutex
	// known: a PATCH succeeded or proved the route missing; noRename: lux has no rename
	// route; told: FirstRenameRefusal has reported it.
	known, noRename, told bool
	// probing is non-nil while one caller's PATCH decides known; it is
	// closed and reset on every path the prober returns by.
	probing chan struct{}
	// slots bounds the endpoint's concurrent PATCHes to renameSlots.
	slots chan struct{}
	// refs and used are guarded by renameEndpoints, not mu.
	refs int
	used time.Time
}

type renameKey struct{ URL, Tenant string }

func (c *Client) renameKey() renameKey {
	return renameKey{strings.TrimRight(c.C.Base, "/"), c.C.Tenant}
}

const (
	renameCapacity = 256
	renameTTL      = 10 * time.Minute
	renameSlots    = 8
)

// renameEndpoints holds at most renameCapacity entries. An entry with
// refs>0 is never expired or evicted, so its callers always share one
// semaphore and one probe; while every entry is referenced, a caller of a
// new endpoint waits on changed (closed and replaced at each release).
var renameEndpoints = struct {
	sync.Mutex
	entries map[renameKey]*renameSupport
	changed chan struct{}
}{entries: make(map[renameKey]*renameSupport), changed: make(chan struct{})}

// renameSupport is c's endpoint's entry with a reference held; the caller
// releases it with releaseRenameSupport.
func (c *Client) renameSupport(ctx context.Context) (*renameSupport, error) {
	key := c.renameKey()
	for {
		renameEndpoints.Lock()
		now := time.Now()
		for k, s := range renameEndpoints.entries {
			if s.refs == 0 && now.Sub(s.used) >= renameTTL {
				delete(renameEndpoints.entries, k)
			}
		}
		s := renameEndpoints.entries[key]
		if s == nil && len(renameEndpoints.entries) >= renameCapacity {
			// Evict the least recently used idle entry, if there is one.
			var oldest renameKey
			var at time.Time
			for k, v := range renameEndpoints.entries {
				if v.refs == 0 && (at.IsZero() || v.used.Before(at)) {
					oldest, at = k, v.used
				}
			}
			if !at.IsZero() {
				delete(renameEndpoints.entries, oldest)
			}
		}
		if s == nil && len(renameEndpoints.entries) < renameCapacity {
			s = &renameSupport{slots: make(chan struct{}, renameSlots)}
			renameEndpoints.entries[key] = s
		}
		if s != nil {
			s.refs++
			s.used = now
			renameEndpoints.Unlock()
			return s, nil
		}
		changed := renameEndpoints.changed
		renameEndpoints.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func releaseRenameSupport(s *renameSupport) {
	renameEndpoints.Lock()
	s.refs--
	s.used = time.Now()
	close(renameEndpoints.changed)
	renameEndpoints.changed = make(chan struct{})
	renameEndpoints.Unlock()
}

// FirstRenameRefusal is true once per retained endpoint entry: the first
// time it is asked after Rename found the endpoint without a rename route.
func (c *Client) FirstRenameRefusal() bool {
	renameEndpoints.Lock()
	s := renameEndpoints.entries[c.renameKey()]
	if s == nil {
		renameEndpoints.Unlock()
		return false
	}
	s.refs++
	renameEndpoints.Unlock()
	defer releaseRenameSupport(s)
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.noRename || s.told {
		return false
	}
	s.told = true
	return true
}

// New is a client of cfg's luxd.
func New(cfg api.Config) (*Client, error) {
	if cfg.URL == "" || cfg.APIKey == "" {
		return nil, errors.New("not signed in to lux: set url and api_key in the lux CLI's config.toml, or LUX_URL and LUX_API_KEY")
	}
	return &Client{C: api.FromConfig(cfg)}, nil
}

// Endpoints makes and keeps one client per endpoint. Base is the lux CLI
// configuration (api.LoadConfig()); lookup finds api_key_env variables.
type Endpoints struct {
	base   api.Config
	lookup func(string) (string, bool)
	mu     sync.Mutex
	byEP   map[config.Endpoint]*Client
}

// NewEndpoints is the client set over base.
func NewEndpoints(base api.Config, lookup func(string) (string, bool)) *Endpoints {
	return &Endpoints{base: base, lookup: lookup, byEP: map[config.Endpoint]*Client{}}
}

// Base is the lux CLI configuration the endpoints fall back to.
func (e *Endpoints) Base() api.Config { return e.base }

// Default is the lux CLI configuration's own endpoint.
func (e *Endpoints) Default() config.Endpoint { return config.DefaultEndpoint(e.base) }

// Of is the endpoint a profile's Runs live on.
func (e *Endpoints) Of(p config.Profile) config.Endpoint { return p.Endpoint(e.base) }

// Client is ep's client, made on first use. Its error names the endpoint
// and where its key was looked for, never a key.
func (e *Endpoints) Client(ep config.Endpoint) (*Client, error) {
	e.mu.Lock()
	c := e.byEP[ep]
	e.mu.Unlock()
	if c != nil {
		return c, nil
	}
	// The key is read without the lock: a key file that is slow to read
	// holds up only its own endpoint's lookups.
	cfg, err := ep.Config(e.base, e.lookup)
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if c := e.byEP[ep]; c != nil {
		return c, nil
	}
	c = &Client{C: api.FromConfig(cfg)}
	e.byEP[ep] = c
	return c, nil
}

// Submit creates a Run.
func (c *Client) Submit(ctx context.Context, sp api.RunSpec) (*api.Run, error) {
	var ans api.SubmitAnswer
	if err := c.C.Do(ctx, http.MethodPost, "/v1/runs", sp, &ans); err != nil {
		return nil, err
	}
	if ans.Run == nil {
		return nil, errors.New("luxd answered the submit without a Run")
	}
	return ans.Run, nil
}

// Validate is submit's validateOnly: no Run is made; secret values may
// be absent.
func (c *Client) Validate(ctx context.Context, sp api.RunSpec) (*api.SubmitAnswer, error) {
	var ans api.SubmitAnswer
	if err := c.C.Do(ctx, http.MethodPost, "/v1/runs?validateOnly=true", sp, &ans); err != nil {
		return nil, err
	}
	return &ans, nil
}

// Get is one Run.
func (c *Client) Get(ctx context.Context, id string) (*api.Run, error) {
	var r api.Run
	if err := c.C.Do(ctx, http.MethodGet, "/v1/runs/"+url.PathEscape(id), nil, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// ErrRenameUnsupported is Rename's error for a luxd without
// PATCH /v1/runs/{id}.
var ErrRenameUnsupported = errors.New("this lux cannot rename Runs")

// Rename sets Run id's name. Until the endpoint's support is known, one
// caller probes and the others wait for it. Only 405, or 404 followed by a
// successful GET of the Run, proves the route missing; 422, other errors
// and a cancelled probe leave support unknown, so the next call probes.
func (c *Client) Rename(ctx context.Context, id, name string) (*api.Run, error) {
	s, err := c.renameSupport(ctx)
	if err != nil {
		return nil, err
	}
	defer releaseRenameSupport(s)
	probe := false
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		s.mu.Lock()
		if s.noRename {
			s.mu.Unlock()
			return nil, ErrRenameUnsupported
		}
		if s.known {
			s.mu.Unlock()
			break
		}
		if s.probing == nil {
			s.probing = make(chan struct{})
			probe = true
			s.mu.Unlock()
			break
		}
		done := s.probing
		s.mu.Unlock()
		select {
		case <-done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if probe {
		// Waiters re-check known on close, whatever this call returns.
		defer func() { s.mu.Lock(); close(s.probing); s.probing = nil; s.mu.Unlock() }()
	}
	select {
	case s.slots <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-s.slots }()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var r api.Run
	err = c.C.Do(ctx, http.MethodPatch, "/v1/runs/"+url.PathEscape(id), map[string]string{"name": name}, &r)
	unsupported := IsStatus(err, http.StatusMethodNotAllowed)
	if IsStatus(err, http.StatusNotFound) {
		_, gerr := c.Get(ctx, id)
		unsupported = gerr == nil
	}
	s.mu.Lock()
	if err == nil {
		s.known = true
	}
	if unsupported {
		s.known = true
		s.noRename = true
	}
	s.mu.Unlock()
	if unsupported {
		return nil, ErrRenameUnsupported
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// ListSession is every Run labelled with the session (at most 1000, the
// API's bound for an unpaged list).
func (c *Client) ListSession(ctx context.Context, session string) ([]*api.Run, error) {
	return c.list(ctx, url.Values{"label": {config.LabelSession + "=" + session}})
}

// ListAll is every Run luxmux made (label app=luxmux).
func (c *Client) ListAll(ctx context.Context) ([]*api.Run, error) {
	return c.list(ctx, url.Values{"label": {config.LabelApp + "=" + config.AppName}})
}

func (c *Client) list(ctx context.Context, q url.Values) ([]*api.Run, error) {
	q.Set("limit", "1000")
	var l api.RunList
	if err := c.C.Do(ctx, http.MethodGet, "/v1/runs?"+q.Encode(), nil, &l); err != nil {
		return nil, err
	}
	return l.Runs, nil
}

// Terminate ends a Run for good. One already terminated is not an error.
func (c *Client) Terminate(ctx context.Context, id string) error {
	err := c.C.Do(ctx, http.MethodPost, "/v1/runs/"+url.PathEscape(id)+"/terminate", nil, nil)
	if IsStatus(err, http.StatusNotFound) {
		return nil
	}
	return err
}

// Resume resumes a stopped, lost, failed or succeeded Run with every one
// of its secrets.
func (c *Client) Resume(ctx context.Context, id string, secrets []api.Secret) error {
	body := map[string]any{}
	if len(secrets) > 0 {
		body["secrets"] = secrets
	}
	return c.C.Do(ctx, http.MethodPost, "/v1/runs/"+url.PathEscape(id)+"/resume", body, nil)
}

// Pools is every pool the caller may use.
func (c *Client) Pools(ctx context.Context) ([]api.Pool, error) {
	var l api.PoolList
	if err := c.C.Do(ctx, http.MethodGet, "/v1/pools", nil, &l); err != nil {
		return nil, err
	}
	return l.Pools, nil
}

// Attach opens the Run's terminal; with restore, its output starts with a
// snapshot of the terminal at rows×cols. luxd sends the host its
// StreamOpen: the client sends none. cut closes the network connection
// under the stream at once: websocket.Conn's Close waits up to 5 s for a
// peer that does not answer its close frame, and its CloseNow then only
// waits for Close.
func (c *Client) Attach(ctx context.Context, id string, restore bool, rows, cols int) (_ *websocket.Conn, cut func(), _ error) {
	q := url.Values{}
	if restore {
		q.Set("restore", "true")
		if rows > 0 && cols > 0 {
			q.Set("rows", strconv.Itoa(rows))
			q.Set("cols", strconv.Itoa(cols))
		}
	}
	path := "/v1/runs/" + url.PathEscape(id) + "/attach"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var body io.Closer
	hc := *c.C.HTTP
	transport := hc.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	hc.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		resp, err := transport.RoundTrip(req)
		if err == nil && resp.StatusCode == http.StatusSwitchingProtocols {
			body = resp.Body
		}
		return resp, err
	})
	cc := *c.C
	cc.HTTP = &hc
	ws, err := cc.Dial(ctx, path)
	if err != nil {
		return nil, nil, err
	}
	return ws, func() { _ = body.Close() }, nil
}

type roundTripFunc func(*http.Request) (*http.Response, error)

// Exec runs command in the Run without a TTY and returns its exit code.
// It sends EOF before reading, so the command gets no input.
func (c *Client) Exec(ctx context.Context, id string, command []string) (int, error) {
	_, code, err := c.ExecOutput(ctx, id, command, 0)
	return code, err
}

// ExecOutput is Exec that also returns what the command wrote to its
// stdout channel, up to limit bytes (0: none kept); the rest is read and
// dropped.
func (c *Client) ExecOutput(ctx context.Context, id string, command []string, limit int) ([]byte, int, error) {
	ws, err := c.C.Dial(ctx, "/v1/runs/"+url.PathEscape(id)+"/exec")
	if err != nil {
		return nil, 0, err
	}
	defer ws.CloseNow()
	if err := wsjson.Write(ctx, ws, api.StreamOpen{Command: command}); err != nil {
		return nil, 0, err
	}
	if err := wsjson.Write(ctx, ws, api.StreamData{EOF: true}); err != nil {
		return nil, 0, err
	}
	var out []byte
	for {
		var d api.StreamData
		if err := wsjson.Read(ctx, ws, &d); err != nil {
			return out, 0, err
		}
		if d.Error != "" {
			return out, 0, errors.New(d.Error)
		}
		if (d.Channel == "" || d.Channel == "stdout") && len(out) < limit {
			out = append(out, d.Data[:min(len(d.Data), limit-len(out))]...)
		}
		if d.ExitCode != nil {
			return out, *d.ExitCode, nil
		}
	}
}

// RunCost is the part of GET /v1/runs/{id}/cost luxmux reads (lux
// internal/server/costs.go, RunCost): its status and per-currency totals.
type RunCost struct {
	Status string          `json:"status"`
	Totals []api.CostTotal `json:"totals"`
}

// Cost is what a Run cost so far.
func (c *Client) Cost(ctx context.Context, id string) (*RunCost, error) {
	var rc RunCost
	if err := c.C.Do(ctx, http.MethodGet, "/v1/runs/"+url.PathEscape(id)+"/cost", nil, &rc); err != nil {
		return nil, err
	}
	return &rc, nil
}

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// EventSubmitted is the type of the event luxd records for a submitted
// Run (lux internal/server/api.go, submitRun): the Run is inserted already
// submitted, so no state event says so.
const EventSubmitted = "submitted"

// Feed follows the tenant's event feed from after (0: from now), calling
// fn for each event until ctx ends, the stream does, or fn fails.
func (c *Client) Feed(ctx context.Context, after int64, fn func(api.FeedEvent)) error {
	q := url.Values{}
	if after > 0 {
		q.Set("after", strconv.FormatInt(after, 10))
	}
	return c.C.Stream(ctx, "/v1/events", q, func(e api.SSEEvent) error {
		if e.Event != "lux" {
			return nil
		}
		var fe api.FeedEvent
		if json.Unmarshal(e.Data, &fe) == nil {
			fn(fe)
		}
		return nil
	})
}

// IsStatus reports whether err is luxd's answer with that HTTP status.
func IsStatus(err error, status int) bool {
	var ae *api.APIError
	return errors.As(err, &ae) && ae.Status == status
}

// Code is luxd's error code ("" when err is not its answer).
func Code(err error) string {
	var ae *api.APIError
	if errors.As(err, &ae) {
		return ae.Code
	}
	return ""
}

// Describe is a one-line account of an error for the screen.
func Describe(err error) string {
	var ae *api.APIError
	if errors.As(err, &ae) {
		if ae.Code != "" {
			return fmt.Sprintf("%s (%s)", ae.Message, ae.Code)
		}
		return ae.Message
	}
	return err.Error()
}

// Described is err with Describe's text that still unwraps to err, so
// IsStatus and errors.Is see through it.
func Described(err error) error {
	if err == nil {
		return nil
	}
	return described{err}
}

type described struct{ err error }

func (d described) Error() string { return Describe(d.err) }
func (d described) Unwrap() error { return d.err }

// Refused reports whether err is luxd refusing the key: 401 or 403.
func Refused(err error) bool {
	return IsStatus(err, http.StatusUnauthorized) || IsStatus(err, http.StatusForbidden)
}

// FailedWord names why a lux did not answer a request: a 401 or 403 is
// the key refused, anything else the lux unreachable.
func FailedWord(err error) string {
	if Refused(err) {
		return "endpoint refused the key"
	}
	return "endpoint unreachable"
}
