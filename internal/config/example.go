package config

// Example is what `luxmux config init` writes: every setting, commented.
var Example = `# luxmux configuration.
# luxmux signs in to lux as the lux CLI does: ~/.config/lux/config.toml,
# or LUX_URL, LUX_API_KEY and LUX_TENANT.

# The prefix key, a control key. Press it twice to send it to the shell.
prefix = "C-b"

# The profile "prefix c" creates a window from.
default_profile = "default"

# Bindings that work without the prefix. Off unless listed here.
# Commands: select N, next, prev, last, new, new-dialog, choose, rename,
# kill, detach, help, prompt-index.
[keys.direct]
# "alt-1" = "select 1"
# "alt-2" = "select 2"
# "alt-n" = "next"
# "alt-p" = "prev"

[profiles.default]
image = "` + DefaultImage + `"
# Light alternative (no preinstalled agents): ubuntu:24.04
# pool = "default"          # omitted: your tenant's default pool
# On a shaped pool, a size (lux pools sizes <pool>), e.g. size = "m-2-arm64";
# on an unshaped pool, cpus, memory and disk instead:
# cpus = 2
# memory = "4Gi"
# disk = "20Gi"
# workdir = "/workspace"
# warm_spare = false        # keep one started Run ready: instant windows, billed while idle
# lux denies the Run's network egress by default: allow hosts or CIDRs,
# or lift the filter.
# egress = ["github.com", "*.ubuntu.com", "140.82.112.0/20"]
# unrestricted_network = false

# A profile with a repository, cloned into /workspace/repos/<name>.
# [profiles.app]
# image = "ghcr.io/example/dev:latest"
# repository = "https://github.com/example/app.git"
# branch = "main"           # empty: the remote's HEAD
# git_credential = "GITHUB_TOKEN"   # a secret name; the value comes from below
# shell = ["bash", "-l"]
# env = { EDITOR = "vim" }
# labels = { team = "example" }
# secrets = ["NPM_TOKEN"]   # environment variables in the Run
# secrets_from = "~/.config/luxmux/app.env"   # values: this .env file, else the environment
`
