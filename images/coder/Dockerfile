# syntax=docker/dockerfile:1.7
# The coder image: the default luxmux image plus the toolchains coding agents
# build and test with (Go, Java, Rust, Clang/LLVM), diff-guard, and rootless
# Podman with a docker shim for nested lux Runs (sandbox.nestedContainers).
# Bump the base with `docker buildx imagetools inspect ghcr.io/marcioapm/luxmux:<version>`
# (the multi-arch index digest), as the default image bumps Ubuntu.
ARG BASE_IMAGE=ghcr.io/marcioapm/luxmux:0.4.0@sha256:704d8200499c05def47db3add629b4ec5ddace1393279574c3d5c697c2d6ea3e
FROM ${BASE_IMAGE}

ARG TARGETARCH
USER root

# One layer per toolchain, each with its ARGs declared just before it. An ARG
# in scope is part of every later RUN's cache key, so the layers go from the
# slowest-moving and heaviest (apt) to the most often bumped (Rust): a Go or
# Rust bump never reruns apt.

# LLVM's own apt repository (apt.llvm.org): the newest stable major for this Ubuntu.
# sha256 of https://apt.llvm.org/llvm-snapshot.gpg.key
ARG LLVM_MAJOR=23
ARG LLVM_KEY_SHA256=8b2a587ffd672c4687e7581dad4b2f6c1bb2ad6b480cd9771ba2ff48e0b8c75d
# Java: Ubuntu's OpenJDK, the current LTS. Gradle comes from each repo's wrapper.
ARG JDK_MAJOR=25
RUN set -eux; \
    # An armored key in signed-by needs the .asc name; the base image has no gpg to dearmor it.
    curl -fsSL https://apt.llvm.org/llvm-snapshot.gpg.key -o /usr/share/keyrings/llvm-archive-keyring.asc; \
    echo "$LLVM_KEY_SHA256  /usr/share/keyrings/llvm-archive-keyring.asc" | sha256sum -c -; \
    . /etc/os-release; \
    echo "deb [signed-by=/usr/share/keyrings/llvm-archive-keyring.asc] https://apt.llvm.org/${VERSION_CODENAME}/ llvm-toolchain-${VERSION_CODENAME}-${LLVM_MAJOR} main" \
      > /etc/apt/sources.list.d/llvm.list; \
    apt-get update; \
    apt-get install -y --no-install-recommends \
      "openjdk-${JDK_MAJOR}-jdk-headless" \
      "clang-${LLVM_MAJOR}" "lld-${LLVM_MAJOR}" "llvm-${LLVM_MAJOR}" \
      "clang-format-${LLVM_MAJOR}" "clang-tidy-${LLVM_MAJOR}" "clangd-${LLVM_MAJOR}" \
      "libc++-${LLVM_MAJOR}-dev" "libc++abi-${LLVM_MAJOR}-dev" \
      cmake ninja-build libssl-dev; \
    apt-get clean; \
    rm -rf /var/lib/apt/lists/*; \
    # Unversioned names for the newest LLVM; cc and c++ stay GCC (build-essential).
    for t in clang clang++ clang-cpp lld ld.lld llvm-ar llvm-nm llvm-objdump llvm-symbolizer clang-format clang-tidy clangd; do \
      ln -s "/usr/bin/${t}-${LLVM_MAJOR}" "/usr/local/bin/${t}"; \
    done

# Rootless Podman for nested lux Runs (sandbox.nestedContainers), plus the
# command-line tools agents reach for. This is the image's last apt step: the
# pasta hard links and the setuid strip below must follow every apt install.
# Its own layer, so a tool added here never reruns the JDK/LLVM layer above.
# Podman's networking is Recommends and is listed explicitly: passt (pasta),
# nftables (netavark's bridge networks, -p, compose), aardvark-dns (compose
# service names), slirp4netns; catatonit backs `--init`.
RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends \
      podman podman-docker podman-compose crun conmon catatonit \
      passt nftables netavark aardvark-dns slirp4netns \
      fuse-overlayfs uidmap libcap2-bin \
      postgresql-client redis-tools sqlite3 socat netcat-openbsd bind9-dnsutils \
      file tree rsync shellcheck yq; \
    apt-get clean; \
    rm -rf /var/lib/apt/lists/*; \
    # AppArmor attaches by exec path: through Ubuntu's pasta -> passt symlink
    # the host's passt profile applies, which cannot open Podman's netns.
    ln -f /usr/bin/passt /usr/bin/pasta; \
    if [ -e /usr/bin/passt.avx2 ]; then ln -f /usr/bin/passt.avx2 /usr/bin/pasta.avx2; fi; \
    # A nested Run has no no-new-privileges, so a setuid program would make the
    # workload root in its container. newuidmap/newgidmap keep only the file
    # capability they need; setuid-root, they would fail to write agent's uid_map.
    find / -xdev -perm /6000 -type f -exec chmod ug-s {} +; \
    setcap cap_setuid=ep /usr/bin/newuidmap; \
    setcap cap_setgid=ep /usr/bin/newgidmap; \
    [ -z "$(find / -xdev -perm /6000 -type f)" ]; \
    # A lux Run maps only container ids 0-65535; useradd's 100000:65536 is outside it.
    printf 'agent:1:999\nagent:1001:64535\n' > /etc/subuid; \
    cp /etc/subuid /etc/subgid; \
    # podman-docker's shim prints a notice on every call unless this exists.
    touch /etc/containers/nodocker
COPY --chmod=0644 containers.conf /etc/containers/containers.conf

# Go tarball hashes: https://go.dev/dl/?mode=json
ARG GO_VERSION=1.27.3
ARG GO_SHA256_AMD64=65efbc80fdc1fa2cd2631cad5b481777a68e8725f34e89f50fc6ac714e379b40
ARG GO_SHA256_ARM64=a06e99309dbcbdca1a8ccd2282c3ee146e55930886e26b4f1193269587165f49
RUN set -eux; \
    case "$TARGETARCH" in \
      amd64) go_sha=$GO_SHA256_AMD64 ;; \
      arm64) go_sha=$GO_SHA256_ARM64 ;; \
      *) echo "unsupported architecture: $TARGETARCH" >&2; exit 1 ;; \
    esac; \
    curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-${TARGETARCH}.tar.gz" -o /tmp/go.tar.gz; \
    echo "$go_sha  /tmp/go.tar.gz" | sha256sum -c -; \
    tar -xzf /tmp/go.tar.gz -C /usr/local --no-same-owner; \
    rm -f /tmp/go.tar.gz

# rustup-init hashes: https://static.rust-lang.org/rustup/archive/<version>/<target>/rustup-init.sha256
ARG RUSTUP_VERSION=1.29.1
ARG RUSTUP_SHA256_AMD64=dda7234360b7f578ca8b0ddcb80145646fa61a67c1720a5abc7051b35c9fcb71
ARG RUSTUP_SHA256_ARM64=15f6e4ce9f583b929c996c91562bad6d4454f3281de858b02cdfdef615fac433
# The default toolchain, plus ones pinned by repositories' rust-toolchain.toml
# (llm-proxy: 1.94.0), preinstalled so a Run needs no download to build them.
ARG RUST_VERSION=1.99.1
ARG RUST_EXTRA_TOOLCHAINS=1.94.0
RUN set -eux; \
    case "$TARGETARCH" in \
      amd64) rust_target=x86_64-unknown-linux-gnu; rustup_sha=$RUSTUP_SHA256_AMD64 ;; \
      arm64) rust_target=aarch64-unknown-linux-gnu; rustup_sha=$RUSTUP_SHA256_ARM64 ;; \
      *) echo "unsupported architecture: $TARGETARCH" >&2; exit 1 ;; \
    esac; \
    curl -fsSL "https://static.rust-lang.org/rustup/archive/${RUSTUP_VERSION}/${rust_target}/rustup-init" -o /tmp/rustup-init; \
    echo "$rustup_sha  /tmp/rustup-init" | sha256sum -c -; \
    chmod +x /tmp/rustup-init; \
    RUSTUP_HOME=/opt/rustup CARGO_HOME=/opt/cargo /tmp/rustup-init -y --no-modify-path \
      --profile minimal --component rustfmt,clippy --default-toolchain "$RUST_VERSION"; \
    for tc in $RUST_EXTRA_TOOLCHAINS; do \
      RUSTUP_HOME=/opt/rustup CARGO_HOME=/opt/cargo /opt/cargo/bin/rustup toolchain install "$tc" \
        --profile minimal --component rustfmt,clippy; \
    done; \
    # The workload user owns the toolchain store, so a repo pinning another
    # toolchain can still install it (into the container layer, gone on stop).
    chown -R agent:agent /opt/rustup; \
    rm -rf /opt/rustup/downloads/* /opt/rustup/tmp/* /opt/cargo/registry /tmp/rustup-init \
      /tmp/agent-cache /root/.cache

COPY --chmod=0755 diff-guard.py /usr/local/bin/diff-guard

# Toolchains stay outside the home; their caches and downloads go to the
# discarded /tmp/agent-cache, like npm's and uv's in the default image, so a
# stop never exports module caches or build outputs with the home volume.
ENV JAVA_HOME=/usr/lib/jvm/java-${JDK_MAJOR}-openjdk-${TARGETARCH} \
    RUSTUP_HOME=/opt/rustup \
    PATH=/opt/cargo/bin:/usr/local/go/bin:/opt/npm/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
    GOTOOLCHAIN=local \
    GOPATH=/tmp/agent-cache/go \
    GOMODCACHE=/tmp/agent-cache/go/mod \
    CARGO_HOME=/tmp/agent-cache/cargo \
    GRADLE_USER_HOME=/tmp/agent-cache/gradle

ARG VERSION=devel
LABEL org.opencontainers.image.source="https://github.com/marcioapm/lux-mux" \
      org.opencontainers.image.description="luxmux coder image: the default luxmux image plus Go, Java (OpenJDK), Rust (rustup), Clang/LLVM, diff-guard and rootless Podman" \
      org.opencontainers.image.licenses="ELv2" \
      org.opencontainers.image.version="${VERSION}"

USER agent
WORKDIR /home/agent
CMD ["/bin/bash", "-l"]
