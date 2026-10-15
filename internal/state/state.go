// Package state keeps a session's local state: the order and names of
// its windows, and the warm spares it has claimed.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/marcioapm/lux/api"

	"github.com/marcioapm/lux-mux/internal/config"
)

// Window is one window of a session: a Run.
type Window struct {
	RunID   string `json:"runId"`
	Name    string `json:"name,omitempty"`
	Profile string `json:"profile,omitempty"`
	// Spec is the profile the Run was made from, kept so a relaunch or a
	// resume needing its secrets works after the configuration changed.
	Spec *config.Profile `json:"spec,omitempty"`
}

// Session is $XDG_STATE_HOME/luxmux/<session>.json.
type Session struct {
	Name    string   `json:"name"`
	Windows []Window `json:"windows"`
	// Active and Last are window indexes (positions in Windows).
	Active int `json:"active"`
	Last   int `json:"last"`
	// Claimed are spare Runs that became windows: their luxmux-spare label
	// stays, so this is how they are told from the spare still waiting.
	Claimed []string `json:"claimed,omitempty"`
	// Spares maps a spec key (config.SpecKey) to its waiting warm spare's
	// Run id.
	Spares map[string]string `json:"spares,omitempty"`
	// Recent images and repositories typed in the dialog, newest first.
	RecentImages []string `json:"recentImages,omitempty"`
	RecentRepos  []string `json:"recentRepos,omitempty"`
}

// Dir is where session files live.
func Dir() (string, error) {
	if d := os.Getenv("XDG_STATE_HOME"); d != "" && filepath.IsAbs(d) {
		return filepath.Join(d, "luxmux"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", "luxmux"), nil
}

// Path is a session's file.
func Path(dir, session string) string { return filepath.Join(dir, session+".json") }

// Load reads a session's file; ok is false when there is none.
func Load(dir, session string) (s *Session, ok bool, err error) {
	b, err := os.ReadFile(Path(dir, session))
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	s = &Session{}
	if err := json.Unmarshal(b, s); err != nil {
		return nil, false, fmt.Errorf("%s: %w", Path(dir, session), err)
	}
	s.Name = session
	return s, true, nil
}

// Save writes the session's file atomically.
func Save(dir string, s *Session) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, s.Name+".*.tmp")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), Path(dir, s.Name))
}

// Remove deletes a session's file; a missing one is fine.
func Remove(dir, session string) error {
	err := os.Remove(Path(dir, session))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// List names the sessions that have a file, sorted.
func List(dir string) ([]string, error) {
	ents, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range ents {
		if n, ok := strings.CutSuffix(e.Name(), ".json"); ok && !e.IsDir() {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out, nil
}

// Live reports whether a Run's state is one a window can follow: anything
// but terminated.
func Live(state string) bool { return state != "terminated" }

// Rebuild makes a session from the Runs lux lists for it (those labelled
// with the session), for when its file is gone: its non-terminated Runs
// that are not a waiting warm spare, oldest first. Names are lost: each
// window is named after its profile.
func Rebuild(session string, runs []*api.Run) *Session {
	s := &Session{Name: session}
	live := make([]*api.Run, 0, len(runs))
	for _, r := range runs {
		if r.Labels[config.LabelSession] != session || !Live(r.State) {
			continue
		}
		live = append(live, r)
	}
	sort.SliceStable(live, func(i, j int) bool { return live[i].CreatedAt.Before(live[j].CreatedAt) })
	for _, r := range live {
		prof := r.Labels[config.LabelProfile]
		if r.Labels[config.LabelSpare] == "true" {
			// Which spares were claimed is only in the lost file: the
			// newest of a spec is taken to be its waiting spare, any
			// older one a claimed window.
			if s.Spares == nil {
				s.Spares = map[string]string{}
			}
			key := r.Labels[config.LabelSpec]
			if key == "" {
				key = prof
			}
			if prev, ok := s.Spares[key]; ok {
				s.Claimed = append(s.Claimed, prev)
				s.Windows = append(s.Windows, Window{RunID: prev, Name: prof, Profile: prof})
			}
			s.Spares[key] = r.ID
			continue
		}
		s.Windows = append(s.Windows, Window{RunID: r.ID, Name: prof, Profile: prof})
	}
	sortWindowsByCreation(s, live)
	return s
}

// sortWindowsByCreation orders windows by their Runs' creation, after
// Rebuild appended claimed spares out of order.
func sortWindowsByCreation(s *Session, runs []*api.Run) {
	at := map[string]time.Time{}
	for _, r := range runs {
		at[r.ID] = r.CreatedAt
	}
	sort.SliceStable(s.Windows, func(i, j int) bool { return at[s.Windows[i].RunID].Before(at[s.Windows[j].RunID]) })
}

// Reconcile drops windows whose Runs lux no longer has or has terminated
// (known maps a Run id to its state; ids it lacks are gone), keeping
// Active and Last pointing at the same windows where they survive.
func (s *Session) Reconcile(known map[string]string) {
	active, last := s.at(s.Active), s.at(s.Last)
	kept := s.Windows[:0]
	for _, w := range s.Windows {
		if st, ok := known[w.RunID]; ok && Live(st) {
			kept = append(kept, w)
		}
	}
	s.Windows = kept
	s.Active, s.Last = s.index(active), s.index(last)
	for p, id := range s.Spares {
		if st, ok := known[id]; !ok || !Live(st) {
			delete(s.Spares, p)
		}
	}
	claimed := s.Claimed[:0]
	for _, id := range s.Claimed {
		if st, ok := known[id]; ok && Live(st) {
			claimed = append(claimed, id)
		}
	}
	s.Claimed = claimed
}

func (s *Session) at(i int) string {
	if i >= 0 && i < len(s.Windows) {
		return s.Windows[i].RunID
	}
	return ""
}

func (s *Session) index(id string) int {
	for i, w := range s.Windows {
		if w.RunID == id {
			return i
		}
	}
	return 0
}

// AddRecent puts v first in list, without duplicates, keeping at most 10.
func AddRecent(list []string, v string) []string {
	if v == "" {
		return list
	}
	out := []string{v}
	for _, x := range list {
		if x != v && len(out) < 10 {
			out = append(out, x)
		}
	}
	return out
}
