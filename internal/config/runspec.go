package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"sort"

	"github.com/marcioapm/lux/api"
)

// Labels luxmux puts on its Runs. lux label keys are letters, digits and
// . _ / -; luxmux's are prefixed so a user's labels cannot collide.
const (
	LabelApp     = "app"
	AppName      = "luxmux"
	LabelSession = "luxmux-session"
	LabelProfile = "luxmux-profile"
	// LabelSpare marks a warm spare (the value is "true"). Labels cannot
	// change after submit, so a claimed spare keeps it: the session's state
	// file records claims.
	LabelSpare = "luxmux-spare"
	// LabelSpec is a warm spare's SpecKey: which windows may claim it.
	LabelSpec = "luxmux-spec"
)

// SpecKey identifies the Run a profile makes, whatever the profile is
// named and wherever it runs: a hash of its RunSpec without luxmux's
// labels or secret values. A warm spare is claimed only by a window whose
// profile has the same key. Empty for a profile that makes no RunSpec.
func SpecKey(p Profile) string {
	sp, err := RunSpec(p, Target{}, nil)
	if err != nil {
		return ""
	}
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

// Target says which session (and profile) a Run is made for.
type Target struct {
	Session string
	Profile string
	Spare   bool
}

// RunSpec maps a profile to the spec of a window's Run: a generic tty
// workload running the shell, with a state volume at /workspace, the
// profile's repository cloned under it, and luxmux's labels. secrets holds
// the values (nil for a validateOnly check, which needs only the names).
func RunSpec(p Profile, t Target, secrets map[string]string) (api.RunSpec, error) {
	if err := p.Validate(); err != nil {
		return api.RunSpec{}, err
	}
	sp := api.RunSpec{
		Name:   "luxmux-" + t.Session,
		Labels: map[string]string{},
		Image:  api.Image{Ref: p.Image},
		Workload: api.Workload{
			Adapter: "generic",
			TTY:     true,
			Command: DefaultShell,
			Workdir: p.Workdir,
		},
		Volumes: []api.Volume{{Name: "workspace", Path: Workspace, Kind: "state"}},
	}
	if p.HomeVolume() {
		sp.Volumes = append(sp.Volumes, api.Volume{Name: "home", Path: Home, Kind: "state"})
	}
	if len(p.Shell) > 0 {
		sp.Workload.Command = append([]string{}, p.Shell...)
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
	if len(p.Env) > 0 {
		sp.Env = map[string]string{}
		for k, v := range p.Env {
			sp.Env[k] = v
		}
	}
	for _, n := range p.SecretNames() {
		s := api.Secret{Name: n, Value: secrets[n]}
		if n != p.GitCredential {
			s.As = "env"
		}
		sp.Secrets = append(sp.Secrets, s)
	}
	if p.Repository != "" {
		name := RepoName(p.Repository)
		sp.Git = &api.Git{Repositories: []api.Repository{{
			Name:       name,
			URL:        p.Repository,
			Ref:        p.GitRef(),
			Credential: p.GitCredential,
		}}}
		if sp.Workload.Workdir == "" {
			sp.Workload.Workdir = Workspace + "/repos/" + name
		}
	}
	if sp.Workload.Workdir == "" {
		sp.Workload.Workdir = Workspace
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
