package info

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Line is one line of the text: a section's title (Key and Value empty)
// or a key and its value.
type Line struct {
	Section, Key, Value string
}

// Lines are w's text as of now, in sections; a field w lacks has no line,
// a section with no field no title. The error of a window that could not
// be read is in its Window section.
func Lines(w Window, now time.Time) []Line {
	var out []Line
	sec := ""
	add := func(k, v string) {
		if v == "" {
			return
		}
		if len(out) == 0 || out[len(out)-1].Section != sec {
			out = append(out, Line{Section: sec})
		}
		out = append(out, Line{sec, k, v})
	}

	sec = "Window"
	add("window", fmt.Sprintf("%d %s", w.Index, w.Name))
	add("profile", w.Profile)
	s := w.Session
	if w.SessionLabel != "" {
		s += " (Runs labelled " + w.SessionLabel + ")"
	}
	add("session", s)
	add("lux", w.Endpoint)
	add("run", w.RunID)
	add("error", w.Error)

	if r := w.Run; r != nil {
		sec = "Run"
		add("name", r.Name)
		st := r.State
		if st != "" && r.StateReason != "" {
			st += " (" + r.StateReason + ")"
		}
		add("state", st)
		if r.CreatedAt != nil {
			add("created", when(*r.CreatedAt, now))
		}
		if b := r.SubmittedBy; b != nil {
			var parts []string
			if b.Key != "" {
				k := "key " + b.Key
				if b.Revoked {
					k += " (revoked)"
				}
				parts = append(parts, k)
			}
			if b.Email != "" {
				parts = append(parts, b.Email)
			}
			add("submitted by", strings.Join(parts, " · "))
		}
		var ls []string
		for _, k := range keys(r.Labels) {
			ls = append(ls, k+"="+r.Labels[k])
		}
		add("labels", strings.Join(ls, " "))
	}

	if im := w.Image; im != nil {
		sec = "Image"
		add("ref", im.Ref)
		add("resolved", im.Resolved)
	}

	if p := w.Placement; p != nil {
		sec = "Placement"
		add("pool", p.Pool)
		add("size", p.Size)
		add("asked", resourcesText(p.Asked))
		add("effective", resourcesText(p.Effective))
		add("host", p.Host)
		if p.Epoch > 0 {
			e := strconv.Itoa(p.Epoch)
			if p.Moves != nil && *p.Moves == 0 {
				e += " (first placement)"
			} else if p.Moves != nil {
				e += fmt.Sprintf(" (%s since the first)", plural(*p.Moves, "move"))
			}
			add("epoch", e)
		}
	}

	if t := w.Timing; t != nil {
		sec = "Timing"
		st := t.Stage
		if st != "" && t.StageReason != "" {
			st += " (" + t.StageReason + ")"
		}
		if st != "" && t.StageSince != nil {
			st += ", since " + when(*t.StageSince, now)
		}
		add("stage", st)
		if t.PlacementSince != nil {
			add("uptime", Duration(now.Sub(*t.PlacementSince))+" (this placement)")
		}
		if t.RuntimeSeconds != nil {
			add("runtime", Duration(seconds(*t.RuntimeSeconds))+" (all placements)")
		}
		if t.WaitingSeconds != nil {
			add("waiting", Duration(seconds(*t.WaitingSeconds))+" (for a host)")
		}
		if t.StartingSeconds != nil {
			add("starting", Duration(seconds(*t.StartingSeconds))+" (on a host)")
		}
	}

	if wl := w.Workload; wl != nil {
		sec = "Workload"
		for i, r := range wl.Repositories {
			v := r.URL
			if r.Ref != "" {
				v += " @ " + r.Ref
			}
			if r.Path != "" {
				v += " → " + r.Path
			}
			k := "repository"
			if len(wl.Repositories) > 1 {
				k = fmt.Sprintf("repository %d", i+1)
			}
			add(k, v)
		}
		add("workdir", wl.Workdir)
	}

	if e := w.Egress; e != nil || len(w.Secrets) > 0 {
		sec = "Access"
		if e != nil {
			switch e.Policy {
			case EgressUnrestricted:
				add("egress", "unrestricted")
			case EgressAllowlist:
				add("egress", "only "+strings.Join(e.Allow, ", "))
			case EgressDeny:
				add("egress", "none (lux denies egress by default)")
			}
		}
		add("secrets", strings.Join(w.Secrets, ", "))
	}

	if c := w.Cost; c != nil {
		sec = "Cost"
		for _, m := range c.Totals {
			v := string(m.Amount) + " " + m.Currency
			var parts []string
			if m.Final != "" {
				parts = append(parts, "final "+string(m.Final))
			}
			if m.Estimate != "" {
				parts = append(parts, "estimated "+string(m.Estimate))
			}
			if len(parts) > 0 {
				v += " (" + strings.Join(parts, ", ") + ")"
			}
			add("so far", v)
		}
		add("status", c.Status)
	}

	if r := w.Resume; r != nil {
		sec = "Resume"
		add("resumable", yesNo(r.Resumable))
		snap := r.Snapshot
		if snap != "" {
			var notes []string
			if r.Uploaded {
				notes = append(notes, "uploaded")
			}
			if len(r.OnHosts) > 0 {
				notes = append(notes, "on "+strings.Join(r.OnHosts, ", "))
			}
			if len(notes) > 0 {
				snap += " (" + strings.Join(notes, "; ") + ")"
			}
		} else {
			snap = "none: it starts from scratch"
		}
		add("snapshot", snap)
		if len(r.Secrets) > 0 {
			held := "lux no longer holds their values"
			if r.SecretsHeld {
				held = "lux holds their values"
			}
			add("secrets", strings.Join(r.Secrets, ", ")+" ("+held+")")
		}
		for _, b := range r.Blockers {
			add("blocker", b)
		}
	}

	if r := w.Resources; r != nil {
		sec = "In use"
		cpu := "—"
		if r.CPUs != nil {
			cpu = strconv.FormatFloat(*r.CPUs, 'f', 2, 64)
		}
		if r.CPULimit != nil {
			cpu += " of " + strconv.FormatFloat(*r.CPULimit, 'f', -1, 64) + " cpus"
		} else {
			cpu += " cpus"
		}
		add("cpu", cpu)
		add("memory", ofLimit(r.MemoryBytes, r.MemoryLimitBytes))
		add("disk", ofLimit(r.DiskBytes, r.DiskSizeBytes))
	}
	return out
}

// Text is Lines as aligned "key  value" lines under section titles.
func Text(w Window, now time.Time) string {
	ls := Lines(w, now)
	width := LineWidth(ls)
	var b strings.Builder
	for _, l := range ls {
		if l.Key == "" {
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(l.Section + "\n")
			continue
		}
		fmt.Fprintf(&b, "  %-*s  %s\n", width, l.Key, l.Value)
	}
	return b.String()
}

// LineWidth is the width needed to align the keys in lines.
func LineWidth(lines []Line) int {
	width := 0
	for _, line := range lines {
		width = max(width, len(line.Key))
	}
	return width
}

func ofLimit(used, limit *int64) string {
	switch {
	case used != nil && limit != nil && *limit > 0:
		return fmt.Sprintf("%s of %s (%d%%)", Bytes(*used), Bytes(*limit), *used*100 / *limit)
	case used != nil:
		return Bytes(*used) + " (no limit)"
	case limit != nil:
		return "— of " + Bytes(*limit)
	}
	return ""
}

func resourcesText(r *Resources) string {
	if r == nil {
		return ""
	}
	var parts []string
	if r.Size != "" {
		parts = append(parts, "size "+r.Size)
	}
	if r.CPUs != nil {
		parts = append(parts, strconv.FormatFloat(*r.CPUs, 'f', -1, 64)+" cpus")
	}
	if r.MemoryBytes != nil {
		parts = append(parts, Bytes(*r.MemoryBytes)+" memory")
	}
	if r.DiskBytes != nil {
		parts = append(parts, Bytes(*r.DiskBytes)+" disk")
	}
	return strings.Join(parts, ", ")
}

// Bytes is n in binary units: 512Mi, 1.5Gi, 900.
func Bytes(n int64) string {
	units := []string{"", "Ki", "Mi", "Gi", "Ti", "Pi"}
	f := float64(n)
	i := 0
	for f >= 1024 && i < len(units)-1 {
		f /= 1024
		i++
	}
	if i == 0 {
		return strconv.FormatInt(n, 10)
	}
	s := strconv.FormatFloat(f, 'f', 1, 64)
	return strings.TrimSuffix(s, ".0") + units[i]
}

// Duration is d in at most two units: 45s, 12m 3s, 5h 2m, 3d 4h.
func Duration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	s := int64(d.Round(time.Second) / time.Second)
	switch {
	case s < 60:
		return fmt.Sprintf("%ds", s)
	case s < 3600:
		return fmt.Sprintf("%dm %ds", s/60, s%60)
	case s < 86400:
		return fmt.Sprintf("%dh %dm", s/3600, s%3600/60)
	}
	return fmt.Sprintf("%dd %dh", s/86400, s%86400/3600)
}

func seconds(f float64) time.Duration { return time.Duration(f * float64(time.Second)) }

func when(t, now time.Time) string {
	return t.UTC().Format("2006-01-02 15:04:05Z") + " (" + Duration(now.Sub(t)) + " ago)"
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return strconv.Itoa(n) + " " + word + "s"
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
