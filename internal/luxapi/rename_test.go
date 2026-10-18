package luxapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marcioapm/lux/api"
)

func renameResponse(code int) *http.Response {
	return &http.Response{StatusCode: code, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"id":"fixture"}`))}
}
func renameClient(t *testing.T, endpoint string, rt roundTripFunc) *Client {
	t.Helper()
	c, err := New(api.Config{URL: endpoint, APIKey: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	c.C.HTTP = &http.Client{Transport: rt}
	return c
}

// isolateRenameRegistry empties the package-global endpoint registry for
// the test and again afterwards, so capacity checks do not depend on
// entries left by other tests.
func isolateRenameRegistry(t *testing.T) {
	t.Helper()
	reset := func() {
		renameEndpoints.Lock()
		renameEndpoints.entries = make(map[renameKey]*renameSupport)
		renameEndpoints.changed = make(chan struct{})
		renameEndpoints.Unlock()
	}
	reset()
	t.Cleanup(reset)
}

func TestRenameDiscoverySingleflight(t *testing.T) {
	for _, status := range []int{405, 404, 200} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			var patches, gets atomic.Int32
			c := renameClient(t, "http://"+t.Name()+".invalid", func(r *http.Request) (*http.Response, error) {
				if r.Method == http.MethodPatch {
					if patches.Add(1) == 1 {
						close(entered)
						select {
						case <-release:
						case <-r.Context().Done():
							return nil, r.Context().Err()
						}
					}
					return renameResponse(status), nil
				}
				gets.Add(1)
				return renameResponse(200), nil
			})
			var wg sync.WaitGroup
			errs := make(chan error, 32)
			for i := 0; i < 32; i++ {
				wg.Add(1)
				go func() { defer wg.Done(); _, err := c.Rename(context.Background(), "fixture", "s/window"); errs <- err }()
			}
			<-entered
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if _, err := c.Rename(ctx, "fixture", "s/cancelled"); !errors.Is(err, context.Canceled) {
				t.Errorf("wait cancellation: %v", err)
			}
			close(release)
			wg.Wait()
			close(errs)
			for err := range errs {
				if status == 200 && err != nil {
					t.Fatal(err)
				}
				if status != 200 && !errors.Is(err, ErrRenameUnsupported) {
					t.Fatal(err)
				}
			}
			want := int32(1)
			if status == 200 {
				want = 32
			}
			if patches.Load() != want {
				t.Fatalf("PATCH=%d want %d", patches.Load(), want)
			}
			wantGET := int32(0)
			if status == 404 {
				wantGET = 1
			}
			if gets.Load() != wantGET {
				t.Fatalf("GET=%d want %d", gets.Load(), wantGET)
			}
		})
	}
}

func TestRenameDiscoveryRetryable(t *testing.T) {
	for _, status := range []int{422, 500, 401, 403, 404} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var calls atomic.Int32
			c := renameClient(t, "http://"+t.Name()+".invalid", func(r *http.Request) (*http.Response, error) {
				if calls.Add(1) == 1 || r.Method == http.MethodGet {
					return renameResponse(status), nil
				}
				return renameResponse(200), nil
			})
			if _, err := c.Rename(context.Background(), "fixture", "invalid"); err == nil || errors.Is(err, ErrRenameUnsupported) {
				t.Fatalf("initial error: %v", err)
			}
			if _, err := c.Rename(context.Background(), "fixture", "valid"); err != nil {
				t.Fatalf("valid retry: %v", err)
			}
		})
	}
}

func TestRenameRegistryBoundAndExpiry(t *testing.T) {
	for i := 0; i < 1000; i++ {
		c := renameClient(t, fmt.Sprintf("http://registry-%d.invalid", i), func(*http.Request) (*http.Response, error) { return renameResponse(200), nil })
		if _, err := c.Rename(context.Background(), "fixture", "name"); err != nil {
			t.Fatal(err)
		}
	}
	renameEndpoints.Lock()
	n := len(renameEndpoints.entries)
	renameEndpoints.Unlock()
	if n > 256 {
		t.Fatalf("retained %d entries", n)
	}
	c := renameClient(t, "http://registry-expiry.invalid", func(*http.Request) (*http.Response, error) { return renameResponse(405), nil })
	if _, err := c.Rename(context.Background(), "fixture", "name"); !errors.Is(err, ErrRenameUnsupported) {
		t.Fatal(err)
	}
	s, err := c.renameSupport(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// An active reference survives expiration and eviction pressure.
	renameEndpoints.Lock()
	s.used = time.Now().Add(-2 * renameTTL)
	renameEndpoints.Unlock()
	again, err := c.renameSupport(context.Background())
	if err != nil || again != s {
		t.Fatalf("active entry duplicated: %v", err)
	}
	releaseRenameSupport(again)
	releaseRenameSupport(s)
	renameEndpoints.Lock()
	s.used = time.Now().Add(-2 * renameTTL)
	renameEndpoints.Unlock()
	c.C.HTTP = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return renameResponse(200), nil })}
	if _, err := c.Rename(context.Background(), "fixture", "after expiry"); err != nil {
		t.Fatalf("expired capability retained: %v", err)
	}
}

func TestRenameEndpointBoundAndCancel(t *testing.T) {
	gate := make(chan struct{})
	var active, peak atomic.Int32
	c := renameClient(t, "http://"+t.Name()+".invalid", func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/prime") {
			return renameResponse(200), nil
		}
		n := active.Add(1)
		defer active.Add(-1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		select {
		case <-gate:
			return renameResponse(200), nil
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
	})
	if _, err := c.Rename(context.Background(), "prime", "name"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); c.Rename(ctx, "fixture", "name") }()
	}
	deadline := time.After(time.Second)
	for active.Load() < 8 {
		select {
		case <-deadline:
			cancel()
			wg.Wait()
			t.Fatal("eight requests did not enter")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	cancel()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		close(gate)
		t.Fatal("cancelled acquisition stuck")
	}
	if peak.Load() > 8 {
		t.Fatalf("peak=%d", peak.Load())
	}
}

// With every entry referenced, the registry admits no new endpoint (the
// caller waits until its deadline) and evicts none of the referenced ones.
func TestRenameRegistryKeepsReferencedEntries(t *testing.T) {
	isolateRenameRegistry(t)
	ok := func(*http.Request) (*http.Response, error) { return renameResponse(200), nil }
	held := renameClient(t, "http://referenced-held.invalid", ok)
	s, err := held.renameSupport(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	refs := []*renameSupport{s}
	for i := 1; i < renameCapacity; i++ {
		r, err := renameClient(t, fmt.Sprintf("http://referenced-%d.invalid", i), ok).renameSupport(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		refs = append(refs, r)
	}
	defer func() {
		for _, r := range refs {
			releaseRenameSupport(r)
		}
	}()
	// The held entry is the least recently used, so LRU eviction would pick it.
	renameEndpoints.Lock()
	s.used = time.Now().Add(-time.Minute)
	renameEndpoints.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	extra := renameClient(t, "http://referenced-extra.invalid", ok)
	if _, err := extra.renameSupport(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("full registry of referenced entries admitted another: %v", err)
	}
	again, err := held.renameSupport(context.Background())
	if err != nil || again != s {
		t.Fatalf("referenced entry evicted: %v", err)
	}
	refs = append(refs, again)
	renameEndpoints.Lock()
	n := len(renameEndpoints.entries)
	renameEndpoints.Unlock()
	if n > renameCapacity {
		t.Fatalf("entries=%d, capacity %d", n, renameCapacity)
	}
}

// A prober cancelled mid-PATCH leaves the capability unknown, so the next
// caller probes again and gets the real 404-then-GET verdict.
func TestRenameCancelledProbeStaysUnknown(t *testing.T) {
	isolateRenameRegistry(t)
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	c := renameClient(t, "http://"+t.Name()+".invalid", func(r *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			cancel()
			return nil, r.Context().Err()
		}
		if r.Method == http.MethodGet {
			return renameResponse(200), nil
		}
		return renameResponse(404), nil
	})
	if _, err := c.Rename(ctx, "fixture", "name"); errors.Is(err, ErrRenameUnsupported) {
		t.Fatal("cancelled probe marked the endpoint unsupported")
	}
	if _, err := c.Rename(context.Background(), "fixture", "name"); !errors.Is(err, ErrRenameUnsupported) || calls != 3 {
		t.Fatalf("second probe: %v calls=%d, want ErrRenameUnsupported after PATCH+GET", err, calls)
	}
}
