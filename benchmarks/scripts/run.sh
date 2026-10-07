#!/bin/sh
# One benchmark run of ours on a freshly seeded database, behind cmd/netem
# (150 ms RTT, 1.6 Mbps down, 750 kbps up). Results go to ../results/<yyyy-mm>/.
#   benchmarks/scripts/run.sh [moves]
set -eu
cd "$(dirname "$0")/../.."
moves=${1:-20}
out=benchmarks/results/$(date -u +%Y-%m)
mkdir -p "$out"
tmp=$(mktemp -d)
go build -o "$tmp/kanban" ./cmd/kanban
go build -o "$tmp/netem" ./cmd/netem
"$tmp/kanban" seed -db "$tmp/k.db" >/dev/null
"$tmp/kanban" serve -db "$tmp/k.db" -addr 127.0.0.1:18080 2>"$tmp/server.log" &
server=$!
"$tmp/netem" -listen 127.0.0.1:19000 -to 127.0.0.1:18080 -rtt 150ms -down 1638 -up 750 2>/dev/null &
netem=$!
trap 'kill $server $netem 2>/dev/null; rm -rf "$tmp"' EXIT
sleep 1
stamp=$(date -u +%Y%m%dT%H%M)
node benchmarks/scripts/interaction-bench.mjs http://127.0.0.1:19000 "$moves" > "$out/kanban-interactions-$stamp.json"
echo "$out/kanban-interactions-$stamp.json"
