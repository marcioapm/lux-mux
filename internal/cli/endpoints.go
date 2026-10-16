package cli

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

// endpoints is the client set: one per lux the profiles name, over the
// lux CLI configuration.
func endpoints(env Env) *luxapi.Endpoints { return luxapi.NewEndpoints(env.Lux(), env.Lookup) }

// knownEndpoints are the endpoints of every configured profile, and those
// sessions' windows and spares live on, each once, in a stable order.
func knownEndpoints(eps *luxapi.Endpoints, cfg *config.Config, sessions ...*state.Session) []config.Endpoint {
	seen := map[config.Endpoint]bool{}
	var out []config.Endpoint
	add := func(ep config.Endpoint) {
		if !seen[ep] {
			seen[ep] = true
			out = append(out, ep)
		}
	}
	for _, ep := range cfg.Endpoints(eps.Base()) {
		add(ep)
	}
	def := eps.Default()
	for _, s := range sessions {
		if s == nil {
			continue
		}
		for _, w := range s.Windows {
			add(w.EndpointOr(def))
		}
		for _, ep := range s.SpareEndpoints {
			add(ep)
		}
		for _, ep := range s.Unlisted {
			add(ep)
		}
	}
	config.SortEndpoints(out)
	return out
}

// Timeouts of the commands' requests. Each endpoint's listing has its own
// listTimeout, so a lux that never answers costs a command that long and
// no more; the requests after the listing get a context made once it
// returned. Variables: tests shorten them.
var (
	listTimeout = 10 * time.Second
	// opTimeout bounds new's and new-window's dry run and submit.
	opTimeout = 30 * time.Second
	// bulkTimeout bounds kill-session's and spares' terminates.
	bulkTimeout = 2 * time.Minute
)

// listEach lists, on every endpoint at once, what list asks of its client,
// each under its own listTimeout. An endpoint whose client cannot be made,
// or whose listing fails, has its Err set; the others' Runs are there.
func listEach(eps *luxapi.Endpoints, list []config.Endpoint, fn func(context.Context, *luxapi.Client) ([]*api.Run, error)) []state.Listing {
	out := make([]state.Listing, len(list))
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
			ctx, cancel := context.WithTimeout(context.Background(), listTimeout)
			defer cancel()
			runs, err := fn(ctx, c)
			out[i].Runs, out[i].Err = runs, luxapi.Described(err)
		}()
	}
	wg.Wait()
	return out
}

// listSession is every endpoint's listing of session's Runs.
func listSession(eps *luxapi.Endpoints, list []config.Endpoint, session string) []state.Listing {
	return listEach(eps, list, func(ctx context.Context, c *luxapi.Client) ([]*api.Run, error) {
		return c.ListSession(ctx, session)
	})
}

// listAll is every endpoint's listing of the Runs luxmux made.
func listAll(eps *luxapi.Endpoints, list []config.Endpoint) []state.Listing {
	return listEach(eps, list, func(ctx context.Context, c *luxapi.Client) ([]*api.Run, error) {
		return c.ListAll(ctx)
	})
}

// target is a Run and the endpoint it was listed on.
type target struct {
	ep config.Endpoint
	r  *api.Run
}

// liveRuns are the listed Runs that are not terminated and that keep
// takes, each once (one lux under two key sources lists it twice).
func liveRuns(ls []state.Listing, keep func(*api.Run) bool) []target {
	var out []target
	seen := map[string]bool{}
	for _, l := range ls {
		for _, r := range l.Runs {
			if state.Live(r.State) && keep(r) && !seen[r.ID] {
				seen[r.ID] = true
				out = append(out, target{l.Endpoint, r})
			}
		}
	}
	return out
}

// reportFailed says on w which endpoints could not be listed while others
// could. When every one failed it says nothing and returns their errors,
// each naming its endpoint (one endpoint: its error as it is).
func reportFailed(w io.Writer, ls []state.Listing) error {
	var errs []error
	for _, l := range ls {
		if l.Err != nil {
			errs = append(errs, fmt.Errorf("endpoint unreachable: %s: %w", l.Endpoint, l.Err))
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

// known maps every listed Run's id to its state.
func known(ls []state.Listing) map[string]string {
	out := map[string]string{}
	for _, l := range ls {
		for _, r := range l.Runs {
			out[r.ID] = r.State
		}
	}
	return out
}

// downOf reports whether an endpoint's listing failed.
func downOf(ls []state.Listing) func(config.Endpoint) bool {
	down := map[config.Endpoint]bool{}
	for _, l := range ls {
		if l.Err != nil {
			down[l.Endpoint] = true
		}
	}
	return func(ep config.Endpoint) bool { return down[ep] }
}
