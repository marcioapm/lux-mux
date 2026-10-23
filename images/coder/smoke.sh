#!/bin/sh
# The coder image's smoke test, run as the default image's is: as the image's
# USER, with an empty home, no network and no credentials. It runs the default
# image's smoke test first (the coder image must still pass it), then checks
# every toolchain it adds builds and runs a program offline. Each producer's
# exit status is checked before its output is compared (no set -e here).
set -u

# The pinned versions; bump them with the Dockerfile's ARGs.
GO_WANT=1.27.3
JDK_WANT=25
LLVM_WANT=23
RUST_WANT=1.99.1
RUST_EXTRA_WANT=1.94.0
PODMAN_WANT=5.7
BUN_WANT=1.4.2
# What headless Chromium (Playwright's Chrome for Testing) needs beyond the base image.
CHROMIUM_SONAMES="libatk-1.0.so.0 libatk-bridge-2.0.so.0 libatspi.so.0 libXcomposite.so.1 libXdamage.so.1 libcups.so.2"

fail() {
	echo "smoke: FAIL: $*" >&2
	exit 1
}

sh /smoke-default.sh || fail "the default image's smoke test failed"

for tool in go java javac rustc cargo rustup bun bunx clang clang++ ld.lld clang-format clang-tidy clangd cmake ninja diff-guard; do
	command -v "$tool" >/dev/null 2>&1 || fail "$tool is not on PATH"
	bash -lc "command -v $tool" >/dev/null 2>&1 || fail "$tool is not on the login shell's PATH"
	echo "ok: $tool on PATH"
done
for tool in clang-format clang-tidy clangd ld.lld cmake ninja; do
	"$tool" --version >/dev/null 2>&1 || fail "'$tool --version' exited non-zero"
	echo "ok: $tool --version"
done

v=$(go version 2>/dev/null) || fail "'go version' exited non-zero"
case "$v" in "go version go$GO_WANT "*) echo "ok: $v" ;; *) fail "go is '$v', want go$GO_WANT" ;; esac
v=$(java -version 2>&1) || fail "'java -version' exited non-zero"
case "$v" in *"version \"$JDK_WANT".*) echo "ok: java $JDK_WANT" ;; *) fail "java is '$v', want $JDK_WANT" ;; esac
[ -x "${JAVA_HOME:-/nonexistent}/bin/javac" ] || fail "JAVA_HOME '${JAVA_HOME:-}' has no bin/javac"
echo "ok: JAVA_HOME is $JAVA_HOME"
v=$(clang++ --version 2>/dev/null) || fail "'clang++ --version' exited non-zero"
case "$v" in *"clang version $LLVM_WANT".*) echo "ok: clang $LLVM_WANT" ;; *) fail "clang++ is '$v', want $LLVM_WANT.x" ;; esac
v=$(rustc --version 2>/dev/null) || fail "'rustc --version' exited non-zero"
case "$v" in "rustc $RUST_WANT "*) echo "ok: $v" ;; *) fail "default rustc is '$v', want $RUST_WANT" ;; esac
v=$(rustup toolchain list 2>/dev/null) || fail "'rustup toolchain list' exited non-zero"
case "$v" in *"$RUST_EXTRA_WANT-"*) echo "ok: rust $RUST_EXTRA_WANT installed" ;; *) fail "rust toolchain $RUST_EXTRA_WANT is not installed" ;; esac
v=$(bun --version 2>&1) || fail "'bun --version' exited non-zero: $v"
[ "$v" = "$BUN_WANT" ] || fail "bun is '$v', want $BUN_WANT"
echo "ok: bun $v"
[ ! -u /usr/local/bin/bun ] && [ ! -g /usr/local/bin/bun ] && [ ! -O /usr/local/bin/bun ] ||
	fail "/usr/local/bin/bun is setuid/setgid or owned by $(id -un), want a root-owned 0755 binary"

# Headless Chromium: Playwright downloads the browser; the image has its libraries.
ld=$(ldconfig -p 2>&1) || fail "'ldconfig -p' exited non-zero: $ld"
tab=$(printf '\t')
for so in $CHROMIUM_SONAMES; do
	# ldconfig -p lines: <tab><soname> (<abi>) => <path>
	case "$ld" in
	*"$tab$so ("*) ;;
	*) fail "$so is not in the dynamic linker cache (Chromium needs it)" ;;
	esac
done
echo "ok: Chromium's libraries resolve ($CHROMIUM_SONAMES)"

out=$(mktemp -d) || fail "mktemp -d failed"
trap 'rm -rf "$out"' 0
cd "$out" || fail "cd $out"

# check NAME WANT CMD...: CMD must exit 0 and print exactly WANT.
check() {
	name=$1 want=$2
	shift 2
	got=$("$@" 2>&1) || fail "$name exited non-zero: $got"
	[ "$got" = "$want" ] || fail "$name printed '$got', want '$want'"
	echo "ok: $name"
}

printf 'package main\nimport "fmt"\nfunc main() { fmt.Println("go-ok") }\n' >main.go &&
	printf 'module smoke\n\ngo 1.27\n' >go.mod || fail "writing the Go fixture failed"
check "go run" go-ok go run .

printf 'public class Smoke { public static void main(String[] a) { System.out.println("java-ok"); } }\n' >Smoke.java ||
	fail "writing the Java fixture failed"
javac Smoke.java >/dev/null 2>&1 || fail "javac failed"
check "javac + java" java-ok java -cp . Smoke

printf '#include <iostream>\n#include <ranges>\nint main() { for (int i : std::views::iota(0, 1)) std::cout << "cxx-ok" << i << "\\n"; }\n' >m.cpp ||
	fail "writing the C++ fixture failed"
clang++ -std=c++23 -fuse-ld=lld m.cpp -o m-gnu >/dev/null 2>&1 || fail "clang++ (libstdc++, lld) failed"
check "clang++ with libstdc++ and lld" cxx-ok0 ./m-gnu
clang++ -std=c++23 -stdlib=libc++ m.cpp -o m-libcxx >/dev/null 2>&1 || fail "clang++ -stdlib=libc++ failed"
check "clang++ with libc++" cxx-ok0 ./m-libcxx

cargo new --quiet --vcs none rs >/dev/null 2>&1 || fail "cargo new failed"
check "cargo run" 'Hello, world!' cargo run --quiet --offline --manifest-path rs/Cargo.toml
printf '[toolchain]\nchannel = "%s"\n' "$RUST_EXTRA_WANT" >rs/rust-toolchain.toml || fail "writing rust-toolchain.toml failed"
# rustup picks the toolchain from the current directory, so run from rs/.
v=$(cd rs && rustc --version 2>/dev/null) || fail "rustc on the pinned toolchain exited non-zero"
case "$v" in "rustc $RUST_EXTRA_WANT "*) ;; *) fail "rust-toolchain.toml selected '$v', want $RUST_EXTRA_WANT" ;; esac
(cd rs && cargo build --quiet --offline >/dev/null 2>&1) ||
	fail "cargo build on the pinned $RUST_EXTRA_WANT toolchain failed (it must not need a download)"
echo "ok: cargo build on the pinned $RUST_EXTRA_WANT toolchain"

git init -q g && cd g || fail "git init failed"
git -c user.email=s@s -c user.name=s commit -q --allow-empty -m base || fail "git commit failed"
mkdir -p tests && printf 'import inspect\nx = inspect.getsource(f)\n' >tests/test_x.py ||
	fail "writing the diff-guard fixture failed"
if diff-guard >/dev/null 2>&1; then fail "diff-guard passed a source-inspection test"; fi
rm -rf tests || fail "rm tests failed"
diff-guard >/dev/null 2>&1 || fail "diff-guard failed a clean tree"
cd .. || fail "cd .."
echo "ok: diff-guard flags and clears"

# Toolchain caches and stores must stay out of the persisted home, like npm's and uv's.
# shellcheck disable=SC2016 # expanded by the login shell, not here
for c in 'go env GOMODCACHE' 'go env GOCACHE' 'echo $CARGO_HOME' 'echo $RUSTUP_HOME' 'echo $GRADLE_USER_HOME'; do
	d=$(bash -lc "$c" 2>/dev/null) || fail "'$c' exited non-zero"
	case "$d" in
	"$HOME" | "$HOME"/* | '') fail "'$c' is '$d', want a path outside $HOME" ;;
	esac
done
for d in go .cargo .rustup .gradle .cache; do
	[ ! -e "$HOME/$d" ] || fail "a toolchain cache was created at $HOME/$d"
done

# The nested engine. Nothing here starts a container: that needs a nested
# Run's grants (the nested proof in README.md).
for tool in podman docker podman-compose pasta passt newuidmap newgidmap fuse-overlayfs slirp4netns crun catatonit nft \
	psql pg_isready redis-cli sqlite3 socat nc dig file tree rsync shellcheck yq; do
	command -v "$tool" >/dev/null 2>&1 || fail "$tool is not on PATH"
done
echo "ok: engine and CLI tools on PATH"
v=$(podman --version 2>/dev/null) || fail "'podman --version' exited non-zero"
case "$v" in "podman version $PODMAN_WANT".*) echo "ok: $v" ;; *) fail "podman is '$v', want $PODMAN_WANT.x" ;; esac
check "docker shim (no notice on stderr)" "$v" docker --version
v=$(pasta --version 2>/dev/null) || fail "'pasta --version' exited non-zero"
echo "ok: $(echo "$v" | head -n 1)"

# AppArmor picks the profile by exec path: pasta must be passt's inode, not a symlink to it.
for p in pasta pasta.avx2; do
	t=/usr/bin/passt${p#pasta}
	[ -e "$t" ] || [ -e "/usr/bin/$p" ] || continue
	[ ! -L "/usr/bin/$p" ] || fail "/usr/bin/$p is a symlink, want a hard link to $t"
	[ "/usr/bin/$p" -ef "$t" ] || fail "/usr/bin/$p is not a hard link to $t"
	echo "ok: /usr/bin/$p is a hard link to $t"
done

check "newuidmap capability" "/usr/bin/newuidmap cap_setuid=ep" getcap /usr/bin/newuidmap
check "newgidmap capability" "/usr/bin/newgidmap cap_setgid=ep" getcap /usr/bin/newgidmap
# A file capability is as good as setuid in a nested Run: these two are the only ones.
c=$(getcap -r /usr /bin /sbin /opt 2>/dev/null | LC_ALL=C sort)
[ "$c" = "$(printf '/usr/bin/newgidmap cap_setgid=ep\n/usr/bin/newuidmap cap_setuid=ep')" ] ||
	fail "file capabilities are '$(echo "$c" | tr '\n' ' ')', want only newuidmap cap_setuid=ep and newgidmap cap_setgid=ep"
echo "ok: no other file capabilities"
# Unreadable directories are skipped; the build asserts the same as root.
s=$(find / -xdev -perm /6000 -type f 2>/dev/null)
[ -z "$s" ] || fail "setuid/setgid files: $(echo "$s" | tr '\n' ' ')"
echo "ok: no setuid or setgid files"
for f in /etc/subuid /etc/subgid; do
	[ "$(cat "$f")" = "$(printf 'agent:1:999\nagent:1001:64535')" ] ||
		fail "$f is '$(cat "$f")', want agent:1:999 and agent:1001:64535 (a lux Run maps ids 0-65535 only)"
done
echo "ok: subordinate ids inside 0-65535"

check "sqlite3" 2 sqlite3 :memory: 'select 1+1'
check "yq" 1 yq -r .a <<EOF
a: 1
EOF
printf '#!/bin/sh\necho shellcheck-ok\n' >ok.sh || fail "writing the shellcheck fixture failed"
check "shellcheck" "" shellcheck ok.sh

# runs NAME PREFIX CMD...: CMD must exit 0 and its first line start with PREFIX.
runs() {
	name=$1 prefix=$2
	shift 2
	got=$("$@" 2>&1) || fail "'$*' exited non-zero: $got"
	case "$(echo "$got" | head -n 1)" in
	"$prefix"*) echo "ok: $name" ;;
	*) fail "'$*' printed '$got', want a first line starting '$prefix'" ;;
	esac
}
runs psql "psql (PostgreSQL) " psql --version
runs pg_isready "pg_isready (PostgreSQL) " pg_isready --version
runs redis-cli "redis-cli " redis-cli --version
runs socat "socat by " socat -V
runs nc "OpenBSD netcat " nc -h
runs dig "DiG " dig -v
runs file "file-" file --version
runs tree "tree v" tree --version
runs rsync "rsync  version " rsync --version

# podman re-execs itself in a user namespace for everything but --version, and
# a plain `docker run` (CI) refuses one under its seccomp and AppArmor
# profiles. There these checks are skipped, unless SMOKE_ENGINE=1 requires them.
if unshare --user true 2>/dev/null; then
	# Exactly as a Run starts it: no XDG_RUNTIME_DIR, nothing in the home.
	v=$(env -u XDG_RUNTIME_DIR -u CONTAINERS_CONF -u CONTAINERS_STORAGE_CONF podman info \
		--format '{{.Host.RemoteSocket.Path}} {{.Store.GraphRoot}} {{.Host.NetworkBackend}} {{.Host.Pasta.Executable}}' 2>&1) ||
		fail "'podman info' without XDG_RUNTIME_DIR failed: $v"
	case "$v" in
	*" $HOME/.local/share/containers/storage netavark /usr/bin/pasta") echo "ok: podman info ($v)" ;;
	*) fail "podman info is '$v', want storage under $HOME/.local/share/containers, netavark and /usr/bin/pasta" ;;
	esac
	# Short names resolve to Docker Hub, as under Docker (registries.conf.d drop-in).
	v=$(podman info --format '{{range .Registries.search}}{{.}} {{end}}' 2>&1) ||
		fail "'podman info' (search registries) failed: $v"
	[ "$v" = "docker.io " ] || fail "podman's search registries are '$v', want only docker.io"
	echo "ok: podman search registries: docker.io"
	v=$(docker compose version 2>&1) || fail "'docker compose version' exited non-zero: $v"
	case "$v" in
	*"podman-compose version "*) echo "ok: docker compose is podman-compose" ;;
	*) fail "'docker compose version' printed '$v'" ;;
	esac
elif [ "${SMOKE_ENGINE:-0}" = 1 ]; then
	fail "SMOKE_ENGINE=1 but no user namespace can be created here"
else
	echo "skip: podman info, search registries and docker compose (no user namespaces here; SMOKE_ENGINE=1 requires them)"
fi
echo "smoke: PASS"
