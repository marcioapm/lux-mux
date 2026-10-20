#!/bin/sh
# The coder image's smoke test, run as the default image's is: as the image's
# USER, with an empty home, no network and no credentials. It runs the default
# image's smoke test first (the coder image must still pass it), then checks
# every toolchain it adds builds and runs a program offline.
set -u

fail() {
	echo "smoke: FAIL: $*" >&2
	exit 1
}

sh /smoke-default.sh || fail "the default image's smoke test failed"

for tool in go java javac rustc cargo rustup clang clang++ ld.lld clang-format clang-tidy cmake ninja diff-guard; do
	command -v "$tool" >/dev/null 2>&1 || fail "$tool is not on PATH"
	bash -lc "command -v $tool" >/dev/null 2>&1 || fail "$tool is not on the login shell's PATH"
	echo "ok: $tool on PATH"
done

v=$(go version 2>/dev/null) || fail "'go version' exited non-zero"
case "$v" in "go version go1."*) echo "ok: $v" ;; *) fail "go is '$v'" ;; esac
v=$(java -version 2>&1 | head -n 1)
case "$v" in *'"25'*) echo "ok: $v" ;; *) fail "java is '$v', want 25" ;; esac
v=$(clang++ --version 2>/dev/null | head -n 1)
case "$v" in *"clang version 23."*) echo "ok: $v" ;; *) fail "clang++ is '$v', want 23.x" ;; esac
rustup toolchain list | grep -q '^1\.94\.0-' || fail "rust toolchain 1.94.0 (llm-proxy's pin) is not installed"
echo "ok: rust 1.94.0 installed"
v=$(rustc --version 2>/dev/null) || fail "'rustc --version' exited non-zero"
echo "ok: default $v"

out=$(mktemp -d) || fail "mktemp -d failed"
trap 'rm -rf "$out"' 0
cd "$out" || fail "cd $out"

printf 'package main\nimport "fmt"\nfunc main() { fmt.Println("go-ok") }\n' >main.go
printf 'module smoke\n\ngo 1.27\n' >go.mod
[ "$(go run . 2>&1)" = go-ok ] || fail "go run did not print go-ok"
echo "ok: go run"

printf 'public class Smoke { public static void main(String[] a) { System.out.println("java-ok"); } }\n' >Smoke.java
javac Smoke.java >/dev/null 2>&1 || fail "javac failed"
[ "$(java -cp . Smoke)" = java-ok ] || fail "java did not print java-ok"
echo "ok: javac + java"

printf '#include <iostream>\n#include <ranges>\nint main() { for (int i : std::views::iota(0, 1)) std::cout << "cxx-ok" << i << "\\n"; }\n' >m.cpp
clang++ -std=c++23 -fuse-ld=lld m.cpp -o m-gnu >/dev/null 2>&1 || fail "clang++ (libstdc++, lld) failed"
[ "$(./m-gnu)" = cxx-ok0 ] || fail "clang++ libstdc++ binary did not print cxx-ok0"
clang++ -std=c++23 -stdlib=libc++ m.cpp -o m-libcxx >/dev/null 2>&1 || fail "clang++ -stdlib=libc++ failed"
[ "$(./m-libcxx)" = cxx-ok0 ] || fail "clang++ libc++ binary did not print cxx-ok0"
echo "ok: clang++ with libstdc++ and libc++"

cargo new --quiet --vcs none rs >/dev/null 2>&1 || fail "cargo new failed"
(cd rs && cargo run --quiet --offline 2>/dev/null) | grep -qx 'Hello, world!' || fail "cargo run failed"
(cd rs && printf '[toolchain]\nchannel = "1.94.0"\n' >rust-toolchain.toml && cargo build --quiet --offline 2>/dev/null) ||
	fail "cargo build on the pinned 1.94.0 toolchain failed (it must not need a download)"
echo "ok: cargo run, default and pinned toolchain"

git init -q g && cd g || fail "git init failed"
git -c user.email=s@s -c user.name=s commit -q --allow-empty -m base || fail "git commit failed"
mkdir -p tests && printf 'import inspect\nx = inspect.getsource(f)\n' >tests/test_x.py
if diff-guard >/dev/null 2>&1; then fail "diff-guard passed a source-inspection test"; fi
rm -rf tests && diff-guard >/dev/null 2>&1 || fail "diff-guard failed a clean tree"
cd .. || fail "cd .."
echo "ok: diff-guard flags and clears"

# Toolchain caches must stay out of the persisted home, like npm's and uv's.
for c in 'go env GOMODCACHE' 'go env GOCACHE' 'echo $CARGO_HOME' 'echo $GRADLE_USER_HOME'; do
	d=$(bash -lc "$c" 2>/dev/null) || fail "'$c' exited non-zero"
	case "$d" in
	"$HOME" | "$HOME"/* | '') fail "'$c' is '$d', want a path outside $HOME" ;;
	esac
done
[ ! -e "$HOME/go" ] && [ ! -e "$HOME/.cargo" ] && [ ! -e "$HOME/.gradle" ] && [ ! -e "$HOME/.cache" ] ||
	fail "a toolchain cache was created under $HOME"
echo "smoke: PASS"
