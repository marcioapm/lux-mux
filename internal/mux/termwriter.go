package mux

import (
	"context"
	"io"
	"sync"
	"time"
)

// termWriter is the only writer of the user's terminal. Writes are queued
// in order and written by one goroutine, so whoever queues (the input
// loop, a window's stream) never waits on a slow or stalled terminal while
// it holds m.mu. A slot holds a place in the order for bytes made later
// (a repaint drawn once its window's emulator caught up).
type termWriter struct {
	out io.Writer

	mu      sync.Mutex
	q       []termItem
	n       int           // bytes queued, slots not counted
	drained chan struct{} // closed and replaced whenever n shrinks
	wake    chan struct{}
	closed  bool
	exited  chan struct{}
}

type termItem struct {
	b []byte
	s *slot
}

// slot is a place in the terminal's output filled later, once.
type slot struct {
	done chan struct{}
	b    []byte
}

func (s *slot) fill(b []byte) {
	s.b = b
	close(s.done)
}

// writeLimit is how much output may wait for the terminal before a
// window's stream reader waits (outside every lock) for it to drain.
const writeLimit = 1 << 20

func newTermWriter(out io.Writer) *termWriter {
	t := &termWriter{out: out, drained: make(chan struct{}), wake: make(chan struct{}, 1), exited: make(chan struct{})}
	go t.run()
	return t
}

// enqueue queues b, which the caller no longer changes. It never blocks.
func (t *termWriter) enqueue(b []byte) {
	if len(b) == 0 {
		return
	}
	t.mu.Lock()
	if !t.closed {
		t.q = append(t.q, termItem{b: b})
		t.n += len(b)
	}
	t.mu.Unlock()
	t.signal()
}

// slot queues a place for bytes filled in later.
func (t *termWriter) slot() *slot {
	s := &slot{done: make(chan struct{})}
	t.mu.Lock()
	if !t.closed {
		t.q = append(t.q, termItem{s: s})
	}
	t.mu.Unlock()
	t.signal()
	return s
}

func (t *termWriter) signal() {
	select {
	case t.wake <- struct{}{}:
	default:
	}
}

// Write copies p into the queue and waits while too much is queued: the
// dialogs draw through it, in order with everything else.
func (t *termWriter) Write(p []byte) (int, error) {
	t.enqueue(append([]byte{}, p...))
	t.waitBelow(context.Background(), writeLimit)
	return len(p), nil
}

// waitBelow waits until at most limit bytes are queued, ctx ends or the
// writer is closed.
func (t *termWriter) waitBelow(ctx context.Context, limit int) {
	for {
		t.mu.Lock()
		if t.n <= limit || t.closed {
			t.mu.Unlock()
			return
		}
		ch := t.drained
		t.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return
		}
	}
}

func (t *termWriter) run() {
	defer close(t.exited)
	for {
		t.mu.Lock()
		for len(t.q) == 0 {
			if t.closed {
				t.mu.Unlock()
				return
			}
			t.mu.Unlock()
			<-t.wake
			t.mu.Lock()
		}
		it := t.q[0]
		t.q[0] = termItem{}
		t.q = t.q[1:]
		t.mu.Unlock()
		b := it.b
		if it.s != nil {
			<-it.s.done
			b = it.s.b
		}
		if len(b) > 0 {
			_, _ = t.out.Write(b)
		}
		t.mu.Lock()
		if it.s == nil {
			t.n -= len(it.b)
			close(t.drained)
			t.drained = make(chan struct{})
		}
		t.mu.Unlock()
	}
}

// close writes what is queued, waiting at most d for the terminal, and
// stops the writer. A terminal that does not take it in time is left.
func (t *termWriter) close(d time.Duration) {
	t.mu.Lock()
	t.closed = true
	t.mu.Unlock()
	t.signal()
	select {
	case <-t.exited:
	case <-time.After(d):
	}
}
