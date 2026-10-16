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

for tool in claude opencode codex git gh node uv rg fd; do
	run "$tool" --version
done
run bash -lc true

# luxmux starts `bash -l`: the agents must stay on its PATH.
for a in claude opencode codex; do
	bash -lc "command -v $a" >/dev/null 2>&1 || fail "$a is not on the login shell's PATH"
done

# Package-manager caches must stay out of the persisted home, login shell included.
# shellcheck disable=SC2016 # expanded by the login shell, not here
for c in 'npm config get cache' 'uv cache dir' 'echo $XDG_CACHE_HOME'; do
	d=$(bash -lc "$c" 2>/dev/null) || fail "'$c' exited non-zero"
	case "$d" in
	"$HOME" | "$HOME"/* | '') fail "'$c' is '$d', want a path outside $HOME" ;;
	esac
done
npm cache verify >/dev/null 2>&1 || fail "npm cache $(npm config get cache) is not usable by $(id -un)"
[ ! -e "$HOME/.npm" ] && [ ! -e "$HOME/.cache" ] || fail "a cache directory was created under $HOME"
echo "smoke: PASS"
