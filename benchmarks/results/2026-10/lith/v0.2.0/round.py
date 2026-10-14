#!/usr/bin/env python3
"""The kanban v0.2.0 round: writer bench and 200-tab load runs, old (lith
v0.1.0, ba00c84) and new (lith v0.2.0) alternating, the order rotating.

    round.py OUT BIN REPO [writer|writer1|load|memlimit|all] [rounds]

BIN holds old/kanban, old/app.test, new/kanban, new/app.test, loadtest.
REPO is the kanban checkout whose benchmarks/scripts/load.sh runs.
Before each run: wait until the host used fewer than three cores for ten
seconds. During each run: the cores the rest of the host used (host busy
minus our processes); above two, the run goes to disturbed/ and is run
again, at most twice. clocks.txt: mean MHz of CPUs 0-11 and 12-19 each
second.
"""
import json
import os
import shutil
import subprocess
import sys
import threading
import time
import urllib.request

OUT, BIN, REPO = sys.argv[1], sys.argv[2], sys.argv[3]
PHASE = sys.argv[4] if len(sys.argv) > 4 else "all"
ROUNDS = int(sys.argv[5]) if len(sys.argv) > 5 else 5
HZ = os.sysconf("SC_CLK_TCK")
CHROME = "gzip, deflate, br, zstd"
os.makedirs(OUT, exist_ok=True)
logf = open(os.path.join(OUT, "runs.log"), "a")


def log(*a):
    s = " ".join(str(x) for x in a)
    print(s, flush=True)
    logf.write(s + "\n")
    logf.flush()


def uptime():
    return subprocess.run(["uptime"], capture_output=True, text=True).stdout.rstrip()


def host_busy():
    with open("/proc/stat") as f:
        v = list(map(int, f.readline().split()[1:9]))
    return sum(v) - v[3] - v[4]


def proc_cpu(pid):
    try:
        with open(f"/proc/{pid}/stat") as f:
            s = f.read()
        fields = s[s.rindex(")") + 2:].split()
        return int(fields[11]) + int(fields[12])
    except (OSError, ValueError, IndexError):
        return None


def ours(argv0s):
    """pids whose argv[0] is one of ours"""
    out = []
    for d in os.listdir("/proc"):
        if not d.isdigit():
            continue
        try:
            with open(f"/proc/{d}/cmdline", "rb") as f:
                a0 = f.read().split(b"\0", 1)[0].decode()
        except OSError:
            continue
        if a0 in argv0s:
            out.append(int(d))
    return out


def clocks(stop):
    with open(os.path.join(OUT, "clocks.txt"), "a") as f:
        f.write("# Mean clock of CPUs 0-11 (the server's) and 12-19 (the load tool's) in MHz, once a second\n")
        while not stop.is_set():
            mhz = []
            for c in range(20):
                try:
                    with open(f"/sys/devices/system/cpu/cpu{c}/cpufreq/scaling_cur_freq") as g:
                        mhz.append(int(g.read()) / 1000)
                except OSError:
                    mhz.append(0)
            f.write(time.strftime("%Y%m%dT%H%M%S", time.gmtime()) + f" {sum(mhz[:12]) / 12:.0f} {sum(mhz[12:]) / 8:.0f}\n")
            f.flush()
            stop.wait(1)


def gate(label):
    waited = 0
    while True:
        b0, t0 = host_busy(), time.time()
        time.sleep(10)
        waited += 10
        cores = (host_busy() - b0) / HZ / (time.time() - t0)
        if cores < 3 or waited >= 120:
            la = open("/proc/loadavg").read().split()[0]
            log(f"{label} gate: {'quiet' if cores < 3 else 'gave up'} after {waited} s: load {la}, {cores:.2f} cores busy")
            return


def watch(popen, argv0s, health=False):
    """Poll until popen ends: the cores the rest of the host used while our
    processes ran, and the hub's open streams mid-run."""
    first, last = {}, {}
    hb0 = hb1 = None
    streams = None
    t_tool = None
    while popen.poll() is None:
        hb = host_busy()
        pids = ours(argv0s)
        cpus = {p: proc_cpu(p) for p in pids}
        cpus = {p: c for p, c in cpus.items() if c is not None}
        running = any(True for p in cpus)
        if running:
            if hb0 is None:
                hb0 = hb
            hb1 = hb
            for p, c in cpus.items():
                first.setdefault(p, (c, hb0 is hb and True))
                last[p] = c
        if health:
            tool = [p for p in pids if open(f"/proc/{p}/cmdline", "rb").read().split(b"\0", 1)[0].decode().endswith("loadtest")] if pids else []
            if tool and t_tool is None:
                t_tool = time.time()
            if t_tool and streams is None and time.time() - t_tool > 15:
                try:
                    h = json.load(urllib.request.urlopen("http://127.0.0.1:18095/_health", timeout=2))
                    streams = h["live"]["streams"]
                except Exception as e:
                    streams = f"error {e}"
        time.sleep(0.5)
    if hb0 is None:
        return None, streams
    mine = sum(last[p] - (first[p][0] if first[p][1] else 0) for p in last)
    # A process first seen after the window opened started inside it: all its CPU counts.
    return (hb1 - hb0 - mine), streams


def timed(popen, argv0s, health=False):
    t0 = time.time()
    others, streams = watch(popen, argv0s, health)
    dt = time.time() - t0
    return (others / HZ / dt if others is not None else float("nan")), streams


def bench(name, pattern, outfile):
    exe = os.path.join(BIN, name, "app.test")
    for attempt in range(3):
        gate(f"{name} {pattern}")
        p = subprocess.Popen(["taskset", "-c", "0-11", exe, "-test.run", "X", "-test.bench", pattern, "-test.benchtime", "5s"],
                             stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True, cwd=os.path.dirname(exe))
        out_lines = []

        def read():
            for line in p.stdout:
                out_lines.append(line)
        th = threading.Thread(target=read)
        th.start()
        others, _ = timed(p, {exe})
        th.join()
        lines = [f"{name} {l}" for l in out_lines if l.startswith("Benchmark")]
        disturbed = others > 2 and attempt < 2
        log(f"{name} {pattern}: others {others:.2f} cores" + (" (again)" if disturbed else ""))
        dest = os.path.join(OUT, "disturbed", outfile) if disturbed else os.path.join(OUT, outfile)
        os.makedirs(os.path.dirname(dest), exist_ok=True)
        with open(dest, "a") as f:
            f.writelines(lines)
        if p.returncode != 0:
            log(f"{name} {pattern}: exit {p.returncode}: {''.join(out_lines)[-2000:]}")
        if not disturbed:
            return


LOADS = {
    "old-br": ("old", "br"),
    "new-br": ("new", "br"),
    "old-zstd": ("old", CHROME),
    "new-zstd": ("new", CHROME),
}


def load(name, extra_env=None, outdir="load"):
    which, accept = LOADS[name]
    kanban = os.path.join(BIN, which, "kanban")
    tool = os.path.join(BIN, "loadtest")
    for attempt in range(3):
        gate(name)
        out = os.path.join(OUT, outdir, name)
        os.makedirs(out, exist_ok=True)
        env = dict(os.environ, KANBAN=kanban, LOADTEST=tool, CONFIGS="200 250ms", ACCEPT=accept, OUT=out)
        env.update(extra_env or {})
        # Files, not pipes: when the load tool fails (a 503), load.sh exits
        # and leaves its server running, which would hold a pipe open.
        with open(os.path.join(OUT, ".load.out"), "w+") as so, open(os.path.join(OUT, ".load.err"), "w+") as se:
            p = subprocess.Popen(["sh", "benchmarks/scripts/load.sh", "1"], cwd=REPO, env=env, stdout=so, stderr=se, text=True)
            others, streams = timed(p, {kanban, tool}, health=True)
            for pid in ours({kanban}):
                os.kill(pid, 15)
            so.seek(0)
            se.seek(0)
            res = {"out": so.read(), "err": se.read()}
        files = [l.strip() for l in res["out"].splitlines() if l.strip().endswith(".json")]
        f = os.path.basename(files[-1]) if files else "(no result)"
        disturbed = others > 2 and attempt < 2 and files
        log(f"{name} {f}: others {others:.2f} cores, {streams} streams open mid-run, exit {p.returncode}" + (" (again)" if disturbed else ""))
        if res["err"].strip():
            log(f"{name} stderr: {res['err'].strip()[-1500:]}")
        if disturbed:
            d = os.path.join(OUT, "disturbed", outdir, name)
            os.makedirs(d, exist_ok=True)
            shutil.move(files[-1], os.path.join(d, f))
            continue
        return


def main():
    stop = threading.Event()
    threading.Thread(target=clocks, args=(stop,), daemon=True).start()
    log(f"start {time.strftime('%H:%M:%S')} ({PHASE}): {uptime()}")
    if PHASE in ("writer", "all"):
        for r in range(1, ROUNDS + 1):
            log(f"writer round {r} {time.strftime('%H:%M:%S')}")
            for name in (("old", "new") if r % 2 else ("new", "old")):
                bench(name, "^BenchmarkMoveCard$/", "writer-bench.txt")
        log(f"writer done {time.strftime('%H:%M:%S')}")
    if PHASE in ("writer1", "all"):
        for r in range(1, 7):
            log(f"one-person round {r} {time.strftime('%H:%M:%S')}")
            for name in (("old", "new") if r % 2 else ("new", "old")):
                bench(name, "^BenchmarkMoveCard$/^1$", "writer-bench-1.txt")
        log(f"writer1 done {time.strftime('%H:%M:%S')}")
    if PHASE in ("load", "all"):
        order = list(LOADS)
        for r in range(ROUNDS):
            log(f"load round {r + 1} {time.strftime('%H:%M:%S')}")
            for name in order[r % 4:] + order[:r % 4]:
                load(name)
        log(f"load done {time.strftime('%H:%M:%S')}")
    if PHASE == "memlimit":
        # production's GOMEMLIMIT: does every tab get in?
        for name in ("old-br", "new-br", "old-zstd", "new-zstd"):
            load(name, {"GOMEMLIMIT": "600MiB"}, outdir="memlimit")
    log(f"end {time.strftime('%H:%M:%S')}: {uptime()}")
    stop.set()


main()
