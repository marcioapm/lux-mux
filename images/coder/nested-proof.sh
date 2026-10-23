#!/bin/bash
# The coder image's nested proof: run inside a lux nested Run (or a stand-in
# with the same podman flags, see README.md) to check the image's rootless
# Podman starts containers, publishes a port and runs a compose stack. Not part
# of CI: it needs sandbox.nestedContainers' grants.
#
# Started as root, it first does the lux shim's part, handing the directories
# the engine-store mount created to agent, then reruns itself as agent with no
# XDG_RUNTIME_DIR, as a Run starts. Started as agent (in a real Run), it just
# runs. Images come from the registry by digest, or, with /images.tar present,
# from that archive: tag them localhost/nested-proof/{alpine:3.22,postgres:18-alpine}
# after pulling the digests below, and pass ALPINE_ID/PG_ID (the image ids on
# this architecture) to have them checked, since an archive keeps no repo digest.
set -u
if [ "$(id -u)" = 0 ]; then
	chown agent:agent /home/agent/.local /home/agent/.local/share /home/agent/.local/share/containers
	exec setpriv --reuid=1000 --regid=1000 --init-groups env -u XDG_RUNTIME_DIR HOME=/home/agent USER=agent LOGNAME=agent bash "$0"
fi
ALPINE=docker.io/library/alpine:3.22@sha256:d406446bc05b82480756ce8f1287964d28768c23955fa9480d2a74a2eae7a654
PG=docker.io/library/postgres:18-alpine@sha256:898c60cb390d1085bc97bdd6a1ae968757421df394b071848b52b1c09c89d6a5
step() { echo; echo "### $*"; }
die() { echo "NESTED FAIL: $*"; exit 1; }

step "who, env"
id; echo "XDG_RUNTIME_DIR=${XDG_RUNTIME_DIR-<unset>}"; uname -m
step "engine store mount"
findmnt -no SOURCE,FSTYPE,TARGET /home/agent/.local/share/containers
step "podman info"
podman info --format 'driver={{.Store.GraphDriverName}} root={{.Store.GraphRoot}} opts={{.Store.GraphOptions}} backing={{index .Store.GraphStatus "Backing Filesystem"}} native={{index .Store.GraphStatus "Native Overlay Diff"}} net={{.Host.NetworkBackend}} pasta={{.Host.Pasta.Executable}} runtime={{.Host.OCIRuntime.Name}} rootless={{.Host.Security.Rootless}}' || die "podman info"

if [ -e /images.tar ]; then
	step "podman load /images.tar"
	podman load -q -i /images.tar || die "podman load"
	ALPINE=localhost/nested-proof/alpine:3.22 PG=localhost/nested-proof/postgres:18-alpine
	for i in "$ALPINE=${ALPINE_ID:-}" "$PG=${PG_ID:-}"; do
		want=${i#*=} got=$(podman image inspect --format '{{.Id}}' "${i%%=*}") || die "${i%%=*} not in /images.tar"
		[ -z "$want" ] || [ "$got" = "$want" ] || die "${i%%=*} is $got, want $want"
	done
	podman images
fi

step "podman run alpine echo nested-ok"
out=$(podman run --rm "$ALPINE" echo nested-ok) || die "podman run: $out"
echo "$out"; [ "$out" = nested-ok ] || die "want nested-ok"

step "docker run (shim), default network: pasta gives the inner container an interface"
docker run --rm "$ALPINE" sh -c 'ip -4 -o addr show | grep -v " lo "; ip route | head -1' || die "inner container network (pasta)"

step "pasta exec path and inode"
ls -li /usr/bin/pasta /usr/bin/passt

step "postgres -p 55432:5432"
podman run -d --name pg -p 55432:5432 -e POSTGRES_PASSWORD=x "$PG" >/dev/null || die "postgres run"
for _ in $(seq 60); do pg_isready -h 127.0.0.1 -p 55432 >/dev/null 2>&1 && break; sleep 1; done
pg_isready -h 127.0.0.1 -p 55432 || { podman logs pg | tail; die "pg_isready"; }
# The image's init restarts the server once; retry until a query lands.
for _ in $(seq 30); do r=$(PGPASSWORD=x psql -h 127.0.0.1 -p 55432 -U postgres -tAc 'select 1' 2>&1) && break; sleep 1; done
echo "psql select 1 -> $r"; [ "$r" = 1 ] || die "psql: $r"
podman rm -f pg >/dev/null

step "docker compose up (postgres + client resolving 'db')"
mkdir -p /tmp/cmp && cd /tmp/cmp || die "cd"
cat >compose.yaml <<EOF
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
    command: ["sh", "-c", "getent hosts db && until pg_isready -q -h db; do sleep 1; done && sleep 2 && psql -h db -U postgres -tAc 'select 40+2'"]
EOF
docker compose up --abort-on-container-exit --exit-code-from client 2>&1 | sed 's/\x1b\[[0-9;]*m//g' | grep -v '^\[db\]' | tail -15
rc=${PIPESTATUS[0]}
echo "compose exit: $rc"
docker compose down -t 1 >/dev/null 2>&1
[ "$rc" = 0 ] || die "compose"
podman network ls
echo; echo "NESTED PASS"
