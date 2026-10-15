#!/bin/sh
# The default image's smoke test: run as the workload user with an empty
# home and no network or credentials. Every command must start and exit 0.
set -eu

claude --version
opencode --version
codex --version
git --version
gh --version
node --version
uv --version
rg --version
fd --version
bash -lc true

# luxmux starts `bash -l`: the agents must stay on its PATH.
bash -lc 'command -v claude opencode codex'
