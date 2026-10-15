// Package config reads luxmux's configuration: the prefix key, direct key
// bindings and the profiles windows are created from.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/marcioapm/lux/api"
	"github.com/pelletier/go-toml/v2"
)

// Profile is how a window's Run is made.
type Profile struct {
	Image string `toml:"image,omitempty" json:"image,omitempty"`
	Pool  string `toml:"pool,omitempty" json:"pool,omitempty"`
	// Size names a size of a shaped pool; CPUs, Memory and Disk are for
	// unshaped pools. lux refuses both together.
	Size       string  `toml:"size,omitempty" json:"size,omitempty"`
	CPUs       float64 `toml:"cpus,omitempty" json:"cpus,omitempty"`
	Memory     string  `toml:"memory,omitempty" json:"memory,omitempty"`
	Disk       string  `toml:"disk,omitempty" json:"disk,omitempty"`
	Repository string  `toml:"repository,omitempty" json:"repository,omitempty"`
	// Branch (or its alias Ref) is checked out; empty is the remote HEAD.
	Branch        string            `toml:"branch,omitempty" json:"branch,omitempty"`
	Ref           string            `toml:"ref,omitempty" json:"ref,omitempty"`
	GitCredential string            `toml:"git_credential,omitempty" json:"gitCredential,omitempty"`
	Shell         []string          `toml:"shell,omitempty" json:"shell,omitempty"`
	Env           map[string]string `toml:"env,omitempty" json:"env,omitempty"`
	Labels        map[string]string `toml:"labels,omitempty" json:"labels,omitempty"`
	Secrets       []string          `toml:"secrets,omitempty" json:"secrets,omitempty"`
	SecretsFrom   string            `toml:"secrets_from,omitempty" json:"secretsFrom,omitempty"`
	Workdir       string            `toml:"workdir,omitempty" json:"workdir,omitempty"`
	WarmSpare     bool              `toml:"warm_spare,omitempty" json:"warmSpare,omitempty"`
	// lux denies egress by default: Egress lists the hosts (or CIDRs) the
	// shell may reach, UnrestrictedNetwork lifts the filter.
	Egress              []string `toml:"egress,omitempty" json:"egress,omitempty"`
	UnrestrictedNetwork bool     `toml:"unrestricted_network,omitempty" json:"unrestrictedNetwork,omitempty"`
}

// Keys holds the optional bindings that work without the prefix.
type Keys struct {
	Direct map[string]string `toml:"direct,omitempty"`
}

// Config is $XDG_CONFIG_HOME/luxmux/config.toml.
type Config struct {
	Prefix         string             `toml:"prefix,omitempty"`
	DefaultProfile string             `toml:"default_profile,omitempty"`
	Keys           Keys               `toml:"keys,omitempty"`
	Profiles       map[string]Profile `toml:"profiles,omitempty"`
}

// DefaultImage is the image of the built-in profile used when the
// configuration defines none.
const DefaultImage = "ubuntu:24.04"

// DefaultShell starts bash as a login shell where the image has it, sh otherwise.
var DefaultShell = []string{"sh", "-c", "command -v bash >/dev/null && exec bash -l || exec sh -l"}

// Dir is luxmux's configuration directory.
func Dir() (string, error) {
	d, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "luxmux"), nil
}

// Path is the configuration file's path.
func Path() (string, error) {
	d, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "config.toml"), nil
}

// Load reads path; a missing file is the defaults.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Parse(nil)
	}
	if err != nil {
		return nil, err
	}
	c, err := Parse(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// Parse decodes a configuration, fills defaults and validates it.
func Parse(b []byte) (*Config, error) {
	var c Config
	if len(b) > 0 {
		d := toml.NewDecoder(bytes.NewReader(b))
		d.DisallowUnknownFields()
		if err := d.Decode(&c); err != nil {
			var sm *toml.StrictMissingError
			if errors.As(err, &sm) {
				return nil, errors.New(sm.String())
			}
			return nil, err
		}
	}
	if c.Prefix == "" {
		c.Prefix = "C-b"
	}
	if c.Profiles == nil {
		c.Profiles = map[string]Profile{}
	}
	if len(c.Profiles) == 0 {
		c.Profiles["default"] = Profile{Image: DefaultImage}
	}
	if c.DefaultProfile == "" {
		if _, ok := c.Profiles["default"]; ok {
			c.DefaultProfile = "default"
		} else {
			c.DefaultProfile = c.ProfileNames()[0]
		}
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// ProfileNames are the profiles' names, sorted.
func (c *Config) ProfileNames() []string {
	names := make([]string, 0, len(c.Profiles))
	for n := range c.Profiles {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Validate reports every problem at once.
func (c *Config) Validate() error {
	var errs []error
	if _, err := ParsePrefix(c.Prefix); err != nil {
		errs = append(errs, err)
	}
	if _, ok := c.Profiles[c.DefaultProfile]; !ok {
		errs = append(errs, fmt.Errorf("default_profile: no profile named %q", c.DefaultProfile))
	}
	for k, v := range c.Keys.Direct {
		if _, err := ParseKey(k); err != nil {
			errs = append(errs, fmt.Errorf("keys.direct: %w", err))
		}
		if _, err := ParseCommand(v); err != nil {
			errs = append(errs, fmt.Errorf("keys.direct %q: %w", k, err))
		}
	}
	for _, n := range c.ProfileNames() {
		if !NameRe.MatchString(n) {
			errs = append(errs, fmt.Errorf("profiles.%s: a name is letters, digits, '.', '_' and '-'", n))
		}
		if err := c.Profiles[n].Validate(); err != nil {
			errs = append(errs, fmt.Errorf("profiles.%s: %w", n, err))
		}
	}
	return errors.Join(errs...)
}

// NameRe is what session and profile names may be: they become file
// names and label values.
var NameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// labelKeyRe is lux's rule for a label key.
var labelKeyRe = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9._/-]{0,62}[A-Za-z0-9])?$`)

var envNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Validate checks what lux would refuse later, and the profile's own rules.
func (p Profile) Validate() error {
	var errs []error
	fail := func(f string, a ...any) { errs = append(errs, fmt.Errorf(f, a...)) }
	if p.Image == "" {
		fail("image is required")
	}
	if p.Size != "" && (p.CPUs != 0 || p.Memory != "" || p.Disk != "") {
		fail("size cannot be combined with cpus, memory or disk")
	}
	if p.CPUs < 0 {
		fail("cpus must be positive")
	}
	for name, v := range map[string]string{"memory": p.Memory, "disk": p.Disk} {
		if v == "" {
			continue
		}
		if _, err := api.ParseBytes(v); err != nil {
			fail("%s: %v", name, err)
		}
	}
	if p.Branch != "" && p.Ref != "" && p.Branch != p.Ref {
		fail("branch and ref are the same setting: give one")
	}
	if p.Repository == "" && (p.Branch != "" || p.Ref != "" || p.GitCredential != "") {
		fail("branch, ref and git_credential need a repository")
	}
	if p.Repository != "" && RepoName(p.Repository) == "" {
		fail("repository: cannot derive a name from %q", p.Repository)
	}
	if p.Workdir != "" && !strings.HasPrefix(p.Workdir, "/") {
		fail("workdir must be absolute")
	}
	for k := range p.Env {
		if !envNameRe.MatchString(k) || strings.HasPrefix(k, "LUX_") {
			fail("env: invalid name %q", k)
		}
	}
	for k, v := range p.Labels {
		if !labelKeyRe.MatchString(k) || len(v) > 256 {
			fail("labels: invalid label %q", k)
		}
		if k == "app" || strings.HasPrefix(k, "luxmux-") {
			fail("labels: %q is luxmux's own", k)
		}
	}
	for _, s := range append(append([]string{}, p.Secrets...), p.GitCredential) {
		if s != "" && !envNameRe.MatchString(s) {
			fail("secrets: invalid name %q", s)
		}
	}
	return errors.Join(errs...)
}

// GitRef is the branch or ref to check out ("" for the remote's HEAD).
func (p Profile) GitRef() string {
	if p.Branch != "" {
		return p.Branch
	}
	return p.Ref
}

// SecretNames are every secret the profile's Runs carry: its secrets and
// its git credential.
func (p Profile) SecretNames() []string {
	var out []string
	seen := map[string]bool{}
	for _, s := range append(append([]string{}, p.Secrets...), p.GitCredential) {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// RepoName is the repository's name in the Run: the URL's last path
// element without .git, lowercased into lux's volume-name alphabet.
func RepoName(url string) string {
	u := strings.TrimSuffix(strings.TrimRight(url, "/"), ".git")
	if i := strings.LastIndexAny(u, "/:"); i >= 0 {
		u = u[i+1:]
	}
	var b strings.Builder
	for _, r := range strings.ToLower(u) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		case r == '.':
			b.WriteRune('-')
		}
	}
	s := strings.TrimLeft(b.String(), "_-")
	if len(s) > 32 {
		s = s[:32]
	}
	return s
}
