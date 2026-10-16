package sessions

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/marcioapm/lux/api"

	"github.com/marcioapm/lux-mux/internal/config"
	"github.com/marcioapm/lux-mux/internal/luxapi"
	"github.com/marcioapm/lux-mux/internal/state"
)

// ListTimeout bounds each endpoint's listing, so a lux that never
// answers costs a caller that long and no more. A variable: tests
// shorten it.
var ListTimeout = 10 * time.Second

// Luxes are the luxes a session's Runs may be on: one client per
// endpoint (Eps), and the configuration whose profiles name endpoints.
// Warn receives the endpoints that could not be listed while others
// could (nil: nowhere).
type Luxes struct {
	Eps  *luxapi.Endpoints
	Cfg  *config.Config
	Warn io.Writer
}

// Endpoints are those of every configured profile and those files'
// windows, spares and unlisted rebuilds live on, each once, in a stable
// order.
func (x Luxes) Endpoints(files ...*state.Session) []config.Endpoint {
	out := append(x.Cfg.Endpoints(x.Eps.Base()), endpointsOf(x.Eps.Default(), files...)...)
	seen := map[config.Endpoint]bool{}
	uniq := out[:0]
	for _, ep := range out {
		if !seen[ep] {
			seen[ep] = true
			uniq = append(uniq, ep)
		}
	}
	config.SortEndpoints(uniq)
	return uniq
}

// endpointsOf are the endpoints files record (def: a window's that
// records none), not deduplicated.
func endpointsOf(def config.Endpoint, files ...*state.Session) []config.Endpoint {
	var out []config.Endpoint
	for _, s := range files {
		if s == nil {
			continue
		}
		for _, w := range s.Windows {
			out = append(out, w.EndpointOr(def))
		}
		for slot := range s.Spares {
			out = append(out, s.SpareEndpoint(slot, def))
		}
		out = append(out, s.Unlisted...)
	}
	return out
}

// ListSession is every endpoint's listing of the Runs labelled label
// (Endpoints of files).
func (x Luxes) ListSession(ctx context.Context, label string, files ...*state.Session) Listings {
	return ListEach(ctx, x.Eps, x.Endpoints(files...), func(ctx context.Context, c *luxapi.Client) ([]*api.Run, error) {
		return c.ListSession(ctx, label)
	})
}

// ListAll is every endpoint's listing of the Runs luxmux made.
func (x Luxes) ListAll(ctx context.Context, files ...*state.Session) Listings {
	return ListEach(ctx, x.Eps, x.Endpoints(files...), func(ctx context.Context, c *luxapi.Client) ([]*api.Run, error) {
		return c.ListAll(ctx)
	})
}

// report is ls.Report on x.Warn.
func (x Luxes) report(ls Listings) error {
	w := x.Warn
	if w == nil {
		w = io.Discard
	}
	return ls.Report(w)
}

// ListEach lists, on every endpoint of list at once, what fn asks of its
// client, each under its own ListTimeout within ctx. An endpoint whose
// client cannot be made, or whose listing fails, has its Err set; the
// others' Runs are there.
func ListEach(ctx context.Context, eps *luxapi.Endpoints, list []config.Endpoint, fn func(context.Context, *luxapi.Client) ([]*api.Run, error)) Listings {
	out := make(Listings, len(list))
	var wg sync.WaitGroup
	for i, ep := range list {
		out[i].Endpoint = ep
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := eps.Client(ep)
			if err != nil {
				out[i].Err = err
				return
			}
			lctx, cancel := context.WithTimeout(ctx, ListTimeout)
			defer cancel()
			runs, err := fn(lctx, c)
			out[i].Runs, out[i].Err = runs, luxapi.Described(err)
		}()
	}
	wg.Wait()
	return out
}

// Listings are the listings of several endpoints.
type Listings []state.Listing

// Target is a Run and the endpoint it was listed on.
type Target struct {
	Endpoint config.Endpoint
	Run      *api.Run
}

// Terminate terminates t's Run on its endpoint.
func (t Target) Terminate(ctx context.Context, eps *luxapi.Endpoints) error {
	c, err := eps.Client(t.Endpoint)
	if err != nil {
		return err
	}
	return luxapi.Described(c.Terminate(ctx, t.Run.ID))
}

// Live are the listed Runs that are not terminated and that keep takes,
// each once (one lux under two key sources lists it twice).
func (ls Listings) Live(keep func(*api.Run) bool) []Target {
	var out []Target
	seen := map[string]bool{}
	for _, l := range ls {
		for _, r := range l.Runs {
			if state.Live(r.State) && keep(r) && !seen[r.ID] {
				seen[r.ID] = true
				out = append(out, Target{l.Endpoint, r})
			}
		}
	}
	return out
}

// Runs are every listed Run, each once.
func (ls Listings) Runs() []*api.Run {
	var out []*api.Run
	seen := map[string]bool{}
	for _, l := range ls {
		for _, r := range l.Runs {
			if !seen[r.ID] {
				seen[r.ID] = true
				out = append(out, r)
			}
		}
	}
	return out
}

// Report says on w which endpoints could not be listed while others
// could. When every one failed it says nothing and returns their errors,
// each naming its endpoint (one endpoint: its error as it is).
func (ls Listings) Report(w io.Writer) error {
	var errs []error
	for _, l := range ls {
		if l.Err != nil {
			errs = append(errs, fmt.Errorf("%s: %s: %w", luxapi.FailedWord(l.Err), l.Endpoint, l.Err))
		}
	}
	switch {
	case len(errs) == 0:
		return nil
	case len(errs) < len(ls):
		for _, e := range errs {
			fmt.Fprintln(w, "luxmux:", e)
		}
		return nil
	case len(ls) == 1:
		return ls[0].Err
	}
	return errors.Join(errs...)
}

// Known maps every listed Run's id to its state.
func (ls Listings) Known() map[string]string {
	out := map[string]string{}
	for _, l := range ls {
		for _, r := range l.Runs {
			out[r.ID] = r.State
		}
	}
	return out
}

// Down reports whether an endpoint's listing failed.
func (ls Listings) Down() func(config.Endpoint) bool {
	down := map[config.Endpoint]bool{}
	for _, l := range ls {
		if l.Err != nil {
			down[l.Endpoint] = true
		}
	}
	return func(ep config.Endpoint) bool { return down[ep] }
}

// Unlisted are the endpoints whose listing failed.
func (ls Listings) Unlisted() []config.Endpoint {
	var out []config.Endpoint
	for _, l := range ls {
		if l.Err != nil {
			out = append(out, l.Endpoint)
		}
	}
	return out
}

// ErrOn is ep's listing error (nil: it answered, or it was not listed).
func (ls Listings) ErrOn(ep config.Endpoint) error {
	for _, l := range ls {
		if l.Endpoint == ep && l.Err != nil {
			return l.Err
		}
	}
	return nil
}

// Files are the session files in dir that can be read.
func Files(dir string) []*state.Session {
	names, _ := state.List(dir)
	var out []*state.Session
	for _, n := range names {
		if s, ok, _ := state.Load(dir, n); ok {
			out = append(out, s)
		}
	}
	return out
}
