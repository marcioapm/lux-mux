package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ResolveSecrets finds the value of each of the profile's secrets as
// `lux run` does for a secret without a value: in the secrets_from .env
// file, else in the environment variable of the same name. A secret found
// nowhere is an error naming it; values never appear in errors.
func (p Profile) ResolveSecrets(lookup func(string) (string, bool)) (map[string]string, error) {
	names := p.SecretNames()
	if len(names) == 0 {
		return nil, nil
	}
	fileVals := map[string]string{}
	if p.SecretsFrom != "" {
		path := p.SecretsFrom
		if strings.HasPrefix(path, "~/") {
			if home, err := os.UserHomeDir(); err == nil {
				path = filepath.Join(home, path[2:])
			}
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("secrets_from: %w", err)
		}
		fileVals = ParseDotenv(string(b))
	}
	out := map[string]string{}
	var missing []string
	for _, n := range names {
		if v, ok := fileVals[n]; ok {
			out[n] = v
		} else if v, ok := lookup(n); ok {
			out[n] = v
		} else {
			missing = append(missing, n)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("secret values not found in secrets_from or the environment: %s", strings.Join(missing, ", "))
	}
	return out, nil
}

// ParseDotenv reads KEY=VALUE lines as `lux run --secrets-from` does:
// blank lines and # comments skipped, an optional "export ", and the
// value's surrounding quotes trimmed.
func ParseDotenv(s string) map[string]string {
	out := map[string]string{}
	for _, l := range strings.Split(s, "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		k, v, ok := strings.Cut(strings.TrimPrefix(l, "export "), "=")
		if ok {
			out[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	return out
}
