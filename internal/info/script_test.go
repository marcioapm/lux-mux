package info

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/marcioapm/lux-mux/internal/config"
)

// scriptShells are /bin/sh and, when installed, BusyBox's sh.
func scriptShells(t *testing.T) map[string][]string {
	shells := map[string][]string{"sh": {"/bin/sh"}}
	if b, err := exec.LookPath("busybox"); err == nil {
		shells["busybox"] = []string{b, "sh"}
	} else {
		t.Log("busybox unavailable: only /bin/sh runs")
	}
	return shells
}

// runScript runs ResourcesScript under shell with the cgroup files in
// cg (nil: no directory), and PATH holding only sh tools and, when df is
// not "", a df printing it (df "-": a df that fails).
func runScript(t *testing.T, shell []string, cg map[string]string, df string) string {
	t.Helper()
	dir := t.TempDir()
	cgDir := filepath.Join(dir, "cg")
	if cg != nil {
		if err := os.Mkdir(cgDir, 0o755); err != nil {
			t.Fatal(err)
		}
		for name, body := range cg {
			p := filepath.Join(cgDir, name)
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	switch df {
	case "":
	case "-":
		writeTool(t, bin, "df", "#!/bin/sh\nexit 1\n")
	default:
		writeTool(t, bin, "df", "#!/bin/sh\n[ \"$1\" = -Pk ] || exit 2\nprintf '%s\\n' 'Filesystem 1024-blocks Used Available Capacity Mounted on' '"+df+"'\n")
	}
	ws := filepath.Join(dir, "workspace")
	if err := os.Mkdir(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	args := append(append([]string{}, shell[1:]...), ResourcesCommand()[1:]...)
	cmd := exec.Command(shell[0], args...)
	cmd.Env = []string{"PATH=" + bin, config.EnvTestCgroup + "=" + cgDir, config.EnvTestWorkspace + "=" + ws}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	return string(out)
}

func writeTool(t *testing.T, bin, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestResourcesScript(t *testing.T) {
	numeric := map[string]string{"cpu.max": "200000 100000\n", "memory.max": "8589934592\n", "memory.current": "1073741824\n",
		"cpu.stat": "nr_periods 0\nusage_usec 123456\nuser_usec 100000\n"}
	unlimited := map[string]string{"cpu.max": "max 100000\n", "memory.max": "max\n", "memory.current": "4096\n", "cpu.stat": "usage_usec 9\n"}
	for name, shell := range scriptShells(t) {
		t.Run(name, func(t *testing.T) {
			// A BusyBox built to prefer its applets runs its own df whatever
			// PATH holds (Ubuntu 26.04's does; 24.04's runs PATH's): its df
			// line is then busybox df of the same directory.
			applet := name == "busybox"
			out := runScript(t, shell, numeric, "/dev/x 41943040 10485760 31457280 25% /workspace")
			r := ParseReading([]byte(out))
			if r.CPULimit == nil || *r.CPULimit != 2 || *r.MemoryLimit != 8<<30 || *r.Memory != 1<<30 || *r.UsageUsec != 123456 {
				t.Fatalf("numeric: %q", out)
			}
			fake := r.DiskSize != nil && r.DiskUsed != nil && *r.DiskSize == 40<<30 && *r.DiskUsed == 10<<30
			if applet && !fake {
				if r.DiskSize == nil || r.DiskUsed == nil || *r.DiskSize != busyboxDiskSize(t, shell[0]) || *r.DiskUsed > *r.DiskSize {
					t.Fatalf("busybox df: %q", out)
				}
			} else if !fake {
				t.Fatalf("df: %q", out)
			}
			out = runScript(t, shell, unlimited, "")
			r = ParseReading([]byte(out))
			if r.CPULimit != nil || r.MemoryLimit != nil || *r.Memory != 4096 || *r.UsageUsec != 9 || (!applet && r.DiskSize != nil) {
				t.Fatalf("max, no df: %q", out)
			}
			out = runScript(t, shell, nil, "-")
			r = ParseReading([]byte(out))
			r.DiskSize, r.DiskUsed = nil, nil
			if !r.empty() || !strings.Contains(out, "cpu.max \n") || !strings.Contains(out, "cpu.usage_usec \n") {
				t.Fatalf("missing files: %q", out)
			}
			if !applet && !strings.Contains(out, "df  \n") {
				t.Fatalf("failing df: %q", out)
			}
		})
	}
}

// TestResourcesScriptCgroupV1: on a cgroup v1 layout (no v2 file) the
// script reads the v1 controllers' files into the same lines: a quota
// over a period, -1 and the page-rounded 2^63-1 as no limit, and
// cpuacct.usage's nanoseconds as usec. A v2 file present wins over v1.
func TestResourcesScriptCgroupV1(t *testing.T) {
	limited := map[string]string{"cpu/cpu.cfs_quota_us": "150000\n", "cpu/cpu.cfs_period_us": "100000\n",
		"memory/memory.limit_in_bytes": "2147483648\n", "memory/memory.usage_in_bytes": "1048576\n",
		"cpuacct/cpuacct.usage": "123456789012\n", "cpu/cpu.stat": "nr_periods 3\nnr_throttled 0\n"}
	unlimited := map[string]string{"cpu/cpu.cfs_quota_us": "-1\n", "cpu/cpu.cfs_period_us": "100000\n",
		"memory/memory.limit_in_bytes": "9223372036854771712\n", "memory/memory.usage_in_bytes": "4096\n",
		"cpuacct/cpuacct.usage": "999\n"}
	both := map[string]string{"cpu.max": "200000 100000\n", "memory.max": "8589934592\n", "memory.current": "1073741824\n",
		"cpu.stat": "usage_usec 77\n"}
	for k, v := range limited {
		both[k] = v
	}
	for name, shell := range scriptShells(t) {
		t.Run(name, func(t *testing.T) {
			out := runScript(t, shell, limited, "-")
			r := ParseReading([]byte(out))
			if r.CPULimit == nil || *r.CPULimit != 1.5 || r.MemoryLimit == nil || *r.MemoryLimit != 2<<30 ||
				r.Memory == nil || *r.Memory != 1<<20 || r.UsageUsec == nil || *r.UsageUsec != 123456789 {
				t.Fatalf("v1 limited: %q → %+v", out, r)
			}
			out = runScript(t, shell, unlimited, "-")
			r = ParseReading([]byte(out))
			if r.CPULimit != nil || r.MemoryLimit != nil || r.Memory == nil || *r.Memory != 4096 || r.UsageUsec == nil || *r.UsageUsec != 0 {
				t.Fatalf("v1 unlimited: %q → %+v", out, r)
			}
			out = runScript(t, shell, both, "-")
			r = ParseReading([]byte(out))
			if r.CPULimit == nil || *r.CPULimit != 2 || *r.MemoryLimit != 8<<30 || *r.Memory != 1<<30 || *r.UsageUsec != 77 {
				t.Fatalf("v2 beside v1: %q → %+v", out, r)
			}
			// A v1 quota without its period is no reading, not half a line.
			out = runScript(t, shell, map[string]string{"cpu/cpu.cfs_quota_us": "50000\n"}, "-")
			if !strings.Contains(out, "cpu.max \n") {
				t.Fatalf("quota alone: %q", out)
			}
		})
	}
}

// busyboxDiskSize is busybox df -Pk's size, in bytes, of the filesystem
// holding the test's temporary directories.
func busyboxDiskSize(t *testing.T, bb string) int64 {
	t.Helper()
	out, err := exec.Command(bb, "df", "-Pk", t.TempDir()).Output()
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	f := strings.Fields(lines[len(lines)-1])
	n, err := strconv.ParseInt(f[1], 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return n * 1024
}

func TestResourcesScriptShellcheck(t *testing.T) {
	sc, err := exec.LookPath("shellcheck")
	if err != nil {
		t.Skip("shellcheck unavailable")
	}
	cmd := exec.Command(sc, "-s", "sh", "-")
	cmd.Stdin = strings.NewReader(ResourcesScript)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("shellcheck: %v\n%s", err, out)
	}
}
