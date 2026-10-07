#!/usr/bin/env python3
"""Medians of the load-*.json runs in results/<yyyy-mm>/, per number of tabs.

    python3 benchmarks/scripts/summarize-load.py benchmarks/results/2026-10
"""
import glob
import json
import statistics
import sys

folder = sys.argv[1]
by_tabs = {}
for f in sorted(glob.glob(folder + "/load-*.json")):
    r = json.load(open(f))
    by_tabs.setdefault(r["tabs"], []).append(r)


def med(rs, *path):
    vals = []
    for r in rs:
        v = r
        for p in path:
            v = v[p]
        vals.append(v)
    return statistics.median(vals)


rows = [
    ("moves per second", ("moves_per_s",), "{:.0f}"),
    ("refused as stale", ("refused",), "{:.0f}"),
    ("moves sent", ("moves",), "{:.0f}"),
    ("send to 204, p50", ("ms_to_204", "p50"), "{:.1f} ms"),
    ("send to 204, p95", ("ms_to_204", "p95"), "{:.1f} ms"),
    ("send to outcome on the stream, p50", ("ms_to_outcome", "p50"), "{:.0f} ms"),
    ("send to outcome on the stream, p95", ("ms_to_outcome", "p95"), "{:.0f} ms"),
    ("frames per tab per second", ("frames_per_tab_s",), "{:.1f}"),
    ("bytes per frame on the wire", ("wire_bytes_per_frame",), "{:.0f}"),
    ("server CPU, cores", ("server", "cpu_cores"), "{:.1f}"),
    ("server peak memory (RSS)", ("server", "peak_rss_mb"), "{:.0f} MB"),
    ("writer: moves per batch", ("server", "moves_per_batch"), "{:.1f}"),
    ("writer: largest batch", ("server", "max_batch"), "{:.0f}"),
    ("writer: time per batch", ("server", "ms_per_batch"), "{:.1f} ms"),
    ("render: time per frame", ("server", "ms_per_render"), "{:.1f} ms"),
    ("load tool CPU, cores", ("tool_cpu_cores",), "{:.1f}"),
]
tabs = sorted(by_tabs)
print("Medians of " + ", ".join(f"{len(by_tabs[t])} runs with {t} tabs" for t in tabs) + ".\n")
print("| | " + " | ".join(f"{t} tabs" for t in tabs) + " |")
print("|---|" + "---|" * len(tabs))
for label, path, f in rows:
    print(f"| {label} | " + " | ".join(f.format(med(by_tabs[t], *path)) for t in tabs) + " |")
