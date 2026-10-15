package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAppendProfileKeepsTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "luxmux", "config.toml")
	if err := AppendProfileTo(path, "first", Profile{Image: "alpine"}); err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, "# mine\nprefix = \"C-a\"\n"+mustRead(t, path))
	if err := AppendProfileTo(path, "second", Profile{Image: "ubuntu", Repository: "https://e.com/r.git", Branch: "dev", CPUs: 2,
		Env: map[string]string{"A": "1"}, Labels: map[string]string{"team": "x"}}); err != nil {
		t.Fatal(err)
	}
	src := mustRead(t, path)
	if !strings.HasPrefix(src, "# mine\n") {
		t.Fatalf("comments lost:\n%s", src)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Prefix != "C-a" || c.Profiles["first"].Image != "alpine" || c.Profiles["second"].GitRef() != "dev" || c.Profiles["second"].CPUs != 2 || c.Profiles["second"].Env["A"] != "1" || c.Profiles["second"].Labels["team"] != "x" {
		t.Fatalf("%+v", c)
	}
	if err := AppendProfileTo(path, "second", Profile{Image: "x"}); err == nil {
		t.Fatal("duplicate name accepted")
	}
	if err := AppendProfileTo(path, "bad name", Profile{Image: "x"}); err == nil {
		t.Fatal("bad name accepted")
	}
}

func mustRead(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
