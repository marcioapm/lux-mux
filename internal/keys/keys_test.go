package keys

import (
	"reflect"
	"testing"

	"github.com/marcioapm/lux-mux/internal/config"
)

const pfx = 0x02 // C-b

// run feeds each read in turn, feeding the rest after a command back, and
// flattens what came out: sent bytes concatenated between commands.
func run(t *testing.T, m *Machine, reads ...string) []any {
	t.Helper()
	var out []any
	sent := ""
	for _, r := range reads {
		b := []byte(r)
		for {
			acts, rest := m.Feed(b)
			for _, a := range acts {
				if a.IsCmd() {
					if sent != "" {
						out = append(out, sent)
						sent = ""
					}
					out = append(out, a.Cmd)
				} else {
					sent += string(a.Send)
				}
			}
			if len(rest) == 0 {
				break
			}
			b = rest
		}
	}
	if sent != "" {
		out = append(out, sent)
	}
	return out
}

func cmd(op string) config.Command { return config.Command{Op: op} }

func TestPrefix(t *testing.T) {
	m, _ := New(pfx, nil)
	cases := []struct {
		reads []string
		want  []any
	}{
		{[]string{"ls -l\r"}, []any{"ls -l\r"}},
		{[]string{"ab\x02cde"}, []any{"ab", cmd(config.OpNew), "de"}},
		{[]string{"\x02", "n"}, []any{cmd(config.OpNext)}},
		{[]string{"\x02\x02x"}, []any{"\x02x"}},
		{[]string{"\x02", "\x02"}, []any{"\x02"}},
		{[]string{"\x027"}, []any{config.Command{Op: config.OpSelect, Arg: 7}}},
		{[]string{"\x02Z", "q"}, []any{"q"}}, // unbound key after the prefix: dropped
		{[]string{"\x02d"}, []any{cmd(config.OpDetach)}},
		{[]string{"\x02C\x02w\x02,\x02&\x02?\x02'\x02l\x02p\x02r"}, []any{cmd(config.OpNewDialog), cmd(config.OpChoose),
			cmd(config.OpRename), cmd(config.OpKill), cmd(config.OpHelp), cmd(config.OpPrompt), cmd(config.OpLast),
			cmd(config.OpPrev), cmd(config.OpRelaunch)}},
		// Alt-n is not a direct binding unless configured.
		{[]string{"\x1bn"}, []any{"\x1bn"}},
	}
	for _, c := range cases {
		if got := run(t, m, c.reads...); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%q: got %#v, want %#v", c.reads, got, c.want)
		}
		if m.AfterPrefix() {
			m.afterPrefix = false
		}
	}
}

func TestBracketedPaste(t *testing.T) {
	m, _ := New(pfx, map[string]string{"alt-n": "next"})
	paste := "\x1b[200~a\x02c\x1bnb\x1b[201~"
	if got := run(t, m, paste+"\x02n"); !reflect.DeepEqual(got, []any{paste, cmd(config.OpNext)}) {
		t.Fatalf("got %#v", got)
	}
	// Markers split across reads.
	got := run(t, m, "x\x1b[20", "0~\x02c\x1b[2", "01~", "\x02p")
	want := []any{"x\x1b[200~\x02c\x1b[201~", cmd(config.OpPrev)}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("split: got %#v, want %#v", got, want)
	}
}

func TestDirect(t *testing.T) {
	m, err := New(pfx, map[string]string{"alt-1": "select 1", "alt-n": "next", "alt-p": "prev", "M-w": "choose"})
	if err != nil {
		t.Fatal(err)
	}
	got := run(t, m, "ab\x1b1cd", "\x1bn", "\x1bp", "\x1bw", "\x1b", "\x1b[A", "\x1bOP")
	want := []any{"ab", config.Command{Op: config.OpSelect, Arg: 1}, "cd", cmd(config.OpNext), cmd(config.OpPrev),
		cmd(config.OpChoose), "\x1b\x1b[A\x1bOP"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v, want %#v", got, want)
	}
	if _, err := New(pfx, map[string]string{"alt-1": "dance"}); err == nil {
		t.Fatal("bad command accepted")
	}
}

func TestPassThroughIsOneSend(t *testing.T) {
	m, _ := New(pfx, nil)
	b := []byte("hello world \x1b[A\x1b[200")
	acts, rest := m.Feed(b[:12])
	if len(acts) != 1 || len(rest) != 0 || &acts[0].Send[0] != &b[0] {
		t.Fatalf("a plain read must pass through as one Send without copying: %#v", acts)
	}
}
