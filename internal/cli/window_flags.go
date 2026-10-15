package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/marcioapm/lux-mux/internal/config"
)

// windowFlags are new's and new-window's flags: the session, the window's
// name and profile, and the overrides of that profile.
type windowFlags struct {
	Session, Name, Profile string

	Image, Pool, Size, Memory, Disk, Workdir string
	CPUs                                     float64
	// Repos are --repo's URL[#BRANCH] values, in order.
	Repos       []string
	Branch, Ref string
	Secrets     []string
	// set names the flags given (an empty value given is an override).
	set map[string]bool
}

// parseWindowFlags reads new's (-s) or new-window's (-t, -n) flags;
// errors and -h go to output.
func parseWindowFlags(command string, args []string, output io.Writer) (windowFlags, error) {
	var o windowFlags
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	fs.SetOutput(output)
	if command == "new" {
		fs.StringVar(&o.Session, "s", "", "session `name` (default: the first free number)")
	} else {
		fs.StringVar(&o.Session, "t", DefaultSession, "session `name`")
		fs.StringVar(&o.Name, "n", "", "window `name` (default: the profile's)")
	}
	fs.StringVar(&o.Profile, "p", "", "`profile` (default: the session's, else default_profile)")
	fs.Func("repo", "repository `URL[#BRANCH]`, repeatable; replaces the profile's", func(s string) error {
		o.Repos = append(o.Repos, s)
		return nil
	})
	fs.StringVar(&o.Branch, "branch", "", "`branch` of the only repository")
	fs.StringVar(&o.Ref, "ref", "", "`ref` (branch, tag or sha) of the only repository")
	fs.StringVar(&o.Image, "image", "", "`image`")
	fs.StringVar(&o.Pool, "pool", "", "`pool`")
	fs.StringVar(&o.Size, "size", "", "`size` of a shaped pool")
	fs.Float64Var(&o.CPUs, "cpus", 0, "`cpus` on an unshaped pool")
	fs.StringVar(&o.Memory, "memory", "", "`memory` on an unshaped pool (e.g. 4Gi)")
	fs.StringVar(&o.Disk, "disk", "", "`disk` on an unshaped pool (e.g. 20Gi)")
	fs.Func("secret", "secret `NAME` to add, repeatable", func(s string) error {
		o.Secrets = append(o.Secrets, s)
		return nil
	})
	fs.StringVar(&o.Workdir, "workdir", "", "the shell's working `directory`")
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	if fs.NArg() > 0 {
		return o, fmt.Errorf("%s: unexpected argument %q", command, fs.Arg(0))
	}
	if len(o.Name) > 64 || strings.ContainsAny(o.Name, "\x00\r\n") {
		return o, fmt.Errorf("%s: -n: a window name is one line of at most 64 bytes", command)
	}
	o.set = map[string]bool{}
	fs.Visit(func(f *flag.Flag) { o.set[f.Name] = true })
	return o, nil
}

// apply puts the overrides on p. --repo replaces p's repositories;
// --branch and --ref set the branch of the only one there is then. A
// size and cpus/memory/disk replace each other.
func (o windowFlags) apply(p config.Profile) (config.Profile, error) {
	var errs []error
	fail := func(f string, a ...any) { errs = append(errs, fmt.Errorf(f, a...)) }
	if o.set["branch"] && o.set["ref"] {
		fail("--branch and --ref: give one")
	}
	explicit := o.set["cpus"] || o.set["memory"] || o.set["disk"]
	if o.set["size"] && explicit {
		fail("--size cannot be combined with --cpus, --memory or --disk")
	}
	if o.set["image"] {
		p.Image = o.Image
	}
	if o.set["pool"] {
		p.Pool = o.Pool
	}
	if o.set["size"] {
		p.Size, p.CPUs, p.Memory, p.Disk = o.Size, 0, "", ""
	}
	if explicit {
		p.Size = ""
		if o.set["cpus"] {
			if o.CPUs <= 0 {
				fail("--cpus must be positive")
			}
			p.CPUs = o.CPUs
		}
		if o.set["memory"] {
			p.Memory = o.Memory
		}
		if o.set["disk"] {
			p.Disk = o.Disk
		}
	}
	if o.set["workdir"] {
		p.Workdir = o.Workdir
	}
	if o.set["repo"] {
		repos := make([]config.Repository, 0, len(o.Repos))
		for _, s := range o.Repos {
			url, branch, _ := strings.Cut(s, "#")
			if url == "" {
				fail("--repo %q: no URL", s)
			}
			repos = append(repos, config.Repository{URL: url, Branch: branch})
		}
		// The profile's credential stays: it is the one these use.
		p.Repository, p.Branch, p.Ref, p.Repositories = "", "", "", repos
	}
	if o.set["branch"] || o.set["ref"] {
		ref := o.Branch
		if o.set["ref"] {
			ref = o.Ref
		}
		switch repos := p.RepositoryList(); {
		case len(repos) != 1:
			fail("--branch and --ref need exactly one repository (the profile's or one --repo); this window has %d", len(repos))
		case len(p.Repositories) == 1:
			if p.Repositories[0].RepoRef() != "" && o.set["repo"] {
				fail("--repo %s: give the branch after '#' or with --branch/--ref, not both", p.Repositories[0].URL)
			}
			p.Repositories = slices.Clone(p.Repositories)
			p.Repositories[0].Branch, p.Repositories[0].Ref = ref, ""
		default:
			p.Branch, p.Ref = ref, ""
		}
	}
	for _, s := range o.Secrets {
		if !slices.Contains(p.Secrets, s) {
			p.Secrets = append(slices.Clone(p.Secrets), s)
		}
	}
	if len(errs) > 0 {
		return config.Profile{}, errors.Join(errs...)
	}
	if err := p.Validate(); err != nil {
		return config.Profile{}, err
	}
	return p, nil
}
