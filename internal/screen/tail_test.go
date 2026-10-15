package screen

import (
	"bytes"
	"strings"
	"testing"
)

func TestTailPending(t *testing.T) {
	for _, tc := range []struct {
		name string
		out  []string
		want string
	}{
		{"nothing", nil, ""},
		{"text", []string{"plain text"}, ""},
		{"mid csi", []string{"x\x1b[3"}, "\x1b[3"},
		{"bare esc", []string{"x\x1b"}, "\x1b"},
		{"csi done", []string{"x\x1b[31m"}, ""},
		{"csi final @", []string{"x\x1b[2@"}, ""},
		{"csi final ~", []string{"x\x1b[2~"}, ""},
		{"csi split over writes", []string{"x\x1b[", "38;5"}, "\x1b[38;5"},
		{"csi cancelled", []string{"\x1b[3\x18"}, ""},
		{"csi substituted", []string{"\x1b[3\x1a"}, ""},
		{"mid osc", []string{"\x1b]0;t"}, "\x1b]0;t"},
		{"osc bel", []string{"\x1b]0;t\x07"}, ""},
		{"osc st", []string{"\x1b]0;t\x1b\\"}, ""},
		{"mid dcs", []string{"\x1bPq#0"}, "\x1bPq#0"},
		{"charset", []string{"\x1b("}, "\x1b("},
		{"charset done", []string{"\x1b(B"}, ""},
		{"utf8 lead", []string{"é"[:1]}, "é"[:1]},
		{"utf8 3 of 4", []string{"ab", "😀"[:3]}, "😀"[:3]},
		{"utf8 whole", []string{"😀"}, ""},
		{"utf8 after csi", []string{"\x1b[1mé"[:5]}, "é"[:1]},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var tl Tail
			for _, o := range tc.out {
				tl.Add([]byte(o))
			}
			if got := tl.Pending(); string(got) != tc.want {
				t.Fatalf("Pending %q, want %q", got, tc.want)
			}
		})
	}
}

// TestTailKeepsLatest: the tail is the latest TailLen bytes across writes
// of any size.
func TestTailKeepsLatest(t *testing.T) {
	var tl Tail
	var all []byte
	for i, n := range []int{10, 300, 1, 255, 3, 256, 100, 200, 7} {
		p := bytes.Repeat([]byte{byte('a' + i)}, n)
		tl.Add(p)
		all = append(all, p...)
		want := all[max(0, len(all)-TailLen):]
		if got := tl.buf[:tl.n]; !bytes.Equal(got, want) {
			t.Fatalf("after write %d: %q, want %q", i, got, want)
		}
	}
	// A sequence started before a long write is no longer pending.
	tl.Reset()
	tl.Add([]byte("\x1b[3"))
	tl.Add([]byte(strings.Repeat("x", 10)))
	if p := tl.Pending(); p != nil {
		t.Fatalf("pending %q", p)
	}
	// An OSC longer than TailLen is cut short: its ESC fell out of the
	// window, so nothing is replayed.
	tl.Add([]byte("\x1b]2;" + strings.Repeat("t", 300)))
	if p := tl.Pending(); p != nil {
		t.Fatalf("long OSC: %d bytes pending", len(p))
	}
}

// TestTailReset: after Reset nothing is pending.
func TestTailReset(t *testing.T) {
	var tl Tail
	tl.Add([]byte("x\x1b[3"))
	tl.Reset()
	if p := tl.Pending(); p != nil {
		t.Fatalf("pending %q", p)
	}
	tl.Add([]byte("\x1b["))
	if p := tl.Pending(); string(p) != "\x1b[" {
		t.Fatalf("pending %q", p)
	}
}

// TestPendingIsACopy: what Pending returns does not change with later
// output.
func TestPendingIsACopy(t *testing.T) {
	var tl Tail
	tl.Add([]byte("\x1b[3"))
	p := tl.Pending()
	tl.Add(bytes.Repeat([]byte("z"), TailLen))
	if string(p) != "\x1b[3" {
		t.Fatalf("%q", p)
	}
}
