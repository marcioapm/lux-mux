// Package info is what luxmux knows of one window's Run: read from the
// window's own lux (GET /v1/runs/{id}, /cost) and, while it runs, from
// inside it (the cgroup and df over lux's exec API). The C-b i dialog and
// luxmux info show it; a field lux does not report is left out.
package info

import (
	"encoding/json"
	"time"

	"github.com/marcioapm/lux-mux/internal/config"
)

// SchemaVersion is luxmux info --json's "schema_version". It changes only
// when a field is removed or changes meaning; new fields keep it.
const SchemaVersion = 1

// Output is luxmux info --json's answer.
type Output struct {
	SchemaVersion int      `json:"schema_version"`
	Session       string   `json:"session"`
	Windows       []Window `json:"windows"`
}

// Target is a window as luxmux knows it without asking lux: what the
// dialog's first frame shows.
type Target struct {
	Index   int
	Name    string
	Profile string
	// Session is the session's name; Label its Runs' luxmux-session label
	// (they differ after a rename).
	Session  string
	Label    string
	Endpoint config.Endpoint
	RunID    string
}

// Problems: why a window's Run could not be read.
const (
	ProblemUnreachable = "unreachable"
	ProblemRefused     = "refused"
	ProblemNotFound    = "not_found"
	ProblemNoKey       = "no_key"
)

// Window is one window's info. Every field but the first group is
// omitted when lux reports nothing for it.
type Window struct {
	Index        int    `json:"index"`
	Name         string `json:"name"`
	Profile      string `json:"profile,omitempty"`
	Session      string `json:"session"`
	SessionLabel string `json:"session_label,omitempty"`
	// Endpoint is the lux's host and tenant, never its key or where the
	// key is read from.
	Endpoint string `json:"endpoint"`
	RunID    string `json:"run_id,omitempty"`
	// Problem is one of the Problem constants; Error says it in words.
	Problem string `json:"problem,omitempty"`
	Error   string `json:"error,omitempty"`
	// FetchedAt is when lux answered.
	FetchedAt *time.Time `json:"fetched_at,omitempty"`

	Run       *Run       `json:"run,omitempty"`
	Image     *Image     `json:"image,omitempty"`
	Placement *Placement `json:"placement,omitempty"`
	Timing    *Timing    `json:"timing,omitempty"`
	Workload  *Workload  `json:"workload,omitempty"`
	Egress    *Egress    `json:"egress,omitempty"`
	Secrets   []string   `json:"secrets,omitempty"`
	Cost      *Cost      `json:"cost,omitempty"`
	Resume    *Resume    `json:"resume,omitempty"`
	Resources *Resources `json:"resources,omitempty"`
}

// Run is the Run itself. Labels are the user's: app=luxmux and the
// luxmux-* labels are left out (their facts are the fields above).
type Run struct {
	Name        string            `json:"name,omitempty"`
	State       string            `json:"state,omitempty"`
	StateReason string            `json:"state_reason,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	SubmittedBy *Submitter        `json:"submitted_by,omitempty"`
	CreatedAt   *time.Time        `json:"created_at,omitempty"`
}

// Submitter is who submitted the Run: an API key (by name, else id) or
// a person signed in.
type Submitter struct {
	Key     string `json:"key,omitempty"`
	Email   string `json:"email,omitempty"`
	Revoked bool   `json:"revoked,omitempty"`
}

// Image is the image as asked and, for a built image, the image id its
// first build resolved to.
type Image struct {
	Ref      string `json:"ref,omitempty"`
	Resolved string `json:"resolved,omitempty"`
}

// Placement is where the Run is and what it got.
type Placement struct {
	Pool string `json:"pool,omitempty"`
	// Size is a shaped pool's size the Run has.
	Size      string     `json:"size,omitempty"`
	Asked     *Resources `json:"asked,omitempty"`
	Effective *Resources `json:"effective,omitempty"`
	Host      string     `json:"host,omitempty"`
	Epoch     int        `json:"epoch,omitempty"`
	// Moves is epoch − 1: placements after the first.
	Moves *int `json:"moves,omitempty"`
}

// Timing is the Run's stage and times. Seconds are numbers.
type Timing struct {
	Stage       string     `json:"stage,omitempty"`
	StageReason string     `json:"stage_reason,omitempty"`
	StageSince  *time.Time `json:"stage_since,omitempty"`
	// PlacementSince is when the running placement started; Uptime
	// counts from it to FetchedAt.
	PlacementSince  *time.Time `json:"placement_since,omitempty"`
	UptimeSeconds   *float64   `json:"uptime_seconds,omitempty"`
	RuntimeSeconds  *float64   `json:"runtime_seconds,omitempty"`
	WaitingSeconds  *float64   `json:"waiting_seconds,omitempty"`
	StartingSeconds *float64   `json:"starting_seconds,omitempty"`
}

// Workload is the shell's repositories and working directory.
type Workload struct {
	Repositories []Repository `json:"repositories,omitempty"`
	Workdir      string       `json:"workdir,omitempty"`
}

// Repository is one checkout; URL is shown without credentials.
type Repository struct {
	Name string `json:"name,omitempty"`
	URL  string `json:"url"`
	Ref  string `json:"ref,omitempty"`
	Path string `json:"path,omitempty"`
}

// Egress is the Run's network policy (spec.network): Policy is
// "unrestricted", "allowlist" (Allow: the hosts and CIDRs it may reach)
// or "deny" (lux's default: no egress).
type Egress struct {
	Policy string   `json:"policy"`
	Allow  []string `json:"allow,omitempty"`
}

// Egress policies.
const (
	EgressUnrestricted = "unrestricted"
	EgressAllowlist    = "allowlist"
	EgressDeny         = "deny"
)

// Cost is GET /v1/runs/{id}/cost's status and per-currency totals.
type Cost struct {
	Status string  `json:"status"`
	Totals []Money `json:"totals"`
}

// Money is one currency's total; amounts are decimal numbers as lux
// reports them, never rounded through a float.
type Money struct {
	Currency string      `json:"currency"`
	Amount   json.Number `json:"amount"`
	Final    json.Number `json:"final,omitempty"`
	Estimate json.Number `json:"estimate,omitempty"`
}

// Resume is what resuming a stopped Run would take.
type Resume struct {
	Resumable   bool     `json:"resumable"`
	Snapshot    string   `json:"snapshot,omitempty"`
	Uploaded    bool     `json:"uploaded,omitempty"`
	OnHosts     []string `json:"on_hosts,omitempty"`
	Secrets     []string `json:"secrets,omitempty"`
	SecretsHeld bool     `json:"secrets_held,omitempty"`
	Blockers    []string `json:"blockers,omitempty"`
}

// Resources are CPUs, memory and disk: asked and effective in Placement;
// in use (read in the Run) in Window.Resources. Bytes and cores are
// numbers.
type Resources struct {
	CPUs             *float64 `json:"cpus,omitempty"`
	CPULimit         *float64 `json:"cpu_limit,omitempty"`
	MemoryBytes      *int64   `json:"memory_bytes,omitempty"`
	MemoryLimitBytes *int64   `json:"memory_limit_bytes,omitempty"`
	DiskBytes        *int64   `json:"disk_bytes,omitempty"`
	DiskSizeBytes    *int64   `json:"disk_size_bytes,omitempty"`
	Size             string   `json:"size,omitempty"`
}

// Base is the part of a Window luxmux knows without lux.
func Base(t Target) Window {
	w := Window{Index: t.Index, Name: t.Name, Profile: t.Profile, Session: t.Session,
		Endpoint: t.Endpoint.Label(), RunID: t.RunID}
	if t.Label != "" && t.Label != t.Session {
		w.SessionLabel = t.Label
	}
	return w
}
