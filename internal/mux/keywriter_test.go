package mux

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/marcioapm/lux-mux/internal/fakelux"
)

// refreshNow closes window 0's stream as a refresh does (luxmux closes it
// so its supervisor attaches again with restore), ignoring the refresh
// limit. It reports false if the window had no stream to close or one
// still closing.
func (h *harness) refreshNow() bool {
	w := h.win(0)
	h.m.mu.Lock()
	defer h.m.mu.Unlock()
	if w.getConn() == nil || w.refreshing || w.refreshDue {
		return false
	}
	w.lastAttach = time.Time{}
	h.m.refreshLocked(w)
	return w.refreshing
}

// typeKeys types n keys "kNNNN," from another goroutine, a few at a time
// with short pauses, as the input loop reads them; it returns what was
// typed and a channel closed once all were.
func (h *harness) typeKeys(n int) ([]byte, chan struct{}) {
	var want bytes.Buffer
	for i := range n {
		fmt.Fprintf(&want, "k%04d,", i)
	}
	typed := make(chan struct{})
	go func() {
		defer close(typed)
		for i := range n {
			if _, err := fmt.Fprintf(h.keys, "k%04d,", i); err != nil {
				return
			}
			if i%4 == 3 {
				time.Sleep(200 * time.Microsecond)
			}
		}
	}()
	return want.Bytes(), typed
}

// TestKeysExactlyOnceInOrderAcrossRefreshes: keys typed without pause
// while the window's stream is closed by a refresh again and again reach
// the shell each exactly once, in the order typed: those in flight when a
// stream closes, those queued while it has none, and those typed as the
// next comes up.
func TestKeysExactlyOnceInOrderAcrossRefreshes(t *testing.T) {
	var keys inputLog
	h := newHarness(t, cfgDefault, func(l *fakelux.Server) { l.InputHook = keys.hook })
	a := h.live(0)
	want, typed := h.typeKeys(1500)
	closes := 0
	for done := false; !done; {
		select {
		case <-typed:
			done = true
		default:
			if h.refreshNow() {
				closes++
			}
			time.Sleep(500 * time.Microsecond)
		}
	}
	if closes < 4 {
		t.Fatalf("the stream was closed %d times while keys were typed: the test did not test it", closes)
	}
	fakelux.WaitFor(context.Background(), 5*time.Second, func() bool { return len(keys.of(a)) >= len(want) })
	time.Sleep(20 * time.Millisecond)
	if got := keys.of(a); !bytes.Equal(got, want) {
		t.Fatalf("after %d closes the shell got:\n%s", closes, keyDiff(got, want))
	}
	h.detach()
}

// TestKeysInOrderAcrossStreamsLuxEnds: when lux ends the stream (an
// attach that fell behind, a network blip) keys written before luxmux
// saw the end may be lost: the stream protocol has no acknowledgement.
// None arrives twice or out of order, and keys typed once the stream is
// gone all arrive on the next.
func TestKeysInOrderAcrossStreamsLuxEnds(t *testing.T) {
	var keys inputLog
	h := newHarness(t, cfgDefault, func(l *fakelux.Server) { l.InputHook = keys.hook })
	a := h.live(0)
	want, typed := h.typeKeys(1500)
	closes := 0
	for done := false; !done; {
		select {
		case <-typed:
			done = true
		default:
			if h.win(0).getConn() != nil && h.lux.Streams(a) > 0 {
				if closes%2 == 0 {
					h.lux.FellBehind(a)
				} else {
					h.lux.Blip(a)
				}
				closes++
			}
			time.Sleep(2 * time.Millisecond)
		}
	}
	if closes < 4 {
		t.Fatalf("the stream was ended %d times while keys were typed: the test did not test it", closes)
	}
	h.live(0)
	h.type_("END")
	h.waitFor("the keys after", func() bool { return bytes.HasSuffix(keys.of(a), []byte("END")) })
	got := bytes.TrimSuffix(keys.of(a), []byte("END"))
	g := strings.Split(strings.TrimSuffix(string(got), ","), ",")
	for i, k := range g {
		if len(k) != 5 || !bytes.Contains(want, []byte(k+",")) {
			t.Fatalf("after %d ends: a key cut or not typed %q", closes, k)
		}
		if i > 0 && k <= g[i-1] {
			t.Fatalf("after %d ends: %s then %s\n%s", closes, g[i-1], k, keyDiff(got, want))
		}
	}
	t.Logf("%d ends: %d of %d keys arrived", closes, len(g), len(want)/6)
	h.detach()
}

// keyDiff describes how got, a run of "kNNNN," keys, differs from want.
func keyDiff(got, want []byte) string {
	g := strings.Split(strings.TrimSuffix(string(got), ","), ",")
	w := strings.Split(strings.TrimSuffix(string(want), ","), ",")
	seen := map[string]int{}
	var dup, order []string
	for i, k := range g {
		seen[k]++
		if seen[k] == 2 {
			dup = append(dup, k)
		}
		if i > 0 && k < g[i-1] {
			order = append(order, g[i-1]+" then "+k)
		}
	}
	var missing []string
	for _, k := range w {
		if seen[k] == 0 {
			missing = append(missing, k)
		}
	}
	return fmt.Sprintf("%d keys, want %d; missing %v; duplicated %v; out of order %v", len(g), len(w), missing, dup, order)
}

// TestKeysTypedAsTheStreamComesUpInOrder: keys typed while the window has
// no stream, and on while its next stream comes up, all arrive once, in
// order.
func TestKeysTypedAsTheStreamComesUpInOrder(t *testing.T) {
	var keys inputLog
	var gate attachGate
	h := newHarness(t, cfgDefault, func(l *fakelux.Server) { l.InputHook = keys.hook; l.AttachHook = gate.hook })
	a := h.live(0)
	gate.hold(a)
	t.Cleanup(func() { gate.release(a) })
	h.lux.Blip(a)
	h.waitFor("attach held", func() bool { return gate.held_() == 1 })
	const n = 300
	var want bytes.Buffer
	for i := range n {
		fmt.Fprintf(&want, "k%04d,", i)
	}
	half := make(chan struct{})
	typed := make(chan struct{})
	go func() {
		defer close(typed)
		for i := range n {
			if i == n/2 {
				close(half)
			}
			if _, err := fmt.Fprintf(h.keys, "k%04d,", i); err != nil {
				return
			}
			if i >= n/2 {
				time.Sleep(100 * time.Microsecond)
			}
		}
	}()
	<-half
	if got := keys.of(a); len(got) != 0 {
		t.Fatalf("keys reached a stream that is down: %q", got)
	}
	gate.release(a)
	<-typed
	h.waitFor("all keys", func() bool { return len(keys.of(a)) >= want.Len() })
	time.Sleep(20 * time.Millisecond)
	if got := keys.of(a); !bytes.Equal(got, want.Bytes()) {
		t.Fatalf("the shell got:\n%s", keyDiff(got, want.Bytes()))
	}
	h.detach()
}

// TestKeysOfAFailedWriteDiscardedOnAMove: keys whose write failed wait at
// the head of the queue for the same shell; when the Run comes back on a
// new placement they are discarded with the notice instead, and only keys
// typed after reach the new shell.
func TestKeysOfAFailedWriteDiscardedOnAMove(t *testing.T) {
	var keys inputLog
	h := newHarness(t, cfgDefault, func(l *fakelux.Server) { l.InputHook = keys.hook })
	a := h.live(0)
	w := h.win(0)
	w.connMu.Lock()
	w.conn = closedConn(t)
	w.connMu.Unlock()
	h.type_("lost")
	h.waitFor("kept after the failed write", func() bool { return string(h.pending(0)) == "lost" })
	h.lux.Move(a, true)
	h.waitFor("reconnecting", func() bool { return h.phase(0) == phaseReconnecting })
	h.type_("-too")
	h.waitFor("kept", func() bool { return string(h.pending(0)) == "lost-too" })
	h.lux.Start(a)
	h.live(0)
	h.waitScreen("discarded notice", h.contains("window 0: 8 typed bytes discarded — it moved to a new shell"))
	h.type_("after")
	h.waitFor("typed after", func() bool { return bytes.Contains(keys.of(a), []byte("after")) })
	if got := string(keys.of(a)); got != "after" {
		t.Fatalf("the Run's shells got %q", got)
	}
	h.detach()
}

// TestKeysWhileLuxReadsSlowly: keys typed while lux has not read the
// previous write yet go out after it, whole, in order, never twice.
func TestKeysWhileLuxReadsSlowly(t *testing.T) {
	var mu sync.Mutex
	var msgs []string
	release := make(chan struct{})
	h := newHarness(t, cfgDefault, func(l *fakelux.Server) {
		l.InputHook = func(id string, d []byte) {
			mu.Lock()
			msgs = append(msgs, string(d))
			first := len(msgs) == 1
			mu.Unlock()
			if first {
				<-release
			}
		}
	})
	h.live(0)
	h.type_("first")
	h.waitFor("first write read", func() bool { mu.Lock(); defer mu.Unlock(); return len(msgs) == 1 })
	for i := range 50 {
		h.type_(fmt.Sprintf("b%02d,", i))
	}
	close(release)
	var want strings.Builder
	want.WriteString("first")
	for i := range 50 {
		fmt.Fprintf(&want, "b%02d,", i)
	}
	h.waitFor("all keys", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(strings.Join(msgs, "")) >= want.Len()
	})
	time.Sleep(20 * time.Millisecond)
	mu.Lock()
	got := strings.Join(msgs, "")
	mu.Unlock()
	if got != want.String() {
		t.Fatalf("got %q\nwant %q", got, want.String())
	}
	h.detach()
}
