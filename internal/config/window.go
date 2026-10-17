package config

import (
	"path"
	"slices"
	"strconv"
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

// luxShimEnv is what lux v0.1.30's shim sets in a Run's environment
// (internal/shim/shim.go, Shim.environment): LUX_RUN_ID, LUX_EPOCH,
// LUX_INPUTS, and LUX_SERVICE_<NAME> plus LUX_SERVICE_<NAME>_URL per service.
var luxShimEnv = []string{"LUX_RUN_ID", "LUX_EPOCH", "LUX_INPUTS"}

// luxSecretRefused reports whether an env secret named name would replace a
// variable lux's shim sets: environment() applies env secrets after its own
// variables, and LUX_RUN_ID is copied into the world-readable env file.
// Other LUX_ names (LUX_API_KEY for the lux CLI in a window) are allowed.
func luxSecretRefused(name string) bool {
	return slices.Contains(luxShimEnv, name) || strings.HasPrefix(name, "LUX_SERVICE_")
}

// MaxEnvString bounds one workload environment string, NAME=value and its
// NUL. Linux refuses a string over MAX_ARG_STRLEN (32 pages, 128 KiB with
// 4 KiB pages) with E2BIG; 8 KiB of headroom stays clear of that limit.
const MaxEnvString = 120 << 10

func envStringSize(name, value string) int { return len(name) + 1 + len(value) + 1 }

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

// The files under the state volume's .luxmux directory. FactsFile and
// WindowFile are complete sh fragments (export lines, values single-quoted,
// so a value may span lines); EnvFile is always regenerated whole as facts
// followed by the window fragment, never edited line by line. LockFile is
// never replaced or removed: both writers lock its inode with flock(1), and
// the kernel drops a lock when its holder's descriptor closes, crash
// included.
const (
	FactsFile  = "facts.sh"
	WindowFile = "window.sh"
	EnvFile    = "env"
	LockFile   = "env.lock"
)

// lockSeconds bounds the wait for the lock. WindowNameScript runs within
// the mux's 5 s exec deadline; the wrapper delays the prompt by at most this.
const lockSeconds = 2

// probeSeconds bounds the whole optional probe group (df, stat, git).
const probeSeconds = "1"

// StartWrapper is the POSIX sh script every window's Run starts with. Its
// text is the same for every profile: on_start ($1) and on_resume ($2)
// are arguments, only ever run as sh -c "$cmd", never part of the text.
// It computes the placement's facts (each empty when its source is
// missing, or when it cannot be read within probeSeconds), decides start or
// resume by a marker on the state volume (written before on_start runs:
// a failed or interrupted on_start is not retried, the next placement
// runs on_resume), publishes the facts and env files under the lock, runs
// the command, then execs the shell: "$@" when the profile has one, else
// bash -l or sh -l as luxmux always has. Without commands it prints
// nothing.
var StartWrapper = buildWrapper()

// WindowNameScript is what luxmux execs in a Run (lux's exec API) when a
// window takes it or is renamed. Under the lock it publishes $1 as the
// window fragment and, once a wrapper has published facts, regenerates the
// env file from the current facts and that fragment. The name is never part
// of the text. It exits non-zero when nothing could be published (no flock,
// lock timeout, read-only volume), so luxmux retries at the next attach.
var WindowNameScript = `_lm_ws=${` + EnvTestWorkspace + `:-/workspace}
_lm_d=$_lm_ws/.luxmux
mkdir -p "$_lm_d" 2>/dev/null
` + quoteFunc + lockFunc + putFunc + `_lm_lock || exit 1
_lm_rc=1 _lm_tw=$_lm_d/.lm-tmp.window.$$ _lm_te=$_lm_d/.lm-tmp.env.$$
_lm_quote "$1"
if printf 'export %s=%s\n' ` + EnvWindow + ` "$_lm_qv" 2>/dev/null >"$_lm_tw" && _lm_put "$_lm_tw" "$_lm_d/` + WindowFile + `"; then
	_lm_rc=0
	if [ -e "$_lm_d/` + FactsFile + `" ]; then
		if cat "$_lm_d/` + FactsFile + `" "$_lm_d/` + WindowFile + `" 2>/dev/null >"$_lm_te"; then
			_lm_put "$_lm_te" "$_lm_d/` + EnvFile + `" || _lm_rc=1
		else
			rm -f "$_lm_te" 2>/dev/null
			_lm_rc=1
		fi
	fi
else
	rm -f "$_lm_tw" 2>/dev/null
fi
_lm_unlock
exit "$_lm_rc"
`

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

// lockFunc defines _lm_lock and _lm_unlock: an exclusive flock(1) on fd 9,
// open on LockFile. util-linux flock waits with -w and reports a timeout as
// -E's 75; any other failure (BusyBox flock has neither option) falls back
// to bounded -n retries. Without flock, or when the file cannot be opened
// (read-only volume), _lm_lock fails and the caller publishes nothing.
// Temporary files are only made under the lock, so the holder removes any
// left by a holder that died.
var lockFunc = buildLockFunc(strconv.Itoa(lockSeconds), strconv.Itoa(10*lockSeconds))

func buildLockFunc(seconds, retries string) string {
	return `_lm_lock() {
	command -v flock >/dev/null 2>&1 || return 1
	{ command exec 9>>"$_lm_d/` + LockFile + `"; } 2>/dev/null || return 1
	flock -E 75 -w ` + seconds + ` 9 >/dev/null 2>&1
	case $? in
	0) ;;
	75) _lm_unlock; return 1 ;;
	*)
		_lm_li=0
		until flock -n 9 >/dev/null 2>&1; do
			_lm_li=$((_lm_li + 1))
			[ "$_lm_li" -le ` + retries + ` ] || { _lm_unlock; return 1; }
			sleep 0.1 2>/dev/null || { sleep 1 2>/dev/null; _lm_li=$((_lm_li + 9)); }
		done
		;;
	esac
	rm -f "$_lm_d"/.lm-tmp.* 2>/dev/null
	return 0
}
_lm_unlock() { command exec 9>&-; }
`
}

// putFunc defines _lm_put TMP DEST: mode 0644, then rename over DEST, so a
// reader holding DEST open keeps the complete old file. TMP is removed on
// failure.
const putFunc = `_lm_put() {
	{ chmod 644 "$1" && mv -f "$1" "$2"; } 2>/dev/null || { rm -f "$1" 2>/dev/null; return 1; }
}
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
` + quoteFunc + lockFunc + putFunc + `LUXMUX_CPU='' _lm_q='' _lm_p=''
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
# Optional probes share one background job and one watchdog. The trap kills
# the currently running child before the job exits, so a timed-out probe
# cannot leave df, stat or git behind.
LUXMUX_DISK='' LUXMUX_BRANCH_CHECKED_OUT='' _lm_t=''
for _lm_t in "${TMPDIR:-/tmp}" "$_lm_d" ''; do
	[ -n "$_lm_t" ] || break
	_lm_t=$_lm_t/.luxmux-probe.$$
	{ rm -rf "$_lm_t" && mkdir -m 700 "$_lm_t"; } 2>/dev/null && break
done
if [ -n "$_lm_t" ]; then
	_lm_external() {
		_lm_cmd=$1; shift
		_lm_path=$PATH
		while :; do
			_lm_dir=${_lm_path%%:*}
			[ -n "$_lm_dir" ] || _lm_dir=.
			if [ -x "$_lm_dir/$_lm_cmd" ]; then exec "$_lm_dir/$_lm_cmd" "$@"; fi
			case $_lm_path in *:*) _lm_path=${_lm_path#*:} ;; *) return 127 ;; esac
		done
	}
	_lm_probe_group() {
		trap '[ -n "${_lm_child-}" ] && { kill -KILL "$_lm_child" 2>/dev/null; wait "$_lm_child" 2>/dev/null; }; exit 124' TERM INT
		_lm_external df -Pk "$_lm_ws" >"$_lm_t/df" 2>/dev/null & _lm_child=$!; wait "$_lm_child"; _lm_df=$?
		if [ "$_lm_df" -ne 0 ]; then
			_lm_external stat -f -c '%S %b' "$_lm_ws" >"$_lm_t/stat" 2>/dev/null & _lm_child=$!; wait "$_lm_child"; _lm_stat=$?
		else
			_lm_stat=1
		fi
		_lm_g=${LUXMUX_REPO_DIR-}
		_lm_git=1
		if [ -n "$_lm_g" ] && [ -e "$_lm_g/.git" ] && command -v git >/dev/null 2>&1; then
			_lm_external git -C "$_lm_g" symbolic-ref -q --short HEAD >"$_lm_t/git" 2>/dev/null & _lm_child=$!; wait "$_lm_child"; _lm_git=$?
			if [ "$_lm_git" -ne 0 ]; then
				_lm_external git -C "$_lm_g" rev-parse --short -q --verify HEAD >"$_lm_t/git" 2>/dev/null & _lm_child=$!; wait "$_lm_child"; _lm_git=$?
			fi
		fi
		printf '%s %s %s\n' "$_lm_df" "$_lm_stat" "$_lm_git" >"$_lm_t/status"
	}
	_lm_probe_group & _lm_pg=$!
	(
		trap 'kill "$_lm_sleep" 2>/dev/null; wait "$_lm_sleep" 2>/dev/null; exit' TERM INT
		sleep ` + probeSeconds + ` & _lm_sleep=$!
		wait "$_lm_sleep"
		kill "$_lm_pg" 2>/dev/null
	) & _lm_wd=$!
	wait "$_lm_pg"; _lm_prc=$?
	kill "$_lm_wd" 2>/dev/null; wait "$_lm_wd" 2>/dev/null
	if [ "$_lm_prc" -eq 0 ] && read -r _lm_df _lm_stat _lm_git <"$_lm_t/status" 2>/dev/null; then
		_lm_b='' _lm_n=''
		[ "$_lm_df" -eq 0 ] && { read -r _lm_x; read -r _lm_x _lm_b _lm_x; } <"$_lm_t/df" 2>/dev/null
		if _lm_num "$_lm_b"; then
			LUXMUX_DISK=$((_lm_b * 1024))
		elif [ "$_lm_stat" -eq 0 ]; then
			{ read -r _lm_b _lm_n; } <"$_lm_t/stat" 2>/dev/null
			_lm_num "$_lm_b" && _lm_num "$_lm_n" && LUXMUX_DISK=$((_lm_b * _lm_n))
		fi
		if [ "$_lm_git" -eq 0 ]; then IFS= read -r LUXMUX_BRANCH_CHECKED_OUT <"$_lm_t/git" 2>/dev/null; fi
	fi
	rm -rf "$_lm_t" 2>/dev/null
fi
mkdir -p "$_lm_d" 2>/dev/null
if [ -e "$_lm_d/started" ]; then
	LUXMUX_START=resume
else
	LUXMUX_START=start
	{ printf '' >"$_lm_d/started"; } 2>/dev/null
fi
# Only a value shaped like lux's Run ids is copied: a secret that took
# LUX_RUN_ID's place never reaches the world-readable env file.
LUXMUX_RUN_ID=
case ${LUX_RUN_ID-} in run_*[!a-z0-9]*) ;; run_?*) LUXMUX_RUN_ID=$LUX_RUN_ID ;; esac
export LUXMUX_CPU LUXMUX_MEMORY LUXMUX_DISK LUXMUX_BRANCH_CHECKED_OUT LUXMUX_START LUXMUX_RUN_ID
_lm_facts() {
	for _lm_n in ` + names + `; do
		eval "_lm_quote \"\${$_lm_n-}\""
		printf 'export %s=%s\n' "$_lm_n" "$_lm_qv" || return 1
	done
}
# The window name is only in the env file: renames change it there, never
# in a running process's environment. Under the lock the window fragment
# is the latest name WindowNameScript published.
if _lm_lock; then
	_lm_tf=$_lm_d/.lm-tmp.facts.$$ _lm_te=$_lm_d/.lm-tmp.env.$$
	if _lm_facts 2>/dev/null >"$_lm_tf" && _lm_put "$_lm_tf" "$_lm_d/` + FactsFile + `"; then
		if { _lm_facts && { [ ! -e "$_lm_d/` + WindowFile + `" ] || cat "$_lm_d/` + WindowFile + `"; }; } 2>/dev/null >"$_lm_te"; then
			_lm_put "$_lm_te" "$_lm_d/` + EnvFile + `"
		else
			rm -f "$_lm_te" 2>/dev/null
		fi
	else
		rm -f "$_lm_tf" 2>/dev/null
	fi
	_lm_unlock
fi
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
