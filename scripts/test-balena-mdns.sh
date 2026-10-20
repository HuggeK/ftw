#!/usr/bin/env bash
# The balena mdns service publishes ftw.local through an avahi-daemon it
# reaches over D-Bus, as on balenaOS, and withdraws it when the service stops.
# A stand-in host runs dbus and avahi in one container; the service mounts its
# bus where the io.balena.features.dbus label puts the host's.
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$ROOT"

compose=deploy/balena/docker-compose.yml

# Core on balena is the deploy/docker recipe, not a copy that can drift.
[ "$(readlink deploy/balena/ftw/Dockerfile)" = ../../docker/Dockerfile ]
grep -Eq '^        FTW_VERSION: v0\.[0-9]+\.[0-9]+(-beta\.[0-9]+)?$' "$compose"
[ "$(grep -c '^    network_mode: host$' "$compose")" = 2 ]
grep -q 'io.balena.features.dbus: "1"' "$compose"
grep -q 'DBUS_SYSTEM_BUS_ADDRESS: unix:path=/host/run/dbus/system_bus_socket' "$compose"
sh -n deploy/balena/mdns/publish.sh

if ! command -v docker >/dev/null 2>&1 || ! docker info >/dev/null 2>&1; then
	if [ -n "${CI:-}" ]; then
		echo "docker is required in CI" >&2
		exit 1
	fi
	echo "balena mdns: static checks passed; docker unavailable, skipping the avahi run"
	exit 0
fi

run=ftw-mdns-test-$$
cleanup() {
	docker rm -f "$run-avahi" "$run-pub" "$run-pub-lan" >/dev/null 2>&1 || true
	docker network rm "$run" "$run-lan" >/dev/null 2>&1 || true
	docker volume rm "$run-dbus" >/dev/null 2>&1 || true
}
trap cleanup EXIT

docker build -q -t ftw-mdns-test-avahi - >/dev/null <<'EOF'
FROM debian:trixie-slim
RUN apt-get update && \
    apt-get install -y --no-install-recommends dbus avahi-daemon avahi-utils && \
    rm -rf /var/lib/apt/lists/*
CMD ["sh", "-c", "mkdir -p /run/dbus && dbus-daemon --system --fork && exec avahi-daemon --no-drop-root --no-rlimits --no-chroot"]
EOF
docker build -q -t ftw-mdns-test deploy/balena/mdns >/dev/null

docker network create "$run" >/dev/null
# No default route: the service must fall back to the interface address.
docker network create --internal "$run-lan" >/dev/null
docker volume create "$run-dbus" >/dev/null
docker run -d --name "$run-avahi" --network "$run" -v "$run-dbus:/run/dbus" ftw-mdns-test-avahi >/dev/null

resolved() {
	docker exec "$run-avahi" avahi-resolve -4 -n ftw.local 2>/dev/null | awk '{ print $2 }'
}

expect_published() {
	local container=$1 network=$2 want got=
	want=$(docker inspect -f "{{(index .NetworkSettings.Networks \"$network\").IPAddress}}" "$container")
	for _ in $(seq 1 30); do
		got=$(resolved)
		[ "$got" = "$want" ] && return 0
		sleep 1
	done
	echo "ftw.local resolved to '$got', want $want" >&2
	docker logs "$container" >&2
	exit 1
}

expect_withdrawn() {
	for _ in $(seq 1 15); do
		[ -z "$(resolved)" ] && return 0
		sleep 1
	done
	echo "ftw.local still resolves after the service stopped" >&2
	exit 1
}

publisher() {
	docker run -d --name "$1" --network "$2" -v "$run-dbus:/host/run/dbus" \
		-e DBUS_SYSTEM_BUS_ADDRESS=unix:path=/host/run/dbus/system_bus_socket \
		ftw-mdns-test >/dev/null
}

stop_cleanly() {
	# TERM must end the publisher, not the engine's kill after the timeout.
	docker stop -t 5 "$1" >/dev/null
	local code
	code=$(docker inspect -f '{{.State.ExitCode}}' "$1")
	if [ "$code" != 0 ]; then
		echo "$1 exited $code on stop, want 0" >&2
		exit 1
	fi
}

publisher "$run-pub" "$run"
expect_published "$run-pub" "$run"
stop_cleanly "$run-pub"
expect_withdrawn

publisher "$run-pub-lan" "$run-lan"
expect_published "$run-pub-lan" "$run-lan"
stop_cleanly "$run-pub-lan"
expect_withdrawn

echo "balena mdns: ftw.local published with and without a default route, withdrawn on stop"
