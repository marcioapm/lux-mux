package config

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/marcioapm/lux/api"
)

const fakeKeyA = "lux_fake_key_for_tests_A"

func lookupOf(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

// TestInlineAPIKeyRefused: api_key in a profile is refused, the message
// naming api_key_env and api_key_file and never echoing the value.
func TestInlineAPIKeyRefused(t *testing.T) {
	_, err := Parse([]byte(`
[profiles.p]
image = "alpine"
api_key = "` + fakeKeyA + `"
`))
	if err == nil {
		t.Fatal("an inline api_key was accepted")
	}
	msg := err.Error()
	if !strings.Contains(msg, "api_key_env") || !strings.Contains(msg, "api_key_file") {
		t.Fatalf("message does not point to the two options: %s", msg)
	}
	if strings.Contains(msg, fakeKeyA) {
		t.Fatalf("message shows the key: %s", msg)
	}
}

// TestMisplacedAPIKeyNotEchoed: a key at the top level (the lux CLI's own
// layout) is refused as a profile's is, and a key under a misspelt name
// is reported by its path and line; neither error holds the value.
func TestMisplacedAPIKeyNotEchoed(t *testing.T) {
	for name, c := range map[string]struct{ src, want string }{
		"top level": {`api_key = "` + fakeKeyA + `"
[profiles.p]
image = "alpine"
`, "api_key_env"},
		"misspelt": {`[profiles.p]
image = "alpine"
lux_api_key = "` + fakeKeyA + `"
`, "line 3: unknown key profiles.p.lux_api_key"},
	} {
		_, err := Parse([]byte(c.src))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", name, err, c.want)
			continue
		}
		if strings.Contains(err.Error(), fakeKeyA) {
			t.Errorf("%s: the error shows the key: %s", name, err)
		}
	}
}

// TestEndpointFields: lux_url, tenant and the key sources parse and make
// the profile's Endpoint; unset ones are the lux CLI configuration's.
func TestEndpointFields(t *testing.T) {
	c, err := Parse([]byte(`
[profiles.a]
image = "alpine"
lux_url = "https://lux.example.com/"
tenant = "example"
api_key_env = "EXAMPLE_LUX_API_KEY"
[profiles.b]
image = "alpine"
`))
	if err != nil {
		t.Fatal(err)
	}
	base := api.Config{URL: "https://base.example.com", APIKey: "lux_fake_base", Tenant: "t0"}
	a := c.Profiles["a"].Endpoint(base)
	if a != (Endpoint{URL: "https://lux.example.com", Tenant: "example", KeyEnv: "EXAMPLE_LUX_API_KEY"}) {
		t.Fatalf("a: %+v", a)
	}
	b := c.Profiles["b"].Endpoint(base)
	if b != DefaultEndpoint(base) || b.URL != "https://base.example.com" || b.Tenant != "t0" {
		t.Fatalf("b: %+v", b)
	}
	if got := c.Endpoints(base); len(got) != 2 {
		t.Fatalf("endpoints %+v", got)
	}
	if a.Label() != "lux.example.com · tenant example" {
		t.Fatalf("label %q", a.Label())
	}
	for _, bad := range []string{`lux_url = "lux.example.com"`, `lux_url = "https://u:p@lux.example.com"`, `api_key_env = "1BAD"`} {
		if _, err := Parse([]byte("[profiles.p]\nimage = \"alpine\"\n" + bad + "\n")); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

// TestKeyFileMode: a key file group or others may read is refused with a
// message saying how to fix it; at 0600 it is read.
func TestKeyFileMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "example.key")
	writeFile(t, path, fakeKeyA+"\n")
	ep := Endpoint{URL: "https://lux.example.com", KeyFile: path}
	for _, mode := range []os.FileMode{0o640, 0o604, 0o644} {
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		_, err := ep.Config(api.Config{}, lookupOf(nil))
		if err == nil || !strings.Contains(err.Error(), "chmod 600") || strings.Contains(err.Error(), fakeKeyA) {
			t.Fatalf("mode %#o: %v", mode, err)
		}
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := ep.Config(api.Config{}, lookupOf(nil))
	if err != nil || cfg.APIKey != fakeKeyA {
		t.Fatalf("0600: %v %q", err, cfg.APIKey)
	}
}

// TestKeyPrecedence: api_key_env when the environment has it, else
// api_key_file, else (neither configured) the lux CLI's key; a configured
// source that has no key is an error, not a fall back to the CLI's.
func TestKeyPrecedence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "k")
	writeFile(t, path, "lux_fake_from_file\n")
	base := api.Config{URL: "https://lux.example.com", APIKey: "lux_fake_base"}
	env := lookupOf(map[string]string{"K": "lux_fake_from_env"})
	cases := []struct {
		ep      Endpoint
		env     func(string) (string, bool)
		want    string
		wantErr string
	}{
		{Endpoint{URL: base.URL, KeyEnv: "K", KeyFile: path}, env, "lux_fake_from_env", ""},
		{Endpoint{URL: base.URL, KeyEnv: "K", KeyFile: path}, lookupOf(nil), "lux_fake_from_file", ""},
		{Endpoint{URL: base.URL, KeyFile: path}, env, "lux_fake_from_file", ""},
		{Endpoint{URL: base.URL}, env, "lux_fake_base", ""},
		{Endpoint{URL: base.URL, KeyEnv: "K"}, lookupOf(nil), "", "api_key_env K is not set"},
		{Endpoint{URL: base.URL, KeyFile: path + ".missing"}, env, "", "api_key_file"},
		{Endpoint{}, env, "", "no url"},
	}
	for i, c := range cases {
		cfg, err := c.ep.Config(base, c.env)
		switch {
		case c.wantErr != "":
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("case %d: err %v, want %q", i, err, c.wantErr)
			}
		case err != nil || cfg.APIKey != c.want || cfg.URL != base.URL:
			t.Errorf("case %d: %v %q, want %q", i, err, cfg.APIKey, c.want)
		}
	}
}

// TestKeyFileKinds: a key file is read only when it is a regular file of
// this user's: a FIFO is refused at once (no wait for a writer), and so
// are a directory and a file of another uid. A symlink is followed and
// its target checked.
func TestKeyFileKinds(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "fifo.key")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := ReadKeyFile(fifo); done <- err }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("fifo: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("reading a FIFO key file blocked")
	}
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadKeyFile(sub); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("directory: %v", err)
	}

	private := filepath.Join(dir, "private.key")
	writeFile(t, private, fakeKeyA+"\n")
	link := filepath.Join(dir, "link.key")
	if err := os.Symlink(private, link); err != nil {
		t.Fatal(err)
	}
	if k, err := ReadKeyFile(link); err != nil || k != fakeKeyA {
		t.Fatalf("symlink to a 0600 file: %q %v", k, err)
	}
	shared := filepath.Join(dir, "shared.key")
	writeFile(t, shared, fakeKeyA+"\n")
	if err := os.Chmod(shared, 0o644); err != nil {
		t.Fatal(err)
	}
	link2 := filepath.Join(dir, "link2.key")
	if err := os.Symlink(shared, link2); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadKeyFile(link2); err == nil || !strings.Contains(err.Error(), "chmod 600") || strings.Contains(err.Error(), fakeKeyA) {
		t.Fatalf("symlink to a 0644 file: %v", err)
	}

	// The file is this process's; another user's uid is refused.
	defer func(f func() int) { currentUID = f }(currentUID)
	uid := os.Getuid()
	currentUID = func() int { return uid + 1 }
	if _, err := ReadKeyFile(private); err == nil || !strings.Contains(err.Error(), "not by this user") || strings.Contains(err.Error(), fakeKeyA) {
		t.Fatalf("another user's file: %v", err)
	}
}

// TestEndpointIDOmitsKey: an endpoint's identity is its URL, tenant and
// key source names: the same for any key value.
func TestEndpointIDOmitsKey(t *testing.T) {
	a := Endpoint{URL: "https://lux.example.com", KeyEnv: "K"}
	b := Endpoint{URL: "https://lux.example.com", KeyEnv: "J"}
	if a.ID() == b.ID() || a.ID() != (Endpoint{URL: "https://lux.example.com", KeyEnv: "K"}).ID() {
		t.Fatal("ID does not follow the key source")
	}
}
