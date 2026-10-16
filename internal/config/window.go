package config

import (
	"path"
	"slices"
	"strings"
)

// The LUXMUX_* variables of a window's Run. The static ones are in the
// RunSpec's env, so they survive moves; the computed ones are set by the
// start wrapper on every placement.
const (
	EnvSession      = "LUXMUX_SESSION"
	EnvProfile      = "LUXMUX_PROFILE"
	EnvImage        = "LUXMUX_IMAGE"
	EnvPool         = "LUXMUX_POOL"
	EnvSize         = "LUXMUX_SIZE"
	EnvRepository   = "LUXMUX_REPOSITORY"
	EnvBranch       = "LUXMUX_BRANCH"
	EnvRepoName     = "LUXMUX_REPO_NAME"
	EnvRepoDir      = "LUXMUX_REPO_DIR"
	EnvWorkdir      = "LUXMUX_WORKDIR"
	EnvRepositories = "LUXMUX_REPOSITORIES"

	EnvCPU              = "LUXMUX_CPU"
	EnvMemory           = "LUXMUX_MEMORY"
	EnvDisk             = "LUXMUX_DISK"
	EnvBranchCheckedOut = "LUXMUX_BRANCH_CHECKED_OUT"
	EnvStart            = "LUXMUX_START"
	EnvRunID            = "LUXMUX_RUN_ID"
	// EnvWindow is in the env file only, written by luxmux over lux's exec
	// API when a window takes the Run and on every rename.
	EnvWindow = "LUXMUX_WINDOW"

	// Tests point the wrapper's workspace and cgroup directories at
	// fixtures with these; nothing else sets them.
	EnvTestWorkspace = "_LUXMUX_TEST_WORKSPACE"
	EnvTestCgroup    = "_LUXMUX_TEST_CGROUP"
)

// StaticEnv and ComputedEnv name the variables in the order the env file
// lists them.
var (
	StaticEnv   = []string{EnvSession, EnvProfile, EnvImage, EnvPool, EnvSize, EnvRepository, EnvBranch, EnvRepoName, EnvRepoDir, EnvWorkdir, EnvRepositories}
	ComputedEnv = []string{EnvCPU, EnvMemory, EnvDisk, EnvBranchCheckedOut, EnvStart, EnvRunID}
)

// reservedEnv reports whether name is one luxmux sets: a profile's env
// or secrets may not use it, so no secret value can reach the env file.
func reservedEnv(name string) bool {
	return slices.Contains(StaticEnv, name) || slices.Contains(ComputedEnv, name) || name == EnvWindow ||
		name == EnvTestWorkspace || name == EnvTestCgroup
}

// UserEnv is env without the variables luxmux sets (nil when none is left):
// a Run's stored env read back as its profile's.
func UserEnv(env map[string]string) map[string]string {
	var out map[string]string
	for k, v := range env {
		if reservedEnv(k) {
			continue
		}
		if out == nil {
			out = map[string]string{}
		}
		out[k] = v
	}
	return out
}

// windowEnv is the static LUXMUX_* values of a window's Run. Every one is
// set, empty when unknown, so the env file always lists the same names.
func windowEnv(p Profile, t Target, workdir string) map[string]string {
	env := map[string]string{
		EnvSession: t.Session, EnvProfile: t.Profile, EnvImage: p.Image, EnvPool: p.Pool, EnvSize: p.Size,
		EnvRepository: "", EnvBranch: "", EnvRepoName: "", EnvRepoDir: "", EnvWorkdir: workdir, EnvRepositories: "",
	}
	var lines []string
	for i, r := range p.RepositoryList() {
		dir := RepoDir(r.RepoName())
		if r.Path != "" {
			dir = path.Clean(r.Path)
		}
		if i == 0 {
			env[EnvRepository], env[EnvBranch], env[EnvRepoName], env[EnvRepoDir] = r.URL, r.RepoRef(), r.RepoName(), dir
		}
		lines = append(lines, strings.Join([]string{r.RepoName(), r.URL, r.RepoRef(), dir}, "\t"))
	}
	env[EnvRepositories] = strings.Join(lines, "\n")
	return env
}

// startArg0 is the wrapper's $0; it marks a window command when a Run's
// spec is read back.
const startArg0 = "luxmux-start"

// WindowCommand is the workload command of a window's Run: the start
// wrapper, given on_start, on_resume and the profile's shell as arguments.
//
// The wrapper is the command rather than lux's init.script: init runs
// outside the terminal (its output goes to the Run's log, not the window)
// and a failing init fails the Run. Run as the command, on_start and
// on_resume print into the window's PTY, before the prompt and in its
// scrollback, and a failure leaves the shell to start.
func WindowCommand(p Profile) []string {
	return append([]string{"sh", "-c", StartWrapper, startArg0, p.OnStart, p.OnResume}, p.Shell...)
}

// ParseWindowCommand reads WindowCommand's arguments back from a Run's
// workload command; ok is false for any other command.
func ParseWindowCommand(cmd []string) (shell []string, onStart, onResume string, ok bool) {
	if len(cmd) < 6 || cmd[0] != "sh" || cmd[1] != "-c" || cmd[3] != startArg0 {
		return nil, "", "", false
	}
	if len(cmd) > 6 {
		shell = slices.Clone(cmd[6:])
	}
	return shell, cmd[4], cmd[5], true
}

// WindowNameCommand is the exec command that records window name in a Run.
func WindowNameCommand(name string) []string {
	return []string{"sh", "-c", WindowNameScript, "luxmux-window", name}
}

// StartWrapper is the POSIX sh script every window's Run starts with. Its
// text is the same for every profile: on_start ($1) and on_resume ($2)
// are arguments, only ever run as sh -c "$cmd", never part of the text.
// It computes the placement's facts (each empty when its source is
// missing: no cgroup file, a read-only or absent /sys, no git), decides
// start or resume by a marker on the state volume (written before
// on_start runs, so a Run moved during it resumes), writes every
// LUXMUX_* variable to the env file atomically, runs the command, then
// execs the shell: "$@" when the profile has one, else bash -l or sh -l
// as luxmux always has. Without commands it prints nothing.
var StartWrapper = buildWrapper()

// quoteFunc defines _lm_quote, which sets _lm_qv to $1 single-quoted for
// sh.
const quoteFunc = `_lm_quote() {
	_lm_qs=$1 _lm_qv=
	while :; do
		case $_lm_qs in
		*\'*) _lm_qv=$_lm_qv${_lm_qs%%\'*}\'\\\'\' ; _lm_qs=${_lm_qs#*\'} ;;
		*) break ;;
		esac
	done
	_lm_qv=\'$_lm_qv$_lm_qs\'
}
`

// WindowNameScript is what luxmux execs in a Run (lux's exec API) when a
// window takes it or is renamed: $1, the window's name, goes to
// .luxmux/window, and the env file's LUXMUX_WINDOW line is replaced, each
// atomically. The name is never part of the text.
var WindowNameScript = `_lm_ws=${` + EnvTestWorkspace + `:-/workspace}
_lm_d=$_lm_ws/.luxmux
mkdir -p "$_lm_d" || exit 1
printf '%s\n' "$1" >"$_lm_d/.window.$$" && mv -f "$_lm_d/.window.$$" "$_lm_d/window" || exit 1
[ -f "$_lm_d/env" ] || exit 0
` + quoteFunc + `_lm_quote "$1"
{
	while IFS= read -r _lm_l || [ -n "$_lm_l" ]; do
		case $_lm_l in export\ ` + EnvWindow + `=*) ;; *) printf '%s\n' "$_lm_l" ;; esac
	done <"$_lm_d/env"
	printf 'export %s=%s\n' ` + EnvWindow + ` "$_lm_qv"
} >"$_lm_d/.env.$$" && chmod 644 "$_lm_d/.env.$$" && mv -f "$_lm_d/.env.$$" "$_lm_d/env"
`

func buildWrapper() string {
	names := strings.Join(append(slices.Clone(StaticEnv), ComputedEnv...), " ")
	return `# luxmux window start: LUXMUX_* facts, on_start/on_resume, then the shell.
_lm_s=$1 _lm_r=$2
shift 2
_lm_ws=${` + EnvTestWorkspace + `:-/workspace}
_lm_cg=${` + EnvTestCgroup + `:-/sys/fs/cgroup}
_lm_d=$_lm_ws/.luxmux
_lm_num() { case $1 in ''|*[!0-9]*) return 1 ;; esac; }
LUXMUX_CPU='' _lm_q='' _lm_p=''
{ read -r _lm_q _lm_p <"$_lm_cg/cpu.max"; } 2>/dev/null
if _lm_num "$_lm_q" && _lm_num "$_lm_p" && [ "$_lm_p" -gt 0 ]; then
	_lm_m=$((_lm_q * 1000 / _lm_p))
	_lm_f=$((_lm_m % 1000))
	LUXMUX_CPU=$((_lm_m / 1000))
	if [ "$_lm_f" -ne 0 ]; then
		case $_lm_f in ?) _lm_f=00$_lm_f ;; ??) _lm_f=0$_lm_f ;; esac
		while [ "${_lm_f%0}" != "$_lm_f" ]; do _lm_f=${_lm_f%0}; done
		LUXMUX_CPU=$LUXMUX_CPU.$_lm_f
	fi
fi
LUXMUX_MEMORY='' _lm_v=''
{ read -r _lm_v <"$_lm_cg/memory.max"; } 2>/dev/null
_lm_num "$_lm_v" && LUXMUX_MEMORY=$_lm_v
LUXMUX_DISK=
_lm_v=$(df -Pk "$_lm_ws" 2>/dev/null | { read -r _lm_x; read -r _lm_x _lm_b _lm_x; printf %s "$_lm_b"; })
if _lm_num "$_lm_v"; then
	LUXMUX_DISK=$((_lm_v * 1024))
else
	_lm_v=$(stat -f -c '%S %b' "$_lm_ws" 2>/dev/null)
	_lm_b=${_lm_v% *} _lm_n=${_lm_v#* }
	_lm_num "$_lm_b" && _lm_num "$_lm_n" && LUXMUX_DISK=$((_lm_b * _lm_n))
fi
LUXMUX_BRANCH_CHECKED_OUT=
_lm_g=${LUXMUX_REPO_DIR-}
if [ -n "$_lm_g" ] && [ -e "$_lm_g/.git" ] && command -v git >/dev/null 2>&1; then
	LUXMUX_BRANCH_CHECKED_OUT=$(git -C "$_lm_g" symbolic-ref -q --short HEAD 2>/dev/null ||
		git -C "$_lm_g" rev-parse --short -q --verify HEAD 2>/dev/null)
fi
mkdir -p "$_lm_d" 2>/dev/null
if [ -e "$_lm_d/started" ]; then
	LUXMUX_START=resume
else
	LUXMUX_START=start
	{ printf '' >"$_lm_d/started"; } 2>/dev/null
fi
LUXMUX_RUN_ID=${LUX_RUN_ID-}
export LUXMUX_CPU LUXMUX_MEMORY LUXMUX_DISK LUXMUX_BRANCH_CHECKED_OUT LUXMUX_START LUXMUX_RUN_ID
# The window name is only in the env file: renames change it there, never
# in a running process's environment. A name written while the file is
# made (luxmux writes .luxmux/window, then the file if it exists) is
# picked up by the second pass.
` + quoteFunc + `_lm_w='' _lm_pass=0
while [ -d "$_lm_d" ] && [ "$_lm_pass" -lt 2 ]; do
	_lm_pass=$((_lm_pass + 1)) _lm_o=$_lm_w _lm_w=
	{ IFS= read -r _lm_w <"$_lm_d/window"; } 2>/dev/null
	[ "$_lm_pass" -eq 2 ] && [ "$_lm_w" = "$_lm_o" ] && break
	_lm_t=$_lm_d/.env.$$
	if {
		for _lm_n in ` + names + `; do
			eval "_lm_quote \"\${$_lm_n-}\""
			printf 'export %s=%s\n' "$_lm_n" "$_lm_qv"
		done
		if [ -n "$_lm_w" ]; then
			_lm_quote "$_lm_w"
			printf 'export %s=%s\n' ` + EnvWindow + ` "$_lm_qv"
		fi
	} 2>/dev/null >"$_lm_t"; then
		chmod 644 "$_lm_t" 2>/dev/null
		mv -f "$_lm_t" "$_lm_d/env" 2>/dev/null || rm -f "$_lm_t" 2>/dev/null
	else
		rm -f "$_lm_t" 2>/dev/null
		break
	fi
done
_lm_run() {
	[ -n "$2" ] || return 0
	trap : INT QUIT
	sh -c "$2"
	_lm_rc=$?
	trap - INT QUIT
	[ "$_lm_rc" -eq 0 ] || printf 'luxmux: %s exited with status %s; starting the shell\n' "$1" "$_lm_rc" >&2
}
if [ "$LUXMUX_START" = start ]; then _lm_run on_start "$_lm_s"; else _lm_run on_resume "$_lm_r"; fi
[ $# -eq 0 ] || exec "$@"
# The shell luxmux has always started: exec replaces this process.
# shellcheck disable=SC2015
command -v bash >/dev/null && exec bash -l || exec sh -l
`
}
