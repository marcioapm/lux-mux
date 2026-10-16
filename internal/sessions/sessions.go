// Package sessions is what luxmux does to whole sessions, from the
// command line or from inside a session: list, open for attaching,
// rename and kill, with the names a rename gave them, across every lux
// the session's Runs may be on (Luxes).
//
// lux cannot change a Run's labels, so a renamed session's Runs keep the
// luxmux-session label of the name they were made under; the state
// directory maps the new name to that label (state.Renames).
package sessions

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
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
// locked: the caller has just taken name's lock (see pruneStale). ls,
// when not nil, is the label's Runs as x.ListSession(ctx, label, name's
// file) lists them, made by the prune check.
func LiveLabel(ctx context.Context, x Luxes, dir, name string, locked bool) (label string, ls Listings, err error) {
	if ls, err = pruneStale(ctx, x, dir, name, locked); err != nil {
		return "", nil, err
	}
	if label, err = Label(dir, name); err != nil {
		return "", nil, err
	}
	return label, ls, nil
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

// Info is one session as the session chooser, ( ) and luxmux ls see it.
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
	// Unreachable names each lux its file has windows on that could not
	// be listed, with why: "endpoint unreachable: <lux>".
	Unreachable []string
}

// List is every session, sorted by name: those with a file and those any
// lux has live Runs of. A lux that cannot be listed while others can is
// said on x.Warn and in the Unreachable of the sessions with windows
// there; with none listed, their errors are returned.
func List(ctx context.Context, x Luxes, dir string) ([]Info, error) {
	ls := x.ListAll(ctx, Files(dir)...)
	if err := x.report(ls); err != nil {
		return nil, err
	}
	return list(dir, ls, x.Eps.Default())
}

func list(dir string, ls Listings, def config.Endpoint) ([]Info, error) {
	files, err := state.List(dir)
	if err != nil {
		return nil, err
	}
	renames, err := state.Renames(dir)
	if err != nil {
		return nil, err
	}
	byLabel := map[string][]*api.Run{}
	for _, t := range ls.Live(func(r *api.Run) bool { return r.Labels[config.LabelSession] != "" }) {
		l := t.Run.Labels[config.LabelSession]
		byLabel[l] = append(byLabel[l], t.Run)
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
			down := map[string]bool{}
			for _, w := range s.Windows {
				ep := w.EndpointOr(def)
				if err := ls.ErrOn(ep); err != nil {
					down[luxapi.FailedWord(err)+": "+ep.Label()] = true
				}
			}
			in.Unreachable = slices.Sorted(maps.Keys(down))
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
// reconciled with its Runs on every lux (and the Runs new-window made, and
// the waiting spares, that it lacks), else rebuilt from their labels.
// existed is false when there is neither. A lux that cannot be listed
// keeps the file's windows and spares there as they are, or, rebuilding,
// is recorded in the session's Unlisted. A stale rename entry naming name
// is dropped first (pruneStale). The caller holds the session's lock,
// just taken to attach it. Each lux is listed once: the prune check's
// listing of name's entry is the one reconciled.
func Open(ctx context.Context, x Luxes, dir, name string) (sess *state.Session, label string, existed bool, err error) {
	own, err := pruneStale(ctx, x, dir, name, true)
	if err != nil {
		return nil, "", false, err
	}
	return reopen(ctx, x, dir, name, own)
}

// Reopen is Open for a session this process had attached: its rename
// entry is kept even when none of its Runs is live (its windows show
// their end).
func Reopen(ctx context.Context, x Luxes, dir, name string) (sess *state.Session, label string, existed bool, err error) {
	return reopen(ctx, x, dir, name, nil)
}

// reopen is Reopen reconciling from ls, pruneStale's listing of name's
// own rename entry, else (nil) from a listing of its own.
func reopen(ctx context.Context, x Luxes, dir, name string, ls Listings) (sess *state.Session, label string, existed bool, err error) {
	if label, err = Label(dir, name); err != nil {
		return nil, "", false, err
	}
	file, _, err := state.Load(dir, name)
	if err != nil {
		return nil, "", false, err
	}
	if ls == nil {
		ls = x.ListSession(ctx, label, file)
	}
	if err := x.report(ls); err != nil {
		return nil, "", false, err
	}
	if err := recoverMove(dir, name, ls.Runs()); err != nil {
		return nil, "", false, err
	}
	sess, ok, err := state.Load(dir, name)
	if err != nil {
		return nil, "", false, err
	}
	def := x.Eps.Default()
	if ok {
		state.RecordExternal(sess, label, ls, x.Cfg.Profiles)
		sess.ReconcileUp(ls.Known(), def, ls.Down())
		state.RecordSparesFrom(sess, label, ls, def)
		return sess, label, true, nil
	}
	sess = state.RebuildLabelledFrom(name, label, ls, def, x.Cfg.Profiles)
	return sess, label, len(sess.Windows) > 0, nil
}

// ErrTaken is CheckFree's answer for a name in use.
var ErrTaken = errors.New("session exists")

// CheckFree checks that name may be given to a new or renamed session:
// a valid name with no file, not a renamed session's name or label, and
// no live Runs labelled with it on any lux. own is the label of the
// session being renamed, which may take its label back as its name (""
// for a new one). A rename entry naming name that is stale (see
// pruneStale) is dropped first. locked: the caller has just taken name's
// lock; unlocked, an entry whose session's lock is held, by this process
// too, is kept.
//
// The listings are returned: for a new session, a lux that did not answer
// may hold Runs of name, which join it once that lux answers (its
// Unlisted). A rename is refused then: Runs labelled with the new name
// would not be the renamed session's.
func CheckFree(ctx context.Context, x Luxes, dir, name, own string, locked bool) (Listings, error) {
	if err := CheckName(name); err != nil {
		return nil, err
	}
	if _, err := pruneStale(ctx, x, dir, name, locked); err != nil {
		return nil, err
	}
	if _, ok, err := state.Load(dir, name); err != nil {
		return nil, err
	} else if ok {
		return nil, fmt.Errorf("session %s: %w", name, ErrTaken)
	}
	renames, err := state.Renames(dir)
	if err != nil {
		return nil, err
	}
	if _, ok := renames[name]; ok {
		return nil, fmt.Errorf("session %s: %w", name, ErrTaken)
	}
	if name == own {
		return nil, nil
	}
	for n, l := range renames {
		if l == name {
			return nil, fmt.Errorf("%s is the label of session %s's Runs (it was renamed): choose another name", name, n)
		}
	}
	ls := x.ListSession(ctx, name)
	for _, t := range ls.Live(func(*api.Run) bool { return true }) {
		return ls, fmt.Errorf("session %s has Runs on lux %s: %w", name, t.Endpoint.Label(), ErrTaken)
	}
	if err := x.report(ls); err != nil {
		return ls, err
	}
	if down := ls.Unlisted(); own != "" && len(down) > 0 {
		return ls, fmt.Errorf("session %s may have Runs on %s, which did not answer: try again when it does", name, labels(down))
	}
	return ls, nil
}

func labels(eps []config.Endpoint) string {
	out := make([]string, len(eps))
	for i, ep := range eps {
		out[i] = ep.Label()
	}
	return strings.Join(out, ", ")
}

// pruneStale drops a rename entry naming name (as its session or its
// label) that is stale: no lux has a live Run carrying its label, and no
// process holds its session's lock. An entry whose label could not be
// listed on every lux is kept. locked makes the one exception: the caller
// has just taken name's lock, which state.Lock refuses while any Mux,
// this process's included, has name attached, so name's lock held by
// this process is the caller's. Any other lock is a session attached
// somewhere, which ends itself when its windows do (Mux.endSession). A
// pruned entry's file, whose windows' Runs are then all gone, goes with
// it. The listing of name's own entry (label renames[name], endpoints of
// name's file), when that entry is kept, is returned; else nil.
func pruneStale(ctx context.Context, x Luxes, dir, name string, locked bool) (Listings, error) {
	renames, err := state.Renames(dir)
	if err != nil {
		return nil, err
	}
	var own Listings
	for n, l := range renames {
		if n != name && l != name {
			continue
		}
		if pid, held := state.Held(dir, n); held && !(locked && n == name && pid == strconv.Itoa(os.Getpid())) {
			continue
		}
		file, _, _ := state.Load(dir, n)
		ls := x.ListSession(ctx, l, file)
		if down := ls.Unlisted(); len(down) == len(ls) && len(ls) > 0 {
			return nil, ls.Report(io.Discard)
		}
		if len(ls.Unlisted()) > 0 || len(ls.Live(func(*api.Run) bool { return true })) > 0 {
			if n == name {
				own = ls
			}
			continue
		}
		if err := state.Remove(dir, n); err != nil {
			return nil, err
		}
		if err := state.DropRenamed(dir, n); err != nil {
			return nil, err
		}
	}
	return own, nil
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

// Killing is a kill of session Name read from every lux: its live Runs,
// each with the lux it was listed on, and what decides whether its file
// may go.
type Killing struct {
	Name string
	Live []Target
	// HasFile and Mapped: it has a file, a rename entry.
	HasFile, Mapped bool
	file            *state.Session
	ls              Listings
}

// PlanKill lists session name's Runs on every lux its file and the
// configuration name. A lux that cannot be listed while others can is
// said on x.Warn; with none listed, their errors are returned. A session
// with neither file, rename entry nor live Run is an error: no session,
// or, while a lux did not answer, that it may still have Runs there.
func PlanKill(ctx context.Context, x Luxes, dir, name string) (*Killing, error) {
	label, err := Label(dir, name)
	if err != nil {
		return nil, err
	}
	file, hasFile, _ := state.Load(dir, name)
	renames, err := state.Renames(dir)
	if err != nil {
		return nil, err
	}
	_, mapped := renames[name]
	ls := x.ListSession(ctx, label, file)
	if err := x.report(ls); err != nil {
		return nil, err
	}
	k := &Killing{Name: name, Live: ls.Live(func(*api.Run) bool { return true }), HasFile: hasFile, Mapped: mapped, file: file, ls: ls}
	if len(k.Live) == 0 && !hasFile && !mapped {
		if err := k.mayRemain(); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("no session %s", name)
	}
	return k, nil
}

// mayRemain is the error for a session with no file of which a lux that
// did not answer may still hold Runs (nil: every lux answered).
func (k *Killing) mayRemain() error {
	down := k.ls.Unlisted()
	if len(down) == 0 {
		return nil
	}
	return fmt.Errorf("session %s may still have Runs on %s: run kill-session again when it answers", k.Name, labels(down))
}

// Do terminates the Runs on each one's own lux (done is called with each
// Run terminated) and forgets the session, its file and rename entry,
// only once every terminate succeeded and no lux its file has windows,
// spares or leftovers on went unlisted. Otherwise the file, with its
// leftovers, is kept and the error names each Run not terminated and its
// lux.
func (k *Killing) Do(ctx context.Context, x Luxes, dir string, done func(id string)) error {
	var errs []error
	for _, t := range k.Live {
		if err := t.Terminate(ctx, x.Eps); err != nil {
			errs = append(errs, fmt.Errorf("%s on %s: %w", t.Run.ID, t.Endpoint.Label(), err))
		} else if done != nil {
			done(t.Run.ID)
		}
	}
	if len(errs) > 0 {
		kept := fmt.Errorf("session %s: file kept, %d Run(s) not terminated", k.Name, len(errs))
		return errors.Join(append([]error{kept}, errs...)...)
	}
	if k.HasFile {
		// Its Runs on a lux that did not answer may still be there.
		var down []config.Endpoint
		for _, ep := range endpointsOf(x.Eps.Default(), k.file) {
			if k.ls.Down()(ep) && !slices.Contains(down, ep) {
				down = append(down, ep)
			}
		}
		if len(down) > 0 {
			return fmt.Errorf("session file kept: %s, which it has windows on, did not answer", labels(down))
		}
	}
	if !k.HasFile {
		if err := k.mayRemain(); err != nil {
			return err
		}
	}
	if err := state.Remove(dir, k.Name); err != nil {
		return err
	}
	return state.DropRenamed(dir, k.Name)
}

// LiveRuns are session name's live Runs on every lux, each with the lux
// it was listed on.
func LiveRuns(ctx context.Context, x Luxes, dir, name string) ([]Target, error) {
	label, err := Label(dir, name)
	if err != nil {
		return nil, err
	}
	file, _, _ := state.Load(dir, name)
	ls := x.ListSession(ctx, label, file)
	if err := x.report(ls); err != nil {
		return nil, err
	}
	return ls.Live(func(*api.Run) bool { return true }), nil
}
