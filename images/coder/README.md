# The coder image

`ghcr.io/marcioapm/luxmux:coder` is the [default image](../default/README.md)
plus the toolchains coding agents build and test with. It is built `FROM`
the default image pinned by digest, so it has everything that one has (the
three agents, Node, Python/uv, gh, ffmpeg, …) and luxmux treats it the same
way: it is the `ghcr.io/marcioapm/luxmux` repository, so its Runs get the
`/home/agent` state volume.

| | |
| --- | --- |
| Base | `ghcr.io/marcioapm/luxmux:0.4.0`, pinned by digest (`BASE_IMAGE`) |
| Go | `1.27.3` from go.dev, in `/usr/local/go`; `GOTOOLCHAIN=local` |
| Java | OpenJDK `25` (Ubuntu's `openjdk-25-jdk-headless`); Gradle comes from each repository's wrapper |
| Rust | rustup `1.29.1` in `/opt/cargo`, default toolchain `1.99.1`, plus `1.94.0` preinstalled (`RUST_EXTRA_TOOLCHAINS`), each with rustfmt and clippy |
| C/C++ | Clang/LLVM `23` from apt.llvm.org: clang, clang++, lld, llvm tools, clang-format, clang-tidy, clangd, libc++; unversioned names in `/usr/local/bin`. `cc`/`c++` stay GCC. cmake, ninja, libssl-dev |
| Gate | `diff-guard` in `/usr/local/bin` |
| Containers | rootless Podman `5.7` (Ubuntu's), `docker` → podman (`podman-docker`), `docker compose` / `podman compose` → `podman-compose` `1.5`; crun, conmon, catatonit (`--init`), netavark, aardvark-dns, nftables, passt/pasta, slirp4netns, fuse-overlayfs, uidmap |
| CLI tools | `psql`/`pg_isready` (postgresql-client), `redis-cli`, `sqlite3`, `socat`, `nc` (OpenBSD), `dig` (bind9-dnsutils), `file`, `tree`, `rsync`, `shellcheck`, `yq` (the jq wrapper) |

Every independently downloaded archive is checked against a sha256 pinned in
the [Dockerfile](Dockerfile); the LLVM apt key is checked too. Tool and
toolchain versions are build `ARG`s there, with the corresponding smoke-test
expectations in `smoke.sh`.

## How it fits lux

- **Caches stay out of the home**, as in the default image: `GOPATH`,
  `GOMODCACHE`, `CARGO_HOME` and `GRADLE_USER_HOME` are under
  `/tmp/agent-cache`, the container's writable layer, discarded on stop. A
  stop never exports module caches or build outputs with the home volume.
  Seed them with lux cache volumes instead.
- **Rust toolchains.** `RUSTUP_HOME=/opt/rustup` is owned by `agent`, so a
  repository whose `rust-toolchain.toml` pins a toolchain that is not
  preinstalled still gets it (downloaded into the container layer, gone on
  stop; static.rust-lang.org must be in the Run's egress). Preinstall the
  ones you use through `RUST_EXTRA_TOOLCHAINS` so a Run needs no download.
  Cargo's binaries (`cargo`, `rustc`, `rustup`) are in `/opt/cargo/bin`; a
  `cargo install` goes to `$CARGO_HOME/bin`, which is not on `PATH`.
- **Go** never downloads a newer toolchain on its own (`GOTOOLCHAIN=local`):
  a module whose `go` line is newer than the image's fails loudly instead.

## Containers in a Run

The image has rootless Podman so an agent can run a repository's
database-backed tests and Docker e2e (`docker run`, `docker compose up`).
It works only in a Run with **`sandbox.nestedContainers: true`**, placed on a
host or EC2 pool that offers nested containers (lux `docs/runspec.md`,
"Nested containers"). Elsewhere `podman` is present but cannot start
containers.

- **`docker` is Podman**, through `podman-docker`'s `/usr/bin/docker` exec
  wrapper (its notice is silenced by `/etc/containers/nodocker`). There is no
  daemon and no socket; tools that speak the Docker API (testcontainers, for
  one) need a Podman API socket from `podman system service`, which the
  image does not start.
- **`docker compose` is podman-compose**, set as Podman's compose provider in
  `/etc/containers/containers.conf`. It drives the podman CLI, so it needs
  no socket either. Services resolve each other by name through
  aardvark-dns; `depends_on` orders starts but does not wait for health.
- **Storage.** lux mounts an ephemeral volume at
  `~/.local/share/containers`, where Podman's default store gets native
  kernel overlay. Nothing configures another graphroot or a
  `mount_program`: off that volume (the container's own overlay root),
  Podman falls back to fuse-overlayfs by itself. Images and containers are
  gone at each placement; they are never in the home snapshot.
- **No `XDG_RUNTIME_DIR` needed.** Without one, Podman keeps its runtime
  state in `/tmp/storage-run-1000`, which is fresh in every placement.
- **pasta is a hard link to passt**, `pasta.avx2` to `passt.avx2` (x86_64),
  made after the image's last apt step. AppArmor attaches profiles by exec
  path: through Ubuntu's `pasta -> passt` symlink a host's `passt` profile
  applies and every inner container fails with "Couldn't open network
  namespace … Permission denied" (lux#115). An apt install or upgrade of
  passt in a later layer restores the symlink; redo the links after it.
- **Subordinate ids inside 0–65535**: `agent:1:999` and `agent:1001:64535` in
  `/etc/subuid` and `/etc/subgid`. A Run maps only container ids 0–65535, so
  `useradd`'s default `agent:100000:65536` cannot be mapped.
- **No setuid or setgid files.** A nested Run has no `no-new-privileges`, so
  any setuid program (`su`, `passwd`, `mount`) would make the agent root in
  its container. The build strips every setuid/setgid bit and fails if one
  remains; `newuidmap`/`newgidmap` get `cap_setuid`/`cap_setgid` as file
  capabilities instead. There is no sudo.
- Inner containers have the Run's egress rules, and share its CPU and memory
  limits unless the Run also sets `sandbox.cgroupDelegation: true`.

## Updating

Bump the `ARG`s in the Dockerfile, then the matching `*_WANT` versions at the
top of `smoke.sh`. Each toolchain is its own layer, ordered apt (JDK, LLVM),
apt (Podman and CLI tools), Go, Rust, so a Go or Rust bump never reruns apt:

- `BASE_IMAGE`: the default image's new release, `docker buildx imagetools
  inspect ghcr.io/marcioapm/luxmux:<version>`, the index digest at the top.
- `GO_VERSION` and its hashes: `https://go.dev/dl/?mode=json`.
- `RUSTUP_VERSION`: `https://static.rust-lang.org/rustup/release-stable.toml`;
  hashes from `rustup/archive/<version>/<target>/rustup-init.sha256`.
- `RUST_VERSION`: `[pkg.rust] version` in
  `https://static.rust-lang.org/dist/channel-rust-stable.toml`.
- `LLVM_MAJOR`: the newest `llvm-toolchain-<codename>-<N>` at
  `https://apt.llvm.org/<codename>/dists/`. The packages are LLVM's own
  builds of the current point release.
- `JDK_MAJOR`: the current OpenJDK LTS in Ubuntu.
- Podman and the CLI tools are Ubuntu's, unpinned, and move when that layer
  rebuilds (a `BASE_IMAGE` bump or an edit to the step). Set `PODMAN_WANT`
  in `smoke.sh` to the new major.minor. Keep this step the image's last apt
  step: the pasta links and the setuid strip must come after every install.
- `diff-guard.py`: copied from its source when it changes.

## Tags and publishing

`.github/workflows/coder-image.yml` mirrors the default image's workflow,
with its own tags so the two images release independently:

- a `coder-vX.Y.Z` tag pushes `:coder-X.Y.Z`, and moves `:coder` only if it
  is the highest `coder-vX.Y.Z` tag;
- a pre-release tag (`coder-v0.2.0-rc.1`) pushes `:coder-0.2.0-rc.1` and
  leaves `:coder`;
- every push to `main` and every pull request touching `images/coder/**`,
  the default smoke test or the workflow builds and smoke-tests both
  architectures without pushing.

Current release: `coder-v0.2.0` (`:coder-0.2.0`, Podman and CLI tools added
to `coder-0.1.0`). To release, tag `coder-v0.2.0` on `main`; the
`VERSION` build arg labels the image `0.2.0` from the tag. The repository
stays `ghcr.io/marcioapm/luxmux`, so luxmux still recognises the image as its
own (`IsDefaultImage`) and its Runs keep the `/home/agent` volume.

## Building and testing locally

```sh
docker buildx build --platform linux/amd64 --load -t luxmux-coder:amd64 images/coder
docker run --rm --network none \
  --tmpfs /home/agent:uid=1000,gid=1000,mode=0700 \
  -v "$PWD/images/default/smoke.sh:/smoke-default.sh:ro" \
  -v "$PWD/images/coder/smoke.sh:/smoke.sh:ro" \
  luxmux-coder:amd64 sh /smoke.sh
```

The smoke test runs the default image's smoke test first, then, offline: every
added tool is on the login shell's `PATH` and the LLVM tools, cmake and ninja
start; Go, Java, Clang and the default and extra Rust toolchains are the
pinned versions and `JAVA_HOME` holds a JDK; a Go program, a Java class, a
C++23 program (with libstdc++ and lld, and with libc++), and a Cargo project on
both the default and the pinned toolchain build and run; diff-guard flags a
source-inspection test and passes a clean tree; and no toolchain cache or store
lands in the home. For the engine it checks the Podman version, the `docker`
shim, the pasta hard links (same inode, not a symlink), the newuidmap and
newgidmap file capabilities, that no setuid or setgid file exists, and the
exact subuid/subgid lines; `sqlite3`, `yq` and `shellcheck` run. Where the
container may create a user namespace it also runs `podman info` as a Run
starts it (no `XDG_RUNTIME_DIR`) and `docker compose version`; elsewhere it
skips those two, unless `SMOKE_ENGINE=1` requires them (CI's second smoke
step, which relaxes only seccomp and AppArmor). Every command's exit status
is checked before its output.

### Nested proof

The smoke test starts no container. `nested-proof.sh` does, in a real nested
Run or in a stand-in with lux's nested flags (`internal/runner/nested.go`,
`placement.go`): rootful Podman, the image in rootful storage, and

```sh
sudo podman run --rm --user root \
  --userns=auto:size=65536 --cap-add=SYS_CHROOT \
  --device=/dev/fuse --device=/dev/net/tun \
  --security-opt=unmask=ALL --security-opt=label=disable \
  --security-opt=seccomp=<host profile with sethostname, setdomainname and setns allowed> \
  --security-opt=apparmor=unconfined \
  -v <fresh volume>:/home/agent/.local/share/containers \
  -v "$PWD/images/coder/nested-proof.sh:/nested-proof.sh:ro" \
  --entrypoint bash luxmux-coder:amd64 /nested-proof.sh
```

(`apparmor=unconfined` only where the host sets
`kernel.apparmor_restrict_unprivileged_userns=1`, as lux does.) It runs, as
`agent` with no `XDG_RUNTIME_DIR`: `podman run alpine echo nested-ok`; a
`docker run` on the default network; Postgres published on `-p 55432:5432`,
checked with `pg_isready` and `psql -c 'select 1'`; and a two-service compose
file (Postgres and a client that reaches it as `db`) under
`docker compose up --abort-on-container-exit`. It prints `NESTED PASS`. Images
come from Docker Hub by digest, or from `/images.tar` (see its header) where
the outer container has no egress.

Size at the versions above, amd64: about 4.9 GB unpacked, 2.7 GB more than
the default image (mostly LLVM, the two Rust toolchains and the JDK; Podman
and the CLI tools are 175 MB of it).
