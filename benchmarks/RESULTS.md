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

`cmd/loadtest` (ours only) opens real streams with brotli, as a browser does, and moves random cards on the 200-card board. It measures what a person waits for: from sending a move to its outcome arriving on their own stream, after the frame that shows it. Moves go out on schedule, whether or not the last one was answered. `scripts/load.sh` runs each configuration three times for 30 s on a fresh server; the server runs on the six performance cores (CPUs 0 to 11) and the load tool on the eight efficiency cores, so they don't take each other's CPU. The host runs other services, so expect some noise. `python3 scripts/summarize-load.py results/2026-10`, medians of 3 runs:

| | 50 tabs, a move each per second | 200 tabs, four moves each per second |
|---|---|---|
| moves per second | 48 | 788 |
| refused as stale (two people, one card) | 17 of 1,499 | 4,422 of 23,919 |
| send to 204, p50 / p95 | 6.8 / 12.4 ms | 5.5 / 17.0 ms |
| send to outcome on the stream, p50 / p95 | 48 / 75 ms | 56 / 98 ms |
| frames per tab per second | 18.3 | 18.5 |
| bytes per frame on the wire (brotli quality 4) | 510 | 992 |
| server CPU | 1.7 cores | 5.1 cores |
| server memory, peak RSS | 266 MB | 1,137 MB |
| writer: moves per transaction, largest | 1.0, 3 | 2.6, 30 |
| load tool CPU | 1.0 cores | 3.8 cores |

At 50 tabs most moves are a transaction of their own and wait for their own fsync, about 5 ms on this disk; at 200 tabs, moves queue while a transaction commits and share the next one.

Where the server's time goes at 200 tabs: rendering the board is cheap (2.4 ms per frame, once for everyone), and so is the database. Each stream compresses every frame for itself, since brotli's window is per connection; at 800 moves a second every frame changes all eight lists (about 140 kB of HTML), and compressing it for 200 streams is most of the 5 cores. A frame's events are formatted once and shared by all streams, and at most half the CPUs compress at once (`-send-slots`), so a burst of frames doesn't hold up actions. A stream that falls behind skips to the newest frame.

Memory follows the streams: a stream that has received frames keeps brotli's window, hash table and buffers, about 2.2 MB at quality 4, and Go's garbage collector roughly doubles that in RSS. 200 quiet tabs need 150 MB; 200 tabs on a board that changes 20 times a second about 1.1 GB. `-brotli-level 3` needs about a quarter less memory and sends about 60% more bytes per frame.

### The writer alone

`go test ./internal/app -run X -bench MoveCard` moves cards with 1, 50 or 200 people at once, without HTTP or streams, with `synchronous=FULL` (an fsync on every commit), each person moving their own cards so that every move is a write:

| people moving at once | moves per second | moves per transaction | time per transaction |
|---|---|---|---|
| 1 | 240 to 300 | 1 | 3.4 to 4.2 ms |
| 50 | about 5,300 | 25 | 4.7 ms |
| 200 | about 10,000 | 105 | 10 to 11 ms |

Anders Murphy's [SQLite benchmark](https://andersmurphy.com/2025/12/02/100000-tps-over-a-billion-rows-the-unreasonable-effectiveness-of-sqlite.html), whose single-writer batching this design follows, reports 98,163 transactions a second with savepoints and `synchronous=FULL`. Ours is about ten times slower, for three reasons. A move runs about fifteen statements (the op id check, membership, two savepoints, the card, its list, the position, the update, the board version and the op record); his transfer runs two updates. The pure Go SQLite driver prepares each statement again on every call. And his machine was a MacBook, where SQLite's fsync doesn't wait for the drive to empty its cache unless `PRAGMA fullfsync` is on; his settings don't list it. Here a commit waits 3 to 5 ms for the disk, which is the ceiling for one person moving alone. For a board, 10,000 moves a second is far more than people make: 200 people each moving a card every 250 ms send 800.

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
