# The default luxmux image

`ghcr.io/marcioapm/luxmux` is the image luxmux's built-in profile runs: a
login shell with the usual development tools, video and speech tools and
three coding agents preinstalled, for `linux/amd64` and `linux/arm64`.

| | |
| --- | --- |
| Base | `ubuntu:26.04`, pinned by digest (`UBUNTU_DIGEST`) |
| Agents | Claude Code `2.1.302`, OpenCode `1.19.3`, Codex `0.163.0` |
| Runtimes | Node.js `26.12.0`, Python 3.14 with `python3-venv`, uv `0.13.1` |
| Tools | bash, git, gh, curl, openssh-client, ripgrep, fd, jq, less, vim, nano, zip/unzip, xz, procps, iproute2, build-essential, pkgconf |
| Video and speech | ffmpeg and ffprobe (with `libx264`, `drawtext`, `subtitles`/`ass`, `overlay`), `fonts-dejavu-core`, espeak-ng, edge-tts `7.3.1` |
| Locales | `C.UTF-8`, `en_US.UTF-8` (the default), tzdata |
| User | `agent`, uid/gid 1000, home `/home/agent` |

Every version is a build `ARG` in the [Dockerfile](Dockerfile). Node and uv
come from their official release archives and gh from GitHub's apt
repository; each download is checked against a sha256 pinned there. The agents
are installed with `npm install --global`, as each vendor documents, into
`/opt/npm` (on `PATH`). ffmpeg, espeak-ng and the fonts are Ubuntu packages;
edge-tts is a `uv tool` venv in `/opt/uv-tools` with `edge-tts` and
`edge-playback` in `/usr/local/bin`. edge-tts needs network access to
Microsoft's speech service (and that host in the profile's `egress`);
espeak-ng is the offline fallback.

## How it fits lux

- **The workload user.** lux runs the workload as the image's `USER` unless
  the spec names one, and sets `HOME` from that user's `/etc/passwd` entry,
  not from the image's `ENV`. Here that is `agent`, home `/home/agent`.
  (Ubuntu's own uid-1000 user `ubuntu` is removed so uid 1000 is `agent`.)
- **The home is on a state volume.** For this image luxmux gives every
  window's Run state volumes at `/workspace` and at `/home/agent` (a
  profile's `persist_home = false` drops the second), so `~/.claude`,
  `~/.codex`, `~/.local/share/opencode`, `~/.config/gh` and shell history
  survive a move to another host. lux hands a fresh volume's root to the
  workload user.
- **The CLIs are outside the home.** `/opt/npm` and `/opt/uv-tools` belong
  to the image, so an empty or restored home never loses or shadows them.
  They are root-owned and
  `agent` has no `sudo`, so a plain `npm install -g` fails with `EACCES`;
  install global packages into the home instead with
  `npm install -g --prefix ~/.local <package>`. Ubuntu's `~/.profile` puts
  `~/.local/bin` on the login shell's `PATH` once it exists (open a new
  window, or `exec bash -l`), and it lives on the home volume.
- **Caches are outside the home.** `XDG_CACHE_HOME=/tmp/agent-cache`,
  `NPM_CONFIG_CACHE=/tmp/agent-cache/npm` and
  `UV_CACHE_DIR=/tmp/agent-cache/uv` keep npm, uv, pip and other XDG caches
  in the container's writable layer, which is discarded on stop, instead of
  in `~/.cache` and `~/.npm`, which the home volume would export and upload
  on every stop. Agent configuration and sessions (`~/.claude`, `~/.codex`,
  `~/.local/share/opencode`, `~/.config`) stay in the home. A profile's
  `env` can point any of these back into the home.
- **Building on this image.** The cache variables apply to your build steps
  too, so a `RUN` as root (`npm install -g`, `uv pip install`, …) leaves a
  root-owned `/tmp/agent-cache` in your image, and `agent` then gets `EACCES`
  at run time. End such a step by removing it:

  ```dockerfile
  USER root
  RUN npm install -g some-tool && rm -rf /tmp/agent-cache
  USER agent
  ```
- **`/.lux`** is lux's and not in the image: the shim at `/.lux/bin/lux-shim`,
  its runtime volume at `/.lux/run`, file secrets on a tmpfs at
  `/.lux/secrets` and service sockets at `/.lux/services`.
- **Requirements of a lux image** that this one meets: `/bin/sh` (`lux exec`
  and the bundling behind `lux push` run it), `bash` for luxmux's login
  shell, `git` for checkouts and `lux push`, and coreutils.
- **Not included:** nested containers (`sandbox.nestedContainers`) need an
  engine, `fuse-overlayfs` and `newuidmap`; build on this image if you need
  them.

## Credentials and network

The image holds no credentials or agent configuration. Pass keys as profile
`secrets`, for example:

```toml
[profiles.default]
image = "ghcr.io/marcioapm/luxmux:latest"
secrets = ["ANTHROPIC_API_KEY", "OPENAI_API_KEY"]
egress = ["api.anthropic.com", "api.openai.com", "github.com", "api.github.com"]
```

lux denies egress by default, so list the hosts each agent needs.

The vendors' documented switches for update checks and telemetry are set,
and a profile's `env` can override any of them:

| Agent | Setting |
| --- | --- |
| Claude Code | `DISABLE_AUTOUPDATER=1`, `DISABLE_TELEMETRY=1`, `DISABLE_ERROR_REPORTING=1` |
| OpenCode | `OPENCODE_DISABLE_AUTOUPDATE=true` |
| Codex | `/etc/codex/config.toml` (the system layer): `check_for_update_on_startup = false`, `analytics.enabled = false`, `feedback.enabled = false` |

## Tags and publishing

`.github/workflows/image.yml` builds each architecture on a native runner and
merges them into one manifest with provenance and SBOM attestations:

- a `vX.Y.Z` tag pushes `:X.Y.Z` (`v0.2.0` → `:0.2.0`), and moves `:latest`
  only if it is the highest `vX.Y.Z` tag in the repository, so `:latest`
  never moves backwards;
- a pre-release tag (`v0.3.0-rc.1`) pushes `:0.3.0-rc.1` and leaves `:latest`;
- `workflow_dispatch` on a tag rebuilds that tag's image, and moves `:latest`
  only when run with `latest = true` (and the tag is the highest release); on
  a branch it builds and smoke-tests without pushing;
- every push to `main`, and every pull request touching `images/**` or the
  workflow, builds and smoke-tests both architectures without pushing, so a
  broken image shows up before a release is tagged.

A luxmux release binary defaults to the image of its own version, and
`luxmux config init` writes that tag; a development, pseudo-version or
pre-release build uses `:latest` for both.

**A release's image exists only once its workflow run is green.** The tag
publishes the luxmux module and starts the image build at the same moment,
so for the length of that run (and for good if it fails) a release binary
names an image that cannot be pulled yet. luxmux cannot detect a failed
pull and does not fall back by itself: set the image by hand, e.g.
`image = "ghcr.io/marcioapm/luxmux:latest"` in the profile. Maintainers:
push the tag, wait for "Default image" to succeed on it, and only then
announce the release.

**The first push creates a private package.** The owner must make it public
once, on GitHub: the package's **Package settings → Change visibility →
Public**. Until then the image cannot be pulled anonymously; set
`image = "ubuntu:26.04"` in the profile (the light alternative `luxmux config
init` documents), which has no agents or extra tools.

## Updating

Bump the `ARG`s in the Dockerfile:

- `UBUNTU_DIGEST`: `docker buildx imagetools inspect ubuntu:26.04`, the index
  digest at the top.
- `NODE_VERSION` and its hashes: the newest `v26.x` in
  `https://nodejs.org/dist/index.json`, hashes from its `SHASUMS256.txt`.
- `UV_VERSION` and its hashes: the release's `*.tar.gz.sha256` assets.
- `CLAUDE_CODE_VERSION`, `OPENCODE_VERSION`, `CODEX_VERSION`:
  `npm view <package> version`.
- `EDGE_TTS_VERSION`: `info.version` in `https://pypi.org/pypi/edge-tts/json`.
- `GH_KEYRING_SHA256` only when GitHub rotates its apt key.

## Building and testing locally

```sh
docker buildx build --platform linux/amd64 --load -t luxmux-image:amd64 images/default
docker run --rm --network none \
  --tmpfs /home/agent:uid=1000,gid=1000,mode=0700 \
  -v "$PWD/images/default/smoke.sh:/smoke.sh:ro" \
  luxmux-image:amd64 sh /smoke.sh
```

The smoke test runs as the image's `USER`, as lux does (so no `--user`),
with an empty home, no network and no credentials. It first checks that the
user is `agent` (uid 1000) and that `HOME` is a writable `/home/agent`, the
path luxmux's home volume is mounted at, then that every agent and tool
starts, that Node is 26.x, that ffmpeg has `libx264` and the `drawtext`,
`subtitles`, `ass` and `overlay` filters, and that a one-second drawtext
render to H.264 and an espeak-ng render to WAV both work offline and read
back with ffprobe. Each failing check prints `smoke: FAIL:` and what failed.

For `linux/arm64`
on an amd64 machine, the same commands work with QEMU binfmt installed.

Size at the versions above, measured as gzip of `docker save` (close to the
registry's compressed layers): amd64 about 828 MB (2.22 GB unpacked), arm64
about 801 MB (2.15 GB unpacked). Most of it is the three agents' native
binaries (Codex alone ships about 410 MB unpacked) and the ffmpeg package's
hard dependencies, which include SDL, Mesa and LLVM (about 180 MB unpacked
for `libllvm21` and `mesa-libgallium`).
