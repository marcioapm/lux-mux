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
	"sync"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/marcioapm/lux/api"

	"github.com/marcioapm/lux-mux/internal/config"
)

// Client is a luxd client.
type Client struct{ C *api.Client }

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
	ws, err := c.C.Dial(ctx, "/v1/runs/"+url.PathEscape(id)+"/exec")
	if err != nil {
		return 0, err
	}
	defer ws.CloseNow()
	if err := wsjson.Write(ctx, ws, api.StreamOpen{Command: command}); err != nil {
		return 0, err
	}
	if err := wsjson.Write(ctx, ws, api.StreamData{EOF: true}); err != nil {
		return 0, err
	}
	for {
		var d api.StreamData
		if err := wsjson.Read(ctx, ws, &d); err != nil {
			return 0, err
		}
		if d.Error != "" {
			return 0, errors.New(d.Error)
		}
		if d.ExitCode != nil {
			return *d.ExitCode, nil
		}
	}
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
