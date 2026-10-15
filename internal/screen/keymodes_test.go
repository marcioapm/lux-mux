package screen

import (
	"reflect"
	"testing"
)

// track is a KeyTracker fed out in pieces of n bytes.
func track(out string, n int) KeyModes {
	var k KeyTracker
	for i := 0; i < len(out); i += n {
		k.Write([]byte(out[i:min(i+n, len(out))]))
	}
	return k.Modes()
}

func TestKeyTracker(t *testing.T) {
	for _, tc := range []struct {
		name string
		out  string
		want KeyModes
	}{
		{"nothing", "plain text \x1b[1;31m red \x1b[?25h", KeyModes{}},
		{"kitty push", "\x1b[>1u", KeyModes{Main: Kitty{Flags: 1, Saved: []int{0}}}},
		{"kitty push twice, pop one", "\x1b[>1u\x1b[>31u\x1b[<u", KeyModes{Main: Kitty{Flags: 1, Saved: []int{0}}}},
		{"kitty pop all", "\x1b[>1u\x1b[>3u\x1b[<5u", KeyModes{Main: Kitty{Saved: []int{}}}},
		{"kitty set", "\x1b[=5u", KeyModes{Main: Kitty{Flags: 5}}},
		{"kitty set or", "\x1b[=1u\x1b[=4;2u", KeyModes{Main: Kitty{Flags: 5}}},
		{"kitty set and-not", "\x1b[=7u\x1b[=2;3u", KeyModes{Main: Kitty{Flags: 5}}},
		{"kitty per screen", "\x1b[>1u\x1b[?1049h\x1b[>8u", KeyModes{Main: Kitty{Flags: 1, Saved: []int{0}}, Alt: Kitty{Flags: 8, Saved: []int{0}}}},
		{"kitty back on main", "\x1b[?1049h\x1b[>8u\x1b[?1049l\x1b[>1u", KeyModes{Main: Kitty{Flags: 1, Saved: []int{0}}, Alt: Kitty{Flags: 8, Saved: []int{0}}}},
		{"kitty query is not a mode", "\x1b[?u", KeyModes{}},
		{"modifyOtherKeys 2", "\x1b[>4;2m", KeyModes{ModifyOtherKeys: 2}},
		{"modifyOtherKeys off", "\x1b[>4;2m\x1b[>4m", KeyModes{}},
		{"modifyOtherKeys reset all", "\x1b[>4;1m\x1b[>m", KeyModes{}},
		{"modifyOtherKeys disable", "\x1b[>4;2m\x1b[>4n", KeyModes{}},
		{"other modify resource", "\x1b[>1;2m", KeyModes{}},
		{"SGR is not it", "\x1b[4;2m", KeyModes{}},
		{"RIS", "\x1b[>1u\x1b[>4;2m\x1bc", KeyModes{}},
		{"cancelled", "\x1b[>4;\x182m", KeyModes{}},
		{"in an OSC", "\x1b]2;\x07\x1b[>4;1m", KeyModes{ModifyOtherKeys: 1}},
	} {
		for _, n := range []int{1, 2, 3, 1000} {
			got := track(tc.out, n)
			if !sameModes(got, tc.want) {
				t.Errorf("%s, %d-byte writes: %+v, want %+v", tc.name, n, got, tc.want)
			}
		}
	}
}

// TestKeyTrackerDepth: a push past the depth drops the oldest entry.
func TestKeyTrackerDepth(t *testing.T) {
	out := ""
	for i := 1; i <= 10; i++ {
		out += "\x1b[>" + string(rune('0'+i%10)) + "u"
	}
	got := track(out, 1000)
	if got.Main.Flags != 0 || len(got.Main.Saved) != kittyDepth-1 {
		t.Fatalf("%+v", got.Main)
	}
}

// keysAfter is what a terminal holds after it got b: a KeyTracker stands
// for the terminal (vterm keeps none of these modes).
func keysAfter(before string, b []byte) KeyModes {
	var k KeyTracker
	k.Write([]byte(before))
	k.Write(b)
	return k.Modes()
}

func sameModes(a, b KeyModes) bool {
	norm := func(k Kitty) Kitty {
		if len(k.Saved) == 0 {
			k.Saved = nil
		}
		return k
	}
	a.Main, a.Alt, b.Main, b.Alt = norm(a.Main), norm(a.Alt), norm(b.Main), norm(b.Alt)
	return reflect.DeepEqual(a, b)
}

// left is what a previous window left: kitty flags on both screens and
// modifyOtherKeys.
const left = "\x1b[>1u\x1b[>3u\x1b[?1049h\x1b[>9u\x1b[>4;2m"

// TestPaintKeyModes: a paint leaves the terminal with exactly the
// window's keyboard modes, whatever the previous window left, on the
// screen it is painted on and the other.
func TestPaintKeyModes(t *testing.T) {
	for _, tc := range []struct {
		name string
		out  string
	}{
		{"none", "$ "},
		{"kitty on main", "\x1b[>1u$ "},
		{"kitty in a full-screen program", "\x1b[?1049h\x1b[>1u\x1b[>5uNVIM"},
		{"kitty on both", "\x1b[=2u\x1b[?1049h\x1b[>1uNVIM"},
		{"alt stack while on main", "\x1b[?1049h\x1b[>1u\x1b[?1049l$ "},
		{"modifyOtherKeys", "\x1b[?1049h\x1b[>4;2mVIM"},
	} {
		w := src(tc.out)
		km := track(tc.out, 1000)
		got := keysAfter(left, Paint(nil, w, "", km))
		if !sameModes(got, km) {
			t.Errorf("%s: terminal %+v, window %+v", tc.name, got, km)
		}
		if c := client(left, Paint(nil, w, "", km)); c.AltScreen() != w.AltScreen() {
			t.Errorf("%s: alt %v", tc.name, c.AltScreen())
		}
	}
}

// TestResetKeyModes: detach, help and luxmux's own screens leave no
// keyboard mode on either screen.
func TestResetKeyModes(t *testing.T) {
	for name, b := range map[string][]byte{
		"Reset": Reset(nil),
		"Text":  Text(nil, []string{"x"}, 24, 80),
	} {
		if got := keysAfter(left, b); !got.Zero() {
			t.Errorf("%s left %+v", name, got)
		}
	}
	if got := keysAfter("\x1b[>4;2m\x1b[>1u", ResetKeysHere(nil)); !got.Zero() {
		t.Errorf("ResetKeysHere left %+v", got)
	}
}
