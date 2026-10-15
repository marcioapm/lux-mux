package ui

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/marcioapm/lux/api"

	"github.com/marcioapm/lux-mux/internal/config"
)

// DialogValues are what the new-window dialog edits, as typed.
type DialogValues struct {
	Base       string // the profile it started from
	Image      string
	Pool       string
	Size       string // a shaped pool's size
	CPUs       string // unshaped pools
	Memory     string
	Disk       string
	Repository string
	Branch     string
	WarmSpare  bool
	SaveAs     string
}

// ValuesFrom fills the dialog from a profile. The warm spare box starts
// off whatever the profile says: a spare is billed while it waits.
func ValuesFrom(name string, p config.Profile) DialogValues {
	v := DialogValues{Base: name, Image: p.Image, Pool: p.Pool, Size: p.Size, Memory: p.Memory, Disk: p.Disk,
		Repository: p.Repository, Branch: p.GitRef()}
	if p.CPUs != 0 {
		v.CPUs = strconv.FormatFloat(p.CPUs, 'f', -1, 64)
	}
	return v
}

// The dialog's fields, as FieldError names them.
const (
	FieldImage      = "image"
	FieldPool       = "pool"
	FieldSize       = "size"
	FieldCPUs       = "cpus"
	FieldMemory     = "memory"
	FieldDisk       = "disk"
	FieldRepository = "repository"
	FieldBranch     = "branch"
	FieldSaveAs     = "save as"
)

// FieldError is a problem with one of the dialog's fields ("" when it
// names none).
type FieldError struct {
	Field string
	Err   error
}

func (e *FieldError) Error() string { return e.Err.Error() }
func (e *FieldError) Unwrap() error { return e.Err }

// FieldErrors are an error's messages by the field they are about; those
// about no field are under "".
func FieldErrors(err error) map[string][]string {
	out := map[string][]string{}
	var walk func(error)
	walk = func(e error) {
		if j, ok := e.(interface{ Unwrap() []error }); ok {
			for _, x := range j.Unwrap() {
				walk(x)
			}
			return
		}
		var fe *FieldError
		if errors.As(e, &fe) {
			out[fe.Field] = append(out[fe.Field], fe.Error())
			return
		}
		out[""] = append(out[""], e.Error())
	}
	if err != nil {
		walk(err)
	}
	return out
}

// fieldOf is the field a profile check's message is about, by the name it
// starts with.
func fieldOf(msg string) string {
	for _, f := range []string{FieldImage, FieldSize, FieldCPUs, FieldMemory, FieldDisk, FieldRepository, FieldBranch} {
		if strings.HasPrefix(msg, f) {
			return f
		}
	}
	return ""
}

// Profile is the profile the dialog's values make, from base (whose
// shell, env, labels, secrets and workdir it keeps). shaped says whether
// the chosen pool has sizes: then the size is used and cpus, memory and
// disk dropped, else the reverse. Every problem found is in the error, as
// a FieldError.
func (v DialogValues) Profile(base config.Profile, shaped bool) (config.Profile, error) {
	p, errs := v.apply(base, shaped)
	if len(errs) > 0 {
		return config.Profile{}, errors.Join(errs...)
	}
	return p, nil
}

// SecretNames are the secrets a Run of the values' profile carries (the
// base's git credential is dropped with its repository).
func (v DialogValues) SecretNames(base config.Profile) []string {
	p, _ := v.apply(base, false)
	return p.SecretNames()
}

// apply maps the values onto base, returning every problem found.
func (v DialogValues) apply(base config.Profile, shaped bool) (config.Profile, []error) {
	var errs []error
	p := base
	p.Image = strings.TrimSpace(v.Image)
	p.Pool = strings.TrimSpace(v.Pool)
	p.Size, p.CPUs, p.Memory, p.Disk = "", 0, "", ""
	if shaped {
		p.Size = strings.TrimSpace(v.Size)
	} else {
		if s := strings.TrimSpace(v.CPUs); s != "" {
			c, err := strconv.ParseFloat(s, 64)
			if err != nil || c <= 0 {
				errs = append(errs, &FieldError{FieldCPUs, fmt.Errorf("cpus: %q is not a positive number", s)})
			} else {
				p.CPUs = c
			}
		}
		p.Memory = strings.TrimSpace(v.Memory)
		p.Disk = strings.TrimSpace(v.Disk)
	}
	repo := strings.TrimSpace(v.Repository)
	if repo != p.Repository {
		// Another repository: the base's credential was for its own.
		p.GitCredential = ""
	}
	p.Repository = repo
	p.Branch, p.Ref = strings.TrimSpace(v.Branch), ""
	if p.Repository == "" {
		p.Branch, p.GitCredential = "", ""
	}
	p.WarmSpare = v.WarmSpare
	if v.SaveAs != "" && !config.NameRe.MatchString(v.SaveAs) {
		errs = append(errs, &FieldError{FieldSaveAs, errors.New("save as: a profile name is letters, digits, '.', '_' and '-'")})
	}
	if err := p.Validate(); err != nil {
		list := []error{err}
		if j, ok := err.(interface{ Unwrap() []error }); ok {
			list = j.Unwrap()
		}
		for _, e := range list {
			errs = append(errs, &FieldError{fieldOf(e.Error()), e})
		}
	}
	return p, errs
}

// Shaped reports whether the pool named name has sizes, and lists them.
// An empty name is the tenant's default pool.
func Shaped(pools []api.Pool, name string) (bool, []api.PoolSize) {
	for _, p := range pools {
		if p.Name == name || (name == "" && p.IsDefault != nil && *p.IsDefault) {
			return len(p.Sizes) > 0, p.Sizes
		}
	}
	return false, nil
}

// Blockers says a validateOnly answer's blockers in words.
func Blockers(a *api.SubmitAnswer) []string {
	var out []string
	for _, b := range a.Blockers {
		s := fmt.Sprintf("%s (%d hosts)", b.Code, b.Hosts)
		if b.Reason != "" {
			s += ": " + b.Reason
		}
		out = append(out, s)
	}
	return out
}
