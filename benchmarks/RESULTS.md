# Benchmark results

Date: 2026-10-07. Machine: Intel i5-13500 (20 threads), 62 GB RAM, Linux 6.18 (Unraid), both apps and the browser on the same host. Chromium 154 through playwright-core 1.63.

| App | Version | Stack |
|---|---|---|
| Ours | this repository | one Go binary, SQLite (`synchronous=FULL`), Datastar 1.0.4 with Starbase's patches, Starbase components |
| PLANKA Community | 2.2.1, official image, our compose file (`planka/docker-compose.yml`) | React 18, Redux, Sails.js on Node, PostgreSQL 16, socket.io |

Both hold the same data. `kanban seed` creates it. `kanban export` and `planka/seed.mjs` load it into PLANKA: 4 users, a small board (3 lists, 9 cards) and the measured one, "Release 2.0" (8 lists, 200 cards, 268 comments, labels and due dates).

## Interactions on a slow phone

`scripts/compare-bench.mjs` runs both apps behind `cmd/netem`, a TCP proxy that adds a 150 ms round trip and limits bandwidth to 1.6 Mbps down and 750 kbps up. It delays data on an open stream or WebSocket too, which Chrome's built-in network emulation doesn't. The measured person's CPU is slowed 4x in Chrome, about a mid-range phone; a second person watches unthrottled. Each move is a pointer drag of a card one lane to the right, timed from the drop.

- **Cold load**: a first visit of a signed-in person, in a new browser context with only the session: no cache, no open connection.
- **"Shown"**: the card is in its new lane in the page.
- **"Confirmed"**: the server has accepted the move. For PLANKA, this is the socket.io acknowledgement of its `PATCH`. For ours, it is when the card's pending look is gone, which happens in the same frame that shows the card in its new lane.

Ours never shows a card in a place the server hasn't confirmed, so its "shown" is already confirmed. PLANKA shows the move at once (optimistically), and its "shown" includes the drop animation of its drag library.

PLANKA's keyboard dragging didn't move cards in these runs: the lift is announced, but the arrow keys do nothing. Pointer drags are used for both apps.

Three runs per app, alternating, 20 moves each. Moves, card openings and comments pool the samples of all runs (60 moves per app), the rest are medians of the runs. `python3 scripts/summarize.py results/2026-10`:

| | Ours | PLANKA |
|---|---|---|
| Cold load, transferred | 58 kB | 2,576 kB |
| Cards in the page | 0.4 s | 15.3 s |
| Largest contentful paint | 0.6 s | 15.4 s |
| Load event (scripts in, dragging works) | 0.6 s | 12.5 s |
| Layout shift on load (CLS) | 0 | 0.020 |
| Move: card shown in its new lane, p50 | 252 ms | 481 ms |
| Move: confirmed by the server, p50 | 253 ms | 712 ms |
| Move: confirmed by the server, p95 | 276 ms | 756 ms |
| Move: seen by a second person, p50 | 199 ms | 656 ms |
| Open a card, p50 | 258 ms | 318 ms |
| Comment: shown, p50 | 302 ms | 90 ms |
| Comment: confirmed, p50 | 302 ms | 229 ms |
| INP over the session | 64 ms | 256 ms |
| JS heap after the session | 4.4 MB | 45.0 MB |
| DOM elements | 1,843 | 3,295 |

What the numbers say:
- **Load:** ours sends 58 kB, and the board is painted after 0.6 s; its cards are in the HTML, and the paint waits only for the stylesheet. Its scripts (Datastar and one file per Starbase component) are in by 0.6 s. PLANKA sends 2.6 MB of JavaScript and data first, so its board appears after 15.4 s on this connection.
- **Moving a card:** ours shows the card in its new place after 252 ms, already confirmed: the frame that shows it also clears its pending look (the watchers see the two a millisecond apart). Until then, Starbase's landing marker holds a gap with a dashed copy of the card where it was dropped, while the card stays dimmed in its old place. PLANKA's target lane opens a gap during the drag, and after the drop its card floats into it; it is in the lane at 481 ms, and the server's confirmation arrives at 712 ms. The second person sees the move after 199 ms in ours and 656 ms in PLANKA, which sends the move to its server only after the drop animation. `results/2026-10/drop/` has the frames of both apps from 100 ms before to 900 ms after a drop.
- **Comments:** PLANKA is faster here. It shows the comment at once (90 ms, before the server has it) and has the confirmation at 229 ms, over the socket that is already open. Ours sends a request and shows the comment with the dialog's next frame at 302 ms.
- **Responsiveness:** INP is 64 ms in ours and 256 ms in PLANKA. The JS heap is 4.4 MB against 45.0 MB.

Run it again: `docker compose down -v && planka/setup.sh && node planka/seed.mjs`, start ours on a fresh database and both proxies (ours on :19000, PLANKA on :19001), then `scripts/compare.sh 3 20` and `python3 scripts/summarize.py results/<yyyy-mm>`.

Ours was measured again on 2026-10-08, each time its client changed; PLANKA's runs are unchanged. The earlier rounds of ours are in `results/2026-10/superseded/`: `before-bundles/` with 17 separate modules (77 kB, scripts in after 1.1 s), `before-landing/` with one file per component and no landing markers (a move shown at 241 ms, p95 256 ms), and `landing-slow/` with the first version of the markers, which forced layouts in every frame (264 ms, p95 354 ms). With the markers as they ship (Starbase c6a6589) a move costs about 11 ms more at the median than without them.

The first runs (in `results/2026-10/superseded/`) had two measuring errors. "Cards on screen" was taken after the load event, so ours read 1.1 s instead of 0.4 s. And our "confirmed" waited for a watcher started after the card had landed, which added about 100 ms of its own start-up on the slowed CPU: it read 350 ms where the page shows the confirmation at the same moment as the card.

`scripts/filmstrip.mjs` records a cold load with tracing on and keeps the last frame painted at 0.5, 1, 2, 4, 8 and 16 s (`results/2026-10/filmstrip/`). Tracing slows the page a little, so its frames appear somewhat later than the timings above.

## Lighthouse

Lighthouse 13.5 runs performance only, three times per form factor, and keeps the median run. The person logs in once in a Chrome profile; Lighthouse then launches Chrome with it, and its storage reset clears the cache but keeps cookies. So every run is a cold load of the signed-in board. PLANKA's client reads its token from `document.cookie`, so a Cookie header alone would only reach its login page. Script: `scripts/lighthouse-auth.mjs`. Both run without a delaying proxy (PLANKA through a pass-through one, since its `BASE_URL` is the proxy), as Lighthouse simulates the network itself (mobile: 150 ms RTT, 1.6 Mbps, 4x CPU).

| | Ours, desktop | PLANKA, desktop | Ours, mobile | PLANKA, mobile |
|---|---|---|---|---|
| Performance score | 100 | 69 | 98 | 33 |
| First contentful paint | 0.3 s | 2.4 s | 1.4 s | 14.0 s |
| Largest contentful paint | 0.3 s | 2.6 s | 1.4 s | 14.9 s |
| Total blocking time | 0 ms | 140 ms | 160 ms | 990 ms |
| Cumulative layout shift | 0 | 0.018 | 0 | 0.027 |
| Requests, transferred | 12, 68 kB | 12, 2,602 kB | 12, 68 kB | 12, 2,602 kB |
| JavaScript transferred | 40 kB | 2,226 kB | 40 kB | 2,226 kB |

Ours keeps its live stream open, so Lighthouse never sees the network go quiet and notes that the page "loaded too slowly to finish within the time limit". The paint metrics above are complete; the time to interactive isn't reported.

Ours loads Datastar and one file per Starbase component (Starbase's `cmd/dist`), all in one wave after the HTML. With 17 separate modules in three waves of imports it made 24 requests and 86 kB, its mobile LCP was 1.9 s and its total blocking time 80 ms: the board's component now runs as one file, in one longer task.

## Load: many people on one board

`cmd/loadtest` (ours only) opens real streams with brotli, as a browser does, and moves random cards on the 200-card board. It measures what a person waits for: from sending a move to its outcome arriving on their own stream, after the frame that shows it. Moves go out on schedule, whether or not the last one was answered. `scripts/load.sh` runs each configuration three times for 30 s on a fresh server; the server runs on the six performance cores (CPUs 0 to 11) and the load tool on the eight efficiency cores, so they don't take each other's CPU. The host runs other services, so expect some noise. `python3 scripts/summarize-load.py results/2026-10`, medians of 3 runs on 2026-10-08, with the statement cache (see "The writer alone"):

| | 50 tabs, a move each per second | 200 tabs, four moves each per second |
|---|---|---|
| moves per second | 48 | 790 |
| refused as stale (two people, one card) | 15 of 1,508 | 4,425 of 23,967 |
| send to 204, p50 / p95 | 5.8 / 13.6 ms | 4.4 / 12.2 ms |
| send to outcome on the stream, p50 / p95 | 47 / 77 ms | 56 / 99 ms |
| frames per tab per second | 18.3 | 18.2 |
| bytes per frame on the wire (brotli quality 4) | 532 | 999 |
| server CPU | 1.6 cores | 4.9 cores |
| server memory, peak RSS | 258 MB | 1,163 MB |
| writer: moves per transaction, largest | 1.0, 5 | 2.3, 38 |
| load tool CPU | 1.0 cores | 3.9 cores |

Before the cache, in runs alternating with these (`results/2026-10/stmt-cache/before/`), a move's 204 took 5.4 / 16.7 ms (p50 / p95) at 200 tabs and the server used 5.1 cores; everything else was the same within the noise. The first runs, from 2026-10-07, are in `results/2026-10/superseded/before-stmt-cache/`.

At 50 tabs most moves are a transaction of their own and wait for their own fsync, about 5 ms on this disk; at 200 tabs, moves queue while a transaction commits and share the next one.

Where the server's time goes at 200 tabs: rendering the board is cheap (2.4 ms per frame, once for everyone), and so is the database. Each stream compresses every frame for itself, since brotli's window is per connection; at 800 moves a second every frame changes all eight lists (about 140 kB of HTML), and compressing it for 200 streams is most of the 5 cores. A frame's events are formatted once and shared by all streams, and at most half the CPUs compress at once (`-send-slots`), so a burst of frames doesn't hold up actions. A stream that falls behind skips to the newest frame.

Memory follows the streams: a stream that has received frames keeps brotli's window, hash table and buffers, about 2.2 MB at quality 4, and Go's garbage collector roughly doubles that in RSS. 200 quiet tabs need 150 MB; 200 tabs on a board that changes 20 times a second about 1.1 GB. `-brotli-level 3` needs about a quarter less memory and sends about 60% more bytes per frame.

### The writer alone

`go test ./internal/app -run X -bench MoveCard` moves cards with 1, 50 or 200 people at once, without HTTP or streams, with `synchronous=FULL` (an fsync on every commit), each person moving their own cards so that every move is a write:

| people moving at once | moves per second | moves per transaction | time per transaction |
|---|---|---|---|
| 1 | 240 to 340 | 1 | 2.9 to 4.2 ms |
| 50 | about 5,100 | 25 | 4.7 to 5.4 ms |
| 200 | about 13,800 | 103 | 6.9 to 7.8 ms |

Four rounds each, alternating with the code before the statement cache, on the performance cores (`taskset -c 0-11`); raw output in `results/2026-10/stmt-cache/writer-bench.txt`.

The pure Go SQLite driver prepares a statement again on every call: it parses and plans the SQL, runs it, and throws the plan away. With 200 people that was 41% of the writer's CPU, 36% in SQLite's SQL parser alone (`results/2026-10/stmt-cache/writer-profile-200.txt`). The writer now holds its connection as a `*sql.Conn` and prepares each statement once on it, `BEGIN`, the savepoints and `COMMIT` included; reads outside a read transaction (the session and membership checks of every action) use statements prepared once per pool connection. With 200 people the writer went from about 10,000 to about 13,800 moves a second; the time left is mostly SQLite's own work (`sqlite3_step`, 40%) and Go's garbage collector (11%). With 1 or 50 people nothing changed: there every transaction waits for its fsync.

Anders Murphy's [SQLite benchmark](https://andersmurphy.com/2025/12/02/100000-tps-over-a-billion-rows-the-unreasonable-effectiveness-of-sqlite.html), whose single-writer batching this design follows, reports 98,163 transactions a second with savepoints and `synchronous=FULL`. Ours is about seven times slower, for two reasons. A move runs about fifteen statements (the op id check, membership, two savepoints, the card, its list, the position, the update, the board version and the op record); his transfer runs two updates. And his machine was a MacBook, where SQLite's fsync doesn't wait for the drive to empty its cache unless `PRAGMA fullfsync` is on; his settings don't list it. Here a commit waits 3 to 5 ms for the disk, which is the ceiling for one person moving alone. For a board, 13,800 moves a second is far more than people make: 200 people each moving a card every 250 ms send 800.

## Footprint

| | Ours | PLANKA |
|---|---|---|
| What runs | one static binary (13.5 MB) under systemd | two containers: PLANKA (image 379 MB) and PostgreSQL 16 (image 294 MB) |
| Processes | 1 | 18 (12 and 6) |
| Memory | 19 MB RSS after start, 28 MB at rest after the runs | 200 MB at rest after the runs (131 MB and 69 MB) |
| The same boards on disk | 143 kB SQLite file (after the runs) | 10.3 MB PostgreSQL database (an empty one is 7.3 MB), in a 65 MB data directory with 32 MB of write-ahead log segments and three template databases |
| Dependencies | 12 Go modules compiled in | 83 client and 37 server packages declared, plus their dependencies |

## Notes and limits

- Both apps run on one machine, with the browser on it as well, so neither pays a real network or a data centre's distance. The proxy adds the same delay to both.
- The comparison covers what both apps do. PLANKA does much more: attachments, task lists, custom fields, notifications, webhooks, a REST API, two-factor login and other views. Its bundle and memory pay for those too.
- PLANKA's numbers are its out-of-the-box behaviour. A tuned deployment (a CDN, HTTP caching, a smaller bundle) would load faster.
- The load test runs against ours only, with the load tool on the same machine.
- Raw data: `results/2026-10/`.

## On lith, 2026-10-13

Kanban now runs on lith, the shared runtime of these projects (commits 8db663e to 41b4d82): lith's sqlite and writer, its live hub with one shared view per board, lith's sse for the streams and its edge for CSRF, compression and static files. These runs compare that code, built against lith d9e58fb, with the binary of 752aaeb, the last commit before the move. lith's livefix merge (6265bce) landed while they ran: the tests and the end-to-end suite pass on it, but the numbers below are for d9e58fb.

Method: the same machine as above, Go 1.26.5 for both binaries, the old one built in a temporary worktree at 752aaeb, both with `CGO_ENABLED=0 -trimpath`. Other heavy jobs on the host were held back (load average about 1 before the runs). Old and new ran alternately, the order rotating from round to round. Each figure is the median, with the lowest and the highest run in brackets. Raw data: `results/2026-10/lith/`.

### The writer alone

The test binaries of `internal/app` from both commits, `taskset -c 0-11 ./app.test -test.run X -test.bench MoveCard -test.benchtime 5s`, five rounds each, and six more of the one-person case, whose fsyncs vary the most:

| people moving at once | moves per second, old | new | moves per transaction, old / new | time per transaction, old / new |
|---|---|---|---|---|
| 1 | 284 (248 to 309) | 272 (169 to 316) | 1 / 1 | 3.5 / 3.6 ms |
| 50 | 4,500 (3,912 to 7,801) | 5,229 (4,342 to 7,554) | 25 / 26 | 5.5 / 4.7 ms |
| 200 | 12,366 (10,739 to 16,217) | 13,181 (10,457 to 17,606) | 104 / 108 | 8.3 / 7.9 ms |

The new writer is lith's, which checkpoints the WAL between batches rather than inside a commit. The bench sends its moves without a tab, so it writes no outcome to a tab's outbox: it measures the move alone, as before.

### 200 tabs

`KANBAN=<binary> CONFIGS="200 250ms" ACCEPT=<...> benchmarks/scripts/load.sh 1`, five rounds of three runs: the old binary, which answers Chrome's Accept-Encoding with brotli; the new one with Chrome's Accept-Encoding, which lith answers with zstd; and the new one with `br` alone, as Safari before 26.3 gets it. The same load tool, the new `cmd/loadtest`, drove all three.

| | old, brotli | new, brotli | new, zstd |
|---|---|---|---|
| moves per second | 790 (788 to 794) | 790 (788 to 791) | 787 (784 to 789) |
| send to 204, p50 | 4.7 (4.6 to 4.8) ms | 5.4 (5.0 to 5.9) ms | 12.6 (10.9 to 13.6) ms |
| send to 204, p95 | 14.4 (13.9 to 16.3) ms | 21.4 (17.6 to 27.5) ms | 46.0 (42.3 to 49.2) ms |
| send to outcome on the stream, p50 | 57 (57 to 58) ms | 61 (60 to 62) ms | 61 (60 to 63) ms |
| send to outcome on the stream, p95 | 101 (98 to 101) ms | 103 (101 to 106) ms | 105 (102 to 109) ms |
| frames per tab per second | 18.0 (18.0 to 18.1) | 19.7 (19.6 to 19.7) | 19.6 (19.6 to 19.7) |
| bytes per frame on the wire | 1,011 (1,007 to 1,018) | 953 (948 to 962) | 3,141 (3,133 to 3,170) |
| server CPU | 4.9 (4.9 to 5.0) cores | 5.2 (5.1 to 5.2) cores | 2.6 (2.6 to 2.7) cores |
| server memory, peak RSS | 1,159 (1,143 to 1,165) MB | 903 (897 to 905) MB | 825 (819 to 826) MB |
| writer: moves per transaction | 2.4 (2.4 to 2.5) | 2.8 (2.6 to 3.0) | 3.1 (2.9 to 3.3) |
| writer: time per transaction | 2.8 (2.7 to 2.8) ms | 3.0 (2.8 to 3.2) ms | 3.4 (3.1 to 3.6) ms |
| render: time per board frame | 2.7 (2.7 to 2.8) ms | 3.2 (3.1 to 3.3) ms | 4.2 (4.1 to 4.4) ms |
| load tool CPU | 3.9 (3.8 to 3.9) cores | 3.9 (3.8 to 3.9) cores | 5.2 (5.2 to 5.4) cores |

No move failed or went unanswered in any run.

### Where the time goes

- **Compressing is still most of the work.** In a CPU profile at 200 tabs, brotli takes 88% of the server's samples; with zstd, the encoder takes 60% and moving its window another 15% (`profile-200.txt`). A stream now holds a compression slot only while it compresses, not during its network write, so streams keep up: old streams skipped 9,973 frames a run, new ones 3. That is 9% more frames per tab, and with brotli 6% more CPU. The new server makes far less garbage, 57 MB a second against 303 (4 collections a run against 20), and its RSS is 22% lower with brotli, 29% with zstd.
- **zstd at lith's level sends three times brotli's bytes.** lith runs zstd at its fastest level, which finds few of the matches in a frame that changes all eight lanes. Offline, on 120 real frames of this board (`encoders.txt`), a frame with 40 moves costs 3,090 bytes at that level and 961 with brotli quality 4, at 73 µs against 607 µs. zstd's SpeedBetterCompression would send 641 bytes at 165 µs, but holds 5.5 MiB per stream against brotli's 2.1 and the fastest level's 1.6. A frame with one move is 491 bytes with zstd and 341 with brotli.
- **The outcome now goes to SQLite with the move.** The writer is bound by fsync: in a 4-second trace at 200 tabs it spent 71% of the time in the commit's fsync and 7% checkpointing (`trace-200-zstd.txt`), so a little more work per commit shows up as queueing. Each move now also writes its outcome, the live region's text and on a refusal the tab's toasts into `tab_outbox` and `tab_state`, which is what lets them survive a deploy: 59 KiB written to disk per move against 52, 3.0 ms per transaction against 2.8 with brotli. A build with that insert switched off, the same otherwise (`switches/`, two runs each), answers moves as the old binary does: 4.3 to 4.6 ms at p50 and 12.4 to 13.8 ms at p95, against 4.7 to 4.8 and 18.3 to 18.5 ms with the insert. Removing the per-tab index from that table (one random page per move) is the next thing to try.
- **zstd's 204 times are mostly the load tool's.** With zstd the load tool needs 5.2 of its 8 cores (3.9 with brotli), so its moves go out in bursts and it reads the 204s late. In a run with the server's own request log, moves took 6 ms at p50 and 21 ms at p95 inside the server while the tool measured 12.9 and 44.8 ms. With the tool on 14 threads and the server on 6, the same zstd server answered in 6.9 ms (p50) and 18.8 ms (p95), and the outcome arrived after 50 and 82 ms (one run). lith's `ZstdLowMemory` changed nothing there (`switches/`).
- **A less busy server clocks down.** The host's CPUs run intel_pstate in powersave with balance_performance, so a core that idles more runs slower when work arrives: the zstd server, at 2.6 cores, renders a board in 4.2 ms where the brotli one, at 5.2 cores, takes 3.2.
- **The outcome arrives 4 ms later at the median with brotli** (61 against 57 ms) and 2 ms later at p95. 0.7 ms of it is the 204 and 0.5 ms the board render (3.2 against 2.7 ms); the rest is in the frame path (the tick and the queue for compression slots) and was not broken down further.

In short: the writer alone is as fast as before or faster. At 200 tabs with brotli on both sides, the new server sends fewer bytes, holds less memory and delivers more frames, for 6% more CPU, 4 ms more to the outcome at the median and 7 ms more to the 204 at p95, the latter from the outbox insert. With zstd, which lith gives Chrome, it uses half the CPU and sends three times the bytes, until kanban can pick zstd's level through lith.

### The outbox without its index on the tab

Each move writes its outcome for the tab that sent it into `tab_outbox`. That table had an index on `(tab, id)`, and tab ids are random, so every message dirtied one more page somewhere in the index. Three ways to keep the outbox in SQLite, so that it survives a deploy, were built from the code before this change (the writer benches from 46cba93, the servers from 0602f1e, which differs from it only in the page and a flag) and measured on 2026-10-13 (raw data: `results/2026-10/lith/outbox/`):

- **A**, as before: the index on `(tab, id)`.
- **B**, no index: a tab's view reads the ids after the newest message it has, starting at the page's position (`outbox`). A page drawn now starts at the newest message of all, since its own tab's messages all come later. A render walks the messages of every tab since its own last one.
- **C**, a small table with each tab's newest message, and on each message the id of the tab's previous one: a view follows that chain and reads only its own messages, for one more row to update per message.

`BenchmarkMoveCardOutcome` is the writer bench with a tab for each person, so that every move writes its outcome too; "none" is `BenchmarkMoveCard`, the move alone. Three rounds, each run on a quiet host, medians with the lowest and the highest run in brackets. The bytes are what the process wrote to storage (`/proc/self/io`): the WAL and the checkpoints.

| people | KiB written per move: A | B | C | none | moves per second: A | B | C | none |
|---|---|---|---|---|---|---|---|---|
| 1 | 111.6 (106.0 to 115.4) | 103.8 (101.6 to 105.4) | 109.3 (104.7 to 111.6) | 100.7 (100.5 to 102.8) | 363 | 529 | 580 | 388 |
| 50 | 12.9 (12.6 to 13.5) | 7.6 (7.5 to 7.8) | 7.8 (7.6 to 7.9) | 6.9 (6.6 to 7.1) | 5,090 | 7,718 | 8,900 | 6,429 |
| 200 | 8.8 (7.9 to 9.0) | 2.6 (2.6 to 2.7) | 2.7 (2.7 to 2.7) | 2.2 (2.2 to 2.3) | 10,580 | 15,380 | 16,430 | 18,760 |

Without the index an outcome costs 0.4 KiB per move at 200 people instead of 6.6, and 0.7 instead of 6.0 at 50. C saves as much in large batches, but its small table adds a page of its own to every transaction, which shows when a transaction holds a move or two (8.6 KiB per move for one person against B's 3.1). The moves per second vary much more from run to run on this host than the bytes do (B at 200 people: 14,370 to 17,260).

The 200-tab run with brotli streams, the four builds alternating; N is a build with the outbox insert switched off, as in the first round:

| | A, index on the tab | B, no index | C, chain per tab | N, no outcome |
|---|---|---|---|---|
| runs | 7 | 7 | 3 | 7 |
| send to 204, p50 | 4.0 (3.6 to 5.2) ms | 4.4 (3.3 to 5.1) ms | 3.8 (3.3 to 4.1) ms | 3.4 (3.1 to 4.0) ms |
| send to 204, p95 | 16.5 (10.2 to 27.1) ms | 14.6 (7.6 to 23.1) ms | 14.2 (14.0 to 17.5) ms | 11.6 (6.9 to 17.8) ms |
| send to outcome on the stream, p50 | 50 (47 to 57) ms | 52 (46 to 55) ms | 50 (47 to 50) ms | |
| send to outcome on the stream, p95 | 84 (76 to 103) ms | 89 (74 to 104) ms | 84 (76 to 85) ms | |
| server CPU | 4.0 (3.7 to 4.8) cores | 4.1 (3.6 to 4.4) cores | 3.9 (3.6 to 3.9) cores | 3.8 (3.5 to 4.4) cores |
| writer: moves per transaction | 2.3 (2.1 to 2.6) | 2.3 (1.9 to 2.5) | 2.1 (2.0 to 2.2) | 2.0 (1.8 to 2.2) |
| writer: time per transaction | 2.4 (2.2 to 2.8) ms | 2.6 (2.0 to 2.7) ms | 2.3 (2.1 to 2.4) ms | 2.1 (1.9 to 2.4) ms |

Other agents' builds and tests came and went on the host meanwhile. In the last four rounds the rest of the host used 1.2 to 5.4 cores during a run (`others.txt`), and a run's p95 follows that more than the build: A's went from 11.7 ms with 1.3 cores busy beside it to 27.1 ms with 5.4. These runs cannot tell A from B; their medians lean the way the bench does. At two moves per transaction the index adds a page or two to a commit, which is small against the noise here. Compare builds only within one set of runs: in the second round below, on a quieter host, the same build as B used 5.1 cores for the same frames and the load tool 3.8 instead of 2.5, so the whole host did less per CPU second then. Its cores, idle between ticks, averaged 2.4 to 3.4 GHz in those brotli runs (they reach 4.8), and how far they ramp up depends on what else the host runs.

B's cost moves to the reads. A render walks every tab's messages since its own last one, about 115 ns each (`scan.txt`: 1,000 messages take 0.19 ms, 100,000 take 11.5 ms). At 200 tabs moving four times a second that is about 200 messages and 25 µs a render; the most is a tab that sat idle beside such a board for the sweep's ten minutes, 480,000 messages and 55 ms, once.

Kept B: it writes the fewest bytes per outcome and adds no statement to a command, and it survives a deploy as A did, since the page reports the last message it got and the new process reads from there (`TestOutcomeAfterFrameAndAcrossRestarts`). C costs a page per transaction more than B, which is most of what A costs when a transaction holds a move or two.

### Second round, 2026-10-13

Since the first round, kanban took up the APIs lith gained meanwhile (commits 04b1e6c to 0f1f88a, on lith fa83bb4; the first round's new binary was built on lith d9e58fb, before its livefix, edge and core merges): the heartbeat as a signal the page's watchdog reads, a tick's frames in one compressed write per stream (`Spec.WriteTick`), renders that query on their worker's prepared statements (`sqlite.QuerierFrom`), op ids recorded with their refusals by `writer.OnceResult`, lith's default read pool, and the outbox without its index on the tab (above). `-zstd-level` sets zstd's level (`sse.Options.ZstdLevel`).

Method as in the first round: the binary and the test binary of 752aaeb against those of 0f1f88a, Go 1.26.5 for both, `CGO_ENABLED=0 -trimpath`, old and new alternating with the order rotating, medians with the lowest and the highest run in brackets. The round started with the load average at 1.6. Every run waited until the rest of the host had used fewer than three cores for ten seconds, and a run during which the rest of the host used more than two was run again, at most twice: `runs.log` lists every run with the cores the rest of the host used, `disturbed/` keeps the runs done again, and `clocks.txt` the mean clock of the server's and the load tool's cores, once a second. Raw data: `results/2026-10/lith/second-round/`.

#### The writer alone

Five rounds of `taskset -c 0-11 ./app.test -test.run X -test.bench '^BenchmarkMoveCard$/' -test.benchtime 5s` for each binary, and six more of the one-person case:

| people moving at once | moves per second, old | new | moves per transaction, old / new | time per transaction, old / new |
|---|---|---|---|---|
| 1 | 317 (264 to 368) | 323 (262 to 512) | 1 / 1 | 3.1 / 3.0 ms |
| 50 | 5,651 (5,241 to 7,524) | 6,055 (5,488 to 9,573) | 25 / 25 | 4.4 / 4.1 ms |
| 200 | 13,894 (13,074 to 15,005) | 14,928 (13,811 to 17,103) | 104 / 108 | 7.5 / 7.1 ms |

As in the first round, the bench moves without a tab and writes no outcome. One person waits for the fsync either way; with 50 and 200 people the new writer handles 7% more moves.

#### 200 tabs

`load.sh` with a load tool built once from 0f1f88a: the old binary with Chrome's Accept-Encoding, which it answers with brotli; the new one with `br` alone (brotli quality 4, as in the first round); the new one with Chrome's Accept-Encoding at zstd's fastest level, lith's default; and at zstd's default level (`SERVER_ARGS="-zstd-level default"`). Five rounds of four runs:

| | old, brotli | new, brotli | new, zstd fastest | new, zstd default |
|---|---|---|---|---|
| moves per second | 790 (789 to 792) | 790 (790 to 794) | 787 (784 to 788) | 785 (784 to 787) |
| send to 204, p50 | 4.5 (4.4 to 4.7) ms | 4.3 (4.2 to 4.4) ms | 9.9 (6.0 to 11.4) ms | 9.0 (7.6 to 9.8) ms |
| send to 204, p95 | 12.9 (12.1 to 16.1) ms | 13.1 (12.8 to 18.9) ms | 43.3 (38.0 to 50.9) ms | 40.2 (38.4 to 41.9) ms |
| send to outcome on the stream, p50 | 57 (56 to 58) ms | 58 (55 to 59) ms | 59 (53 to 61) ms | 58 (56 to 59) ms |
| send to outcome on the stream, p95 | 100 (99 to 103) ms | 97 (95 to 100) ms | 102 (95 to 109) ms | 100 (96 to 101) ms |
| frames per tab per second | 18.0 (17.9 to 18.1) | 19.7 (19.6 to 19.7) | 19.6 (19.4 to 19.6) | 19.7 (19.6 to 19.7) |
| bytes per frame on the wire | 1,010 (1,005 to 1,011) | 967 (961 to 970) | 3,175 (3,154 to 3,193) | 2,243 (2,240 to 2,250) |
| server CPU | 4.9 (4.9 to 5.0) cores | 5.1 (4.7 to 5.2) cores | 2.7 (2.3 to 2.7) cores | 2.7 (2.5 to 2.7) cores |
| server memory, peak RSS | 1,162 (1,150 to 1,183) MB | 900 (898 to 902) MB | 821 (820 to 833) MB | 1,267 (1,247 to 1,274) MB |
| writer: moves per transaction | 2.4 (2.3 to 2.4) | 2.3 (2.3 to 2.4) | 2.6 (2.3 to 2.8) | 2.6 (2.5 to 2.6) |
| writer: time per transaction | 2.7 (2.6 to 2.8) ms | 2.5 (2.4 to 2.6) ms | 2.9 (2.5 to 3.0) ms | 2.9 (2.7 to 2.9) ms |
| render: time per board frame | 2.7 (2.7 to 2.9) ms | 3.3 (3.0 to 3.4) ms | 4.5 (3.6 to 4.8) ms | 4.3 (3.9 to 4.6) ms |
| load tool CPU | 3.9 (3.9 to 3.9) cores | 3.8 (3.5 to 3.9) cores | 5.2 (4.4 to 5.2) cores | 5.1 (4.9 to 5.2) cores |

No move failed or went unanswered. The rest of the host used 1.1 to 1.9 cores during the 200-tab runs, and none had to be done again. During the bench runs the same measure also counts the kernel's work for the bench's own fsyncs; 13 of them were done again.

- **The 204 is back to the old binary's.** With brotli a move's 204 takes 13.1 ms at p95 against the old binary's 12.9, where the first round measured 21.4 against 14.4. Since then the outbox lost its index on the tab (above), and lith's writer, hub and edge changed too; these runs do not separate their shares.
- **The outcome arrives as soon as with the old binary:** 58 against 57 ms at p50 and 97 against 100 ms at p95, where the first round had 61 against 57 and 103 against 101.
- **CPU and memory are as in the first round.** With brotli the new server uses 5.1 cores against 4.9 for 9% more frames, peaks at 900 MB against 1,162 and sends 967 bytes a frame against 1,010. Writing a tick's frames in one write did not lower the CPU measurably.
- **zstd's default level** sends 29% fewer bytes than the fastest level (2,243 against 3,175 a frame) for the same server CPU, 2.7 cores, and peaks 446 MB higher (1,267 against 821 MB), about 2.2 MB more per busy stream. Brotli still sends the fewest bytes, for twice the CPU. Kanban keeps lith's default, the fastest level; `-zstd-level default` makes the trade.
- **zstd's 204 times are still mostly the load tool's**, which needs 5.1 to 5.2 of its 8 cores to decode zstd against 3.8 for brotli.
- **The cores ran well below their top clock.** The server's averaged 2.4 to 3.4 GHz in the brotli runs and 1.9 to 2.8 GHz with zstd (`clocks.txt`); they reach 4.8. A less busy server, and a quieter host, get lower clocks and so more CPU seconds per frame.

In short: on current lith, the new binary answers a move and delivers its outcome as fast as the binary before the move, and its writer is 7% faster under load. With brotli it uses 4% more CPU and 23% less memory; with zstd at lith's fastest level, half the CPU for three times brotli's bytes, and at zstd's default level 29% fewer bytes for 2.2 MB more per stream.
