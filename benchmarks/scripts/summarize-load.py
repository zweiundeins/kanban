#!/usr/bin/env python3
"""Medians of the load-*.json runs in a folder, per number of tabs, with the
lowest and highest run in brackets.

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


def values(rs, path):
    out = []
    for r in rs:
        v = r
        for p in path:
            v = v[p]
        out.append(v)
    return out


def cell(rs, path, f, unit):
    vs = values(rs, path)
    s = f.format(statistics.median(vs))
    if len(vs) > 1:
        s += " (" + f.format(min(vs)) + " to " + f.format(max(vs)) + ")"
    return s + unit


rows = [
    ("moves per second", ("moves_per_s",), "{:.0f}", ""),
    ("refused as stale", ("refused",), "{:.0f}", ""),
    ("moves sent", ("moves",), "{:.0f}", ""),
    ("send to 204, p50", ("ms_to_204", "p50"), "{:.1f}", " ms"),
    ("send to 204, p95", ("ms_to_204", "p95"), "{:.1f}", " ms"),
    ("send to outcome on the stream, p50", ("ms_to_outcome", "p50"), "{:.0f}", " ms"),
    ("send to outcome on the stream, p95", ("ms_to_outcome", "p95"), "{:.0f}", " ms"),
    ("frames per tab per second", ("frames_per_tab_s",), "{:.1f}", ""),
    ("bytes per frame on the wire", ("wire_bytes_per_frame",), "{:.0f}", ""),
    ("server CPU, cores", ("server", "cpu_cores"), "{:.1f}", ""),
    ("server peak memory (RSS)", ("server", "peak_rss_mb"), "{:.0f}", " MB"),
    ("writer: moves per batch", ("server", "moves_per_batch"), "{:.1f}", ""),
    ("writer: largest batch", ("server", "max_batch"), "{:.0f}", ""),
    ("writer: time per batch", ("server", "ms_per_batch"), "{:.1f}", " ms"),
    ("render: time per frame", ("server", "ms_per_render"), "{:.1f}", " ms"),
    ("load tool CPU, cores", ("tool_cpu_cores",), "{:.1f}", ""),
]
tabs = sorted(by_tabs)
encodings = sorted({r.get("encoding", "br") for rs in by_tabs.values() for r in rs})
print("Medians of " + ", ".join(f"{len(by_tabs[t])} runs with {t} tabs" for t in tabs) + ", streams " + "/".join(encodings) + ".\n")
print("| | " + " | ".join(f"{t} tabs" for t in tabs) + " |")
print("|---|" + "---|" * len(tabs))
for label, path, f, unit in rows:
    print(f"| {label} | " + " | ".join(cell(by_tabs[t], path, f, unit) for t in tabs) + " |")
