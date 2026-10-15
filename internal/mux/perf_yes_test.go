package mux

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
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

// yesServer is a luxd whose one Run floods `yes` output on every attach,
// forever: a short snapshot, the Restored marker, then 32 KiB frames of
// "y\r\n". Each attach waits rtt first (the handshake on a far link); each
// input frame's bytes are recorded.
type yesServer struct {
	*httptest.Server
	rtt     time.Duration
	attachN atomic.Int64
	mu      sync.Mutex
	input   []byte
}

func newYesServer(rtt time.Duration) *yesServer {
	s := &yesServer{rtt: rtt}
	frame, _ := json.Marshal(api.StreamData{Channel: "stdout", Data: bytes.Repeat([]byte("y\r\n"), streamChunk/3)})
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
		s.attachN.Add(1)
		time.Sleep(s.rtt)
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
				s.mu.Lock()
				s.input = append(s.input, d.Data...)
				s.mu.Unlock()
			}
		}()
		_ = wsjson.Write(ctx, ws, api.StreamData{Data: []byte("\x1b[?25l\x1b[?1049l\x1b[H\x1b[2J\x1b[?25h")})
		_ = wsjson.Write(ctx, ws, api.StreamData{Restored: true})
		for ws.Write(ctx, websocket.MessageText, frame) == nil {
		}
	})
	s.Server = httptest.NewServer(mux)
	return s
}

func (s *yesServer) got() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte{}, s.input...)
}

// delayWriter is a terminal whose every write takes delay; it counts
// luxmux's "refreshing" placeholders.
type delayWriter struct {
	delay        time.Duration
	n            atomic.Int64
	placeholders atomic.Int64
}

func (w *delayWriter) Write(p []byte) (int, error) {
	time.Sleep(w.delay)
	w.n.Add(int64(len(p)))
	if bytes.Contains(p, []byte("refreshing its screen")) {
		w.placeholders.Add(1)
	}
	return len(p), nil
}

// TestPerfYesFlood is `yes` in the shown window at 200×500, the size
// where vterm parses it slowest, with a notice every 500 ms (another
// window ending, a spare error), 50 ms to attach, and a terminal taking
// 1 ms per write; meanwhile Ctrl-C is pressed 100 times. Every Ctrl-C
// must reach the Run, and the shown window is not attached again for its
// stale emulator. Set LUXMUX_PERF=1 to run it.
func TestPerfYesFlood(t *testing.T) {
	if os.Getenv("LUXMUX_PERF") == "" {
		t.Skip("LUXMUX_PERF=1 runs the measurements")
	}
	for _, tc := range []struct {
		rows, cols int
		delay      time.Duration
	}{{200, 500, time.Millisecond}, {200, 500, 0}, {50, 200, time.Millisecond}} {
		srv := newYesServer(50 * time.Millisecond)
		out := &delayWriter{delay: tc.delay}
		kr, kw := io.Pipe()
		cfg, _ := config.Parse(nil)
		cl, _ := luxapi.New(api.Config{URL: srv.URL, APIKey: "k"})
		m, err := New(Options{Session: "perf", Config: cfg, Lux: cl, In: kr, Out: out,
			Size: func() (int, int) { return tc.rows, tc.cols }, NoticeFor: 200 * time.Millisecond},
			&state.Session{Name: "perf", Windows: []state.Window{{RunID: "run_1", Name: "w", Profile: "default"}}})
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		go func() { _, _ = m.Run(context.Background()); close(done) }()
		time.Sleep(time.Second)
		a0, p0, n0, t0 := srv.attachN.Load(), out.placeholders.Load(), out.n.Load(), time.Now()
		stop := make(chan struct{})
		go func() {
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				case <-time.After(500 * time.Millisecond):
					m.Notice(fmt.Sprintf("notice %d", i))
				}
			}
		}()
		for range 100 {
			_, _ = kw.Write([]byte{0x03})
			time.Sleep(40 * time.Millisecond)
		}
		close(stop)
		el := time.Since(t0)
		deadline := time.Now().Add(5 * time.Second)
		for bytes.Count(srv.got(), []byte{0x03}) < 100 && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
		}
		m.mu.Lock()
		stale := m.windows[0].q.isStale()
		m.mu.Unlock()
		lost := 100 - bytes.Count(srv.got(), []byte{0x03})
		t.Logf("%dx%d yes, terminal %v/write, 50 ms attach, notice every 500 ms, %v: terminal %.0f MB/s; reattaches %d; placeholders %d; emulator stale %v; Ctrl-C lost %d/100",
			tc.rows, tc.cols, tc.delay, el.Round(time.Millisecond), float64(out.n.Load()-n0)/el.Seconds()/1e6,
			srv.attachN.Load()-a0, out.placeholders.Load()-p0, stale, lost)
		if lost != 0 {
			t.Errorf("%d Ctrl-C lost", lost)
		}
		_, _ = kw.Write([]byte("\x02d"))
		<-done
		srv.Close()
	}
}
