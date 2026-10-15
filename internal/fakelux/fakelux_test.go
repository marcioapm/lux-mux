package fakelux

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// bigScreen fills a 200×500 terminal with cells whose snapshot is over
// SnapshotLimit: every cell its own truecolour pen and combining marks.
func bigScreen(t *Terminal) {
	var b strings.Builder
	for y := 0; y < 200; y++ {
		fmt.Fprintf(&b, "\x1b[%d;1H", y+1)
		for x := 0; x < 500; x++ {
			fmt.Fprintf(&b, "\x1b[1;3;4;9;38;2;%d;%d;%d;48;2;%d;%d;%dme\u0301\u0302", x%256, y%256, (x*y)%256, (x+y)%256, x%7, y%9)
		}
	}
	t.Output([]byte(b.String()))
}

// TestRestoreAttachWithoutSnapshotIsStrict: as lux-shim, a restore attach
// whose screen is too big to snapshot still gets the restore budget: one
// that falls RestoreBudget behind is ended (Behind), not left to miss
// output silently as a plain attach does.
func TestRestoreAttachWithoutSnapshotIsStrict(t *testing.T) {
	tm := newTerminal(200, 500)
	bigScreen(tm)
	snap, sub := tm.Join(true, 200, 500)
	if snap != nil {
		t.Fatalf("a %d-byte snapshot: the screen is not over the limit", len(snap))
	}
	chunk := bytes.Repeat([]byte("x"), 32<<10)
	for range RestoreBudget/len(chunk) + 1 {
		tm.Output(chunk)
	}
	select {
	case <-sub.Behind:
	default:
		t.Fatal("a restore attach without a snapshot was not ended past its budget")
	}
	_, plain := tm.Join(false, 0, 0)
	for range PlainQueue + 10 {
		tm.Output([]byte("y"))
	}
	select {
	case <-plain.Behind:
		t.Fatal("a plain attach was ended")
	default:
	}
}

// TestAttachOverMaxSize: as luxd, an attach larger than 200×500 is
// refused with 422.
func TestAttachOverMaxSize(t *testing.T) {
	s := New()
	defer s.Close()
	resp, err := http.Post(s.URL+"/v1/runs", "application/json", strings.NewReader(`{"image":{"ref":"alpine"}}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	id := s.RunIDs()[0]
	WaitFor(context.Background(), 5*time.Second, func() bool { return s.Run(id).State == "running" })
	for _, tc := range []struct {
		rows, cols, want int
	}{{201, 80, 422}, {24, 501, 422}, {200, 500, http.StatusUpgradeRequired}} {
		// Not a websocket upgrade: a size luxd takes gets as far as the
		// upgrade (426), one it refuses is answered 422 before that.
		resp, err := http.Get(fmt.Sprintf("%s/v1/runs/%s/attach?restore=true&rows=%d&cols=%d", s.URL, id, tc.rows, tc.cols))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Errorf("%d×%d: status %d, want %d", tc.rows, tc.cols, resp.StatusCode, tc.want)
		}
	}
}
