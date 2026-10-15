// Command luxmux is a tmux-style multiplexer whose windows are shells on
// lux Runs.
package main

import (
	"fmt"
	"os"

	"github.com/marcioapm/lux/api"

	"github.com/marcioapm/lux-mux/internal/cli"
	"github.com/marcioapm/lux-mux/internal/config"
	"github.com/marcioapm/lux-mux/internal/state"
	"github.com/marcioapm/lux-mux/internal/ui"
)

// lookup finds secret values; the end-to-end test replaces it.
var lookup = os.LookupEnv

func main() {
	cfgPath, err := config.Path()
	if err != nil {
		fmt.Fprintln(os.Stderr, "luxmux:", err)
		os.Exit(1)
	}
	stateDir, err := state.Dir()
	if err != nil {
		fmt.Fprintln(os.Stderr, "luxmux:", err)
		os.Exit(1)
	}
	os.Exit(cli.Main(cli.Env{
		Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr,
		Lux:        api.LoadConfig,
		ConfigPath: cfgPath, StateDir: stateDir,
		Lookup: lookup,
		UI:     ui.New(),
	}, os.Args[1:]))
}
