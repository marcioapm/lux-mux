package fakelux

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/marcioapm/lux/api"
)

// specError is a refusal of a submit: its HTTP status, code and message.
type specError struct {
	status        int
	code, message string
}

// normalized is lux's own RunSpec.Normalize (lux internal/spec/spec.go)
// run on a copy of sp with luxd's built-in defaults: what luxd refuses
// with 422 invalid_spec before anything else. Normalize's Defaults type
// is internal to lux; zeroOf names it through the method's signature.
func normalized(sp api.RunSpec) (api.RunSpec, *specError) {
	var cp api.RunSpec
	b, _ := json.Marshal(sp)
	_ = json.Unmarshal(b, &cp)
	d := zeroOf(cp.Normalize)
	if err := cp.Normalize(d); err != nil {
		return cp, &specError{422, "invalid_spec", err.Error()}
	}
	return cp, nil
}

func zeroOf[T any](func(T) error) (t T) { return }

// resolveSize is luxd's resolveSubmitSize (lux internal/server/sizes.go)
// for the pools SetPools gives: a pool with sizes is shaped. On an
// unshaped pool a size is refused and nothing is resolved. On a shaped
// one a size names one of its sizes, and cpus, memory and disk without
// one are rounded up to the smallest size covering each given; neither
// takes the default size. It returns what the Run's sizing says (nil on
// an unshaped pool) and sets res to the size's resources.
func resolveSize(pool api.Pool, raw api.Resources, res *api.Resources) (*api.RunSizing, *specError) {
	if len(pool.Sizes) == 0 {
		if raw.Size != "" {
			return nil, &specError{422, "size_unsupported", fmt.Sprintf("pool %s has no shape, so no sizes: ask with resources.cpus, memory and disk", pool.Name)}
		}
		return nil, nil
	}
	req := &api.SizeRequest{Size: raw.Size}
	if raw.CPUs != 0 {
		req.CPUs = &raw.CPUs
	}
	if raw.Memory != 0 {
		req.Memory = &raw.Memory
	}
	if raw.Disk != 0 {
		req.Disk = &raw.Disk
	}
	explicit := req.CPUs != nil || req.Memory != nil || req.Disk != nil
	var got *api.PoolSize
	switch {
	case raw.Size != "" && explicit:
		return nil, &specError{422, "size_conflict", fmt.Sprintf("pool %s: resources.size cannot be combined with cpus, memory or disk", pool.Name)}
	case raw.Size != "":
		for i := range pool.Sizes {
			if pool.Sizes[i].Name == raw.Size {
				got = &pool.Sizes[i]
			}
		}
		if got == nil {
			var names []string
			for _, z := range pool.Sizes {
				names = append(names, z.Name)
			}
			return nil, &specError{422, "unknown_size", fmt.Sprintf("pool %s: unknown size %q (sizes: %s)", pool.Name, raw.Size, strings.Join(names, ", "))}
		}
	case explicit:
		for i := range pool.Sizes {
			z := &pool.Sizes[i]
			if z.CPUs >= raw.CPUs && z.Memory >= raw.Memory && z.Disk >= raw.Disk {
				got = z
				break
			}
		}
		if got == nil {
			return nil, &specError{422, "size_too_large", fmt.Sprintf("pool %s: no size holds the resources asked for", pool.Name)}
		}
	default:
		for i := range pool.Sizes {
			if pool.Sizes[i].Default {
				got = &pool.Sizes[i]
			}
		}
		if got == nil {
			got = &pool.Sizes[0]
		}
	}
	res.CPUs, res.Memory, res.Disk, res.Size = got.CPUs, got.Memory, got.Disk, ""
	return &api.RunSizing{Requested: req, Resolved: &api.SizeResolved{Size: got.Name, CPUs: got.CPUs, Memory: got.Memory, Disk: got.Disk}}, nil
}

// missingSecrets is luxd's requireSecrets (lux internal/server/api.go):
// a submit, unlike its dry run, needs every secret's value.
func missingSecrets(sp api.RunSpec) *specError {
	var missing []string
	for _, sec := range sp.Secrets {
		if sec.Value == "" {
			missing = append(missing, sec.Name)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return &specError{422, "secrets_required", "secret values required: " + strings.Join(missing, ", ")}
}

func (e *specError) write(w http.ResponseWriter) { writeErr(w, e.status, e.code, e.message) }
