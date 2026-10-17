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

for tool in claude opencode codex git gh node uv rg fd espeak-ng edge-tts; do
	run "$tool" --version
done
run ffprobe -version
run bash -lc true

v=$(node --version 2>/dev/null) || fail "'node --version' exited non-zero"
case "$v" in
v26.*) echo "ok: node is $v" ;;
*) fail "node is '$v', want v26.x" ;;
esac

# ffmpeg must have the encoder and filters video work relies on.
encoders=$(ffmpeg -hide_banner -encoders 2>/dev/null) || fail "'ffmpeg -encoders' exited non-zero"
echo "$encoders" | grep -qw libx264 || fail "ffmpeg has no libx264 encoder"
echo "ok: ffmpeg encoder libx264"
filters=$(ffmpeg -hide_banner -filters 2>/dev/null) || fail "'ffmpeg -filters' exited non-zero"
for f in drawtext subtitles ass overlay; do
	echo "$filters" | grep -qE "^ [^ ]+ $f " || fail "ffmpeg has no $f filter"
	echo "ok: ffmpeg filter $f"
done

# Renders, offline: drawtext needs a font from fontconfig, libx264 must encode.
out=$(mktemp -d) || fail "mktemp -d failed"
run ffmpeg -hide_banner -nostdin -f lavfi -i color=c=black:s=320x240:d=1 \
	-vf drawtext=text=ok:fontcolor=white -c:v libx264 "$out/x.mp4"
probe=$(ffprobe -v error -select_streams v:0 -show_entries stream=codec_name,width,height \
	-of csv=p=0 "$out/x.mp4" 2>/dev/null) || fail "ffprobe cannot read the rendered mp4"
[ "$probe" = h264,320,240 ] || fail "rendered mp4 stream is '$probe', want h264,320,240"
echo "ok: rendered mp4 is $probe"
run espeak-ng -w "$out/x.wav" "smoke test"
codec=$(ffprobe -v error -select_streams a:0 -show_entries stream=codec_name \
	-of csv=p=0 "$out/x.wav" 2>/dev/null) || fail "ffprobe cannot read the espeak-ng wav"
case "$codec" in
pcm_*) echo "ok: espeak-ng wav is $codec" ;;
*) fail "espeak-ng wav codec is '$codec', want pcm_*" ;;
esac
rm -rf "$out"

# luxmux starts `bash -l`: the agents must stay on its PATH.
for a in claude opencode codex ffmpeg edge-tts; do
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
