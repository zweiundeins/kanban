#!/bin/sh
# Alternating runs of compare-bench.mjs against ours and PLANKA, both behind
# cmd/netem (ours on :19000, PLANKA on :19001), results in ../results/<yyyy-mm>/.
#   benchmarks/scripts/compare.sh [runs] [moves]
set -eu
cd "$(dirname "$0")"
runs=${1:-3}
moves=${2:-20}
out=../results/$(date -u +%Y-%m)
mkdir -p "$out"
for i in $(seq 1 "$runs"); do
	for app in ours planka; do
		port=19000; [ "$app" = planka ] && port=19001
		f="$out/compare-$app-$(date -u +%Y%m%dT%H%M%S).json"
		node compare-bench.mjs "$app" "http://127.0.0.1:$port" "$moves" > "$f"
		echo "$f"
	done
done
