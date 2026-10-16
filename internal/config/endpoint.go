package config

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/marcioapm/lux/api"
)

// Endpoint is a lux a window lives on: its URL, its tenant, and where its
// API key is read from, never the key itself. Windows with equal
// Endpoints share one client and one event stream.
type Endpoint struct {
	URL    string `json:"url"`
	Tenant string `json:"tenant,omitempty"`
	// KeyEnv and KeyFile are the profile's api_key_env and api_key_file;
	// with neither, the key is the lux CLI configuration's.
	KeyEnv  string `json:"keyEnv,omitempty"`
	KeyFile string `json:"keyFile,omitempty"`
}

// Endpoint is the lux p's Runs live on: each of lux_url and tenant p
// leaves unset is base's (api.LoadConfig()).
func (p Profile) Endpoint(base api.Config) Endpoint {
	return Endpoint{
		URL:     strings.TrimRight(cmp.Or(p.LuxURL, base.URL), "/"),
		Tenant:  cmp.Or(p.Tenant, base.Tenant),
		KeyEnv:  p.APIKeyEnv,
		KeyFile: expandHome(p.APIKeyFile),
	}
}

// DefaultEndpoint is the lux CLI configuration's own endpoint.
func DefaultEndpoint(base api.Config) Endpoint { return Profile{}.Endpoint(base) }

// Endpoints are the distinct endpoints of the configured profiles, in a
// stable order.
func (c *Config) Endpoints(base api.Config) []Endpoint {
	seen := map[Endpoint]bool{}
	var out []Endpoint
	for _, n := range c.ProfileNames() {
		if e := c.Profiles[n].Endpoint(base); !seen[e] {
			seen[e] = true
			out = append(out, e)
		}
	}
	SortEndpoints(out)
	return out
}

// SortEndpoints orders endpoints by URL, tenant and key source.
func SortEndpoints(l []Endpoint) {
	sort.SliceStable(l, func(i, j int) bool { return l[i].ID() < l[j].ID() })
}

// ID is a short stable name of e for maps and files; it hashes only the
// URL, tenant and key source names.
func (e Endpoint) ID() string {
	sum := sha256.Sum256([]byte(e.URL + "\x00" + e.Tenant + "\x00" + e.KeyEnv + "\x00" + e.KeyFile))
	return hex.EncodeToString(sum[:6])
}

// Host is e's URL without its scheme, for the screen.
func (e Endpoint) Host() string {
	if u, err := url.Parse(e.URL); err == nil && u.Host != "" {
		return u.Host + strings.TrimRight(u.Path, "/")
	}
	if e.URL == "" {
		return "(no lux url)"
	}
	return e.URL
}

// Label is e's URL host and tenant in one line: lux.example.com · tenant t.
func (e Endpoint) Label() string {
	s := e.Host()
	if e.Tenant != "" {
		s += " · tenant " + e.Tenant
	}
	return s
}

// KeySource says where e's key is read from, without the key.
func (e Endpoint) KeySource() string {
	switch {
	case e.KeyEnv != "" && e.KeyFile != "":
		return fmt.Sprintf("$%s, else %s", e.KeyEnv, e.KeyFile)
	case e.KeyEnv != "":
		return "$" + e.KeyEnv
	case e.KeyFile != "":
		return e.KeyFile
	}
	return "the lux CLI configuration"
}

// String is Label and KeySource: what errors name e by.
func (e Endpoint) String() string { return e.Label() + " (key: " + e.KeySource() + ")" }

// SpareSlot is the session's warm-spare slot for a spec key on ep: the
// spec key itself on the default endpoint def, else the key and ep's ID,
// so equal specs on two luxes keep a spare each.
func SpareSlot(specKey string, ep, def Endpoint) string {
	if specKey == "" || ep == def {
		return specKey
	}
	return specKey + "@" + ep.ID()
}

// Config is the client configuration for e: the key from KeyEnv when the
// environment has it, else from KeyFile, else (neither set) base's.
// Errors name where the key was looked for, never a value.
func (e Endpoint) Config(base api.Config, lookup func(string) (string, bool)) (api.Config, error) {
	cfg := api.Config{URL: e.URL, Tenant: e.Tenant}
	if e.URL == "" {
		return cfg, errors.New("not signed in to lux: no url: set lux_url in the profile, url in the lux CLI's config.toml, or LUX_URL")
	}
	switch {
	case e.KeyEnv != "":
		if v, ok := lookup(e.KeyEnv); ok && strings.TrimSpace(v) != "" {
			cfg.APIKey = strings.TrimSpace(v)
			return cfg, nil
		}
		if e.KeyFile == "" {
			return cfg, fmt.Errorf("lux %s: api_key_env %s is not set", e.Label(), e.KeyEnv)
		}
		fallthrough
	case e.KeyFile != "":
		k, err := ReadKeyFile(e.KeyFile)
		if err != nil {
			return cfg, fmt.Errorf("lux %s: %w", e.Label(), err)
		}
		cfg.APIKey = k
		return cfg, nil
	}
	cfg.APIKey = base.APIKey
	if cfg.APIKey == "" {
		return cfg, fmt.Errorf("not signed in to lux %s: set api_key in the lux CLI's config.toml or LUX_API_KEY, or api_key_env or api_key_file in the profile", e.Label())
	}
	return cfg, nil
}

// keyFileLimit bounds what is read of a key file.
const keyFileLimit = 4096

// currentUID is the user a key file must belong to.
var currentUID = os.Getuid

// ReadKeyFile reads an API key file: its first line, trimmed. The file is
// opened once and checked through that descriptor, so what is checked is
// what is read. It must be a regular file owned by this user that group
// and others cannot read, as ssh requires of a private key. A symlink is
// followed: its target is what is checked. O_NONBLOCK keeps the open of
// a FIFO from waiting for a writer; the type check then refuses it.
func ReadKeyFile(path string) (string, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", fmt.Errorf("api_key_file: %w", err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("api_key_file: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("api_key_file %s: not a regular file (%s)", path, fi.Mode().Type())
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		return "", fmt.Errorf("api_key_file %s: mode %#o lets group or others read it; chmod 600 %s", path, perm, path)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != currentUID() {
		return "", fmt.Errorf("api_key_file %s: owned by uid %d, not by this user (uid %d)", path, st.Uid, currentUID())
	}
	b, err := io.ReadAll(io.LimitReader(f, keyFileLimit))
	if err != nil {
		return "", fmt.Errorf("api_key_file: %w", err)
	}
	k, _, _ := strings.Cut(string(b), "\n")
	if k = strings.TrimSpace(k); k == "" {
		return "", fmt.Errorf("api_key_file %s: empty", path)
	}
	return k, nil
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[2:])
		}
	}
	return p
}
