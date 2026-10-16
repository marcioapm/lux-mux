// Package keys splits the bytes typed into luxmux into what goes to the
// active window and the commands luxmux intercepts: the prefix key and
// the key after it, and the direct bindings when configured.
package keys

import (
	"bytes"

	"github.com/marcioapm/lux-mux/internal/config"
)

// Action is one result of Feed: bytes for the active window, or a command.
type Action struct {
	Send []byte
	Cmd  config.Command
}

// IsCmd reports whether the action is a command.
func (a Action) IsCmd() bool { return a.Cmd.Op != "" }

var (
	pasteStart = []byte("\x1b[200~")
	pasteEnd   = []byte("\x1b[201~")
)

// Machine is the key state machine. It is not safe for concurrent use.
type Machine struct {
	prefix byte
	direct []binding
	// afterPrefix: the prefix was the last key; the next one is a command.
	afterPrefix bool
	// pasting: inside a bracketed paste, where nothing is intercepted.
	pasting bool
	// partial: the end of the previous chunk, a possible start of a paste
	// marker split across reads.
	partial []byte
	// eaten are the keys, sent encoded, whose presses luxmux took: their
	// releases are dropped too.
	eaten []rune
}

type binding struct {
	seq []byte
	cmd config.Command
}

// New is a machine for the prefix key and the direct bindings (key name →
// command, as in the configuration's [keys.direct]).
func New(prefix byte, direct map[string]string) (*Machine, error) {
	m := &Machine{prefix: prefix}
	for k, v := range direct {
		seq, err := config.ParseKey(k)
		if err != nil {
			return nil, err
		}
		cmd, err := config.ParseCommand(v)
		if err != nil {
			return nil, err
		}
		m.direct = append(m.direct, binding{seq, cmd})
	}
	return m, nil
}

// AfterPrefix reports whether the machine waits for a command key.
func (m *Machine) AfterPrefix() bool { return m.afterPrefix }

// PrefixKeys maps the key after the prefix to its command.
var PrefixKeys = map[byte]config.Command{
	'c': {Op: config.OpNew}, 'C': {Op: config.OpNewDialog},
	'n': {Op: config.OpNext}, 'p': {Op: config.OpPrev}, 'l': {Op: config.OpLast},
	'\'': {Op: config.OpPrompt}, 'w': {Op: config.OpChoose},
	',': {Op: config.OpRename}, '&': {Op: config.OpKill}, 'd': {Op: config.OpDetach},
	'?': {Op: config.OpHelp}, 'r': {Op: config.OpRelaunch},
	's': {Op: config.OpChooseSession}, '(': {Op: config.OpPrevSession}, ')': {Op: config.OpNextSession},
	'$': {Op: config.OpRenameSession},
}

// Feed takes the bytes of one read and returns what to do with them, in
// order. It stops at the first command: rest is the input after it, to be
// fed again (or handed to whatever the command opened). Bytes for the
// window are returned as they came, in as few Sends as possible: outside a
// prefix or a binding, a read is passed through whole. Keys a program had
// the terminal send as escape sequences (the kitty keyboard protocol,
// xterm modifyOtherKeys) are recognised as the keys they stand for.
func (m *Machine) Feed(b []byte) (acts []Action, rest []byte) {
	if len(m.partial) > 0 {
		b = append(m.partial, b...)
		m.partial = nil
	}
	start := 0
	flush := func(end int) {
		if end > start {
			acts = append(acts, Action{Send: b[start:end]})
		}
	}
	for i := 0; i < len(b); {
		if m.pasting {
			j := bytes.Index(b[i:], pasteEnd)
			if j < 0 {
				// Keep a possible split end marker for the next read.
				keep := splitSuffix(b[i:], pasteEnd)
				flush(len(b) - keep)
				if keep > 0 {
					m.partial = append([]byte{}, b[len(b)-keep:]...)
				}
				return acts, nil
			}
			i += j + len(pasteEnd)
			m.pasting = false
			continue
		}
		c := b[i]
		if c == 0x1b {
			if ev, n, ok := decodeKey(b[i:]); ok {
				if cmd, done := m.encodedKey(ev, b, &i, n, &start, flush); done {
					acts = append(acts, Action{Cmd: cmd})
					return acts, b[i:]
				}
				continue
			}
		}
		if m.afterPrefix {
			m.afterPrefix = false
			if c == m.prefix {
				// The prefix twice: the second one goes through.
				start = i
				i++
				continue
			}
			start = i + 1
			if cmd, ok := commandFor(c); ok {
				acts = append(acts, Action{Cmd: cmd})
				return acts, b[i+1:]
			}
			// An unbound key after the prefix is dropped, as tmux does.
			i++
			continue
		}
		if c == m.prefix {
			flush(i)
			m.afterPrefix = true
			i++
			start = i
			continue
		}
		if c == 0x1b {
			if bytes.HasPrefix(b[i:], pasteStart) {
				m.pasting = true
				i += len(pasteStart)
				continue
			}
			if i+len(pasteStart) > len(b) && bytes.HasPrefix(pasteStart, b[i:]) && len(b)-i >= 3 {
				// "\x1b[2" or more of the marker ending a read: held
				// until the next read says whether a paste starts.
				flush(i)
				m.partial = append([]byte{}, b[i:]...)
				return acts, nil
			}
		}
		if cmd, n, ok := m.directAt(b[i:]); ok {
			flush(i)
			acts = append(acts, Action{Cmd: cmd})
			return acts, b[i+n:]
		}
		i++
	}
	flush(len(b))
	return acts, nil
}

func commandFor(c byte) (config.Command, bool) {
	if c >= '0' && c <= '9' {
		return config.Command{Op: config.OpSelect, Arg: int(c - '0')}, true
	}
	cmd, ok := PrefixKeys[c]
	return cmd, ok
}

// directAt matches a direct binding at the start of b. A binding is only
// recognised whole within one read (terminals send Alt-x in one write), so
// a lone Escape is never delayed.
func (m *Machine) directAt(b []byte) (config.Command, int, bool) {
	for _, d := range m.direct {
		if bytes.HasPrefix(b, d.seq) {
			// Alt-x is ESC x; ESC [ or ESC O begin other keys' sequences.
			return d.cmd, len(d.seq), true
		}
	}
	return config.Command{}, 0, false
}

// splitSuffix is the length of the longest suffix of b that is a proper
// prefix of marker.
func splitSuffix(b, marker []byte) int {
	for n := min(len(marker)-1, len(b)); n > 0; n-- {
		if bytes.HasSuffix(b, marker[:n]) {
			return n
		}
	}
	return 0
}
