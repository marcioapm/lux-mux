package info

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/marcioapm/lux/api"

	"github.com/marcioapm/lux-mux/internal/config"
	"github.com/marcioapm/lux-mux/internal/luxapi"
)

// Client is what gathering asks of a window's lux; *luxapi.Client is one.
type Client interface {
	Get(ctx context.Context, id string) (*api.Run, error)
	Cost(ctx context.Context, id string) (*luxapi.RunCost, error)
	ExecOutput(ctx context.Context, id string, command []string, limit int) ([]byte, int, error)
}

// Timeouts: the Run's GET and its cost share FetchTimeout; the resources
// exec has ExecTimeout of its own.
const (
	FetchTimeout = 5 * time.Second
	ExecTimeout  = 3 * time.Second
)

// fetchTimeout is FetchTimeout; tests shorten it.
var fetchTimeout = FetchTimeout

// Fetch reads t's Run and its cost from c, within ctx and FetchTimeout. A Run lux cannot
// be asked for (it does not answer, it refuses the key, it has no such
// Run) is Base(t) with Problem and Error set. A cost lux cannot answer is
// left out. now is when the answer is dated.
func Fetch(ctx context.Context, c Client, t Target, now func() time.Time) Window {
	w := Base(t)
	if t.RunID == "" {
		w.Problem, w.Error = ProblemNotFound, "the window has no Run yet"
		return w
	}
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	r, err := c.Get(ctx, t.RunID)
	if err != nil {
		w.Problem, w.Error = Classify(err, t.Endpoint)
		return w
	}
	cost, err := c.Cost(ctx, t.RunID)
	if err != nil {
		cost = nil
	}
	return FromRun(w, r, cost, now())
}

// Classify is a failed request as a Problem and its one line. The line
// names the lux by host and tenant, never by its key or key source.
func Classify(err error, ep config.Endpoint) (problem, line string) {
	switch {
	case luxapi.Refused(err):
		return ProblemRefused, "lux refused the key (" + ep.Label() + ")"
	case luxapi.IsStatus(err, http.StatusNotFound):
		return ProblemNotFound, "lux has no such Run (" + ep.Label() + ")"
	}
	return ProblemUnreachable, "lux unreachable (" + ep.Label() + ")"
}

// NoKey is the Window for a target whose lux key cannot be read now.
func NoKey(t Target) Window {
	w := Base(t)
	w.Problem, w.Error = ProblemNoKey, "no API key for this lux now ("+t.Endpoint.Label()+")"
	return w
}

// FromRun is base filled from lux's Run r and its cost (nil: none), as
// of now. It is pure: each field comes from the api.Run field named in
// its comment, and is left out when lux leaves that zero.
func FromRun(base Window, r *api.Run, cost *luxapi.RunCost, now time.Time) Window {
	w := base
	at := now.UTC()
	w.FetchedAt = &at
	w.Problem, w.Error = "", ""
	if r.ID != "" {
		w.RunID = r.ID
	}
	if l := r.Labels[config.LabelSession]; l != "" && l != w.Session {
		w.SessionLabel = l
	}
	if w.Profile == "" {
		w.Profile = r.Labels[config.LabelProfile]
	}
	w.Run = runOf(r)
	w.Image = imageOf(r)
	w.Placement = placementOf(r)
	w.Timing = timingOf(r, now)
	w.Workload = workloadOf(r)
	w.Egress = egressOf(r)
	// Run.Secrets are references (name, fingerprint): only the names.
	for _, s := range r.Secrets {
		if s.Name != "" {
			w.Secrets = append(w.Secrets, s.Name)
		}
	}
	slices.Sort(w.Secrets)
	w.Cost = costOf(cost)
	w.Resume = resumeOf(r)
	w.Resources = nil
	return w
}

// UserLabels are a Run's labels without luxmux's own (app=luxmux and every
// luxmux-* key): those are the window's session, profile, spare and
// resources, shown as fields of their own.
func UserLabels(l map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range l {
		if strings.HasPrefix(k, "luxmux-") || (k == config.LabelApp && v == config.AppName) {
			continue
		}
		out[k] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func runOf(r *api.Run) *Run {
	out := &Run{Name: r.Name, State: r.State, StateReason: r.StateReason, Labels: UserLabels(r.Labels)}
	if !r.CreatedAt.IsZero() {
		t := r.CreatedAt.UTC()
		out.CreatedAt = &t
	}
	if s := r.SubmittedBy; s != nil {
		key := s.KeyName
		if key == "" {
			key = s.KeyID
		}
		if key != "" || s.Email != "" {
			out.SubmittedBy = &Submitter{Key: key, Email: s.Email, Revoked: s.Revoked}
		}
	}
	if out.Name == "" && out.State == "" && out.Labels == nil && out.SubmittedBy == nil && out.CreatedAt == nil {
		return nil
	}
	return out
}

func imageOf(r *api.Run) *Image {
	out := &Image{Ref: r.Spec.Image.Ref}
	// Run.Image is set for a built image only: lux v0.1.30 reports no
	// digest for a pulled ref.
	if r.Image != nil {
		out.Resolved = r.Image.ImageID
	}
	if *out == (Image{}) {
		return nil
	}
	return out
}

func placementOf(r *api.Run) *Placement {
	out := &Placement{Pool: r.Pool, Host: r.Host, Epoch: r.Epoch}
	if r.Epoch > 0 {
		moves := r.Epoch - 1
		out.Moves = &moves
	}
	if s := r.Sizing; s != nil {
		if q := s.Requested; q != nil {
			a := &Resources{Size: q.Size, CPUs: q.CPUs}
			if q.Memory != nil {
				a.MemoryBytes = ptr(int64(*q.Memory))
			}
			if q.Disk != nil {
				a.DiskBytes = ptr(int64(*q.Disk))
			}
			out.Asked = nonEmpty(a)
		}
		if v := s.Resolved; v != nil {
			out.Size = v.Size
			out.Effective = nonEmpty(&Resources{CPUs: pos(v.CPUs), MemoryBytes: posBytes(v.Memory), DiskBytes: posBytes(v.Disk)})
		}
	}
	if out.Asked == nil {
		// An unshaped pool records no request: luxmux's own label has it.
		out.Asked = askedLabel(r.Labels[config.LabelResources])
	}
	if out.Effective == nil {
		res := r.Spec.Resources
		out.Effective = nonEmpty(&Resources{CPUs: pos(res.CPUs), MemoryBytes: posBytes(res.Memory), DiskBytes: posBytes(res.Disk)})
	}
	if out.Pool == "" && out.Size == "" && out.Asked == nil && out.Effective == nil && out.Host == "" && out.Epoch == 0 {
		return nil
	}
	return out
}

func askedLabel(v string) *Resources {
	if v == "" {
		return nil
	}
	var p config.Profile
	if !config.ApplyResourcesLabel(&p, v) {
		return nil
	}
	a := &Resources{Size: p.Size, CPUs: pos(p.CPUs)}
	if b, err := api.ParseBytes(p.Memory); err == nil && p.Memory != "" {
		a.MemoryBytes = posBytes(b)
	}
	if b, err := api.ParseBytes(p.Disk); err == nil && p.Disk != "" {
		a.DiskBytes = posBytes(b)
	}
	return nonEmpty(a)
}

func timingOf(r *api.Run, now time.Time) *Timing {
	out := &Timing{Stage: r.Stage, StageReason: r.StageReason}
	if !r.StageSince.IsZero() {
		t := r.StageSince.UTC()
		out.StageSince = &t
	}
	if r.RuntimeSince != nil && !r.RuntimeSince.IsZero() {
		t := r.RuntimeSince.UTC()
		out.PlacementSince = &t
		if d := now.Sub(t).Seconds(); d >= 0 {
			out.UptimeSeconds = &d
		}
	}
	out.RuntimeSeconds = pos(r.RuntimeSeconds)
	out.WaitingSeconds = pos(r.PlacementWaitSeconds)
	out.StartingSeconds = pos(r.PlacementStartSeconds)
	if *out == (Timing{}) {
		return nil
	}
	return out
}

func workloadOf(r *api.Run) *Workload {
	out := &Workload{Workdir: r.Spec.Workload.Workdir}
	if g := r.Spec.Git; g != nil {
		for _, rp := range g.Repositories {
			out.Repositories = append(out.Repositories, Repository{Name: rp.Name, URL: PlainURL(rp.URL), Ref: rp.Ref, Path: rp.Path})
		}
	}
	if out.Workdir == "" && len(out.Repositories) == 0 {
		return nil
	}
	return out
}

// PlainURL is u without credentials in it. A password never stays; an
// SSH URL (ssh://, git+ssh://, ssh+git://) keeps its login, which is not
// a secret. A u url.Parse refuses loses everything of its authority up to
// the last "@"; one that still holds an "@" after that (a credential with
// a "/" in it) is not shown at all. A u without "://" is scp-style: see
// plainSCP.
func PlainURL(u string) string {
	scheme, rest, ok := strings.Cut(u, "://")
	if !ok {
		return plainSCP(u)
	}
	ssh := sshScheme(scheme)
	p, err := url.Parse(u)
	if err == nil {
		if p.User == nil {
			return u
		}
		if name := p.User.Username(); ssh && name != "" && !strings.Contains(name, ":") {
			p.User = url.User(name)
		} else {
			p.User = nil
		}
		return p.String()
	}
	auth, path, slash := strings.Cut(rest, "/")
	login := ""
	if i := strings.LastIndex(auth, "@"); i >= 0 {
		if name, _, _ := strings.Cut(auth[:i], ":"); ssh && name != "" && !strings.Contains(name, "@") {
			login = name + "@"
		}
		auth = auth[i+1:]
	}
	if slash {
		auth += "/" + path
	}
	if strings.Contains(scheme+auth, "@") {
		return UnparseableURL
	}
	return scheme + "://" + login + auth
}

func sshScheme(s string) bool {
	switch strings.ToLower(s) {
	case "ssh", "git+ssh", "ssh+git":
		return true
	}
	return false
}

// plainSCP keeps SSH logins and local paths, but fails closed on passwords.
// host:team@b:c.git is ambiguous with user:pass@host:path and is cut.
func plainSCP(u string) string {
	head, _, _ := strings.Cut(u, "/")
	first := strings.Index(head, "@")
	if first < 0 {
		// A password with a "/" in it hides its "@" past the first "/".
		// Only a colon before that slash can introduce a password;
		// absolute, relative and drive-letter local paths stay intact.
		colon := strings.Index(head, ":")
		drive := len(head) == 2 && colon == 1 && ((head[0] >= 'A' && head[0] <= 'Z') || (head[0] >= 'a' && head[0] <= 'z'))
		if colon >= 0 && !drive && strings.Contains(u, "@") {
			return UnparseableURL
		}
		return u
	}
	if !strings.Contains(head[:first], ":") {
		return u
	}
	out := u[strings.LastIndex(head, "@")+1:]
	if !strings.Contains(out, ":") {
		return UnparseableURL
	}
	return out
}

// UnparseableURL stands in for a repository URL PlainURL cannot clean.
const UnparseableURL = "(unparseable url)"

func egressOf(r *api.Run) *Egress {
	n := r.Spec.Network
	out := &Egress{Policy: EgressDeny}
	for _, e := range n.Egress {
		switch {
		case e.Host != "":
			out.Allow = append(out.Allow, e.Host)
		case e.CIDR != "":
			out.Allow = append(out.Allow, e.CIDR)
		}
	}
	switch {
	case n.Unrestricted:
		out.Policy, out.Allow = EgressUnrestricted, nil
	case len(out.Allow) > 0:
		out.Policy = EgressAllowlist
	}
	return out
}

func costOf(c *luxapi.RunCost) *Cost {
	if c == nil || c.Status == "" || c.Status == "pending" {
		return nil
	}
	out := &Cost{Status: c.Status}
	// A settled cost (complete or final) of zero is a real zero; an
	// unsettled one is lux not knowing yet.
	settled := c.Status == "complete" || c.Status == "final"
	for _, t := range c.Totals {
		amt, ok := number(t.Amount)
		if !ok || t.Currency == "" || (isZero(amt) && !settled) {
			continue
		}
		m := Money{Currency: t.Currency, Amount: amt}
		if f, ok := number(t.Final); ok && !isZero(f) && f != amt {
			m.Final = f
		}
		if e, ok := number(t.Estimate); ok && !isZero(e) {
			m.Estimate = e
		}
		out.Totals = append(out.Totals, m)
	}
	if len(out.Totals) == 0 {
		return nil
	}
	return out
}

func number(s string) (json.Number, bool) {
	if _, err := strconv.ParseFloat(s, 64); err != nil {
		return "", false
	}
	return json.Number(s), true
}

func isZero(n json.Number) bool {
	f, err := n.Float64()
	return err == nil && f == 0
}

func resumeOf(r *api.Run) *Resume {
	rs := r.Resume
	if rs == nil {
		return nil
	}
	out := &Resume{Resumable: r.Resumable, Uploaded: rs.Uploaded, OnHosts: slices.Clone(rs.OnHosts),
		Secrets: slices.Clone(rs.Secrets), SecretsHeld: rs.SecretsHeld, Blockers: slices.Clone(rs.Blockers)}
	if rs.Snapshot != nil {
		out.Snapshot = *rs.Snapshot
	}
	return out
}

func ptr[T any](v T) *T { return &v }

func pos(f float64) *float64 {
	if f <= 0 {
		return nil
	}
	return &f
}

func posBytes(b api.Bytes) *int64 {
	if b <= 0 {
		return nil
	}
	v := int64(b)
	return &v
}

func nonEmpty(r *Resources) *Resources {
	if r == nil || *r == (Resources{}) {
		return nil
	}
	return r
}

// errNoSample: the exec answered, but the Run reported no resource.
var errNoSample = errors.New("no resource readings in the Run")

// keys sorts a label map's keys, for the text.
func keys(m map[string]string) []string { return slices.Sorted(maps.Keys(m)) }
