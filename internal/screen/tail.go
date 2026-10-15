package screen

import "unicode/utf8"

// TailLen is how much of a window's latest output Pending looks at: an
// escape sequence longer than this at the end of the output is cut short
// (OSC titles are the only long ones in practice).
const TailLen = 256

// Tail keeps the last TailLen bytes written through it.
type Tail struct {
	buf [TailLen]byte
	n   int
}

// Add records p as the latest output.
func (t *Tail) Add(p []byte) {
	if len(p) >= TailLen {
		t.n = copy(t.buf[:], p[len(p)-TailLen:])
		return
	}
	if t.n+len(p) > TailLen {
		keep := TailLen - len(p)
		copy(t.buf[:], t.buf[t.n-keep:t.n])
		t.n = keep
	}
	t.n += copy(t.buf[t.n:], p)
}

// Reset forgets the output.
func (t *Tail) Reset() { t.n = 0 }

// Pending is the escape sequence or UTF-8 character the output ended in
// the middle of, nil if it ended between them. A client terminal shown a
// repaint must be sent this before the rest of the output arrives, or the
// rest is printed as text.
func (t *Tail) Pending() []byte {
	p := pending(t.buf[:t.n])
	if len(p) == 0 {
		return nil
	}
	return append([]byte{}, p...)
}

func pending(b []byte) []byte {
	// The last ESC starts the last sequence, unless a C0 control after it
	// ended one (CAN, SUB) or a string sequence was terminated.
	esc := -1
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] == 0x1b {
			esc = i
			break
		}
	}
	if esc >= 0 {
		if seq := b[esc:]; !complete(seq) {
			// "ESC \" ends a string sequence: the ESC is its terminator,
			// not a new sequence, when the bytes before it opened one.
			return seq
		}
	}
	return partialUTF8(b)
}

// complete reports whether seq, starting with ESC, is a finished sequence.
func complete(seq []byte) bool {
	if len(seq) < 2 {
		return false
	}
	switch seq[1] {
	case '[': // CSI: parameters, then a final byte 0x40-0x7e
		for _, c := range seq[2:] {
			if c == 0x18 || c == 0x1a {
				return true
			}
			if c >= 0x40 && c <= 0x7e {
				return true
			}
		}
		return false
	case ']', 'P', 'X', '^', '_': // OSC and string sequences: BEL or ST
		for _, c := range seq[2:] {
			if c == 0x07 || c == 0x18 || c == 0x1a {
				return true
			}
		}
		// ST (ESC \) would make a later ESC the last one.
		return false
	case '\\':
		return true
	}
	// ESC, intermediates 0x20-0x2f, then a final byte.
	for _, c := range seq[1:] {
		if c < 0x20 || c > 0x2f {
			return true
		}
	}
	return false
}

func partialUTF8(b []byte) []byte {
	for i := len(b) - 1; i >= 0 && i >= len(b)-utf8.UTFMax; i-- {
		c := b[i]
		if c < 0x80 {
			return nil
		}
		if c >= 0xc0 {
			if !utf8.FullRune(b[i:]) {
				return b[i:]
			}
			return nil
		}
	}
	return nil
}
