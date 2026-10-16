// Package config reads luxmux's configuration: the prefix key, direct key
// bindings and the profiles windows are created from.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"slices"
	"sort"
	"strings"

	"github.com/marcioapm/lux/api"
	"github.com/pelletier/go-toml/v2"
)

// Repository is one entry of a profile's repositories list.
type Repository struct {
	URL string `toml:"url" json:"url"`
	// Branch (or its alias Ref) is checked out; empty is the remote HEAD.
	Branch string `toml:"branch,omitempty" json:"branch,omitempty"`
	Ref    string `toml:"ref,omitempty" json:"ref,omitempty"`
	// Name defaults to RepoName(URL); Path to lux's /workspace/repos/<name>.
	Name string `toml:"name,omitempty" json:"name,omitempty"`
	Path string `toml:"path,omitempty" json:"path,omitempty"`
	// GitCredential overrides the profile's for this repository.
	GitCredential string `toml:"git_credential,omitempty" json:"gitCredential,omitempty"`
}

// Profile is how a window's Run is made.
type Profile struct {
	Image string `toml:"image,omitempty" json:"image,omitempty"`
	Pool  string `toml:"pool,omitempty" json:"pool,omitempty"`
	// Size names a size of a shaped pool; CPUs, Memory and Disk are for
	// unshaped pools. lux refuses both together.
	Size   string  `toml:"size,omitempty" json:"size,omitempty"`
	CPUs   float64 `toml:"cpus,omitempty" json:"cpus,omitempty"`
	Memory string  `toml:"memory,omitempty" json:"memory,omitempty"`
	Disk   string  `toml:"disk,omitempty" json:"disk,omitempty"`
	// Repository, Branch and Ref are the one-repository shorthand of
	// Repositories; a profile sets one form or the other.
	Repository string `toml:"repository,omitempty" json:"repository,omitempty"`
	// Branch (or its alias Ref) is checked out; empty is the remote HEAD.
	Branch        string            `toml:"branch,omitempty" json:"branch,omitempty"`
	Ref           string            `toml:"ref,omitempty" json:"ref,omitempty"`
	Repositories  []Repository      `toml:"repositories,omitempty" json:"repositories,omitempty"`
	GitCredential string            `toml:"git_credential,omitempty" json:"gitCredential,omitempty"`
	Shell         []string          `toml:"shell,omitempty" json:"shell,omitempty"`
	Env           map[string]string `toml:"env,omitempty" json:"env,omitempty"`
	Labels        map[string]string `toml:"labels,omitempty" json:"labels,omitempty"`
	Secrets       []string          `toml:"secrets,omitempty" json:"secrets,omitempty"`
	SecretsFrom   string            `toml:"secrets_from,omitempty" json:"secretsFrom,omitempty"`
	Workdir       string            `toml:"workdir,omitempty" json:"workdir,omitempty"`
	WarmSpare     bool              `toml:"warm_spare,omitempty" json:"warmSpare,omitempty"`
	// OnStart runs as a child sh -c of the window's start wrapper, before
	// it execs the shell, on the Run's first start; OnResume on each later
	// placement (see StartWrapper).
	OnStart  string `toml:"on_start,omitempty" json:"onStart,omitempty"`
	OnResume string `toml:"on_resume,omitempty" json:"onResume,omitempty"`
	// lux denies egress by default: Egress lists the hosts (or CIDRs) the
	// shell may reach, UnrestrictedNetwork lifts the filter.
	Egress              []string `toml:"egress,omitempty" json:"egress,omitempty"`
	UnrestrictedNetwork bool     `toml:"unrestricted_network,omitempty" json:"unrestrictedNetwork,omitempty"`
	// PersistHome puts /home/agent on a state volume. Unset means true
	// for luxmux's default image and false for any other.
	PersistHome *bool `toml:"persist_home,omitempty" json:"persistHome,omitempty"`
	// The lux the profile's Runs live on (see Endpoint); each one unset
	// is the lux CLI configuration's.
	LuxURL     string `toml:"lux_url,omitempty" json:"luxUrl,omitempty"`
	Tenant     string `toml:"tenant,omitempty" json:"tenant,omitempty"`
	APIKeyEnv  string `toml:"api_key_env,omitempty" json:"apiKeyEnv,omitempty"`
	APIKeyFile string `toml:"api_key_file,omitempty" json:"apiKeyFile,omitempty"`
	// APIKey is decoded only to be refused with a message: a key is never
	// kept in config.toml, and never written out from here.
	APIKey *string `toml:"api_key,omitempty" json:"-"`
}

// HomeVolume reports whether the profile's Runs get the home volume.
func (p Profile) HomeVolume() bool {
	if p.PersistHome != nil {
		return *p.PersistHome
	}
	return IsDefaultImage(p.Image)
}

// IsDefaultImage reports whether ref names luxmux's default image, at any
// tag or digest.
func IsDefaultImage(ref string) bool {
	if i := strings.Index(ref, "@"); i >= 0 {
		ref = ref[:i]
	}
	if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		ref = ref[:i]
	}
	return ref == ImageRepository
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
	// APIKey is the lux CLI's own top-level key, decoded only to be
	// refused as a profile's is.
	APIKey *string `toml:"api_key,omitempty"`
}

// inlineKeyRefusal is the message for an api_key anywhere in config.toml.
const inlineKeyRefusal = "api_key: a key is never kept in config.toml: set api_key_env (an environment variable holding it) or api_key_file (a file holding it, mode 0600)"

// ImageRepository is where luxmux's default image is published.
const ImageRepository = "ghcr.io/marcioapm/luxmux"

// DefaultImage is the image of the built-in profile used when the
// configuration defines none, and the one `config init` writes: the
// default image tagged with this binary's release version, :latest for
// any other build.
var DefaultImage = imageFromBuildInfo(debug.ReadBuildInfo)

func imageFromBuildInfo(read func() (*debug.BuildInfo, bool)) string {
	info, ok := read()
	if !ok || info == nil {
		return imageForVersion("")
	}
	return imageForVersion(info.Main.Version)
}

// imageForVersion maps a release version (v0.2.0) to its tag (0.2.0).
// Everything else maps to latest: "(devel)" and pseudo-versions have no
// image of their own, and pre-release binaries use latest even though the
// image workflow publishes :X.Y.Z-rc.N for an rc tag.
func imageForVersion(version string) string {
	if releaseRe.MatchString(version) {
		return ImageRepository + ":" + strings.TrimPrefix(version, "v")
	}
	return ImageRepository + ":latest"
}

var releaseRe = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`)

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
				return nil, unknownKeys(sm)
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

// unknownKeys names each key the configuration does not know by its path
// and line. The decoder's own message quotes the source line, which can
// hold a key value typed under a misspelt name.
func unknownKeys(sm *toml.StrictMissingError) error {
	errs := make([]error, 0, len(sm.Errors))
	for i := range sm.Errors {
		e := &sm.Errors[i]
		line, _ := e.Position()
		errs = append(errs, fmt.Errorf("line %d: unknown key %s", line, strings.Join(e.Key(), ".")))
	}
	return errors.Join(errs...)
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
	if c.APIKey != nil {
		errs = append(errs, errors.New(inlineKeyRefusal))
	}
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

// repoNameRe is lux's rule for a repository's name (a volume name).
var repoNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

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
	if len(p.Repositories) > 0 && (p.Repository != "" || p.Branch != "" || p.Ref != "") {
		fail("repositories: set either repositories or the repository/branch/ref shorthand, not both")
	}
	if len(p.RepositoryList()) == 0 && (p.Branch != "" || p.Ref != "" || p.GitCredential != "") {
		fail("branch, ref and git_credential need a repository")
	}
	if p.Repository != "" && RepoName(p.Repository) == "" {
		fail("repository: cannot derive a name from %q", p.Repository)
	}
	names := map[string]bool{}
	var paths []string
	for i, r := range p.Repositories {
		at := fmt.Sprintf("repositories[%d]", i)
		if r.URL == "" {
			fail("%s: url is required", at)
		}
		if r.Branch != "" && r.Ref != "" && r.Branch != r.Ref {
			fail("%s: branch and ref are the same setting: give one", at)
		}
		name := r.RepoName()
		if !repoNameRe.MatchString(name) {
			fail("%s: name %q: lowercase letters, digits, '_' and '-', at most 32 (set name)", at, name)
		} else if names[name] {
			fail("%s: name %q is taken by another repository (set name)", at, name)
		}
		names[name] = true
		dir := RepoDir(name)
		if r.Path != "" {
			dir = path.Clean(r.Path)
			if !strings.HasPrefix(dir, Workspace+"/") {
				fail("%s: path must be under %s, the state volume", at, Workspace)
			}
		}
		// lux's runner skips a checkout whose directory already holds a
		// .git, so two at one path, or one inside another, lose a clone
		// without an error.
		for j, other := range paths {
			switch {
			case dir == other:
				fail("%s: path %s is also repositories[%d]'s: each repository needs a directory of its own (set path)", at, dir, j)
			case strings.HasPrefix(dir, other+"/"), strings.HasPrefix(other, dir+"/"):
				fail("%s: path %s and repositories[%d]'s %s are one inside the other (set path)", at, dir, j, other)
			}
		}
		paths = append(paths, dir)
	}
	if p.Workdir != "" && !strings.HasPrefix(p.Workdir, "/") {
		fail("workdir must be absolute")
	}
	if p.APIKey != nil {
		fail(inlineKeyRefusal)
	}
	if p.APIKeyEnv != "" && !envNameRe.MatchString(p.APIKeyEnv) {
		fail("api_key_env: invalid environment variable name %q", p.APIKeyEnv)
	}
	if p.LuxURL != "" {
		if u, err := url.Parse(p.LuxURL); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			fail("lux_url: want an http(s) URL such as https://lux.example.com")
		} else if u.User != nil {
			fail("lux_url: no credentials in the URL: set api_key_env or api_key_file")
		}
	}
	for k := range p.Env {
		if !envNameRe.MatchString(k) || strings.HasPrefix(k, "LUX_") {
			fail("env: invalid name %q", k)
		} else if reservedEnv(k) {
			fail("env: %q is set by luxmux", k)
		}
	}
	for name, v := range map[string]string{"on_start": p.OnStart, "on_resume": p.OnResume} {
		if strings.ContainsRune(v, 0) {
			fail("%s: a command cannot contain a NUL byte", name)
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
	named := append(slices.Clone(p.Secrets), p.GitCredential)
	for _, r := range p.Repositories {
		named = append(named, r.GitCredential)
	}
	for _, s := range named {
		switch {
		case s != "" && !envNameRe.MatchString(s):
			fail("secrets: invalid name %q", s)
		case reservedEnv(s):
			fail("secrets: %q is set by luxmux", s)
		case reservedSecretNamespace(s):
			fail("secrets: %q: the LUX_ prefix is lux's (it sets LUX_RUN_ID, LUX_EPOCH and others in every Run): a secret of that name would replace its value", s)
		}
	}
	if n := envStringSize(EnvRepositories, windowEnv(p, Target{}, Workspace)[EnvRepositories]); n > MaxEnvString {
		fail("repositories: %s would be %d bytes, over the %d a Run's environment variable may hold: use fewer repositories or shorter urls, branches and paths", EnvRepositories, n, MaxEnvString)
	}
	return errors.Join(errs...)
}

// RepositoryList is the profile's repositories in either form, in order:
// the list, or the shorthand as a list of one.
func (p Profile) RepositoryList() []Repository {
	if len(p.Repositories) > 0 {
		return slices.Clone(p.Repositories)
	}
	if p.Repository == "" {
		return nil
	}
	return []Repository{{URL: p.Repository, Branch: p.Branch, Ref: p.Ref}}
}

// RepoRef is r's branch or ref ("" for the remote's HEAD).
func (r Repository) RepoRef() string {
	if r.Branch != "" {
		return r.Branch
	}
	return r.Ref
}

// RepoName is r's name in the Run: Name, else derived from its URL.
func (r Repository) RepoName() string {
	if r.Name != "" {
		return r.Name
	}
	return RepoName(r.URL)
}

// Credential is the secret r is fetched with: its own, else the profile's.
func (p Profile) Credential(r Repository) string {
	if r.GitCredential != "" {
		return r.GitCredential
	}
	return p.GitCredential
}

// CredentialNames are the git credentials the profile's repositories use,
// each once, in order.
func (p Profile) CredentialNames() []string {
	var out []string
	for _, r := range p.RepositoryList() {
		if c := p.Credential(r); c != "" && !slices.Contains(out, c) {
			out = append(out, c)
		}
	}
	return out
}

// GitRef is the branch or ref to check out ("" for the remote's HEAD).
func (p Profile) GitRef() string {
	if p.Branch != "" {
		return p.Branch
	}
	return p.Ref
}

// SecretNames are every secret the profile's Runs carry: its secrets and
// its repositories' git credentials.
func (p Profile) SecretNames() []string {
	var out []string
	seen := map[string]bool{}
	for _, s := range append(slices.Clone(p.Secrets), p.CredentialNames()...) {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// RepoDir is where lux clones a repository named name when it has no path.
func RepoDir(name string) string { return Workspace + "/repos/" + name }

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
