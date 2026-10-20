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

Every version is a build `ARG` in the [Dockerfile](Dockerfile), and every
download is checked against a sha256 pinned there (the LLVM apt key too).

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

## Updating

Bump the `ARG`s in the Dockerfile, then the matching `*_WANT` versions at the
top of `smoke.sh`. Each toolchain is its own layer, ordered apt (JDK, LLVM),
Go, Rust, so a Go or Rust bump never reruns apt:

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
- `diff-guard.py`: copied from its source when it changes.

## Tags and publishing

`.github/workflows/coder-image.yml` mirrors the default image's workflow,
with its own tags so the two images release independently:

- a `coder-vX.Y.Z` tag pushes `:coder-X.Y.Z`, and moves `:coder` only if it
  is the highest `coder-vX.Y.Z` tag;
- a pre-release tag (`coder-v0.1.0-rc.1`) pushes `:coder-0.1.0-rc.1` and
  leaves `:coder`;
- every push to `main` and every pull request touching `images/coder/**`,
  the default smoke test or the workflow builds and smoke-tests both
  architectures without pushing.

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
lands in the home. Every command's exit status is checked before its output.

Size at the versions above, amd64: about 4.7 GB unpacked, 2.5 GB more than
the default image (mostly LLVM, the two Rust toolchains and the JDK).
