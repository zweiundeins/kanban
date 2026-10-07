#!/usr/bin/env python3
"""The compare-bench runs in results/<yyyy-mm>/ as one table, ours next to PLANKA.

Timings of repeated interactions (moves, opening a card, comments) pool the
samples of all runs, so a p95 rests on every move; the rest are medians of
the runs.

    python3 benchmarks/scripts/summarize.py benchmarks/results/2026-10
"""
import glob
import json
import statistics
import sys

folder = sys.argv[1] if len(sys.argv) > 1 else "benchmarks/results/" + __import__("datetime").date.today().strftime("%Y-%m")
runs = {"ours": [], "planka": []}
for f in sorted(glob.glob(folder + "/compare-*.json")):
    app = "planka" if "-planka-" in f else "ours"
    try:
        runs[app].append(json.load(open(f)))
    except json.JSONDecodeError:
        print("skipping broken", f, file=sys.stderr)


def pooled(app, key, q):
    xs = sorted(x for r in runs[app] for x in (r.get(key) or {}).get("samples", []) if x is not None)
    return xs[min(len(xs) - 1, int(len(xs) * q))] if xs else None


def count(app, key):
    return sum(len([x for x in (r.get(key) or {}).get("samples", []) if x is not None]) for r in runs[app])


def med(app, *path):
    if len(path) == 2 and path[1] in ("p50", "p95") and path[0].endswith("_ms") and path[0] != "load":
        return pooled(app, path[0], 0.5 if path[1] == "p50" else 0.95)
    vals = []
    for r in runs[app]:
        v = r
        for p in path:
            v = v.get(p) if isinstance(v, dict) else None
        if v is not None:
            vals.append(v)
    return statistics.median(vals) if vals else None


def fmt(v, unit=""):
    if v is None:
        return "n/a"
    if unit == "MB":
        return f"{v / 1e6:.1f} MB"
    if unit == "kB":
        return f"{v / 1e3:.0f} kB"
    if unit == "s":
        return f"{v / 1000:.1f} s"
    if isinstance(v, float) and not v.is_integer():
        return f"{v:.3f}" if v < 1 else f"{v:.0f}{unit}"
    return f"{int(v)}{unit}"


rows = [
    ("Cold load, transferred", ("load", "transferred_bytes"), "kB"),
    ("Cards on screen", ("load", "ms_to_cards"), "s"),
    ("Largest contentful paint", ("load", "lcp_ms"), "s"),
    ("Load event (scripts in, dragging works)", ("load", "ms_to_load_event"), "s"),
    ("Layout shift on load (CLS)", ("load", "cls"), ""),
    ("Move: card shown in its new lane, p50", ("move_drop_to_shown_ms", "p50"), " ms"),
    ("Move: confirmed by the server, p50", ("move_drop_to_confirmed_ms", "p50"), " ms"),
    ("Move: confirmed by the server, p95", ("move_drop_to_confirmed_ms", "p95"), " ms"),
    ("Move: seen by a second person, p50", ("move_seen_by_second_person_ms", "p50"), " ms"),
    ("Open a card, p50", ("open_card_ms", "p50"), " ms"),
    ("Comment: shown, p50", ("comment_to_shown_ms", "p50"), " ms"),
    ("Comment: confirmed, p50", ("comment_to_confirmed_ms", "p50"), " ms"),
    ("INP over the session", ("after_session", "inp_ms"), " ms"),
    ("JS heap after the session", ("after_session", "js_heap_used_bytes"), "MB"),
    ("DOM elements", ("after_session", "dom_nodes"), ""),
]
print(f"{len(runs['ours'])} runs (ours) and {len(runs['planka'])} runs (PLANKA); "
      f"moves pooled: {count('ours', 'move_drop_to_confirmed_ms')} and {count('planka', 'move_drop_to_confirmed_ms')}.\n")
print("| | Ours | PLANKA |")
print("|---|---|---|")
for label, path, unit in rows:
    print(f"| {label} | {fmt(med('ours', *path), unit)} | {fmt(med('planka', *path), unit)} |")
