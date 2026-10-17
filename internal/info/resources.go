package info

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/marcioapm/lux-mux/internal/config"
	"github.com/marcioapm/lux-mux/internal/luxapi"
)

// ResourcesScript is the one command luxmux execs in a running Run to
// read what it uses (lux v0.1.30 has no usage endpoint): the cgroup v2
// files cpu.max, cpu.stat's usage_usec, memory.max and memory.current,
// and df -Pk of the state volume. It prints one "key value…" line each,
// a missing file as an empty value; an image without df prints no df
// line. POSIX sh, BusyBox's included; no timeout(1): the caller's
// ExecTimeout ends the exec, and lux's shim kills the command's process
// group when the exec's client goes away. The test-only variables point
// it at fixtures, as the start wrapper's do.
var ResourcesScript = `_lm_cg=${` + config.EnvTestCgroup + `:-/sys/fs/cgroup}
_lm_ws=${` + config.EnvTestWorkspace + `:-/workspace}
for _lm_f in cpu.max memory.max memory.current; do
	_lm_v=
	{ IFS= read -r _lm_v <"$_lm_cg/$_lm_f"; } 2>/dev/null
	printf '%s %s\n' "$_lm_f" "$_lm_v"
done
_lm_u=
{
	while read -r _lm_k _lm_x; do
		if [ "$_lm_k" = usage_usec ]; then
			_lm_u=$_lm_x
			break
		fi
	done <"$_lm_cg/cpu.stat"
} 2>/dev/null
printf 'cpu.usage_usec %s\n' "$_lm_u"
if command -v df >/dev/null 2>&1; then
	df -Pk "$_lm_ws" 2>/dev/null | {
		_lm_t='' _lm_n=''
		read -r _lm_x && read -r _lm_x _lm_t _lm_n _lm_x
		printf 'df %s %s\n' "$_lm_t" "$_lm_n"
	}
fi
exit 0
`

// ResourcesCommand is the exec command running ResourcesScript.
func ResourcesCommand() []string { return []string{"sh", "-c", ResourcesScript, "luxmux-resources"} }

// Reading is one run of ResourcesScript, parsed. A nil field: the Run
// reported nothing for it (no file, "max", not a number).
type Reading struct {
	CPULimit    *float64
	UsageUsec   *int64
	MemoryLimit *int64
	Memory      *int64
	DiskSize    *int64
	DiskUsed    *int64
}

// ParseReading reads ResourcesScript's output.
func ParseReading(out []byte) Reading {
	var r Reading
	for _, line := range strings.Split(string(out), "\n") {
		k, v, _ := strings.Cut(line, " ")
		f := strings.Fields(v)
		switch k {
		case "cpu.max":
			if len(f) == 2 {
				q, e1 := strconv.ParseInt(f[0], 10, 64)
				p, e2 := strconv.ParseInt(f[1], 10, 64)
				if e1 == nil && e2 == nil && q > 0 && p > 0 {
					c := float64(q) / float64(p)
					r.CPULimit = &c
				}
			}
		case "memory.max":
			r.MemoryLimit = count(f)
		case "memory.current":
			r.Memory = count(f)
		case "cpu.usage_usec":
			r.UsageUsec = count(f)
		case "df":
			if len(f) == 2 {
				if t := count(f[:1]); t != nil && *t > 0 {
					r.DiskSize = ptr(*t * 1024)
				}
				if u := count(f[1:]); u != nil {
					r.DiskUsed = ptr(*u * 1024)
				}
			}
		}
	}
	return r
}

func count(f []string) *int64 {
	if len(f) != 1 {
		return nil
	}
	n, err := strconv.ParseInt(f[0], 10, 64)
	if err != nil || n < 0 {
		return nil
	}
	return &n
}

func (r Reading) empty() bool {
	return r.CPULimit == nil && r.UsageUsec == nil && r.MemoryLimit == nil && r.Memory == nil && r.DiskSize == nil && r.DiskUsed == nil
}

// ErrBusy: an exec of this window's resources is still in flight.
var ErrBusy = errors.New("a resources read is in flight")

// Sampler reads resources, at most one exec in flight per window, and
// keeps each window's last cpu.stat sample: CPU in use is a rate, the
// usage between two readings over the time between them. It never waits
// for a second reading: until there is one, CPU in use is unknown.
type Sampler struct {
	mu       sync.Mutex
	inFlight map[string]bool
	last     map[string]cpuSample
}

type cpuSample struct {
	usec int64
	at   time.Time
}

// NewSampler is an empty Sampler.
func NewSampler() *Sampler {
	return &Sampler{inFlight: map[string]bool{}, last: map[string]cpuSample{}}
}

// Key names a placement: a Run moved to a new placement has a new cgroup,
// whose usage starts again.
func Key(runID string, epoch int) string { return runID + "#" + strconv.Itoa(epoch) }

// Read execs ResourcesCommand in Run id over c, within ExecTimeout and
// ctx, and returns what it read, the CPU rate from the previous reading
// of key when there is one. at is when the reading is dated. It returns
// ErrBusy while another Read of key is in flight, and an error when the
// exec fails or the Run reports nothing.
func (s *Sampler) Read(ctx context.Context, c Client, key, id string, now func() time.Time) (*Resources, error) {
	s.mu.Lock()
	if s.inFlight[key] {
		s.mu.Unlock()
		return nil, ErrBusy
	}
	s.inFlight[key] = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.inFlight, key)
		s.mu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(ctx, ExecTimeout)
	defer cancel()
	out, code, err := c.ExecOutput(ctx, id, ResourcesCommand(), 4096)
	if err != nil {
		return nil, err
	}
	if code != 0 {
		return nil, errors.New("the resources script exited with status " + strconv.Itoa(code))
	}
	r := ParseReading(out)
	if r.empty() {
		return nil, errNoSample
	}
	return s.resources(key, r, now()), nil
}

// resources is r with the CPU rate against key's previous sample, which
// r's becomes.
func (s *Sampler) resources(key string, r Reading, at time.Time) *Resources {
	out := &Resources{CPULimit: r.CPULimit, MemoryBytes: r.Memory, MemoryLimitBytes: r.MemoryLimit,
		DiskBytes: r.DiskUsed, DiskSizeBytes: r.DiskSize}
	if r.UsageUsec == nil {
		return out
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	prev, ok := s.last[key]
	s.last[key] = cpuSample{*r.UsageUsec, at}
	if ok {
		if dt := at.Sub(prev.at).Seconds(); dt > 0 && *r.UsageUsec >= prev.usec {
			rate := float64(*r.UsageUsec-prev.usec) / 1e6 / dt
			out.CPUs = &rate
		}
	}
	return out
}

// Forget drops every sample but those of keep's keys.
func (s *Sampler) Forget(keep ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k := range s.last {
		if !containsKey(keep, k) {
			delete(s.last, k)
		}
	}
}

func containsKey(l []string, k string) bool {
	for _, x := range l {
		if x == k {
			return true
		}
	}
	return false
}

// Dial is the client of a window's lux: each window is read on its own
// endpoint, never the lux CLI configuration's unless that is its own.
type Dial func(config.Endpoint) (Client, error)

// Endpoints is a Dial over eps's clients.
func Endpoints(eps interface {
	Client(config.Endpoint) (*luxapi.Client, error)
}) Dial {
	return func(ep config.Endpoint) (Client, error) {
		c, err := eps.Client(ep)
		if err != nil {
			return nil, err
		}
		return c, nil
	}
}

// Gather is WithResources on t's own lux (dial(t.Endpoint)); a lux whose
// key cannot be read now is NoKey.
func Gather(ctx context.Context, dial Dial, s *Sampler, t Target, now func() time.Time) Window {
	c, err := dial(t.Endpoint)
	if err != nil {
		return NoKey(t)
	}
	return WithResources(ctx, c, s, t, now)
}

// WithResources is Fetch, then, while the Run is running, a Read of its
// resources with s (nil s: none). A failed or busy Read leaves the
// section out.
func WithResources(ctx context.Context, c Client, s *Sampler, t Target, now func() time.Time) Window {
	w := Fetch(ctx, c, t, now)
	if s == nil || w.Problem != "" || w.Run == nil || w.Run.State != "running" {
		return w
	}
	epoch := 0
	if w.Placement != nil {
		epoch = w.Placement.Epoch
	}
	if r, err := s.Read(ctx, c, Key(w.RunID, epoch), w.RunID, now); err == nil {
		w.Resources = r
	}
	return w
}
