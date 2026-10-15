#!/bin/sh
# The default image's smoke test. Run it as lux would: as the image's USER
# (no --user), with an empty home, no network and no credentials. Every
# failed check names itself on stderr and exits 1.
set -u

fail() {
	echo "smoke: FAIL: $*" >&2
	exit 1
}

# The workload user and home luxmux's home volume is mounted for.
[ "$(id -un)" = agent ] || fail "workload user is $(id -un), want agent"
[ "$(id -u)" = 1000 ] || fail "workload uid is $(id -u), want 1000"
[ "${HOME:-}" = /home/agent ] || fail "HOME is '${HOME:-}', want /home/agent"
probe="$HOME/.smoke-write.$$"
# A subshell: a failed redirection on a special builtin would exit this shell.
(: >"$probe") 2>/dev/null || fail "HOME $HOME is not writable by $(id -un)"
rm -f "$probe"

run() {
	"$@" >/dev/null 2>&1 || fail "'$*' exited non-zero"
	echo "ok: $*"
}

run claude --version
run opencode --version
run codex --version
run git --version
run gh --version
run node --version
run uv --version
run rg --version
run fd --version
run bash -lc true

# luxmux starts `bash -l`: the agents must stay on its PATH.
for a in claude opencode codex; do
	bash -lc "command -v $a" >/dev/null 2>&1 || fail "$a is not on the login shell's PATH"
done
echo "smoke: PASS"
