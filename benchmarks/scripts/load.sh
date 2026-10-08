#!/bin/sh
# Load test of ours: three runs each of 50 tabs moving a card every second
# and 200 tabs moving one every 250 ms, a fresh server per run. The server
# and cmd/loadtest share the machine, so they run on separate cores (on the
# i5-13500: performance cores 0-11 for the server, efficiency cores 12-19
# for the load tool). Results go to ../results/<yyyy-mm>/load-*.json.
#   SERVER_CPUS=0-11 TOOL_CPUS=12-19 benchmarks/scripts/load.sh [runs]
set -eu
cd "$(dirname "$0")/../.."
runs=${1:-3}
server_cpus=${SERVER_CPUS:-0-11}
tool_cpus=${TOOL_CPUS:-12-19}
out=${OUT:-benchmarks/results/$(date -u +%Y-%m)}
mkdir -p "$out"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o "$tmp/kanban" ./cmd/kanban
go build -o "$tmp/loadtest" ./cmd/loadtest
for i in $(seq 1 "$runs"); do
	for cfg in "50 1s" "200 250ms"; do
		set -- $cfg
		rm -f "$tmp"/k.db*
		"$tmp/kanban" seed -db "$tmp/k.db" >/dev/null
		taskset -c "$server_cpus" "$tmp/kanban" serve -db "$tmp/k.db" -addr 127.0.0.1:18095 -action-limit 0 2>"$tmp/server.log" &
		server=$!
		sleep 1
		f="$out/load-$1-$(date -u +%Y%m%dT%H%M%S).json"
		taskset -c "$tool_cpus" "$tmp/loadtest" -base http://127.0.0.1:18095 -tabs "$1" -interval "$2" -duration 30s -out "$f" >/dev/null
		kill "$server"
		wait "$server" 2>/dev/null || true
		echo "$f"
	done
done
