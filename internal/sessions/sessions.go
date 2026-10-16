// Package sessions is what luxmux does to whole sessions, from the
// command line or from inside a session: list, open for attaching,
// rename and kill, with the names a rename gave them.
//
// lux cannot change a Run's labels, so a renamed session's Runs keep the
// luxmux-session label of the name they were made under; the state
// directory maps the new name to that label (state.Renames).
package sessions

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"
	"strconv"
	"time"

	"github.com/marcioapm/lux/api"

	"github.com/marcioapm/lux-mux/internal/config"
	"github.com/marcioapm/lux-mux/internal/luxapi"
	"github.com/marcioapm/lux-mux/internal/state"
)

// CheckName refuses what cannot be a session's name.
func CheckName(n string) error {
	if !config.NameRe.MatchString(n) {
		return fmt.Errorf("session name %q: letters, digits, '.', '_' and '-'", n)
	}
	return nil
}

// RenamedError is the answer for the old name of a renamed session: its
// Runs still carry it as their label, so it is not free, and it is not
// the session's name any more.
type RenamedError struct{ Old, New string }

func (e *RenamedError) Error() string {
	return fmt.Sprintf("session %s was renamed to %s: luxmux attach -t %s", e.Old, e.New, e.New)
}

// Label is the luxmux-session label of session name's Runs: the name it
// had when they were made. The old name of a renamed session is a
// *RenamedError. It reads the state directory only: LiveLabel also drops
// a stale rename entry, which needs lux.
func Label(dir, name string) (string, error) {
	m, err := state.Renames(dir)
	if err != nil {
		return "", err
	}
	return labelIn(m, name)
}

// LiveLabel is Label after a stale rename entry naming name was dropped
// (pruneStale): a renamed session whose Runs all ended frees both names.
// The caller holds name's lock, or none.
func LiveLabel(ctx context.Context, c *luxapi.Client, dir, name string) (string, error) {
	if err := pruneStale(ctx, c, dir, name); err != nil {
		return "", err
	}
	return Label(dir, name)
}

func labelIn(m map[string]string, name string) (string, error) {
	if l, ok := m[name]; ok {
		return l, nil
	}
	for n, l := range m {
		if l == name {
			return "", &RenamedError{Old: name, New: n}
		}
	}
	return name, nil
}

// Info is one session as the session chooser and ( ) see it.
type Info struct {
	Name  string
	Label string
	// Windows: its file's windows, else its live Runs that are not a
	// waiting spare.
	Windows int
	// Running counts its live Runs that are running, spares included;
	// Spares its waiting spares (its file's, else its spare-labelled Runs).
	Running, Spares int
	DefaultProfile  string
	// Created is when its oldest live Run was made (zero: none).
	Created time.Time
	HasFile bool
	// Elsewhere: another process holds its lock, Pid the one it wrote.
	Elsewhere bool
	Pid       string
}

// List is every session, sorted by name: those with a file and those lux
// has live Runs of.
func List(ctx context.Context, c *luxapi.Client, dir string) ([]Info, error) {
	runs, err := c.ListAll(ctx)
	if err != nil {
		return nil, err
	}
	return list(dir, runs)
}

func list(dir string, runs []*api.Run) ([]Info, error) {
	files, err := state.List(dir)
	if err != nil {
		return nil, err
	}
	renames, err := state.Renames(dir)
	if err != nil {
		return nil, err
	}
	byLabel := map[string][]*api.Run{}
	for _, r := range runs {
		if l := r.Labels[config.LabelSession]; l != "" && state.Live(r.State) {
			byLabel[l] = append(byLabel[l], r)
		}
	}
	nameOf := map[string]string{}
	for n, l := range renames {
		nameOf[l] = n
	}
	names := map[string]bool{}
	for _, f := range files {
		names[f] = true
	}
	for l := range byLabel {
		if n, ok := nameOf[l]; ok {
			names[n] = true
		} else {
			names[l] = true
		}
	}
	out := make([]Info, 0, len(names))
	for n := range names {
		label, err := labelIn(renames, n)
		if err != nil {
			// A file under a renamed session's old name: not listed.
			continue
		}
		in := Info{Name: n, Label: label}
		for _, r := range byLabel[label] {
			if r.Labels[config.LabelSpare] == "true" {
				in.Spares++
			}
			if r.State == "running" {
				in.Running++
			}
			if in.Created.IsZero() || r.CreatedAt.Before(in.Created) {
				in.Created = r.CreatedAt
			}
		}
		if s, ok, _ := state.Load(dir, n); ok {
			in.HasFile = true
			in.Windows = len(s.Windows)
			in.Spares = len(s.Spares)
			in.DefaultProfile = s.DefaultProfile
		} else {
			in.Windows = len(byLabel[label]) - in.Spares
		}
		in.Pid, in.Elsewhere = state.Held(dir, n)
		out = append(out, in)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Open reads session name for attaching, as luxmux attach does: its file
// reconciled with its Runs (and the Runs new-window made, and the waiting
// spares, that it lacks), else rebuilt from their labels. existed is
// false when there is neither. A stale rename entry naming name is
// dropped first (pruneStale). The caller holds the session's lock.
func Open(ctx context.Context, c *luxapi.Client, dir string, cfg *config.Config, name string) (sess *state.Session, label string, existed bool, err error) {
	if err := pruneStale(ctx, c, dir, name); err != nil {
		return nil, "", false, err
	}
	return Reopen(ctx, c, dir, cfg, name)
}

// Reopen is Open for a session this process had attached: its rename
// entry is kept even when none of its Runs is live (its windows show
// their end).
func Reopen(ctx context.Context, c *luxapi.Client, dir string, cfg *config.Config, name string) (sess *state.Session, label string, existed bool, err error) {
	if label, err = Label(dir, name); err != nil {
		return nil, "", false, err
	}
	runs, err := c.ListSession(ctx, label)
	if err != nil {
		return nil, "", false, err
	}
	if err := recoverMove(dir, name, runs); err != nil {
		return nil, "", false, err
	}
	sess, ok, err := state.Load(dir, name)
	if err != nil {
		return nil, "", false, err
	}
	if ok {
		known := map[string]string{}
		for _, r := range runs {
			known[r.ID] = r.State
		}
		state.RecordExternal(sess, label, runs, cfg.Profiles)
		sess.Reconcile(known)
		state.RecordSpares(sess, label, runs)
		return sess, label, true, nil
	}
	sess = state.RebuildLabelled(name, label, runs, cfg.Profiles)
	return sess, label, len(sess.Windows) > 0, nil
}

// ErrTaken is CheckFree's answer for a name in use.
var ErrTaken = errors.New("session exists")

// CheckFree checks that name may be given to a new or renamed session:
// a valid name with no file, not a renamed session's name or label, and
// no live Runs labelled with it. own is the label of the session being
// renamed, which may take its label back as its name ("" for a new one).
// A rename entry naming name that is stale (see pruneStale) is dropped
// first.
func CheckFree(ctx context.Context, c *luxapi.Client, dir, name, own string) error {
	if err := CheckName(name); err != nil {
		return err
	}
	if err := pruneStale(ctx, c, dir, name); err != nil {
		return err
	}
	if _, ok, err := state.Load(dir, name); err != nil {
		return err
	} else if ok {
		return fmt.Errorf("session %s: %w", name, ErrTaken)
	}
	renames, err := state.Renames(dir)
	if err != nil {
		return err
	}
	if _, ok := renames[name]; ok {
		return fmt.Errorf("session %s: %w", name, ErrTaken)
	}
	if name == own {
		return nil
	}
	for n, l := range renames {
		if l == name {
			return fmt.Errorf("%s is the label of session %s's Runs (it was renamed): choose another name", name, n)
		}
	}
	runs, err := c.ListSession(ctx, name)
	if err != nil {
		return err
	}
	for _, r := range runs {
		if state.Live(r.State) {
			return fmt.Errorf("session %s has Runs on lux: %w", name, ErrTaken)
		}
	}
	return nil
}

// pruneStale drops a rename entry naming name (as its session or its
// label) that is stale: no live Run carries its label, and no process
// has the session, but for this one holding name's own lock (a caller
// about to create or attach it). Its file, whose windows' Runs are then
// all gone, goes with it, as when an attached session's last window ends
// (Mux.endSession).
func pruneStale(ctx context.Context, c *luxapi.Client, dir, name string) error {
	renames, err := state.Renames(dir)
	if err != nil {
		return err
	}
	for n, l := range renames {
		if n != name && l != name {
			continue
		}
		if pid, held := state.Held(dir, n); held && (n != name || pid != strconv.Itoa(os.Getpid())) {
			continue
		}
		runs, err := c.ListSession(ctx, l)
		if err != nil {
			return err
		}
		if slices.ContainsFunc(runs, func(r *api.Run) bool { return state.Live(r.State) }) {
			continue
		}
		if err := state.Remove(dir, n); err != nil {
			return err
		}
		if err := state.DropRenamed(dir, n); err != nil {
			return err
		}
	}
	return nil
}

// Move renames session old to new (label is its Runs' label). The caller
// holds both names' locks and checked new with CheckFree. The steps, each
// leaving a state Open recovers from: the rename entry new→label first
// (old's file is then a leftover: recoverMove), then new's file, then
// old's file is removed.
func Move(dir, old, new, label string) error {
	s, ok, err := state.Load(dir, old)
	if err != nil {
		return err
	}
	if err := state.SetRenamed(dir, old, new, label); err != nil {
		return err
	}
	if ok {
		s.Name = new
		if err := state.Save(dir, s); err != nil {
			return err
		}
	}
	return state.Remove(dir, old)
}

// recoverMove finishes a Move to session name (its Runs labelled label,
// listed in runs) that stopped part way: a file of another name that no
// rename entry claims and whose windows are all Runs of label is the
// old name's. It becomes name's file when name has none, and is removed.
// The caller holds name's lock, which a Move to name needs too.
func recoverMove(dir, name string, runs []*api.Run) error {
	if len(runs) == 0 {
		return nil
	}
	renames, err := state.Renames(dir)
	if err != nil {
		return err
	}
	ours := map[string]bool{}
	for _, r := range runs {
		ours[r.ID] = true
	}
	files, err := state.List(dir)
	if err != nil {
		return err
	}
	for _, f := range files {
		if _, mapped := renames[f]; mapped || f == name {
			continue
		}
		s, ok, err := state.Load(dir, f)
		if err != nil || !ok || len(s.Windows) == 0 ||
			slices.ContainsFunc(s.Windows, func(w state.Window) bool { return !ours[w.RunID] }) {
			continue
		}
		if _, ok, err := state.Load(dir, name); err != nil {
			return err
		} else if !ok {
			s.Name = name
			if err := state.Save(dir, s); err != nil {
				return err
			}
		}
		if err := state.Remove(dir, f); err != nil {
			return err
		}
	}
	return nil
}

// Kill terminates session name's live Runs and forgets it: its file and
// its rename entry, once every terminate succeeded. done is called with
// each Run terminated. The session chooser holds the session's lock for
// it; luxmux kill-session takes none, and a luxmux attached to the
// session ends once its windows do.
func Kill(ctx context.Context, c *luxapi.Client, dir, name string, done func(id string)) error {
	label, err := Label(dir, name)
	if err != nil {
		return err
	}
	runs, err := c.ListSession(ctx, label)
	if err != nil {
		return err
	}
	var errs []error
	for _, r := range runs {
		if !state.Live(r.State) {
			continue
		}
		if err := c.Terminate(ctx, r.ID); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", r.ID, err))
		} else if done != nil {
			done(r.ID)
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	if err := state.Remove(dir, name); err != nil {
		return err
	}
	return state.DropRenamed(dir, name)
}

// LiveRuns are session name's live Runs.
func LiveRuns(ctx context.Context, c *luxapi.Client, dir, name string) ([]*api.Run, error) {
	label, err := Label(dir, name)
	if err != nil {
		return nil, err
	}
	runs, err := c.ListSession(ctx, label)
	if err != nil {
		return nil, err
	}
	var live []*api.Run
	for _, r := range runs {
		if state.Live(r.State) {
			live = append(live, r)
		}
	}
	return live, nil
}
