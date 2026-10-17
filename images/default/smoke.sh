#!/bin/sh
# The default image's smoke test. Run it as lux would: as the image's USER
# (no --user), with an empty home, no network and no credentials. Every
# failed check names itself on stderr and exits 1.
set -u

fail() {
	echo "smoke: FAIL: $*" >&2
	exit 1
}

run() {
	"$@" >/dev/null 2>&1 || fail "'$*' exited non-zero"
	echo "ok: $*"
}

# One ffprobe field list as a CSV line, without the section prefix.
ffprobe_csv() {
	ffprobe -v error -of csv=p=0 "$@" 2>/dev/null
}

# The workload user and the home luxmux's home volume is mounted at.
[ "$(id -un)" = agent ] || fail "workload user is $(id -un), want agent"
[ "$(id -u)" = 1000 ] || fail "workload uid is $(id -u), want 1000"
[ "${HOME:-}" = /home/agent ] || fail "HOME is '${HOME:-}', want /home/agent"
probe="$HOME/.smoke-write.$$"
# A subshell: a failed redirection on a special builtin would exit this shell.
(: >"$probe") 2>/dev/null || fail "HOME $HOME is not writable by $(id -un)"
rm -f "$probe"

for tool in claude opencode codex git gh node uv rg fd espeak-ng edge-tts; do
	v=$("$tool" --version 2>/dev/null) || fail "'$tool --version' exited non-zero"
	[ -n "$v" ] || fail "'$tool --version' printed nothing"
	echo "ok: $tool --version"
done
run ffprobe -version
run bash -lc true

v=$(bash -lc 'node --version' 2>/dev/null) || fail "login 'node --version' exited non-zero"
case "$v" in
v26.*) echo "ok: node is $v" ;;
*) fail "node is '$v', want v26.x" ;;
esac

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
trap 'rm -rf "$out"' 0
ffmpeg -hide_banner -nostdin -f lavfi -i color=c=black:s=320x240:r=25:d=1 \
	-vf drawtext=text=ok:fontcolor=white -c:v libx264 "$out/x.mp4" >/dev/null 2>&1 ||
	fail "drawtext/libx264 render exited non-zero"
echo "ok: drawtext/libx264 render"
stream=$(ffprobe_csv -select_streams v:0 -show_entries stream=codec_name,width,height "$out/x.mp4") ||
	fail "ffprobe cannot read the rendered mp4"
[ "$stream" = h264,320,240 ] || fail "rendered mp4 stream is '$stream', want h264,320,240"
echo "ok: rendered mp4 is $stream"
ffmpeg -v error -xerror -nostdin -i "$out/x.mp4" -f framemd5 "$out/frames.md5" >/dev/null 2>&1 ||
	fail "the rendered mp4 does not decode"
frames=$(grep -c '^0,' "$out/frames.md5") || fail "the rendered mp4 has no decoded frames"
[ "$frames" = 25 ] || fail "the rendered mp4 decodes to $frames frames, want 25"
echo "ok: rendered mp4 decodes to $frames frames"

espeak-ng -w "$out/x.wav" "smoke test" >/dev/null 2>&1 || fail "espeak-ng render exited non-zero"
echo "ok: espeak-ng render"
codec=$(ffprobe_csv -select_streams a:0 -show_entries stream=codec_name "$out/x.wav") ||
	fail "ffprobe cannot read the espeak-ng wav"
case "$codec" in
pcm_*) echo "ok: espeak-ng wav is $codec" ;;
*) fail "espeak-ng wav codec is '$codec', want pcm_*" ;;
esac
dur=$(ffprobe_csv -show_entries format=duration "$out/x.wav") ||
	fail "ffprobe cannot read the espeak-ng wav duration"
awk -v duration="$dur" 'BEGIN {
	exit !(duration ~ /^[0-9]+([.][0-9]+)?$/ && duration + 0 >= 0.5)
}' || fail "espeak-ng wav lasts '$dur' s, want >= 0.5 s"
echo "ok: espeak-ng wav duration >= 0.5s"

python3 -m venv "$out/venv" >/dev/null 2>&1 || fail "python3 -m venv failed"
echo "ok: python3 -m venv"
locale_errors=$(bash -lc locale 2>&1 >/dev/null) || fail "login locale exited non-zero"
[ -z "$locale_errors" ] || fail "the login shell's locale is not available"
echo "ok: login locale"

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
