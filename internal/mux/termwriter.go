package mux

import (
	"io"
	"sync"
	"time"
)

// termWriter is the only writer of the user's terminal. Writes are queued
// in order and written by one goroutine, so whoever queues (the input
// loop, a window's stream) never waits on a slow or stalled terminal. A
// slot holds a place in the order for bytes made later (a repaint drawn
// once its window's emulator caught up).
//
// Live output never waits either: lux ends a stream whose reader stops
// reading, so the stream must be read whatever the terminal does. When
// the live output queued for the terminal passes liveLimit, it is dropped
// and no more is queued until the terminal has taken everything else;
// then caughtUp runs, and the window is repainted from its emulator. As
// tmux does, a terminal that cannot keep up misses output (from its own
// scrollback), never the screen's state.
type termWriter struct {
	out io.Writer
	// caughtUp runs, on the writer's goroutine, once the queue is empty
	// after live output was dropped.
	caughtUp func()

	mu      sync.Mutex
	q       []termItem
	n       int           // bytes queued, slots not counted
	live    int           // of n, live output
	drained chan struct{} // closed and replaced whenever n shrinks
	behind  bool          // live output was dropped; caughtUp is due
	wake    chan struct{}
	closed  bool
	exited  chan struct{}

	// wtok is held (a token in a 1-slot channel) around every write to out,
	// so kill can wait for a write in flight with a deadline.
	wtok chan struct{}
	// dead is closed by kill: nothing more is written.
	dead     chan struct{}
	killOnce sync.Once
}

// writeChunk bounds one write to out: a killed writer stops within one
// chunk of a write that a slow terminal holds up.
const writeChunk = 32 << 10

type termItem struct {
	b    []byte
	s    *slot
	live bool
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

// liveLimit is how much live output may wait for the terminal; dialogs
// writing through Write wait while more than that is queued.
const liveLimit = 4 << 20

// newTermWriter makes the writer; the caller runs its run goroutine.
func newTermWriter(out io.Writer) *termWriter {
	return &termWriter{out: out, drained: make(chan struct{}), wake: make(chan struct{}, 1), exited: make(chan struct{}),
		wtok: make(chan struct{}, 1), dead: make(chan struct{})}
}

// enqueue queues b, which the caller no longer changes. It never blocks.
func (t *termWriter) enqueue(b []byte) { t.add(termItem{b: b}) }

// enqueueLive queues a window's live output, or drops it (see
// termWriter). It never blocks.
func (t *termWriter) enqueueLive(b []byte) { t.add(termItem{b: b, live: true}) }

func (t *termWriter) add(it termItem) {
	if len(it.b) == 0 {
		return
	}
	t.mu.Lock()
	switch {
	case t.closed:
	case it.live && t.behind:
	case it.live && t.live+len(it.b) > liveLimit:
		kept := t.q[:0]
		for _, x := range t.q {
			if x.live {
				t.n -= len(x.b)
			} else {
				kept = append(kept, x)
			}
		}
		clear(t.q[len(kept):])
		t.q = kept
		t.live = 0
		t.behind = true
	default:
		t.q = append(t.q, it)
		t.n += len(it.b)
		if it.live {
			t.live += len(it.b)
		}
	}
	t.mu.Unlock()
	t.signal()
}

// caughtUpDone is called by the repaint caughtUp asked for, under m.mu
// before it queues the repaint: live output after it is queued again.
func (t *termWriter) caughtUpDone() {
	t.mu.Lock()
	t.behind = false
	t.mu.Unlock()
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

// Write copies p into the queue and waits while more than liveLimit is
// queued: the dialogs draw through it, in order with everything else.
func (t *termWriter) Write(p []byte) (int, error) {
	t.enqueue(append([]byte{}, p...))
	for {
		t.mu.Lock()
		if t.n <= liveLimit || t.closed {
			t.mu.Unlock()
			return len(p), nil
		}
		ch := t.drained
		t.mu.Unlock()
		<-ch
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
			if t.behind && t.caughtUp != nil {
				t.mu.Unlock()
				t.caughtUp()
				t.mu.Lock()
				if len(t.q) > 0 || t.closed {
					continue
				}
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
			select {
			case <-it.s.done:
				b = it.s.b
			case <-t.dead:
				return
			}
		}
		if !t.write(b) {
			return
		}
		t.mu.Lock()
		if it.s == nil {
			t.n -= len(it.b)
			if it.live {
				t.live -= len(it.b)
			}
			close(t.drained)
			t.drained = make(chan struct{})
		}
		t.mu.Unlock()
	}
}

// write writes b to out in chunks of writeChunk, each under wtok; it stops,
// returning false, once kill has run.
func (t *termWriter) write(b []byte) bool {
	for len(b) > 0 {
		n := min(len(b), writeChunk)
		select {
		case t.wtok <- struct{}{}:
		case <-t.dead:
			return false
		}
		select {
		case <-t.dead:
			<-t.wtok
			return false
		default:
		}
		_, _ = t.out.Write(b[:n])
		<-t.wtok
		b = b[n:]
	}
	select {
	case <-t.dead:
		return false
	default:
		return true
	}
}

// kill stops the writer for good: whatever is queued, and every slot not
// yet written, is discarded. It waits at most d for a write already in
// flight, then calls last with out, which nothing else writes to again.
func (t *termWriter) kill(d time.Duration, last func(io.Writer)) {
	t.killOnce.Do(func() {
		t.mu.Lock()
		close(t.dead)
		t.closed = true
		clear(t.q)
		t.q, t.n, t.live, t.behind = nil, 0, 0, false
		close(t.drained)
		t.drained = make(chan struct{})
		t.mu.Unlock()
		t.signal()
		select {
		case t.wtok <- struct{}{}:
		case <-time.After(d):
		}
		last(t.out)
	})
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
