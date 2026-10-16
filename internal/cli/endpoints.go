package cli

import (
	"time"

	"github.com/marcioapm/lux-mux/internal/config"
	"github.com/marcioapm/lux-mux/internal/luxapi"
	"github.com/marcioapm/lux-mux/internal/sessions"
)

// endpoints is the client set: one per lux the profiles name, over the
// lux CLI configuration.
func endpoints(env Env) *luxapi.Endpoints { return luxapi.NewEndpoints(env.Lux(), env.Lookup) }

// luxes are the luxes cfg's profiles name over eps, with the endpoints
// that cannot be listed said on stderr.
func luxes(env Env, eps *luxapi.Endpoints, cfg *config.Config) sessions.Luxes {
	return sessions.Luxes{Eps: eps, Cfg: cfg, Warn: env.Stderr}
}

// Timeouts of the commands' requests after the listing, which has its
// own sessions.ListTimeout per lux: each gets a context made once the
// listing returned. Variables: tests shorten them.
var (
	// opTimeout bounds new's and new-window's dry run and submit.
	opTimeout = 30 * time.Second
	// bulkTimeout bounds kill-session's and spares' terminates.
	bulkTimeout = 2 * time.Minute
)
