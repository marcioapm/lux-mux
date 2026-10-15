package mux

import (
	"bytes"
	"io"
	"sync"
	"testing"
	"time"
)

// gateOut records writes; the first one blocks until open is closed,
// later ones (from any goroutine) do not wait for it.
type gateOut struct {
	mu      sync.Mutex
	writes  [][]byte
	n       int
	entered chan struct{}
	open    chan struct{}
}

func (g *gateOut) Write(p []byte) (int, error) {
	g.mu.Lock()
	g.n++
	first := g.n == 1
	g.mu.Unlock()
	if first {
		close(g.entered)
		<-g.open
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.writes = append(g.writes, append([]byte{}, p...))
	return len(p), nil
}

func (g *gateOut) got() [][]byte {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([][]byte{}, g.writes...)
}

// TestKillDiscardsQueueAndWaitsForWriteInFlight: kill waits for the write
// the terminal is holding, then nothing queued (output, slots filled
// before or after) is written; only kill's last write follows.
func TestKillDiscardsQueueAndWaitsForWriteInFlight(t *testing.T) {
	g := &gateOut{entered: make(chan struct{}), open: make(chan struct{})}
	tw := newTermWriter(g)
	go tw.run()
	tw.enqueue([]byte("in-flight"))
	<-g.entered
	tw.enqueue(bytes.Repeat([]byte("q"), 3*writeChunk))
	filled := tw.slot()
	filled.fill([]byte("slot-filled-before"))
	later := tw.slot()
	tw.enqueueLive([]byte("live"))

	killed := make(chan struct{})
	go func() {
		tw.kill(5*time.Second, func(out io.Writer) { _, _ = out.Write([]byte("RESET")) })
		close(killed)
	}()
	select {
	case <-killed:
		t.Fatal("kill wrote its Reset while a write was in flight")
	case <-time.After(200 * time.Millisecond):
	}
	close(g.open)
	<-killed
	later.fill([]byte("slot-filled-after"))
	select {
	case <-tw.exited:
	case <-timeout():
		t.Fatal("the writer did not stop")
	}
	tw.enqueue([]byte("after-kill"))
	if w := g.got(); len(w) != 2 || string(w[0]) != "in-flight" || string(w[1]) != "RESET" {
		t.Fatalf("writes %q", w)
	}
}

// TestKillWhileWaitingOnSlot: a writer waiting for a slot's bytes stops at
// kill, and the slot filled afterwards is not written.
func TestKillWhileWaitingOnSlot(t *testing.T) {
	g := &gateOut{entered: make(chan struct{}), open: make(chan struct{})}
	close(g.open)
	tw := newTermWriter(g)
	go tw.run()
	s := tw.slot()
	tw.enqueue([]byte("behind-the-slot"))
	// The writer took the slot off the queue and waits for it.
	if !waitUntil(func() bool {
		tw.mu.Lock()
		defer tw.mu.Unlock()
		return len(tw.q) == 1
	}) {
		t.Fatal("the writer did not reach the slot")
	}
	tw.kill(5*time.Second, func(out io.Writer) { _, _ = out.Write([]byte("RESET")) })
	select {
	case <-tw.exited:
	case <-timeout():
		t.Fatal("the writer did not stop")
	}
	s.fill([]byte("slot-filled-after"))
	if w := g.got(); len(w) != 1 || string(w[0]) != "RESET" {
		t.Fatalf("writes %q", w)
	}
}

func waitUntil(cond func() bool) bool {
	for range 500 {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// written is everything g was given, joined.
func (g *gateOut) written() []byte {
	return bytes.Join(g.got(), nil)
}

// TestLiveOverflowKeepsOtherOutput: live output past liveLimit is dropped,
// but what else was queued between it (a repaint, a prompt) is written,
// in order, and caughtUp runs once the terminal took it.
func TestLiveOverflowKeepsOtherOutput(t *testing.T) {
	g := &gateOut{entered: make(chan struct{}), open: make(chan struct{})}
	tw := newTermWriter(g)
	caught := make(chan struct{}, 1)
	tw.caughtUp = func() {
		tw.caughtUpDone()
		caught <- struct{}{}
	}
	go tw.run()
	tw.enqueue([]byte("first|"))
	<-g.entered
	tw.enqueueLive(bytes.Repeat([]byte("L"), 3<<20))
	tw.enqueue([]byte("PAINT|"))
	s := tw.slot()
	tw.enqueueLive(bytes.Repeat([]byte("M"), 2<<20))
	if !tw.isBehind() {
		t.Fatal("not behind past liveLimit")
	}
	s.fill([]byte("SLOT|"))
	close(g.open)
	select {
	case <-caught:
	case <-timeout():
		t.Fatal("caughtUp did not run")
	}
	if got := string(g.written()); got != "first|PAINT|SLOT|" {
		t.Fatalf("written %d bytes: %.40q", len(got), got)
	}
	tw.close(time.Second)
}

// TestDialogWriteWaitsForTerminal: a dialog's Write returns only once no
// more than liveLimit waits for the terminal.
func TestDialogWriteWaitsForTerminal(t *testing.T) {
	g := &gateOut{entered: make(chan struct{}), open: make(chan struct{})}
	tw := newTermWriter(g)
	go tw.run()
	tw.enqueue([]byte("first"))
	<-g.entered
	tw.enqueue(bytes.Repeat([]byte("q"), liveLimit+1))
	done := make(chan struct{})
	go func() {
		_, _ = tw.Write([]byte("dialog"))
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("Write returned with more than liveLimit queued")
	case <-time.After(100 * time.Millisecond):
	}
	close(g.open)
	select {
	case <-done:
	case <-timeout():
		t.Fatal("Write did not return once the terminal took the queue")
	}
	tw.close(time.Second)
}

// TestKillStopsLongWrite: a large item being written when kill runs stops
// at the next writeChunk boundary.
func TestKillStopsLongWrite(t *testing.T) {
	g := &gateOut{entered: make(chan struct{}), open: make(chan struct{})}
	tw := newTermWriter(g)
	go tw.run()
	tw.enqueue(bytes.Repeat([]byte("x"), 4*writeChunk))
	<-g.entered
	killed := make(chan struct{})
	go func() {
		tw.kill(5*time.Second, func(out io.Writer) { _, _ = out.Write([]byte("RESET")) })
		close(killed)
	}()
	<-tw.dead
	close(g.open)
	<-killed
	<-tw.exited
	if w := g.got(); len(w) != 2 || len(w[0]) != writeChunk || string(w[1]) != "RESET" {
		t.Fatalf("%d writes, the first %d bytes", len(w), len(w[0]))
	}
}
