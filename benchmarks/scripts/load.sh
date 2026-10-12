#!/bin/sh
# Load test of ours: three runs each of 50 tabs moving a card every second
# and 200 tabs moving one every 250 ms, a fresh server per run. The server
# and cmd/loadtest share the machine, so they run on separate cores (on the
# i5-13500: performance cores 0-11 for the server, efficiency cores 12-19
# for the load tool). Results go to ../results/<yyyy-mm>/load-*.json.
#   SERVER_CPUS=0-11 TOOL_CPUS=12-19 benchmarks/scripts/load.sh [runs]
# KANBAN=<binary> runs a binary built elsewhere (an older commit, say),
# CONFIGS="200 250ms" picks the configurations, ACCEPT is the streams'
# Accept-Encoding (Chrome's by default: zstd where the server has it).
set -eu
cd "$(dirname "$0")/../.."
runs=${1:-3}
server_cpus=${SERVER_CPUS:-0-11}
tool_cpus=${TOOL_CPUS:-12-19}
configs=${CONFIGS:-"50 1s
200 250ms"}
accept=${ACCEPT:-gzip, deflate, br, zstd}
out=${OUT:-benchmarks/results/$(date -u +%Y-%m)}
mkdir -p "$out"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
kanban=${KANBAN:-$tmp/kanban}
[ -n "${KANBAN:-}" ] || CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o "$kanban" ./cmd/kanban
go build -o "$tmp/loadtest" ./cmd/loadtest
for i in $(seq 1 "$runs"); do
	echo "$configs" | while read -r tabs interval; do
		rm -f "$tmp"/k.db*
		"$kanban" seed -db "$tmp/k.db" >/dev/null
		taskset -c "$server_cpus" "$kanban" serve -db "$tmp/k.db" -addr 127.0.0.1:18095 -action-limit 0 2>"$tmp/server.log" &
		server=$!
		sleep 1
		f="$out/load-$tabs-$(date -u +%Y%m%dT%H%M%S).json"
		taskset -c "$tool_cpus" "$tmp/loadtest" -base http://127.0.0.1:18095 -tabs "$tabs" -interval "$interval" -duration 30s -accept-encoding "$accept" -out "$f" >/dev/null
		kill "$server"
		wait "$server" 2>/dev/null || true
		echo "$f"
	done
done
