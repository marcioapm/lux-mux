// Package state keeps a session's local state: the order and names of
// its windows, and the warm spares it has claimed.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
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
	// Endpoint is the lux the Run lives on: URL, tenant and the name of
	// its key's source, never the key. Nil (files from before endpoints):
	// the lux CLI configuration's.
	Endpoint *config.Endpoint `json:"endpoint,omitempty"`
}

// EndpointOr is w's endpoint, def when it has none recorded.
func (w Window) EndpointOr(def config.Endpoint) config.Endpoint {
	if w.Endpoint != nil {
		return *w.Endpoint
	}
	return def
}

// Session is $XDG_STATE_HOME/luxmux/<session>.json.
type Session struct {
	Name    string   `json:"name"`
	Windows []Window `json:"windows"`
	// DefaultProfile is the profile C-b c makes windows of (luxmux new
	// -p, or the dialog's checkbox); empty: the configuration's.
	DefaultProfile string `json:"defaultProfile,omitempty"`
	// Active and Last are window indexes (positions in Windows).
	Active int `json:"active"`
	Last   int `json:"last"`
	// Claimed are spare Runs that became windows: their luxmux-spare label
	// stays, so this is how they are told from the spare still waiting.
	Claimed []string `json:"claimed,omitempty"`
	// Spares maps a warm-spare slot (config.SpareSlot) to its waiting
	// spare's Run id.
	Spares map[string]string `json:"spares,omitempty"`
	// SpareEndpoints is the endpoint of each slot in Spares. A slot it
	// lacks (files from before every slot recorded one) is the lux CLI
	// configuration's at read time.
	SpareEndpoints map[string]config.Endpoint `json:"spareEndpoints,omitempty"`
	// Unlisted are endpoints the session's Runs could not be listed on
	// when it was rebuilt: their windows are added once one answers.
	Unlisted []config.Endpoint `json:"unlisted,omitempty"`
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
	return writeJSON(dir, s.Name+".*.tmp", Path(dir, s.Name), s)
}

// writeJSON writes v to path atomically, through a temporary file in dir
// named after pattern (os.CreateTemp).
func writeJSON(dir, pattern, path string, v any) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, pattern)
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
	return os.Rename(tmp.Name(), path)
}

// ErrLocked is Lock's error when another process has the session.
var ErrLocked = errors.New("session is attached elsewhere")

// lockTries×lockEvery is how long Lock retries a held lock: Held's probe
// holds a shared lock for a moment.
const (
	lockTries = 4
	lockEvery = 5 * time.Millisecond
)

// Lock takes the session's advisory lock, <dir>/<session>.lock, held
// until unlock is called or the process exits: one luxmux process per
// session. The file names the process that holds it.
func Lock(dir, session string) (unlock func(), err error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(lockPath(dir, session), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	for i := 1; ; i++ {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if !errors.Is(err, syscall.EWOULDBLOCK) || i == lockTries {
			break
		}
		time.Sleep(lockEvery)
	}
	if err != nil {
		pid := lockPid(f)
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			if pid != "" {
				return nil, fmt.Errorf("%w (pid %s)", ErrLocked, pid)
			}
			return nil, ErrLocked
		}
		return nil, err
	}
	_ = f.Truncate(0)
	_, _ = f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
	// Once: a second unlock must not flock a descriptor number reused
	// by another file.
	return sync.OnceFunc(func() {
		_ = f.Truncate(0)
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}), nil
}

func lockPath(dir, session string) string { return filepath.Join(dir, session+".lock") }

// lockPid is the pid a lock file names ("": none).
func lockPid(f *os.File) string {
	b := make([]byte, 32)
	n, _ := f.ReadAt(b, 0)
	return strings.TrimSpace(string(b[:n]))
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
		// Only session names: renamesFile starts with a dot, which no
		// session name does.
		if n, ok := strings.CutSuffix(e.Name(), ".json"); ok && !e.IsDir() && config.NameRe.MatchString(n) {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out, nil
}

// Held reports whether another process holds session's lock, and the pid
// it wrote. The probe takes a shared lock without blocking and drops it
// at once; Lock retries past it. It only probes a lock file that names a
// pid (Unlock empties it).
func Held(dir, session string) (pid string, held bool) {
	f, err := os.Open(lockPath(dir, session))
	if err != nil {
		return "", false
	}
	defer f.Close()
	if pid = lockPid(f); pid == "" {
		// Unlock empties the file, and Lock writes its pid once it holds it.
		return "", false
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err != nil {
		return pid, errors.Is(err, syscall.EWOULDBLOCK)
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return "", false
}

// renamesFile maps a renamed session's name to the luxmux-session label
// its Runs keep: lux cannot change a Run's labels.
const renamesFile = ".renames.json"

// Renames is the rename map: session name → its Runs' label. A session
// absent from it is labelled with its own name.
func Renames(dir string) (map[string]string, error) {
	b, err := os.ReadFile(filepath.Join(dir, renamesFile))
	if errors.Is(err, os.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	m := map[string]string{}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Join(dir, renamesFile), err)
	}
	return m, nil
}

// SetRenamed records that session old is now named new, its Runs keeping
// label. A session renamed back to its label leaves the map.
func SetRenamed(dir, old, new, label string) error {
	m, err := Renames(dir)
	if err != nil {
		return err
	}
	delete(m, old)
	if new != label {
		m[new] = label
	}
	return writeRenames(dir, m)
}

// DropRenamed forgets a session's entry (it was killed or ended).
func DropRenamed(dir, name string) error {
	m, err := Renames(dir)
	if err != nil {
		return err
	}
	if _, ok := m[name]; !ok {
		return nil
	}
	delete(m, name)
	return writeRenames(dir, m)
}

func writeRenames(dir string, m map[string]string) error {
	return writeJSON(dir, ".renames.*.tmp", filepath.Join(dir, renamesFile), m)
}

// Live reports whether a Run's state is one a window can follow: anything
// but terminated.
func Live(state string) bool { return state != "terminated" }

// Terminating reports whether a Run is being terminated: luxd accepted
// the terminate and keeps it stopping, reason terminate, until its runner
// stopped it (lux internal/server/reaper.go, requestStop).
func Terminating(r *api.Run) bool {
	return r.StageReason == "terminate" || r.StateReason == "terminate"
}

// Adoptable reports whether r is a Run luxmux new-window made for
// session that a window may still follow: labelled external, not a spare,
// neither terminated nor being terminated.
func Adoptable(r *api.Run, session string) bool {
	return r.Labels[config.LabelSession] == session && r.Labels[config.LabelExternal] == "true" &&
		r.Labels[config.LabelSpare] != "true" && Live(r.State) && !Terminating(r)
}

// RecordExternal appends to sess the Runs new-window made that its file
// lacks (label: the luxmux-session label of the session's Runs):
// new-window found the session attached and left the record to that
// process, which detached before it adopted them. A Run being terminated
// (its window was closed there) is not one.
func RecordExternal(sess *Session, label string, runs []*api.Run, profiles map[string]config.Profile) {
	for _, r := range runs {
		if !Adoptable(r, label) {
			continue
		}
		if slices.ContainsFunc(sess.Windows, func(w Window) bool { return w.RunID == r.ID }) {
			continue
		}
		sess.Windows = append(sess.Windows, WindowFromRun(r, profiles[r.Labels[config.LabelProfile]]))
	}
}

// RecordSpares puts into sess's empty spare slots the waiting spares lux
// has of it (label: its Runs' luxmux-session label) that its file does
// not know: a submit a switch cancelled may still have made its Run. A
// slot already filled keeps its spare; the newest Run wins an empty one.
func RecordSpares(sess *Session, label string, runs []*api.Run) {
	known := map[string]bool{}
	for _, w := range sess.Windows {
		known[w.RunID] = true
	}
	for _, id := range sess.Claimed {
		known[id] = true
	}
	for _, id := range sess.Spares {
		known[id] = true
	}
	filled := map[string]bool{}
	for key, id := range sess.Spares {
		filled[key] = id != ""
	}
	byAge := slices.Clone(runs)
	sort.SliceStable(byAge, func(i, j int) bool { return byAge[i].CreatedAt.After(byAge[j].CreatedAt) })
	for _, r := range byAge {
		if r.Labels[config.LabelSession] != label || r.Labels[config.LabelSpare] != "true" || known[r.ID] ||
			!waitingSpare[r.State] || Terminating(r) {
			continue
		}
		key := r.Labels[config.LabelSpec]
		if key == "" || filled[key] {
			continue
		}
		if sess.Spares == nil {
			sess.Spares = map[string]string{}
		}
		sess.Spares[key], filled[key] = r.ID, true
	}
}

// waitingSpare are the states of a spare that may still be claimed.
var waitingSpare = map[string]bool{"running": true, "submitted": true, "scheduled": true, "provisioning": true, "starting": true}

// RebuildLabelled makes session name from the Runs lux lists for it (those
// labelled label: its name, or its old one if it was renamed, see
// Renames), for when its file is gone: its non-terminated Runs that are
// not a waiting warm spare, oldest first. Names are lost: each window is
// named after its profile. A Run whose spec lux returns gets that spec
// as its window's (profiles: the configuration's, by name).
func RebuildLabelled(name, label string, runs []*api.Run, profiles map[string]config.Profile) *Session {
	return rebuild(name, label, []Listing{{Runs: runs}}, nil, profiles)
}

// Listing is what one endpoint listed for a session; Err set: it could
// not be listed.
type Listing struct {
	Endpoint config.Endpoint
	Runs     []*api.Run
	Err      error
}

// RebuildFrom makes a session from what every endpoint lists for it
// (Runs labelled with the session), for when its file is gone: its
// non-terminated Runs that are not a waiting warm spare, oldest first.
// Names are lost: each window is named after its profile. A Run whose
// spec lux returns gets that spec as its window's (profiles: the
// configuration's, by name). Each window and spare records the endpoint
// its Run was listed on, and each spare's slot is config.SpareSlot of its
// spec on that endpoint (def: the lux CLI configuration's). A Run two
// endpoints list (one lux, two key sources) is taken from the first.
// Listings that failed add nothing and are recorded in Unlisted.
func RebuildFrom(session string, ls []Listing, def config.Endpoint, profiles map[string]config.Profile) *Session {
	return rebuild(session, session, ls, &def, profiles)
}

func rebuild(name, label string, ls []Listing, def *config.Endpoint, profiles map[string]config.Profile) *Session {
	s := &Session{Name: name}
	live := []*api.Run{}
	epOf := map[string]config.Endpoint{}
	for _, l := range ls {
		if l.Err != nil {
			s.Unlisted = append(s.Unlisted, l.Endpoint)
			continue
		}
		for _, r := range l.Runs {
			if r.Labels[config.LabelSession] != label || !Live(r.State) {
				continue
			}
			if _, dup := epOf[r.ID]; dup {
				continue
			}
			epOf[r.ID] = l.Endpoint
			live = append(live, r)
		}
	}
	sort.SliceStable(live, func(i, j int) bool { return live[i].CreatedAt.Before(live[j].CreatedAt) })
	// def nil (RebuildLabelled): one lux, the default; nothing records it.
	window := func(id, prof string) Window {
		w := Window{RunID: id, Name: prof, Profile: prof}
		if def != nil {
			ep := epOf[id]
			w.Endpoint = &ep
		}
		return w
	}
	for _, r := range live {
		prof := r.Labels[config.LabelProfile]
		if r.Labels[config.LabelSpare] == "true" {
			// Which spares were claimed is only in the lost file: the
			// newest of a spec is taken to be its waiting spare, any
			// older one a claimed window.
			key := r.Labels[config.LabelSpec]
			if key == "" {
				key = prof
			}
			if def != nil {
				key = config.SpareSlot(key, epOf[r.ID], *def)
			}
			if prev, ok := s.Spares[key]; ok {
				s.Claimed = append(s.Claimed, prev)
				s.Windows = append(s.Windows, window(prev, prof))
			}
			if def != nil {
				s.SetSpare(key, r.ID, epOf[r.ID])
			} else {
				if s.Spares == nil {
					s.Spares = map[string]string{}
				}
				s.Spares[key] = r.ID
			}
			continue
		}
		s.Windows = append(s.Windows, window(r.ID, prof))
	}
	sortWindowsByCreation(s, live)
	byID := map[string]*api.Run{}
	for _, r := range live {
		byID[r.ID] = r
	}
	for i, w := range s.Windows {
		if r := byID[w.RunID]; r != nil && r.Spec.Image.Ref != "" {
			nw := WindowFromRun(r, profiles[w.Profile])
			nw.Endpoint = w.Endpoint
			s.Windows[i] = nw
		}
	}
	return s
}

// WindowFromRun is the window of a Run luxmux did not record: its spec
// read back from lux over base, the configured profile it names, which
// keeps what lux does not store (secrets_from, labels, warm_spare).
func WindowFromRun(r *api.Run, base config.Profile) Window {
	sp := r.Spec
	p := base
	p.Image, p.Pool, p.Workdir = sp.Image.Ref, sp.Placement.Pool, sp.Workload.Workdir
	p.Shell, p.Env, p.UnrestrictedNetwork = sp.Workload.Command, sp.Env, sp.Network.Unrestricted
	// luxd stores resources filled in: its defaults for what the submit
	// left unset, or the resolved size on a shaped pool; spec.resources is
	// never read. What the window asked is sizing.requested (shaped pools
	// only), else luxmux's LabelResources, else, with neither (a Run from
	// before the label, or not luxmux's), the profile's own values.
	label, labelled := r.Labels[config.LabelResources]
	switch {
	case r.Sizing != nil && r.Sizing.Requested != nil:
		q := r.Sizing.Requested
		p.Size, p.CPUs, p.Memory, p.Disk = q.Size, 0, "", ""
		if q.CPUs != nil {
			p.CPUs = *q.CPUs
		}
		if q.Memory != nil {
			p.Memory = fmt.Sprint(int64(*q.Memory))
		}
		if q.Disk != nil {
			p.Disk = fmt.Sprint(int64(*q.Disk))
		}
	case labelled:
		config.ApplyResourcesLabel(&p, label)
	}
	p.Repository, p.Branch, p.Ref, p.Repositories = "", "", "", nil
	if sp.Git != nil {
		for _, repo := range sp.Git.Repositories {
			ref := repo.Ref
			if ref == "HEAD" {
				// luxd stores the remote's HEAD as "HEAD": the profile's "".
				ref = ""
			}
			cred := repo.Credential
			if cred == base.GitCredential {
				cred = ""
			}
			// lux fills in defaults; dropping them keeps the window's
			// SpecKey equal to its profile's.
			name, path := repo.Name, repo.Path
			if path == config.RepoDir(name) {
				path = ""
			}
			if name == config.RepoName(repo.URL) {
				name = ""
			}
			p.Repositories = append(p.Repositories, config.Repository{URL: repo.URL, Ref: ref, Name: name, Path: path, GitCredential: cred})
		}
	}
	p.Secrets = nil
	for _, sec := range sp.Secrets {
		if sec.As == "env" {
			p.Secrets = append(p.Secrets, sec.Name)
		}
	}
	p.Egress = nil
	for _, e := range sp.Network.Egress {
		if e.Host != "" {
			p.Egress = append(p.Egress, e.Host)
		} else {
			p.Egress = append(p.Egress, e.CIDR)
		}
	}
	profile := r.Labels[config.LabelProfile]
	name := r.Labels[config.LabelWindow]
	if name == "" {
		name = profile
	}
	return Window{RunID: r.ID, Name: name, Profile: profile, Spec: &p}
}

// sortWindowsByCreation orders windows by their Runs' creation, after
// RebuildFrom appended claimed spares out of order.
func sortWindowsByCreation(s *Session, runs []*api.Run) {
	at := map[string]time.Time{}
	for _, r := range runs {
		at[r.ID] = r.CreatedAt
	}
	sort.SliceStable(s.Windows, func(i, j int) bool { return at[s.Windows[i].RunID].Before(at[s.Windows[j].RunID]) })
}

// ReconcileUp drops windows whose Runs lux no longer has or has
// terminated (known maps a Run id to its state; ids it lacks are gone),
// keeping Active and Last pointing at the same windows where they
// survive. The windows and spares on an endpoint down reports (nil: none)
// are kept as they are; def is the endpoint of a window or spare that
// records none.
func (s *Session) ReconcileUp(known map[string]string, def config.Endpoint, down func(config.Endpoint) bool) {
	isDown := func(ep config.Endpoint) bool { return down != nil && down(ep) }
	keep := func(id string) bool { st, ok := known[id]; return ok && Live(st) }
	active, last := s.at(s.Active), s.at(s.Last)
	kept := s.Windows[:0]
	held := map[string]bool{}
	for _, w := range s.Windows {
		if keep(w.RunID) || isDown(w.EndpointOr(def)) {
			kept = append(kept, w)
			held[w.RunID] = true
		}
	}
	s.Windows = kept
	s.Active, s.Last = s.index(active), s.index(last)
	for p, id := range s.Spares {
		if !keep(id) && !isDown(s.SpareEndpoint(p, def)) {
			delete(s.Spares, p)
		}
	}
	for p := range s.SpareEndpoints {
		if _, ok := s.Spares[p]; !ok {
			delete(s.SpareEndpoints, p)
		}
	}
	claimed := s.Claimed[:0]
	for _, id := range s.Claimed {
		if keep(id) || held[id] {
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

// SetSpare records id as slot's waiting spare, on ep.
func (s *Session) SetSpare(slot, id string, ep config.Endpoint) {
	if s.Spares == nil {
		s.Spares = map[string]string{}
	}
	if s.SpareEndpoints == nil {
		s.SpareEndpoints = map[string]config.Endpoint{}
	}
	s.Spares[slot] = id
	s.SpareEndpoints[slot] = ep
}

// DropSpare forgets slot's spare and its endpoint.
func (s *Session) DropSpare(slot string) {
	delete(s.Spares, slot)
	delete(s.SpareEndpoints, slot)
}

// SpareEndpoint is the endpoint slot's spare lives on; def when the file
// records none for it.
func (s *Session) SpareEndpoint(slot string, def config.Endpoint) config.Endpoint {
	if ep, ok := s.SpareEndpoints[slot]; ok {
		return ep
	}
	return def
}

// RecordsSpare reports whether id is the Run of one of s's spare slots,
// a leftover's included.
func (s *Session) RecordsSpare(id string) bool {
	for _, v := range s.Spares {
		if v == id {
			return true
		}
	}
	return false
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
