package mux

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/marcioapm/lux-mux/internal/config"
	"github.com/marcioapm/lux-mux/internal/fakelux"
)

// TestDetachNameWriteStopsAtClose: a detach can leave luxmux's window-name
// exec in flight (its stream is cancelled, not waited for). Once the fake
// lux is closed, that exec must write nothing more in the Run's .luxmux
// directory, or the test's TempDir cleanup fails with "directory not
// empty". A flock stub on PATH holds only the name write (the start
// wrapper exports LUXMUX_START before it locks; the exec has none).
func TestDetachNameWriteStopsAtClose(t *testing.T) {
	flock, err := exec.LookPath("flock")
	if err != nil {
		t.Skip("no flock(1): the name write publishes nothing")
	}
	bin := t.TempDir()
	stub := "#!/bin/sh\n" +
		`[ -n "${LUXMUX_START-}" ] || { : >"$` + config.EnvTestWorkspace + `/.name-write"; sleep 0.5; }` + "\n" +
		`exec ` + flock + ` "$@"` + "\n"
	if err := os.WriteFile(filepath.Join(bin, "flock"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "runs")
	h := newHarnessEnv(t, hooksConfig, map[string]string{"EXAMPLE_TOKEN": exampleSecret}, func(s *fakelux.Server) {
		s.RunShells(dir, "PS1=PROMPT> ", "ENV=", "TERM=xterm", "PATH="+bin+":"+os.Getenv("PATH"))
	})
	id := h.live(0)
	ws := fakelux.WorkspaceDir(dir, id)
	h.waitFor("the name write", func() bool {
		_, err := os.Stat(filepath.Join(ws, ".name-write"))
		return err == nil
	})
	h.detach()
	h.lux.Close()
	window := filepath.Join(ws, ".luxmux", config.WindowFile)
	if fakelux.WaitFor(context.Background(), time.Second, func() bool {
		_, err := os.Stat(window)
		return !errors.Is(err, os.ErrNotExist)
	}) {
		t.Fatalf("%s was written after the fake lux was closed", window)
	}
}
