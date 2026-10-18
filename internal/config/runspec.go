package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/marcioapm/lux/api"
)

// Labels luxmux puts on its Runs. lux label keys are letters, digits and
// . _ / -; luxmux's are prefixed so a user's labels cannot collide.
const (
	LabelApp     = "app"
	AppName      = "luxmux"
	LabelSession = "luxmux-session"
	LabelProfile = "luxmux-profile"
	// LabelExternal marks a window's Run made by luxmux new-window: an
	// attached luxmux adopts it as a window (the value is "true").
	LabelExternal = "luxmux-external"
	// LabelWindow is the name new-window -n gave the window.
	LabelWindow = "luxmux-window"
	// LabelSpare marks a warm spare (the value is "true"). Labels cannot
	// change after submit, so a claimed spare keeps it: the session's state
	// file records claims.
	LabelSpare = "luxmux-spare"
	// LabelSpec is a warm spare's SpecKey: which windows may claim it.
	LabelSpec = "luxmux-spec"
	// LabelResources is what the window asked of resources, e.g.
	// "cpus=4,memory=8Gi" or "size=large"; absent when it asked
	// nothing. luxd stores its defaults in spec.resources and records the
	// request (sizing.requested) only on shaped pools.
	LabelResources = "luxmux-resources"
)

// SpecKey identifies the Run a profile makes, whatever the profile is
// named and wherever it runs: a hash of its RunSpec without luxmux's
// labels or secret values. A warm spare is claimed only by a window whose
// profile has the same key. It covers on_start and on_resume (arguments of
// the window command) and every static LUXMUX_* value but the session's
// and profile's (spares are scoped to a session and checked against their
// profile on claim). Empty for a profile that makes no RunSpec.
func SpecKey(p Profile) string {
	sp, err := RunSpec(p, Target{}, nil)
	if err != nil {
		return ""
	}
	// spec.resources already carries it; keys from before the label stay.
	delete(sp.Labels, LabelResources)
	b, err := json.Marshal(sp)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

// Workspace is the state volume every window's Run has, where
// repositories are cloned and what survives a move.
const Workspace = "/workspace"

// Home is the default image's agent home, a state volume when the
// profile's HomeVolume says so.
const Home = "/home/agent"

// Target says which session (and profile) a Run is made for. Name is the
// Run's name (RunName); empty, the Run is named luxmux-<Session>, which is
// also what SpecKey hashes, so the name never changes a key.
type Target struct {
	Session string
	Profile string
	Name    string
	Spare   bool
}

// MaxRunName is the longest Run name lux takes, in bytes.
const MaxRunName = 200

// SpareWindow stands for the window in a waiting spare's Run name.
const SpareWindow = "spare"

// RunName is the name of the Run of window window in the session shown as
// session: "<session>/<window>", each control character replaced by a
// space and cut to MaxRunName bytes on a character boundary, which lux
// accepts. Rebuilding a session splits it at the first '/' (session names
// have none).
func RunName(session, window string) string {
	s := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, session+"/"+window)
	if len(s) <= MaxRunName {
		return s
	}
	n := MaxRunName
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// WindowOfRunName is the window a Run named name says it is of, if that
// name has the form RunName gives for a session named session.
func WindowOfRunName(name, session string) (string, bool) {
	s, w, ok := strings.Cut(name, "/")
	if !ok || s != session || w == "" {
		return "", false
	}
	return w, true
}

// RunSpec maps a profile to the spec of a window's Run: a generic tty
// workload running the shell, with a state volume at /workspace, the
// profile's repositories cloned under it, and luxmux's labels. secrets holds
// the values (nil for a validateOnly check, which needs only the names).
func RunSpec(p Profile, t Target, secrets map[string]string) (api.RunSpec, error) {
	if err := p.Validate(); err != nil {
		return api.RunSpec{}, err
	}
	name := t.Name
	if name == "" {
		name = "luxmux-" + t.Session
	}
	sp := api.RunSpec{
		Name:   name,
		Labels: map[string]string{},
		Image:  api.Image{Ref: p.Image},
		Workload: api.Workload{
			Adapter: "generic",
			TTY:     true,
			Command: WindowCommand(p),
			Workdir: p.Workdir,
		},
		Volumes: []api.Volume{{Name: "workspace", Path: Workspace, Kind: "state"}},
	}
	if p.HomeVolume() {
		sp.Volumes = append(sp.Volumes, api.Volume{Name: "home", Path: Home, Kind: "state"})
	}
	for k, v := range p.Labels {
		sp.Labels[k] = v
	}
	sp.Labels[LabelApp] = AppName
	sp.Labels[LabelSession] = t.Session
	sp.Labels[LabelProfile] = t.Profile
	if t.Spare {
		sp.Labels[LabelSpare] = "true"
		sp.Labels[LabelSpec] = SpecKey(p)
	}
	sp.Env = map[string]string{}
	for k, v := range p.Env {
		sp.Env[k] = v
	}
	creds := p.CredentialNames()
	for _, n := range p.SecretNames() {
		s := api.Secret{Name: n, Value: secrets[n]}
		if !slices.Contains(creds, n) {
			s.As = "env"
		}
		sp.Secrets = append(sp.Secrets, s)
	}
	// Each repository is cloned to its path, else lux's default
	// (/workspace/repos/<name>); the shell starts in the first one.
	for _, r := range p.RepositoryList() {
		if sp.Git == nil {
			sp.Git = &api.Git{}
		}
		sp.Git.Repositories = append(sp.Git.Repositories, api.Repository{
			Name: r.RepoName(), URL: r.URL, Ref: r.RepoRef(), Path: r.Path, Credential: p.Credential(r)})
		if sp.Workload.Workdir == "" {
			sp.Workload.Workdir = r.Path
			if r.Path == "" {
				sp.Workload.Workdir = RepoDir(r.RepoName())
			}
		}
	}
	if sp.Workload.Workdir == "" {
		sp.Workload.Workdir = Workspace
	}
	// Secrets never go here: Validate keeps their names off these.
	for k, v := range windowEnv(p, t, sp.Workload.Workdir) {
		sp.Env[k] = v
	}
	sp.Resources.Size = p.Size
	sp.Resources.CPUs = p.CPUs
	if p.Memory != "" {
		b, err := api.ParseBytes(p.Memory)
		if err != nil {
			return api.RunSpec{}, fmt.Errorf("memory: %w", err)
		}
		sp.Resources.Memory = b
	}
	if p.Disk != "" {
		b, err := api.ParseBytes(p.Disk)
		if err != nil {
			return api.RunSpec{}, fmt.Errorf("disk: %w", err)
		}
		sp.Resources.Disk = b
	}
	if v := resourcesLabel(sp.Resources); v != "" {
		sp.Labels[LabelResources] = v
	}
	sp.Placement.Pool = p.Pool
	sp.Network.Unrestricted = p.UnrestrictedNetwork
	for _, e := range p.Egress {
		if _, _, err := net.ParseCIDR(e); err == nil {
			sp.Network.Egress = append(sp.Network.Egress, api.EgressRule{CIDR: e})
		} else {
			sp.Network.Egress = append(sp.Network.Egress, api.EgressRule{Host: e})
		}
	}
	return sp, nil
}

// resourcesLabel is LabelResources's value for r as RunSpec sets it:
// size, else cpus, memory and disk, each only when set. lux takes a label
// value of up to 256 bytes, any characters; this is far shorter.
func resourcesLabel(r api.Resources) string {
	if r.Size != "" {
		return "size=" + r.Size
	}
	var parts []string
	if r.CPUs != 0 {
		parts = append(parts, "cpus="+strconv.FormatFloat(r.CPUs, 'g', -1, 64))
	}
	if r.Memory != 0 {
		parts = append(parts, "memory="+compactBytes(r.Memory))
	}
	if r.Disk != 0 {
		parts = append(parts, "disk="+compactBytes(r.Disk))
	}
	return strings.Join(parts, ",")
}

// compactBytes writes b in the largest binary unit that divides it
// exactly, as api.ParseBytes reads it back: 8Gi, 1536Mi, 1000.
func compactBytes(b api.Bytes) string {
	n := int64(b)
	for _, u := range []string{"", "Ki", "Mi", "Gi"} {
		if n%1024 != 0 || u == "Gi" {
			return strconv.FormatInt(n, 10) + u
		}
		n /= 1024
	}
	return ""
}

// ApplyResourcesLabel sets p's size, cpus, memory and disk to what a
// LabelResources value v says, the rest unset. A value it cannot read
// leaves p as it was and returns false.
func ApplyResourcesLabel(p *Profile, v string) bool {
	var q Profile
	for _, kv := range strings.Split(v, ",") {
		k, val, ok := strings.Cut(kv, "=")
		if !ok || val == "" {
			return false
		}
		switch k {
		case "size":
			q.Size = val
		case "cpus":
			c, err := strconv.ParseFloat(val, 64)
			if err != nil || c <= 0 {
				return false
			}
			q.CPUs = c
		case "memory", "disk":
			if _, err := api.ParseBytes(val); err != nil {
				return false
			}
			if k == "memory" {
				q.Memory = val
			} else {
				q.Disk = val
			}
		default:
			return false
		}
	}
	p.Size, p.CPUs, p.Memory, p.Disk = q.Size, q.CPUs, q.Memory, q.Disk
	return true
}

// SecretValues is the resume request's secrets: every one of a Run's
// secrets with its value, in name order.
func SecretValues(values map[string]string) []api.Secret {
	names := make([]string, 0, len(values))
	for n := range values {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]api.Secret, 0, len(names))
	for _, n := range names {
		out = append(out, api.Secret{Name: n, Value: values[n]})
	}
	return out
}
