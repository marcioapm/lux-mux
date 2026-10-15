package screen

import (
	"bytes"
	"slices"
	"strconv"
)

// KeyModes are the keyboard-reporting modes a program asks the terminal
// for that vterm does not keep: the kitty keyboard protocol's flags, a
// stack per screen (CSI > f u push, CSI < n u pop, CSI = f ; m u set), and
// xterm's modifyOtherKeys level (CSI > 4 ; n m). They change what the
// terminal sends for keys, the prefix key included, so a window's modes
// must reach the terminal only while it is shown.
type KeyModes struct {
	Main, Alt Kitty
	// ModifyOtherKeys is the xterm level: 0 off, 1 or 2.
	ModifyOtherKeys int
}

// Kitty is one screen's kitty keyboard state: the current flags and, below
// them, the entries pushed earlier (oldest first).
type Kitty struct {
	Flags int
	Saved []int
}

// kittyDepth is how many entries luxmux keeps per screen, the current one
// included; a push past it drops the oldest (kitty and ghostty keep 8 or
// more, so replaying this many fits any of them).
const kittyDepth = 8

func (k *Kitty) push(f int) {
	k.Saved = append(k.Saved, k.Flags)
	if len(k.Saved) > kittyDepth-1 {
		k.Saved = k.Saved[1:]
	}
	k.Flags = f
}

func (k *Kitty) pop(n int) {
	for ; n > 0; n-- {
		if len(k.Saved) == 0 {
			k.Flags = 0
			return
		}
		k.Flags = k.Saved[len(k.Saved)-1]
		k.Saved = k.Saved[:len(k.Saved)-1]
	}
}

func (k *Kitty) set(f, mode int) {
	switch mode {
	case 2:
		k.Flags |= f
	case 3:
		k.Flags &^= f
	default:
		k.Flags = f
	}
}

func (k Kitty) zero() bool { return k.Flags == 0 && len(k.Saved) == 0 }

// Zero reports whether no mode is set.
func (km KeyModes) Zero() bool { return km.Main.zero() && km.Alt.zero() && km.ModifyOtherKeys == 0 }

// appendKitty appends what makes the current screen of a terminal whose
// kitty stack was emptied hold k: the oldest entry set, the rest pushed.
func appendKitty(b []byte, k Kitty) []byte {
	if k.zero() {
		return b
	}
	all := append(slices.Clone(k.Saved), k.Flags)
	b = append(b, "\x1b[="...)
	b = strconv.AppendInt(b, int64(all[0]), 10)
	b = append(b, ";1u"...)
	for _, f := range all[1:] {
		b = append(b, "\x1b[>"...)
		b = strconv.AppendInt(b, int64(f), 10)
		b = append(b, 'u')
	}
	return b
}

// kittyReset empties the current screen's kitty stack (a pop of more
// entries than it holds empties it) and zeroes its flags.
const kittyReset = "\x1b[<99u\x1b[=0;1u"

// ResetKeys appends what turns every mode in KeyModes off: modifyOtherKeys,
// and the kitty flags of both screens (kitty keeps one stack per screen).
// It ends on the main screen, the cursor where it was there.
func ResetKeys(b []byte) []byte {
	return append(b, "\x1b[>4m\x1b[?1049l"+kittyReset+"\x1b[?1049h"+kittyReset+"\x1b[?1049l"...)
}

// ResetKeysHere turns off the keyboard modes in effect, leaving the screen
// shown as it is: modifyOtherKeys and the current screen's kitty flags.
func ResetKeysHere(b []byte) []byte { return append(b, "\x1b[>4m"+kittyReset...) }

// KeyTracker follows a program's output for the sequences that set
// KeyModes, across writes cut anywhere. Its zero value is a program that
// set none.
type KeyTracker struct {
	km  KeyModes
	alt bool
	st  int
	// The CSI being read: its private prefix, intermediate, parameters.
	prefix byte
	inter  byte
	params []int
	cur    int
	seen   bool
}

const (
	kGround = iota
	kEsc
	kCSI
	kIgnore // a CSI with too many parameters, until its final byte
)

// Modes are the modes the program set, as of everything written.
func (k *KeyTracker) Modes() KeyModes {
	km := k.km
	km.Main.Saved = slices.Clone(km.Main.Saved)
	km.Alt.Saved = slices.Clone(km.Alt.Saved)
	return km
}

// Reset forgets every mode: a new program.
func (k *KeyTracker) Reset() { *k = KeyTracker{} }

// Write takes the program's output.
func (k *KeyTracker) Write(p []byte) {
	for i := 0; i < len(p); i++ {
		if k.st == kGround {
			j := bytes.IndexByte(p[i:], 0x1b)
			if j < 0 {
				return
			}
			i += j
			k.st = kEsc
			continue
		}
		c := p[i]
		switch {
		case c == 0x18 || c == 0x1a:
			k.st = kGround
			continue
		case c == 0x1b:
			k.st = kEsc
			continue
		}
		switch k.st {
		case kEsc:
			switch c {
			case '[':
				k.st, k.prefix, k.inter, k.params, k.cur, k.seen = kCSI, 0, 0, k.params[:0], 0, false
			case 'c':
				// RIS resets the kitty stacks and modifyOtherKeys too.
				*k = KeyTracker{params: k.params[:0]}
			default:
				k.st = kGround
			}
		default:
			k.csiByte(c)
		}
	}
}

func (k *KeyTracker) csiByte(c byte) {
	switch {
	case c < 0x20:
		// A C0 control inside a CSI is executed; the CSI goes on.
	case c >= '0' && c <= '9':
		k.cur = min(k.cur*10+int(c-'0'), 1<<20)
		k.seen = true
	case c == ';' || c == ':':
		k.param()
		k.seen = true
	case c >= '<' && c <= '?':
		if !k.seen && k.prefix == 0 {
			k.prefix = c
		}
	case c >= 0x20 && c <= 0x2f:
		k.inter = c
	case c >= 0x40 && c <= 0x7e:
		if k.seen {
			k.param()
		}
		if k.st == kCSI {
			k.dispatch(c)
		}
		k.st = kGround
	default:
		k.st = kGround
	}
}

func (k *KeyTracker) param() {
	if len(k.params) >= 16 {
		k.st = kIgnore
		return
	}
	k.params = append(k.params, k.cur)
	k.cur = 0
}

// arg is parameter i, def when absent.
func (k *KeyTracker) arg(i, def int) int {
	if i < len(k.params) {
		return k.params[i]
	}
	return def
}

func (k *KeyTracker) kitty() *Kitty {
	if k.alt {
		return &k.km.Alt
	}
	return &k.km.Main
}

func (k *KeyTracker) dispatch(final byte) {
	if k.inter != 0 {
		return
	}
	switch {
	case k.prefix == '>' && final == 'u':
		k.kitty().push(k.arg(0, 0))
	case k.prefix == '<' && final == 'u':
		k.kitty().pop(max(k.arg(0, 1), 1))
	case k.prefix == '=' && final == 'u':
		k.kitty().set(k.arg(0, 0), k.arg(1, 1))
	case k.prefix == '>' && final == 'm':
		// CSI > m resets every modify* resource, CSI > 4 m and
		// CSI > 4 ; n m modifyOtherKeys.
		if len(k.params) == 0 || k.params[0] == 4 {
			k.km.ModifyOtherKeys = k.arg(1, 0)
		}
	case k.prefix == '>' && final == 'n':
		// CSI > 4 n disables modifyOtherKeys.
		if k.arg(0, -1) == 4 {
			k.km.ModifyOtherKeys = 0
		}
	case k.prefix == '?' && (final == 'h' || final == 'l'):
		for _, n := range k.params {
			if n == 1049 || n == 1047 || n == 47 {
				k.alt = final == 'h'
			}
		}
	}
}
