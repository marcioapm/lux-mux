# lux-mux

`luxmux` is a tmux-style terminal multiplexer whose windows are shells
running on [lux](https://github.com/marcioapm/lux) Runs. Each window is one
Run: a container with a login shell, your repository cloned into it, and a
state volume that survives the Run being moved to another host. Detach,
close the laptop, attach again from another machine: the shells are still
there, screens included.

It is full-screen only: one window is shown at a time, with no panes and no
status bar. The window's identity is in your terminal's title
(`luxmux:<session> [2] name`).

## Install

```sh
go install github.com/marcioapm/lux-mux/cmd/luxmux@latest
```

luxmux signs in to lux the same way the lux CLI does: `~/.config/lux/config.toml`
(`url`, `api_key`, `tenant`), or `LUX_URL`, `LUX_API_KEY` and `LUX_TENANT`.
There is no separate login.

## Quick start

```sh
luxmux config init        # writes ~/.config/luxmux/config.toml, commented
luxmux                    # attach the session "main", creating it
```

The first window starts at once and shows its Run's progress (queued,
pulling image, cloning, running) until the shell is ready.

| Command | |
| --- | --- |
| `luxmux` | attach the default session (`main`), or create it |
| `luxmux new [-s NAME]` | create a session and attach it |
| `luxmux attach [-t NAME]` | attach a session |
| `luxmux ls` | list sessions: windows, running Runs, spares |
| `luxmux kill-session -t NAME [-y]` | terminate the session's Runs (after asking) and forget it |
| `luxmux spares [--rm]` | list waiting warm spares; `--rm` terminates them |
| `luxmux config init [--force]` | write the example configuration |

Detaching leaves every Run running. A shell's `exit` ends its Run (it is
terminated) and closes its window; the last window's exit ends the session.

## Keys

The prefix is `C-b` (configurable). Press it twice to send it to the shell.

| After the prefix | |
| --- | --- |
| `c` | new window from the default profile, no questions |
| `C` | new window through the dialog |
| `n` / `p` | next / previous window |
| `0`–`9` | select window |
| `l` | last window |
| `'` | select a window by index |
| `w` | window chooser |
| `,` | rename window |
| `&` | kill window (asks y/N; terminates its Run) |
| `r` | relaunch an ended window |
| `d` | detach |
| `?` | key help |

Everything else you type goes to the shell immediately; bracketed paste
passes through untouched.

Programs that turn on the kitty keyboard protocol or xterm's
modifyOtherKeys (nvim, vim, fish and others do) make your terminal send the
prefix and the keys after it as escape sequences; luxmux recognises those
too, and pressing the prefix twice sends the program one prefix in the
same encoding. These keyboard modes are kept per window: switching to
another window, opening a prompt, help or a dialog, and detaching turn
them off in your terminal, and showing the window again turns its own
back on.

**The chooser** (`w`) lists windows with their state, image, size,
repository@branch and age; arrows or `j`/`k` move, Enter switches, `/`
filters as you type, `,` renames, `x` kills (with confirmation), Esc or `q`
closes. The lower part previews the highlighted window's screen.

**Direct bindings** work without the prefix, and are off unless configured:

```toml
[keys.direct]
"alt-1" = "select 1"
"alt-n" = "next"
"alt-p" = "prev"
```

## Profiles

`$XDG_CONFIG_HOME/luxmux/config.toml` (usually `~/.config/luxmux/config.toml`):

```toml
prefix = "C-b"
default_profile = "app"

[profiles.app]
image = "ghcr.io/example/dev:latest"
pool = "m"                     # omitted: your tenant's default pool
size = "m-2-arm64"             # shaped pools; or cpus/memory/disk on unshaped ones
repository = "https://github.com/example/app.git"
branch = "main"                # empty: the remote's HEAD
git_credential = "GITHUB_TOKEN"
shell = ["bash", "-l"]         # default: bash -l if the image has bash, else sh -l
env = { EDITOR = "vim" }
labels = { team = "example" }
secrets = ["NPM_TOKEN"]
secrets_from = "~/.config/luxmux/app.env"
workdir = "/workspace/repos/app"
egress = ["github.com", "registry.npmjs.org"]   # lux denies egress by default
# unrestricted_network = true
warm_spare = false
```

Each window's Run is a generic tty workload running the profile's shell,
with a state volume at `/workspace`; the repository is cloned to
`/workspace/repos/<name>` and is the shell's working directory.

Secret values come from the `secrets_from` .env file, else from the
environment variable of the same name, as with `lux run`. They are sent
with the submit and never written anywhere by luxmux.

**The dialog** (`C`) starts from a profile and lets you edit the image,
pool, size (the pool's sizes, read from lux; cpus/memory/disk on an
unshaped pool), repository and branch, offers the warm spare (off by
default), and can save the result as a new profile. It is checked with
lux's dry run first, and anything that would keep it from starting now is
shown before you create it.

## Moves and reconnects

Each window follows its Run on its own:

- **A dropped stream** (a network blip, the laptop sleeping) while the Run
  runs on the same host: luxmux attaches again, with backoff, and the
  screen is restored. It is the same shell.
- **A move** (the Run is drained, migrated or resumed elsewhere): the
  window shows `reconnecting… (<stage>)` and attaches when the Run is
  running again. That is a **fresh shell** in a new container: the files in
  `/workspace` are restored, running processes are not.
- **Stopped for its secrets, or lost** (lux keeps secret values in memory
  only; a lost Run's host stopped answering): luxmux resumes the Run with
  the values of the secrets lux lists for it, then attaches.
- **Stopped by a person** (`lux stop`, the console): luxmux does not undo
  that. The window shows `stopped by <reason> — r resume, & close`.
- **Ended for good** (terminated, failed for a reason of lux's such as
  `timeout`): the window shows why, and offers `r` to relaunch a fresh Run
  of the same profile in that slot or `&` to close it.
- **The shell exited** (lux reports the Run succeeded, or failed with
  `exit code N` after its stream said the workload exited): the window
  closes and its Run is terminated.

## Screens and scrollback

The shown window's output goes to your terminal as it arrives, unmodified,
so your terminal's own scrollback has it. Every window also keeps an
emulator of its screen; switching to a window repaints your terminal from
it (the screen, colours, cursor, and the modes its program set). The
history of a window you switch to is **not** pushed into your terminal's
scrollback, to avoid flooding it; when a new luxmux process attaches, lux's
restore snapshot of the window shown at that moment, including up to 2000
lines of history, does go to your terminal. There is no copy mode.

Backpressure: luxmux never slows a window's stream down for its emulator,
and never makes the keyboard wait for your terminal. The shown window's
output is queued for your terminal unchanged; keys go to the shell while
your terminal (or the link to it) is busy, and `C-b d` detaches even when
it takes nothing. Each window's emulator may lag its stream by up to
8 MiB. Past that (a flood of CJK, combining marks or truecolour can be
parsed slower than it arrives), luxmux drops that window's queued output,
resets its emulator and marks it stale; the next time the window is shown,
previewed or has an overlay taken down, luxmux attaches it again with
restore and lux's snapshot rebuilds its screen. Your terminal itself has
received all of the shown window's output either way.

If your terminal (or the link to it) cannot keep up, up to 4 MiB of the
shown window's output waits for it; past that, luxmux drops what waits
and, once the terminal took the rest, repaints the window from its
emulator: you miss scrolled-by output in your terminal's scrollback, never
the screen's state, and lux never drops the stream for it.

lux attaches at most 200×500: a larger terminal shows each window in its
top-left 200 rows and 500 columns.

A host whose lux-shim predates restore sends no snapshot: the window starts
blank, and luxmux asks the shell to redraw its prompt. So does a host with
restore whose screen was too large to snapshot.

## Warm spares

With `warm_spare = true`, luxmux keeps one extra started Run of that
profile, labelled as a spare (with a hash of the Run's spec). A new window
whose spec is the same takes it at once and a replacement is started; a
window from the dialog with an edited spec gets a Run of its own, and a
spare is kept for it only if it was saved as a profile. A spare found
stopped or ended when claimed is terminated and replaced. **A spare is a running Run and is
billed while it waits**: enable it only where instant windows are worth
that. `kill-session` and `luxmux spares --rm` terminate spares; the
session's last window closing does too.

## Sessions and local state

A session's window order, names and spare claims are kept in
`$XDG_STATE_HOME/luxmux/<session>.json` (`~/.local/state/luxmux/`). Every
Run is also labelled `luxmux-session=<session>` and
`luxmux-profile=<profile>` (and `app=luxmux`), so when that file is gone —
another machine, a cleaned home directory — `luxmux attach -t NAME`
rebuilds the session from lux: its non-terminated Runs, ordered by
creation. That rebuild is lossy: window names become their profile names,
and of several spares of one spec the newest is taken to be the waiting
one.

One luxmux process attaches a session at a time: a second `luxmux attach`
of an attached session fails, naming the process that has it.

## Development

```sh
make build   # bin/luxmux
make test    # go test -race ./...
make lint    # gofmt and go vet
```

The tests run against a fake luxd (`internal/fakelux`) whose attach
streams are backed by in-process terminals; the end-to-end test drives
`luxmux` in a pty with local shells behind it.

## License

lux-mux is source-available under the [Elastic License 2.0](LICENSE) (ELv2).
Copyright 2026 The lux-mux authors. It is not an OSI-approved open-source license.

Personal use, internal business use, modification, and redistribution are
permitted subject to the license. For example, using lux-mux as internal
infrastructure for a noncompeting commercial application is permitted. Providing
lux-mux to third parties as a hosted or managed service that gives users access
to a substantial set of its features or functionality is not permitted. For
example, hosting lux-mux and giving customers access to a substantial set of its
features as a hosted or managed terminal service falls within this restriction.

ELv2 also prohibits circumventing license-key functionality or removing or
obscuring licensing, copyright, or other notices. It does not impose a general
ban on commercial redistribution or selling self-hosted derivatives.

This summary does not change the terms; the full [license](LICENSE) governs.
Third-party components retain their own licenses. For separate permission to
provide a hosted or managed service covered by the restriction, contact the
maintainer.
