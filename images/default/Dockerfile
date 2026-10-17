# syntax=docker/dockerfile:1.7
# Bump the base with `docker buildx imagetools inspect ubuntu:26.04` (the multi-arch index digest).
ARG UBUNTU_DIGEST=sha256:c098726dd9b9733f3e885f8de0ef375206a311b0a317eadc40d347b1115fb944
FROM ubuntu:26.04@${UBUNTU_DIGEST}

ARG TARGETARCH
# Node 26 tarball hashes: https://nodejs.org/dist/v<version>/SHASUMS256.txt
ARG NODE_VERSION=26.12.0
ARG NODE_SHA256_AMD64=fd47865ecd6391ebb431fc815e01b9135bf83ded5cf373247e9331e196b7aded
ARG NODE_SHA256_ARM64=fd6cc729fa4448c2ac27c3fca5606019ced63c65a15c152c8df03e0cbce7f4c5
# uv hashes: the release's uv-<arch>-unknown-linux-gnu.tar.gz.sha256 assets
ARG UV_VERSION=0.13.1
ARG UV_SHA256_AMD64=eadd9d27c134ab5ac1ffd4473b97a741407f3ea238288f03121fc4deddf90220
ARG UV_SHA256_ARM64=9041c73c99fdc5892cf128140beb354eca9231d502068cc36e6cb53a8e59ad14
# sha256 of https://cli.github.com/packages/githubcli-archive-keyring.gpg
ARG GH_KEYRING_SHA256=6084d5d7bd8e288441e0e94fc6275570895da18e6751f70f057485dc2d1a811b

ENV DEBIAN_FRONTEND=noninteractive \
    LANG=en_US.UTF-8 \
    LC_ALL=en_US.UTF-8 \
    NPM_CONFIG_PREFIX=/opt/npm \
    PATH=/opt/npm/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin

RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends \
      bash git curl ca-certificates openssh-client ripgrep fd-find jq less vim nano unzip zip xz-utils \
      procps iproute2 build-essential pkgconf python3 python3-venv locales tzdata \
      ffmpeg fonts-dejavu-core espeak-ng; \
    # fd-find's own `fd` is in /usr/lib/cargo/bin, off PATH; only fdfind is in /usr/bin.
    ln -s /usr/bin/fdfind /usr/local/bin/fd; \
    printf '%s\n' 'C.UTF-8 UTF-8' 'en_US.UTF-8 UTF-8' > /etc/locale.gen; \
    locale-gen; \
    curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg \
      -o /usr/share/keyrings/githubcli-archive-keyring.gpg; \
    echo "$GH_KEYRING_SHA256  /usr/share/keyrings/githubcli-archive-keyring.gpg" | sha256sum -c -; \
    echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" \
      > /etc/apt/sources.list.d/github-cli.list; \
    apt-get update; \
    apt-get install -y --no-install-recommends gh; \
    apt-get clean; \
    rm -rf /var/lib/apt/lists/*; \
    case "$TARGETARCH" in \
      amd64) node_arch=x64; node_sha=$NODE_SHA256_AMD64; uv_arch=x86_64; uv_sha=$UV_SHA256_AMD64 ;; \
      arm64) node_arch=arm64; node_sha=$NODE_SHA256_ARM64; uv_arch=aarch64; uv_sha=$UV_SHA256_ARM64 ;; \
      *) echo "unsupported architecture: $TARGETARCH" >&2; exit 1 ;; \
    esac; \
    curl -fsSL "https://nodejs.org/dist/v${NODE_VERSION}/node-v${NODE_VERSION}-linux-${node_arch}.tar.xz" -o /tmp/node.tar.xz; \
    echo "$node_sha  /tmp/node.tar.xz" | sha256sum -c -; \
    tar -xJf /tmp/node.tar.xz -C /usr/local --strip-components=1 --no-same-owner \
      --exclude='*/include' --exclude='*/share/doc'; \
    curl -fsSL "https://github.com/astral-sh/uv/releases/download/${UV_VERSION}/uv-${uv_arch}-unknown-linux-gnu.tar.gz" -o /tmp/uv.tar.gz; \
    echo "$uv_sha  /tmp/uv.tar.gz" | sha256sum -c -; \
    tar -xzf /tmp/uv.tar.gz -C /usr/local/bin --strip-components=1 --no-same-owner "uv-${uv_arch}-unknown-linux-gnu/uv"; \
    rm -f /tmp/node.tar.xz /tmp/uv.tar.gz

# The CLIs live under /opt/npm, outside the home, so a fresh or restored home keeps them.
# Agent versions are declared here, not at the top: an ARG in scope is part of a RUN's
# cache key, so bumping one rebuilds only this layer.
ARG CLAUDE_CODE_VERSION=2.1.302
ARG OPENCODE_VERSION=1.19.3
ARG CODEX_VERSION=0.163.0
RUN set -eux; \
    npm install --global --no-update-notifier --no-fund --no-audit \
      "@anthropic-ai/claude-code@${CLAUDE_CODE_VERSION}" \
      "opencode-ai@${OPENCODE_VERSION}" \
      "@openai/codex@${CODEX_VERSION}"; \
    npm cache clean --force; \
    rm -rf /root/.npm; \
    # opencode-ai's postinstall links the variant it selected into bin/; the others are never run.
    oc=/opt/npm/lib/node_modules/opencode-ai; \
    for d in "$oc"/node_modules/opencode-linux-*; do \
      [ "$d/bin/opencode" -ef "$oc/bin/opencode.exe" ] || rm -rf "$d"; \
    done; \
    mkdir -p /etc/codex; \
    printf '%s\n' 'check_for_update_on_startup = false' '' '[analytics]' 'enabled = false' '' '[feedback]' 'enabled = false' \
      > /etc/codex/config.toml; \
    userdel --remove ubuntu; \
    useradd --create-home --uid 1000 --user-group --shell /bin/bash agent

# edge-tts, like the agents, lives outside the home: a venv under /opt/uv-tools on the
# system Python, its entry point linked into /usr/local/bin. Wheels only (no compiler
# toolchain for Python headers here), no cache left behind, no managed-Python download.
ARG EDGE_TTS_VERSION=7.3.1
RUN set -eux; \
    UV_TOOL_DIR=/opt/uv-tools UV_TOOL_BIN_DIR=/usr/local/bin UV_PYTHON_DOWNLOADS=never \
      uv tool install --no-cache --no-build --python /usr/bin/python3 "edge-tts==${EDGE_TTS_VERSION}"

# Update checks and telemetry off, by each vendor's documented switch; a Run's env can override them.
ENV DISABLE_AUTOUPDATER=1 \
    DISABLE_TELEMETRY=1 \
    DISABLE_ERROR_REPORTING=1 \
    OPENCODE_DISABLE_AUTOUPDATE=true

# The workload's package-manager caches go outside the home: luxmux snapshots
# /home/agent on every stop, and wheels or tarballs there would be exported and
# uploaded each time. Set after the build steps, so /tmp/agent-cache is created
# by the workload user at run time, not left root-owned by the build. Images
# built on this one must remove it after root RUN steps (see README.md).
ENV XDG_CACHE_HOME=/tmp/agent-cache \
    NPM_CONFIG_CACHE=/tmp/agent-cache/npm \
    UV_CACHE_DIR=/tmp/agent-cache/uv

# The release version only labels the image; declared last so it invalidates nothing.
ARG VERSION=devel
LABEL org.opencontainers.image.source="https://github.com/marcioapm/lux-mux" \
      org.opencontainers.image.description="luxmux default workload image: shell tools, Node 26, Python with uv, ffmpeg, espeak-ng and edge-tts, Claude Code, OpenCode and Codex" \
      org.opencontainers.image.licenses="ELv2" \
      org.opencontainers.image.version="${VERSION}"

USER agent
WORKDIR /home/agent
CMD ["/bin/bash", "-l"]
