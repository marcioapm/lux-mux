package mux

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/marcioapm/lux/api"

	"github.com/marcioapm/lux-mux/internal/config"
	"github.com/marcioapm/lux-mux/internal/luxapi"
	"github.com/marcioapm/lux-mux/internal/state"
)

// perfServer is a luxd with one running Run whose attach stream sends
// payload in 32 KiB output frames (after a restore snapshot when asked),
// and reports the time each input frame arrives.
type perfServer struct {
	*httptest.Server
	payload []byte
	inputs  chan time.Time
}

func newPerfServer(payload []byte) *perfServer {
	s := &perfServer{payload: payload, inputs: make(chan time.Time, 1<<16)}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/runs/{id}", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(api.Run{ID: r.PathValue("id"), State: "running", Epoch: 1})
	})
	mux.HandleFunc("GET /v1/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	mux.HandleFunc("GET /v1/runs/{id}/attach", func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		ws.SetReadLimit(8 << 20)
		defer ws.CloseNow()
		ctx := r.Context()
		go func() {
			for {
				var d api.StreamData
				if wsjson.Read(ctx, ws, &d) != nil {
					return
				}
				if len(d.Data) > 0 {
					s.inputs <- time.Now()
				}
			}
		}()
		if r.URL.Query().Get("restore") == "true" {
			// A short snapshot: a blank screen.
			_ = wsjson.Write(ctx, ws, api.StreamData{Data: []byte("\x1b[?25l\x1b[?1049l\x1b[H\x1b[2J\x1b[?25h")})
		}
		frames := encodeFrames(s.payload)
		for _, f := range frames {
			if ws.Write(ctx, websocket.MessageText, f) != nil {
				return
			}
		}
		<-ctx.Done()
	})
	s.Server = httptest.NewServer(mux)
	return s
}

func encodeFrames(p []byte) [][]byte {
	var out [][]byte
	for len(p) > 0 {
		n := min(len(p), streamChunk)
		b, _ := json.Marshal(api.StreamData{Channel: "stdout", Data: p[:n]})
		out = append(out, b)
		p = p[n:]
	}
	return out
}

// countWriter counts bytes and signals when it reached want.
type countWriter struct {
	n    atomic.Int64
	want int64
	done chan struct{}
	once sync.Once
}

func (c *countWriter) Write(p []byte) (int, error) {
	if c.n.Add(int64(len(p))) >= c.want {
		c.once.Do(func() { close(c.done) })
	}
	return len(p), nil
}

// plainAttach is the baseline: lux attach's loop, frames to the terminal.
func plainAttach(ctx context.Context, cl *luxapi.Client, out io.Writer, keys io.Reader) error {
	ws, err := cl.Attach(ctx, "run_1", true, 24, 80)
	if err != nil {
		return err
	}
	ws.SetReadLimit(8 << 20)
	defer ws.CloseNow()
	go func() {
		buf := make([]byte, 32<<10)
		for {
			n, err := keys.Read(buf)
			if n > 0 {
				_ = wsjson.Write(ctx, ws, api.StreamData{Data: buf[:n]})
			}
			if err != nil {
				return
			}
		}
	}()
	for {
		var d api.StreamData
		if err := wsjson.Read(ctx, ws, &d); err != nil {
			return nil
		}
		_, _ = out.Write(d.Data)
	}
}

func perfPayload(size int) []byte {
	line := []byte("\x1b[32mcompiling\x1b[m src/module/file.go: ok (12 ms) lorem ipsum dolor sit amet\r\n")
	p := make([]byte, 0, size)
	for len(p) < size {
		p = append(p, line...)
	}
	return p[:size]
}

func startPerfMux(t *testing.T, srv *perfServer, out io.Writer, keys io.Reader) (*Mux, chan struct{}) {
	cfg, _ := config.Parse(nil)
	cl, _ := luxapi.New(api.Config{URL: srv.URL, APIKey: "k"})
	m, err := New(Options{Session: "perf", Config: cfg, Lux: cl, In: keys, Out: out,
		Size: func() (int, int) { return 50, 200 }, RestoreWait: time.Second},
		&state.Session{Name: "perf", Windows: []state.Window{{RunID: "run_1", Name: "w", Profile: "default"}}})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		_, _ = m.Run(context.Background())
		close(done)
	}()
	return m, done
}

// TestPerf measures luxmux's added cost against a plain attach client:
// throughput of 50 MB through one attach stream (to the terminal, and into
// the window's emulator), and the latency a keystroke takes to reach the
// websocket. Set LUXMUX_PERF=1 to run it.
func TestPerf(t *testing.T) {
	if os.Getenv("LUXMUX_PERF") == "" {
		t.Skip("LUXMUX_PERF=1 runs the measurements")
	}
	const size = 50 << 20
	payload := perfPayload(size)

	// Throughput.
	var plainDur, muxDur, muxEmuDur time.Duration
	for range 3 {
		srv := newPerfServer(payload)
		cw := &countWriter{want: size, done: make(chan struct{})}
		cl, _ := luxapi.New(api.Config{URL: srv.URL, APIKey: "k"})
		ctx, cancel := context.WithCancel(context.Background())
		pr, _ := io.Pipe()
		start := time.Now()
		go func() { _ = plainAttach(ctx, cl, cw, pr) }()
		<-cw.done
		plainDur = minNonZero(plainDur, time.Since(start))
		cancel()
		srv.Close()

		srv = newPerfServer(payload)
		cw = &countWriter{want: size, done: make(chan struct{})}
		kr, kw := io.Pipe()
		start = time.Now()
		m, done := startPerfMux(t, srv, cw, kr)
		<-cw.done
		d := time.Since(start)
		m.mu.Lock()
		m.windows[0].sync()
		m.mu.Unlock()
		de := time.Since(start)
		muxDur = minNonZero(muxDur, d)
		muxEmuDur = minNonZero(muxEmuDur, de)
		_, _ = kw.Write([]byte("\x02d"))
		<-done
		srv.Close()
	}
	mbps := func(d time.Duration) float64 { return float64(size) / d.Seconds() / 1e6 }
	t.Logf("throughput, 50 MB: plain %.0f MB/s, luxmux to terminal %.0f MB/s (%.0f%%), luxmux incl. emulator caught up %.0f MB/s (%.0f%%)",
		mbps(plainDur), mbps(muxDur), 100*plainDur.Seconds()/muxDur.Seconds(), mbps(muxEmuDur), 100*plainDur.Seconds()/muxEmuDur.Seconds())

	// Keystroke latency: one key at a time, the next after the previous
	// arrived.
	const n = 2000
	lat := func(keys io.Writer, srv *perfServer) []time.Duration {
		out := make([]time.Duration, 0, n)
		for i := 0; i < n; i++ {
			t0 := time.Now()
			_, _ = keys.Write([]byte{'a' + byte(i%26)})
			t1 := <-srv.inputs
			out = append(out, t1.Sub(t0))
		}
		return out
	}
	srv := newPerfServer(nil)
	cl, _ := luxapi.New(api.Config{URL: srv.URL, APIKey: "k"})
	ctx, cancel := context.WithCancel(context.Background())
	pr, pw := io.Pipe()
	go func() { _ = plainAttach(ctx, cl, io.Discard, pr) }()
	waitInput(t, pw, srv)
	plain := lat(pw, srv)
	cancel()
	srv.Close()

	srv = newPerfServer(nil)
	kr, kw := io.Pipe()
	_, done := startPerfMux(t, srv, io.Discard, kr)
	waitInput(t, kw, srv)
	muxL := lat(kw, srv)
	_, _ = kw.Write([]byte("\x02d"))
	<-done
	srv.Close()
	pm, p99 := stats(plain)
	mm, m99 := stats(muxL)
	t.Logf("keystroke → websocket, %d keys: plain mean %v p99 %v; luxmux mean %v p99 %v; added mean %v",
		n, pm, p99, mm, m99, mm-pm)
}

// waitInput sends keys until one reaches the server: the stream is up.
func waitInput(t *testing.T, w io.Writer, srv *perfServer) {
	for i := 0; i < 500; i++ {
		_, _ = w.Write([]byte("x"))
		select {
		case <-srv.inputs:
			for len(srv.inputs) > 0 {
				<-srv.inputs
			}
			return
		case <-time.After(10 * time.Millisecond):
		}
	}
	t.Fatal("the stream never took input")
}

func stats(d []time.Duration) (mean, p99 time.Duration) {
	s := append([]time.Duration{}, d...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	var sum time.Duration
	for _, x := range s {
		sum += x
	}
	return sum / time.Duration(len(s)), s[len(s)*99/100]
}

func minNonZero(a, b time.Duration) time.Duration {
	if a == 0 || b < a {
		return b
	}
	return a
}
