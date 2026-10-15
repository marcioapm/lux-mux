package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/pelletier/go-toml/v2"
)

// AppendProfile adds [profiles.NAME] to the configuration file, keeping
// what is in it (comments included). A profile of that name already there
// is an error: luxmux does not rewrite a user's file.
func AppendProfile(name string, p Profile) error {
	path, err := Path()
	if err != nil {
		return err
	}
	return AppendProfileTo(path, name, p)
}

// AppendProfileTo is AppendProfile for the file at path.
func AppendProfileTo(path, name string, p Profile) error {
	if !NameRe.MatchString(name) {
		return fmt.Errorf("profile name %q: letters, digits, '.', '_' and '-'", name)
	}
	if err := p.Validate(); err != nil {
		return err
	}
	old, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if len(old) > 0 {
		var raw Config
		if err := toml.Unmarshal(old, &raw); err != nil {
			return err
		}
		if _, ok := raw.Profiles[name]; ok {
			return fmt.Errorf("profile %q exists already", name)
		}
	}
	b, err := toml.Marshal(map[string]any{"profiles": map[string]Profile{name: p}})
	if err != nil {
		return err
	}
	out := append([]byte{}, old...)
	if len(out) > 0 && !bytes.HasSuffix(out, []byte("\n")) {
		out = append(out, '\n')
	}
	out = append(out, '\n')
	out = append(out, b...)
	if _, err := Parse(out); err != nil {
		return fmt.Errorf("the configuration would not parse: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, out, 0o600)
}
