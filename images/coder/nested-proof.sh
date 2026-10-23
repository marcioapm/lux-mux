#!/bin/bash
# The coder image's nested proof: run inside a lux nested Run (or a stand-in
# with the same podman flags, see README.md) to check the image's rootless
# Podman starts containers on the native-overlay engine store, gives them a
# network, publishes a port and runs a compose stack. Not part of CI: it needs
# sandbox.nestedContainers' grants.
#
# Started as root, it first does the lux shim's part, handing the directories
# the engine-store mount created to agent, then reruns itself as agent with no
# XDG_RUNTIME_DIR, as a Run starts. Started as agent (in a real Run), it just
# runs. Images come from the registry by digest, or, with /images.tar present,
# from that archive: tag them localhost/nested-proof/{alpine:3.22,postgres:18-alpine}
# after pulling the digests below, and pass ALPINE_ID and PG_ID (the image ids
# on this architecture), since an archive keeps no repo digest.
#
# The engine-store mount at ~/.local/share/containers is required. Only for the
# fallback demo, ALLOW_NO_STORE_MOUNT=1 accepts its absence and asserts the
# fuse-overlayfs fallback instead; that run does not prove a lux placement.
set -u
if [ "$(id -u)" = 0 ]; then
	for d in /home/agent/.local /home/agent/.local/share /home/agent/.local/share/containers; do
		[ ! -e "$d" ] || chown agent:agent "$d" || exit 1
	done
	exec setpriv --reuid=1000 --regid=1000 --init-groups env -u XDG_RUNTIME_DIR HOME=/home/agent USER=agent LOGNAME=agent bash "$0"
fi
ALPINE=docker.io/library/alpine:3.22@sha256:d406446bc05b82480756ce8f1287964d28768c23955fa9480d2a74a2eae7a654
PG=docker.io/library/postgres:18-alpine@sha256:898c60cb390d1085bc97bdd6a1ae968757421df394b071848b52b1c09c89d6a5
STORE=$HOME/.local/share/containers
step() { echo; echo "### $*"; }
die() { echo "NESTED FAIL: $*"; exit 1; }

# Every resource is named after this run's id, so cleanup removes only its own.
run_id=$(od -An -N4 -tx1 /dev/urandom | tr -d ' \n') && [ -n "$run_id" ] || die "run id"
probe=nested-proof-probe-$run_id pg=nested-proof-pg-$run_id project=nested-proof-$run_id
probe_started=0 pg_started=0 compose_file='' compose_dir=''
cleanup() {
	local rc=0
	if [ -n "$compose_file" ]; then
		timeout --kill-after=5s 60s docker compose -f "$compose_file" down -t 1 >/dev/null 2>&1 || rc=1
	fi
	if [ "$probe_started" = 1 ]; then
		timeout --kill-after=5s 30s podman rm -f -t 0 --ignore "$probe" >/dev/null || rc=1
	fi
	if [ "$pg_started" = 1 ]; then
		timeout --kill-after=5s 30s podman rm -f -t 0 --ignore "$pg" >/dev/null || rc=1
	fi
	if [ -n "$compose_dir" ]; then rm -rf "$compose_dir" || rc=1; fi
	if [ "$rc" = 0 ]; then probe_started=0 pg_started=0 compose_file='' compose_dir=''; fi
	return "$rc"
}
on_exit() {
	local rc=$?
	trap - EXIT
	cleanup || { echo "NESTED FAIL: cleanup"; [ "$rc" != 0 ] || rc=1; }
	exit "$rc"
}
trap on_exit EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

step "who, env"
id; echo "XDG_RUNTIME_DIR=${XDG_RUNTIME_DIR-<unset>}"; uname -m

step "engine store mount"
if findmnt -no SOURCE,FSTYPE,TARGET --mountpoint "$STORE"; then
	store_mounted=1
elif [ "${ALLOW_NO_STORE_MOUNT:-0}" = 1 ]; then
	store_mounted=0
	echo "FALLBACK DEMO (ALLOW_NO_STORE_MOUNT=1): no mount at $STORE; expecting fuse-overlayfs"
else
	die "no engine-store mount at $STORE (ALLOW_NO_STORE_MOUNT=1 runs the fallback demo)"
fi

step "podman info"
info=$(podman info --format 'driver={{.Store.GraphDriverName}} root={{.Store.GraphRoot}} opts={{.Store.GraphOptions}} backing={{index .Store.GraphStatus "Backing Filesystem"}} native={{index .Store.GraphStatus "Native Overlay Diff"}} net={{.Host.NetworkBackend}} pasta={{.Host.Pasta.Executable}} runtime={{.Host.OCIRuntime.Name}} rootless={{.Host.Security.Rootless}}') ||
	die "podman info: $info"
echo "$info"
root=$(podman info --format '{{.Store.GraphRoot}}') || die "podman info graphroot"
case "$info" in "driver=overlay "*) ;; *) die "storage driver is not overlay" ;; esac
case "$info" in *mount_program*) die "a mount_program is configured" ;; esac
if [ "$store_mounted" = 1 ]; then
	case "$root" in "$STORE"/*) ;; *) die "graphroot $root is not beneath $STORE" ;; esac
	fs=$(findmnt -no TARGET -T "$root") || die "findmnt -T $root"
	[ "$fs" = "$STORE" ] || die "graphroot $root is on $fs, not the engine-store mount"
	case "$info" in *" native=true "*) ;; *) die "overlay is not native on the engine-store mount" ;; esac
else
	case "$info" in *" native=false "*) ;; *) die "fallback demo: want native=false off the mount" ;; esac
fi

if [ -e /images.tar ]; then
	step "podman load /images.tar"
	[ -n "${ALPINE_ID:-}" ] || die "ALPINE_ID is required with /images.tar"
	[ -n "${PG_ID:-}" ] || die "PG_ID is required with /images.tar"
	timeout --kill-after=10s 300s podman load -q -i /images.tar || die "podman load"
	ALPINE=localhost/nested-proof/alpine:3.22 PG=localhost/nested-proof/postgres:18-alpine
	for i in "$ALPINE=$ALPINE_ID" "$PG=$PG_ID"; do
		want=${i#*=} got=$(podman image inspect --format '{{.Id}}' "${i%%=*}") || die "${i%%=*} not in /images.tar"
		[ "$got" = "$want" ] || die "${i%%=*} is $got, want $want"
	done
	podman images
fi

step "podman run alpine; overlay is $([ "$store_mounted" = 1 ] && echo native || echo fuse-overlayfs)"
probe_started=1
timeout --kill-after=10s 300s podman run -d --name "$probe" "$ALPINE" sleep 120 >/dev/null || die "podman run $probe"
out=$(timeout --kill-after=5s 60s podman exec "$probe" echo nested-ok) || die "podman exec: $out"
echo "$out"; [ "$out" = nested-ok ] || die "want nested-ok"
if [ "$store_mounted" = 1 ]; then
	! pgrep -x fuse-overlayfs >/dev/null || die "fuse-overlayfs is running on the engine-store mount"
	echo "no fuse-overlayfs process"
else
	pgrep -x fuse-overlayfs >/dev/null || die "fallback demo: no fuse-overlayfs process"
	echo "fuse-overlayfs process running"
fi
timeout --kill-after=5s 30s podman rm -f -t 0 "$probe" >/dev/null || die "podman rm $probe"
probe_started=0

step "docker run (shim), default network: a non-loopback IPv4 address and a default route"
# shellcheck disable=SC2016 # expanded by the inner container's sh
timeout --kill-after=10s 120s docker run --rm "$ALPINE" sh -c '
	addrs=$(ip -4 -o addr show) || { echo "ip addr failed"; exit 1; }
	printf "%s\n" "$addrs" | grep -v " lo " | grep -q " inet " || { echo "no non-loopback IPv4 address: $addrs"; exit 1; }
	routes=$(ip -4 route show) || { echo "ip route failed"; exit 1; }
	default=$(printf "%s\n" "$routes" | grep "^default ") || { echo "no default route: $routes"; exit 1; }
	printf "%s\n" "$addrs" | grep -v " lo "; printf "%s\n" "$default"' ||
	die "inner container network"

step "pasta exec path and inode"
ls -li /usr/bin/pasta /usr/bin/passt

step "postgres -p 55432:5432"
pg_started=1
timeout --kill-after=10s 300s podman run -d --name "$pg" -p 55432:5432 -e POSTGRES_PASSWORD=x "$PG" >/dev/null ||
	die "postgres run"
for _ in $(seq 60); do pg_isready -t 3 -h 127.0.0.1 -p 55432 >/dev/null 2>&1 && break; sleep 1; done
pg_isready -t 3 -h 127.0.0.1 -p 55432 || { podman logs "$pg" 2>&1 | tail; die "pg_isready"; }
# The image's init restarts the server once; retry until a query lands.
query_ok=0
for _ in $(seq 30); do
	if r=$(PGCONNECT_TIMEOUT=3 PGPASSWORD=x psql -h 127.0.0.1 -p 55432 -U postgres -v ON_ERROR_STOP=1 -tAc 'select 1' 2>&1); then
		query_ok=1
		break
	fi
	sleep 1
done
echo "psql select 1 -> $r"
[ "$query_ok" = 1 ] && [ "$r" = 1 ] || die "psql: $r"
timeout --kill-after=5s 30s podman rm -f -t 0 "$pg" >/dev/null || die "podman rm $pg"
pg_started=0

step "docker compose up (postgres + client resolving 'db')"
marker=nested-compose-query-ok-$run_id
compose_dir=$(mktemp -d /tmp/nested-proof.XXXXXX) || die "mktemp"
compose_file=$compose_dir/compose.yaml
cat >"$compose_file" <<EOF || die "writing $compose_file"
name: $project
services:
  db:
    image: $PG
    environment:
      POSTGRES_PASSWORD: x
  client:
    image: $PG
    depends_on: [db]
    environment:
      PGPASSWORD: x
      PGCONNECT_TIMEOUT: "3"
    command:
      - sh
      - -c
      - >-
        getent hosts db &&
        until pg_isready -q -t 3 -h db; do sleep 1; done && sleep 2 &&
        r=\$\$(psql -h db -U postgres -v ON_ERROR_STOP=1 -tAc 'select 40+2') &&
        echo "select 40+2 -> \$\$r" && [ "\$\$r" = 42 ] && echo $marker
EOF
cd "$compose_dir" || die "cd $compose_dir"
timeout --kill-after=10s 180s docker compose -f "$compose_file" up --abort-on-container-exit --exit-code-from client \
	>"$compose_dir/up.log" 2>&1
rc=$?
sed 's/\x1b\[[0-9;]*m//g' "$compose_dir/up.log" | grep -v '^\[db\]' | tail -15
echo "compose exit: $rc"
[ "$rc" = 0 ] || die "compose up failed or timed out: $rc"
client_log=$(timeout --kill-after=5s 60s docker compose -f "$compose_file" logs client 2>&1) ||
	die "compose logs client: $client_log"
grep -Eq "(^|[[:space:]])$marker\$" <<<"$(sed 's/\x1b\[[0-9;]*m//g; s/\r$//' <<<"$client_log")" ||
	die "compose client did not confirm its query (no $marker in its log)"
echo "client log has $marker"
cd / || die "cd /"

step "cleanup"
removed_dir=$compose_dir
cleanup || die "cleanup"
[ ! -e "$removed_dir" ] || die "$removed_dir left"
left=$(podman ps -a --format '{{.Names}}' --filter "name=$run_id") || die "podman ps"
[ -z "$left" ] || die "containers left: $left"
left=$(podman network ls --format '{{.Name}}' --filter "name=$run_id") || die "podman network ls"
[ -z "$left" ] || die "networks left: $left"
echo "removed this run's containers, network and $removed_dir"
echo
if [ "$store_mounted" = 1 ]; then echo "NESTED PASS"; else echo "FALLBACK DEMO PASS (no engine-store mount: not a proof of a lux placement)"; fi
