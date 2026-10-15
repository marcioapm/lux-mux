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

	"github.com/coder/websocket"
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

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

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
