package keys

import "github.com/marcioapm/lux-mux/internal/config"

// keyEvent is a key a terminal sent as an escape sequence because the
// program asked for it: the kitty keyboard protocol's
// CSI code[:shifted[:base]] [; mods[:event] [; text]] u, or xterm
// modifyOtherKeys' CSI 27 ; mods ; code ~.
type keyEvent struct {
	code    rune // the key's Unicode code point (unshifted, in kitty's form)
	shifted rune // kitty's shifted key, 0 when not reported
	mods    int  // modifier bits: the sent value less 1
	event   int  // 1 press, 2 repeat, 3 release
}

// Modifier bits, as both protocols number them (value - 1).
const (
	modShift = 1
	modAlt   = 2
	modCtrl  = 4
	// Caps Lock and Num Lock: reported, not modifiers of the key.
	modLocks = 64 | 128
)

// decodeKey reads a key event at the start of b (b[0] is ESC). Only a
// whole sequence within b is recognised: terminals send a key in one write.
func decodeKey(b []byte) (keyEvent, int, bool) {
	if len(b) < 4 || b[1] != '[' {
		return keyEvent{}, 0, false
	}
	// Up to three ;-separated fields of up to three :-separated numbers.
	var f [3][3]int
	var seen [3][3]bool
	fi, si := 0, 0
	for i := 2; i < len(b) && i < 64; i++ {
		c := b[i]
		switch {
		case c >= '0' && c <= '9':
			if fi < 3 && si < 3 {
				f[fi][si] = min(f[fi][si]*10+int(c-'0'), 0x10ffff+1)
				seen[fi][si] = true
			}
		case c == ':':
			si++
		case c == ';':
			fi, si = fi+1, 0
		case c == 'u':
			if !seen[0][0] || fi > 2 {
				return keyEvent{}, 0, false
			}
			ev := keyEvent{code: rune(f[0][0]), shifted: rune(f[0][1]), mods: max(f[1][0]-1, 0), event: max(f[1][1], 1)}
			return ev, i + 1, true
		case c == '~':
			// modifyOtherKeys: exactly 27 ; mods ; code.
			if fi != 2 || f[0][0] != 27 || !seen[2][0] || !seen[1][0] || si != 0 {
				return keyEvent{}, 0, false
			}
			return keyEvent{code: rune(f[2][0]), mods: max(f[1][0]-1, 0), event: 1}, i + 1, true
		default:
			return keyEvent{}, 0, false
		}
	}
	return keyEvent{}, 0, false
}

func (e keyEvent) press() bool   { return e.event == 1 }
func (e keyEvent) release() bool { return e.event == 3 }

// modifierOnly reports a modifier or lock key itself (kitty reports them
// with "report all keys"), pressed on the way to the key that counts.
func (e keyEvent) modifierOnly() bool {
	return (e.code >= 57441 && e.code <= 57452) || (e.code >= 57358 && e.code <= 57360)
}

// usShift is the shifted character of an unshifted US-layout key, for a
// kitty event that reports Shift without its shifted key.
var usShift = map[rune]rune{
	'1': '!', '2': '@', '3': '#', '4': '$', '5': '%', '6': '^', '7': '&', '8': '*', '9': '(', '0': ')',
	'-': '_', '=': '+', '[': '{', ']': '}', '\\': '|', ';': ':', '\'': '"', ',': '<', '.': '>', '/': '?', '`': '~',
}

// legacy is what a terminal sends for the key without those protocols:
// a control character for Ctrl with a letter or @[\]^_ and space, ESC
// before it for Alt, the character itself otherwise; nil for keys with
// no such form (Super, function keys).
func (e keyEvent) legacy() []byte {
	mods := e.mods &^ modLocks
	r := e.code
	if mods&modShift != 0 {
		switch {
		case e.shifted != 0:
			r = e.shifted
		case r >= 'a' && r <= 'z':
			r -= 'a' - 'A'
		case usShift[r] != 0:
			r = usShift[r]
		}
	}
	if r < 0x20 || r >= 0x7f {
		return nil
	}
	c := byte(r)
	var out []byte
	if mods&modAlt != 0 {
		out = append(out, 0x1b)
	}
	switch mods &^ (modShift | modAlt) {
	case 0:
		return append(out, c)
	case modCtrl:
		switch {
		case c >= 'a' && c <= 'z':
			return append(out, c-'a'+1)
		case c >= '@' && c <= '_':
			return append(out, c-'@')
		case c == ' ':
			return append(out, 0)
		}
	}
	return nil
}

// eatenKeys is how many keys whose presses luxmux took it remembers, to
// drop their releases too.
const eatenKeys = 8

// eat records that luxmux took the press of key code.
func (m *Machine) eat(code rune) {
	m.eaten = append(m.eaten, code)
	if len(m.eaten) > eatenKeys {
		m.eaten = m.eaten[1:]
	}
}

// uneat forgets code; it reports whether it was remembered.
func (m *Machine) uneat(code rune) bool {
	for i, c := range m.eaten {
		if c == code {
			m.eaten = append(m.eaten[:i], m.eaten[i+1:]...)
			return true
		}
	}
	return false
}

// encodedKey handles the key event ev, n bytes at b[*i]. It reports the
// command it makes, if any, with *i past it. Otherwise *i is moved past
// the event, which either stays in the bytes for the window (start not
// moved) or is dropped (start moved past it).
func (m *Machine) encodedKey(ev keyEvent, b []byte, i *int, n int, start *int, flush func(int)) (config.Command, bool) {
	at := *i
	*i += n
	drop := func() {
		flush(at)
		*start = *i
	}
	leg := ev.legacy()
	isPrefix := len(leg) == 1 && leg[0] == m.prefix
	switch {
	case ev.modifierOnly():
		// Modifier keys themselves (kitty's "report all keys") always
		// reach the window, which saw or will see their other half.
	case ev.release():
		// The release of a key whose press luxmux took is not the
		// window's; any other release is.
		if m.uneat(ev.code) {
			drop()
		}
	case m.afterPrefix && !ev.press():
		// A repeat: still waiting for the next key.
		drop()
	case m.afterPrefix && isPrefix:
		// The prefix twice: the second goes to the window as it came,
		// release and all.
		m.afterPrefix = false
		flush(at)
		*start = at
		m.uneat(ev.code)
	case m.afterPrefix:
		m.afterPrefix = false
		drop()
		m.eat(ev.code)
		if len(leg) == 1 {
			if cmd, ok := commandFor(leg[0]); ok {
				return cmd, true
			}
		}
		// An unbound key after the prefix is dropped, as tmux does.
	case isPrefix && ev.press():
		drop()
		m.afterPrefix = true
		m.eat(ev.code)
	case ev.press():
		if cmd, ok := m.directFor(leg); ok {
			drop()
			m.eat(ev.code)
			return cmd, true
		}
	}
	return config.Command{}, false
}

// SkipReleases drops the encoded releases at the start of b of keys whose
// presses luxmux took (they may arrive after the command opened help or a
// prompt, which must not read them as keys); it returns the rest.
func (m *Machine) SkipReleases(b []byte) []byte {
	for len(b) > 0 && b[0] == 0x1b {
		ev, n, ok := decodeKey(b)
		if !ok || !ev.release() || !m.uneat(ev.code) {
			break
		}
		b = b[n:]
	}
	return b
}

// directFor is the direct binding whose bytes are leg.
func (m *Machine) directFor(leg []byte) (config.Command, bool) {
	if len(leg) == 0 {
		return config.Command{}, false
	}
	for _, d := range m.direct {
		if string(d.seq) == string(leg) {
			return d.cmd, true
		}
	}
	return config.Command{}, false
}
