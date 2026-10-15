package keys

import (
	"reflect"
	"testing"

	"github.com/marcioapm/lux-mux/internal/config"
)

// The prefix and the keys after it, as a terminal sends them once a
// program enabled the kitty keyboard protocol or modifyOtherKeys.
func TestPrefixEncoded(t *testing.T) {
	sel := func(n int) config.Command { return config.Command{Op: config.OpSelect, Arg: n} }
	cases := []struct {
		name  string
		pfx   byte
		reads []string
		want  []any
	}{
		{"kitty C-b, plain w", pfx, []string{"\x1b[98;5uw"}, []any{cmd(config.OpChoose)}},
		{"kitty C-b, kitty n", pfx, []string{"ab\x1b[98;5u\x1b[110u"}, []any{"ab", cmd(config.OpNext)}},
		{"kitty C-b split from its key", pfx, []string{"\x1b[98;5u", "d"}, []any{cmd(config.OpDetach)}},
		{"modifyOtherKeys C-b", pfx, []string{"\x1b[27;5;98~n"}, []any{cmd(config.OpNext)}},
		{"modifyOtherKeys C-a prefix", 0x01, []string{"x\x1b[27;5;97~p"}, []any{"x", cmd(config.OpPrev)}},
		{"kitty C-a prefix", 0x01, []string{"\x1b[97;5u3"}, []any{sel(3)}},
		{"kitty C-b with Num Lock", pfx, []string{"\x1b[98;133u", "c"}, []any{cmd(config.OpNew)}},
		// Report event types: press, release, then the key's press/release.
		{"kitty press/release events", pfx, []string{"\x1b[98;5:1u\x1b[98;5:3u\x1b[110;1:1u\x1b[110;1:3u"}, []any{cmd(config.OpNext)}},
		// Report all keys: the modifier keys themselves reach the window.
		{"kitty report all keys", pfx, []string{"\x1b[57442;5u\x1b[98;5u\x1b[57442;1:3u\x1b[57441;2u\x1b[63;2u"},
			[]any{"\x1b[57442;5u\x1b[57442;1:3u\x1b[57441;2u", cmd(config.OpHelp)}},
		{"kitty shifted key reported", pfx, []string{"\x1b[98;5u\x1b[99:67;2u"}, []any{cmd(config.OpNewDialog)}},
		// A layout whose Shift-3 is '?': the shifted key reported wins.
		{"kitty shifted key of another layout", pfx, []string{"\x1b[98;5u\x1b[51:63;2u"}, []any{cmd(config.OpHelp)}},
		{"kitty with text field", pfx, []string{"\x1b[98;5u\x1b[44;1;44u"}, []any{cmd(config.OpRename)}},
		{"kitty shift without shifted key", pfx, []string{"\x1b[98;5u\x1b[55;2u"}, []any{cmd(config.OpKill)}},
		{"modifyOtherKeys shifted", pfx, []string{"\x1b[27;5;98~\x1b[27;2;63~"}, []any{cmd(config.OpHelp)}},
		// The prefix twice: the window gets the second in its own encoding.
		{"kitty prefix twice", pfx, []string{"\x1b[98;5u\x1b[98;5ux"}, []any{"\x1b[98;5ux"}},
		{"modifyOtherKeys prefix twice", pfx, []string{"\x1b[27;5;98~\x1b[27;5;98~"}, []any{"\x1b[27;5;98~"}},
		{"kitty prefix twice with events", pfx, []string{"\x1b[98;5:1u\x1b[98;5:3u\x1b[98;5:1u\x1b[98;5:3u"}, []any{"\x1b[98;5:1u\x1b[98;5:3u"}},
		{"mixed: raw then kitty", pfx, []string{"\x02\x1b[98;5u"}, []any{"\x1b[98;5u"}},
		// Other encoded keys pass through untouched.
		{"other keys", pfx, []string{"\x1b[99;5u\x1b[27;5;99~\x1b[13u\x1b[1;5A"}, []any{"\x1b[99;5u\x1b[27;5;99~\x1b[13u\x1b[1;5A"}},
		{"a key's release passes", pfx, []string{"\x1b[97;1:3u"}, []any{"\x1b[97;1:3u"}},
		{"unbound after prefix dropped", pfx, []string{"\x1b[98;5u\x1b[122u", "q"}, []any{"q"}},
		{"Alt-b is not C-b", pfx, []string{"\x1b[98;3u"}, []any{"\x1b[98;3u"}},
		{"Super-b is not C-b", pfx, []string{"\x1b[98;9u"}, []any{"\x1b[98;9u"}},
		{"a repeat after the prefix waits", pfx, []string{"\x1b[98;5:1u\x1b[98;5:2u\x1b[100u"}, []any{cmd(config.OpDetach)}},
	}
	for _, c := range cases {
		m, _ := New(c.pfx, nil)
		if got := run(t, m, c.reads...); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s %q: got %#v, want %#v", c.name, c.reads, got, c.want)
		}
	}
}

// Direct bindings are recognised in their encoded forms too.
func TestDirectEncoded(t *testing.T) {
	m, err := New(pfx, map[string]string{"alt-n": "next", "C-t": "choose"})
	if err != nil {
		t.Fatal(err)
	}
	got := run(t, m, "a\x1b[110;3u", "\x1b[27;3;110~", "\x1b[116;5u", "\x1b[116;5:3u", "\x1b[110;1u")
	want := []any{"a", cmd(config.OpNext), cmd(config.OpNext), cmd(config.OpChoose), "\x1b[110;1u"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

// SkipReleases drops the releases of keys luxmux took, only those.
func TestSkipReleases(t *testing.T) {
	m, _ := New(pfx, nil)
	if got := run(t, m, "\x1b[98;5:1u\x1b[63;2:1u"); !reflect.DeepEqual(got, []any{cmd(config.OpHelp)}) {
		t.Fatalf("got %#v", got)
	}
	rest := m.SkipReleases([]byte("\x1b[98;5:3u\x1b[63;2:3u\x1b[97;1:3ux"))
	if string(rest) != "\x1b[97;1:3ux" {
		t.Fatalf("rest %q", rest)
	}
	if rest := m.SkipReleases([]byte("\x1b[98;5:3u")); len(rest) == 0 {
		t.Fatal("a release skipped twice")
	}
}

// Pasted text that looks like an encoded prefix is pasted.
func TestPasteEncodedPrefix(t *testing.T) {
	m, _ := New(pfx, nil)
	paste := "\x1b[200~\x1b[98;5un\x1b[201~"
	if got := run(t, m, paste); !reflect.DeepEqual(got, []any{paste}) {
		t.Fatalf("got %#v", got)
	}
}
