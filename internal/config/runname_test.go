package config

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestRunName: <session>/<window>, each control character a space, cut
// to MaxRunName bytes without splitting a character.
func TestRunName(t *testing.T) {
	long := strings.Repeat("w", 300)
	// "s/" and 197 a's are 199 bytes: the 2-byte é would end at byte 201.
	across := strings.Repeat("a", 197) + "é" + "tail"
	for _, tc := range []struct{ session, window, want string }{
		{"work", "api", "work/api"},
		{"work", "a/b", "work/a/b"},
		{"work", "tab\there\nnl\x1b[1mesc\x7f\u0085", "work/tab here nl [1mesc  "},
		{"work", long, "work/" + long[:MaxRunName-len("work/")]},
		{"s", across, "s/" + strings.Repeat("a", 197)},
		{"s", strings.Repeat("a", 196) + "é", "s/" + strings.Repeat("a", 196) + "é"},
		{"s", "bad\xffbyte", "s/bad\uFFFDbyte"},
	} {
		got := RunName(tc.session, tc.window)
		if got != tc.want {
			t.Errorf("RunName(%q, %q) = %q, want %q", tc.session, tc.window, got, tc.want)
		}
		if len(got) > MaxRunName || !utf8.ValidString(got) {
			t.Errorf("RunName(%q, %q) = %q: %d bytes, valid UTF-8 %v", tc.session, tc.window, got, len(got), utf8.ValidString(got))
		}
	}
}

// TestRunSpecName: a window's Run is submitted with the name its Target
// gives; without one, as before (luxmux-<session>).
func TestRunSpecName(t *testing.T) {
	p := Profile{Image: "alpine"}
	sp, err := RunSpec(p, Target{Session: "label", Profile: "p", Name: RunName("shown", "api")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if sp.Name != "shown/api" || sp.Labels[LabelSession] != "label" {
		t.Fatalf("name %q, labels %v", sp.Name, sp.Labels)
	}
	if sp, _ := RunSpec(p, Target{Session: "label", Profile: "p"}, nil); sp.Name != "luxmux-label" {
		t.Fatalf("unnamed: %q", sp.Name)
	}
}

// TestSpecKeyIgnoresRunName: windows of different names (and a spare)
// share a key, so any of them may claim the same spare.
func TestSpecKeyIgnoresRunName(t *testing.T) {
	p := Profile{Image: "alpine", Repository: "https://example.com/app.git"}
	key := SpecKey(p)
	labels := map[string]bool{}
	for _, name := range []string{RunName("s", "api"), RunName("s", "web"), RunName("other", SpareWindow)} {
		sp, err := RunSpec(p, Target{Session: "s", Profile: "p", Name: name, Spare: true}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if sp.Name != name {
			t.Fatalf("%q: name %q", name, sp.Name)
		}
		labels[sp.Labels[LabelSpec]] = true
	}
	if len(labels) != 1 || !labels[key] {
		t.Fatalf("spec labels %v, key %q", labels, key)
	}
}
