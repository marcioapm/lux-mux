package mux

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/marcioapm/lux/api"

	"github.com/marcioapm/lux-mux/internal/fakelux"
)

// TestSuccessfulWriteAfterDiscardKeepsLaterKeys: a write that succeeds
// after the queue was discarded under it removes nothing typed later.
func TestSuccessfulWriteAfterDiscardKeepsLaterKeys(t *testing.T) {
	var keys inputLog
	inWrite, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	h := newHarnessOpts(t, cfgDefault, func(l *fakelux.Server) { l.InputHook = keys.hook }, func(o *Options) {
		o.writeKeys = func(ctx context.Context, c *websocket.Conn, d api.StreamData) error {
			held := false
			once.Do(func() { held = true })
			if held {
				close(inWrite)
				<-release
				return nil
			}
			return wsjson.Write(ctx, c, d)
		}
	})
	a := h.live(0)
	h.type_("old")
	<-inWrite
	w := h.win(0)
	w.dropInput()
	h.type_("later-keys")
	h.waitFor("later keys queued", func() bool { return string(h.pending(0)) == "later-keys" })
	close(release)
	h.waitFor("later keys delivered", func() bool { return len(keys.of(a)) >= len("later-keys") })
	if got := string(keys.of(a)); got != "later-keys" {
		t.Fatalf("received %q", got)
	}
	h.detach()
}

// TestRelaunchAfterMoveKeepsNewKeys: after a Run that moved (epoch 2)
// ends and r relaunches it (a new Run, epoch 1), keys typed while the new
// Run's stream is down reach it, not discarded as for a moved shell.
func TestRelaunchAfterMoveKeepsNewKeys(t *testing.T) {
	var keys inputLog
	var gate attachGate
	h := newHarness(t, cfgDefault, func(l *fakelux.Server) { l.InputHook = keys.hook; l.AttachHook = gate.hook })
	a := h.live(0)
	h.lux.Move(a, false)
	h.waitFor("new epoch", func() bool {
		h.m.mu.Lock()
		defer h.m.mu.Unlock()
		return h.m.windows[0].epoch >= 2
	})
	h.lux.End(a, "failed", "timeout")
	h.waitFor("ended", func() bool { return h.phase(0) == phaseDead })
	// The relaunched Run is the fake's next id: its first attach is held,
	// so the keys typed now wait for its first stream.
	b := fmt.Sprintf("run_%04d", len(h.lux.RunIDs())+1)
	gate.hold(b)
	t.Cleanup(func() { gate.release(b) })
	h.type_("r")
	h.waitFor("new Run attaching", func() bool { return h.window(0) == b && gate.held_() > 0 })
	h.type_("new-run-keys")
	h.waitFor("queued", func() bool { return string(h.pending(0)) == "new-run-keys" })
	gate.release(b)
	h.waitFor("delivered", func() bool { return string(keys.of(b)) == "new-run-keys" })
	if strings.Contains(string(h.term.rawBytes()), "discarded") {
		t.Fatal("discard notice for the relaunched Run")
	}
	h.detach()
}

// TestFailedWriteAfterTheRunEndedKeepsNothing: keys whose write was in
// flight when the Run ended (its queue discarded) are not put back when
// that write then fails: the shell they were typed for is gone, and a
// relaunched Run must not get them.
func TestFailedWriteAfterTheRunEndedKeepsNothing(t *testing.T) {
	var keys inputLog
	var mu sync.Mutex
	hold := false
	inWrite := make(chan struct{})
	release := make(chan struct{})
	returned := make(chan struct{})
	h := newHarnessOpts(t, cfgDefault, func(l *fakelux.Server) { l.InputHook = keys.hook },
		func(o *Options) {
			o.writeKeys = func(ctx context.Context, c *websocket.Conn, d api.StreamData) error {
				mu.Lock()
				held := hold
				hold = false
				mu.Unlock()
				if !held {
					return wsjson.Write(ctx, c, d)
				}
				close(inWrite)
				<-release
				defer close(returned)
				return errors.New("injected: the stream closed under the write")
			}
		})
	a := h.live(0)
	mu.Lock()
	hold = true
	mu.Unlock()
	h.type_("old")
	<-inWrite
	h.lux.End(a, "failed", "timeout")
	h.waitFor("ended", func() bool { return h.phase(0) == phaseDead })
	close(release)
	<-returned
	// The writer handles the failure within microseconds of the return.
	for range 20 {
		if p := h.pending(0); len(p) != 0 {
			t.Fatalf("kept after the Run ended: %q", p)
		}
		time.Sleep(5 * time.Millisecond)
	}
	h.type_("r")
	h.waitFor("relaunched", func() bool { return h.window(0) != "" && h.window(0) != a })
	b := h.live(0)
	h.type_("new")
	h.waitFor("typed", func() bool { return bytes.Contains(keys.of(b), []byte("new")) })
	if got := string(keys.of(b)); got != "new" {
		t.Fatalf("the relaunched Run got %q", got)
	}
	h.detach()
}

// TestFailedWriteIsResentWhole: keys whose write failed are written again
// whole, ahead of keys typed after, on the next stream. A write that
// failed after lux got its message (the close raced its return) is
// therefore sent twice: the documented duplicate. Keys are lost in
// neither case.
func TestFailedWriteIsResentWhole(t *testing.T) {
	for _, tc := range []struct {
		name string
		// delivered: the failing write reached lux before the error.
		delivered bool
		want      string
	}{
		{"not delivered", false, "abc,def"},
		{"delivered before the error", true, "abcabc,def"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var keys inputLog
			var gate attachGate
			var mu sync.Mutex
			failNext := false
			failed := make(chan struct{})
			h := newHarnessOpts(t, cfgDefault, func(l *fakelux.Server) { l.InputHook = keys.hook; l.AttachHook = gate.hook },
				func(o *Options) {
					o.writeKeys = func(ctx context.Context, c *websocket.Conn, d api.StreamData) error {
						mu.Lock()
						fail := failNext
						failNext = false
						mu.Unlock()
						if !fail {
							return wsjson.Write(ctx, c, d)
						}
						defer close(failed)
						if tc.delivered {
							if err := wsjson.Write(ctx, c, d); err != nil {
								return err
							}
						}
						return errors.New("injected: the stream closed under the write")
					}
				})
			a := h.live(0)
			mu.Lock()
			failNext = true
			mu.Unlock()
			gate.hold(a)
			t.Cleanup(func() { gate.release(a) })
			h.type_("abc")
			<-failed
			if tc.delivered {
				h.waitFor("first copy read", func() bool { return string(keys.of(a)) == "abc" })
			}
			h.waitFor("kept at the head", func() bool { return string(h.pending(0)) == "abc" })
			h.type_(",def")
			h.waitFor("queued after", func() bool { return string(h.pending(0)) == "abc,def" })
			h.lux.Blip(a)
			h.waitFor("attach held", func() bool { return gate.held_() == 1 })
			gate.release(a)
			h.waitFor("sent", func() bool { return len(keys.of(a)) >= len(tc.want) })
			if got := keys.of(a); !bytes.Equal(got, []byte(tc.want)) {
				t.Fatalf("the shell got %q, want %q", got, tc.want)
			}
			h.detach()
		})
	}
}
