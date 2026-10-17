package config

import (
	"fmt"
	"strconv"
	"strings"
)

// ParseKey reads a key name: "C-b" or "ctrl-b" (a control character),
// "M-n" or "alt-n" (ESC then the key), or a single character. It returns
// the bytes a terminal sends for it.
func ParseKey(name string) ([]byte, error) {
	low := strings.ToLower(name)
	switch {
	case strings.HasPrefix(low, "c-") || strings.HasPrefix(low, "ctrl-"):
		rest := name[strings.Index(name, "-")+1:]
		if len(rest) != 1 {
			return nil, fmt.Errorf("key %q: want ctrl- and one character", name)
		}
		c := rest[0]
		switch {
		case c >= 'a' && c <= 'z':
			return []byte{c - 'a' + 1}, nil
		case c >= 'A' && c <= 'Z':
			return []byte{c - 'A' + 1}, nil
		case c >= '@' && c <= '_':
			return []byte{c - '@'}, nil
		case c == ' ':
			return []byte{0}, nil
		}
		return nil, fmt.Errorf("key %q: no control character for %q", name, c)
	case strings.HasPrefix(low, "m-") || strings.HasPrefix(low, "alt-"):
		rest := name[strings.Index(name, "-")+1:]
		if len(rest) != 1 || rest[0] < 0x20 || rest[0] >= 0x7f {
			return nil, fmt.Errorf("key %q: want alt- and one printable character", name)
		}
		return []byte{0x1b, rest[0]}, nil
	case len(name) == 1 && name[0] >= 0x20 && name[0] < 0x7f:
		return []byte{name[0]}, nil
	}
	return nil, fmt.Errorf("key %q: want C-x, M-x or a single character", name)
}

// ParsePrefix reads the prefix key, which must be one control character
// so that it never begins an escape sequence.
func ParsePrefix(name string) (byte, error) {
	b, err := ParseKey(name)
	if err != nil {
		return 0, fmt.Errorf("prefix: %w", err)
	}
	if len(b) != 1 || b[0] >= 0x20 || b[0] == 0x1b {
		return 0, fmt.Errorf("prefix %q: want a control key such as C-b or C-a", name)
	}
	return b[0], nil
}

// Command is what a key does.
type Command struct {
	Op  string
	Arg int
}

// Commands a key can be bound to.
const (
	OpSelect    = "select"     // select N
	OpNext      = "next"       // next window
	OpPrev      = "prev"       // previous window
	OpLast      = "last"       // last window
	OpNew       = "new"        // new window, default profile
	OpNewDialog = "new-dialog" // new window through the dialog
	OpChoose    = "choose"     // window chooser
	OpRename    = "rename"
	OpKill      = "kill"
	OpDetach    = "detach"
	OpHelp      = "help"
	OpInfo      = "info" // the shown window's info
	OpPrompt    = "prompt-index"
	OpRelaunch  = "relaunch"
	OpSendPfx   = "send-prefix"
	// Sessions.
	OpChooseSession = "choose-session" // session chooser
	OpNextSession   = "next-session"
	OpPrevSession   = "prev-session"
	OpRenameSession = "rename-session"
)

var ops = map[string]bool{OpNext: true, OpPrev: true, OpLast: true, OpNew: true, OpNewDialog: true,
	OpChoose: true, OpRename: true, OpKill: true, OpDetach: true, OpHelp: true, OpInfo: true, OpPrompt: true,
	OpChooseSession: true, OpNextSession: true, OpPrevSession: true, OpRenameSession: true}

// ParseCommand reads a binding's command: "select 3", "next", "prev",
// "last", "new", "new-dialog", "choose", "rename", "kill", "detach",
// "help", "info", "prompt-index", "choose-session", "next-session",
// "prev-session" or "rename-session".
func ParseCommand(s string) (Command, error) {
	f := strings.Fields(s)
	if len(f) == 0 {
		return Command{}, fmt.Errorf("empty command")
	}
	if f[0] == OpSelect {
		if len(f) != 2 {
			return Command{}, fmt.Errorf("want select N")
		}
		n, err := strconv.Atoi(f[1])
		if err != nil || n < 0 {
			return Command{}, fmt.Errorf("want select N, N a window index")
		}
		return Command{Op: OpSelect, Arg: n}, nil
	}
	if len(f) != 1 || !ops[f[0]] {
		return Command{}, fmt.Errorf("unknown command %q", s)
	}
	return Command{Op: f[0]}, nil
}
