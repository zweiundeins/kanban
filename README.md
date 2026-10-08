# Kanban

A collaborative Kanban board, rendered on the server. Several people move cards on the same board and see each other's changes as they happen. Nothing on the page claims more than the server has confirmed.

Live demo: [kanban.zweiundeins.gmbh](https://kanban.zweiundeins.gmbh), with the demo accounts below; it goes back to its seed every night at 03:00 UTC.

It is one static Go binary (13.5 MB) with one SQLite file, Datastar in the browser and [Starbase](https://starbase.zweiundeins.gmbh) components for dragging, renaming in place, the card dialog and messages. It is a case study by [zwei und eins](https://zweiundeins.gmbh), compared with [PLANKA](https://planka.app) Community (React, Redux, Sails.js, PostgreSQL, WebSockets).

## What it does

- Log in with email and password. A project holds boards.
- Lists: add, rename in place, move by dragging their grip, with Alt and the arrow keys on it, or from the list menu, archive.
- Cards: add, rename, move by dragging or with the keyboard (focus a card, hold Alt, use the arrow keys, release Alt), archive.
- Card dialog: Markdown description (raw HTML stays off), labels from the board's set, a due date and whether it's done, comments (delete your own). Every card has its own URL.
- Collaboration: everyone on a board sees every change, and who else is on the board or has a card open. When two people move the same card or list, the server takes the first move and tells the second person who moved it.

Compared with PLANKA, there are no card members, search or filters, other board views, activity history, comment editing, card deletion (only archiving), stopwatch, attachments, task lists, custom fields, notifications, webhooks, REST API or two-factor login.

## How it works

- **Commands and views (CQRS).** Each action is a `POST` that runs one command and answers `204`. The page learns the outcome from its own stream, which sends the whole board (`view = f(state)`) after every change. A board is rendered at most every 50 ms, once for everyone on it. Unchanged cards come from a cache keyed by their content. A slow connection gets the newest frame instead of a backlog.
- **Brotli over the stream.** The compression window is larger than a frame, and a frame carries only the lists that changed, so a frame after one move costs 0.5 to 1 kB on the wire. The window is per connection, so each stream compresses for itself; everything else about a frame is done once, and at most half the CPUs compress at a time, so actions never wait behind a burst of frames.
- **One writer, batched.** All writes go through one goroutine and one SQLite connection. While a batch commits, new commands queue up. The next transaction takes all of them, each in its own savepoint, so one refusal undoes only itself. Checks like "did this card move since the page was drawn" run inside the command, in microseconds, with no lock held across the network. This follows Anders Murphy's findings on SQLite and contention. The writer prepares each statement once on its connection and keeps it, since the driver would parse every statement again on every call. Reads use their own pool. SQLite runs with `synchronous=FULL`, so it is as durable as PostgreSQL's defaults.
- **No optimistic updates.** A dropped card stays where the server last put it, looking pending, until the frame that shows it in its new place arrives. Meanwhile the board (Starbase's landing marker) opens a gap where the card was dropped and shows a dashed copy there: what the person did, not what the server has done. Then the move's outcome follows on the same stream. A refused move is marked and explained. Every action carries an op id, so a retried request gets the first answer instead of acting twice.
- **Robust streams.** The stream reconnects after a deploy or restart and always starts with the whole board. A watchdog replaces a connection that went quiet. A banner says when the page may be out of date.

## Run it

Needs Go 1.26 or later. Chromium and Node 24 are only needed for the end-to-end tests.

```sh
go tool task dev        # seed data/kanban.db if needed, run with live reload on http://127.0.0.1:8080
go tool task test       # go vet, gofmt, go test -race
go tool task e2e        # the real binary in Chromium: honest pending, two people, conflicts, restarts, security
go tool task build      # ./kanban, static, everything embedded
```

The demo accounts are `anna@example.com`, `ben@example.com`, `chiara@example.com` and `dev@example.com`, all with the password `demo`. Open two browsers to see each other.

`kanban serve` takes flags (or `KANBAN_*` environment variables). The main ones:
- `-addr` and `-db`
- `-secure` and `-proxied`, behind a TLS proxy
- `-origins` for allowed `Origin` values
- `-reset-at 03:00` to put a public demo back to the seed every night, in process, keeping the accounts
- `-synchronous NORMAL` (faster, but can lose the last commits on power loss)
- `-brotli-level` and `-brotli-lgwin`
- `-send-slots`, how many streams may compress a frame at once (half the CPUs by default, so actions don't wait behind a burst of frames)

`kanban backup -db … -to …` writes a consistent copy while the server runs.

## Deploy

`deploy/` has a sandboxed systemd unit, a backup timer (every six hours, fourteen days kept), a Caddy snippet and an install script:

```sh
go tool task build
scp kanban deploy/* root@host:/tmp/kanban-deploy/
ssh root@host sh /tmp/kanban-deploy/install.sh
```

On SIGTERM the server stops accepting connections, ends the streams and commits what is queued. The pages reconnect to the new process.

## Measured

`cmd/loadtest` opens real streams (brotli, like a browser) and moves random cards on the 200-card board, on schedule. It measures what a person waits for: from sending a move to its outcome arriving on their own stream.

Conditions: Intel i5-13500, the server on its performance cores and the load tool on its efficiency cores, `synchronous=FULL`, brotli quality 4, medians of three 30-second runs, 2026-10-08 (`benchmarks/scripts/load.sh`).

| | 50 tabs, a move each per second | 200 tabs, four moves each per second |
|---|---|---|
| moves per second | 48 | 790 |
| refused as stale (two people, one card) | 15 of 1,508 | 4,425 of 23,967 |
| send to 204, p95 | 13.6 ms | 12.2 ms |
| send to outcome on the stream, p95 | 77 ms | 99 ms |
| bytes per frame on the wire | 532 | 999 |
| server CPU | 1.6 cores | 4.9 cores |
| server memory, peak RSS | 258 MB | 1.2 GB |

Most of the server's work under load is compressing: every stream compresses every frame for itself. Each busy stream holds about 2.2 MB of brotli state at quality 4; 200 quiet tabs need 150 MB in all. `-brotli-level 3` needs about a quarter less memory and sends about 60% more bytes per frame.

The writer alone (`go test ./internal/app -bench MoveCard`) handles about 13,800 moves a second when 200 people move at once, about 5,100 with 50, and 240 to 340 for one person, whose every move waits for its own fsync. Before it prepared each statement once (2026-10-08), preparing them again on every call took 41% of its CPU, and it handled about 10,000.

### Compared with PLANKA

Same boards in both apps; the measured person on a slowed phone (4x CPU) behind a 150 ms round trip. Three runs each, 60 moves per app; details, method and raw data are in [benchmarks/RESULTS.md](benchmarks/RESULTS.md).

| | Ours | PLANKA Community 2.2.1 |
|---|---|---|
| Cold load of the 200-card board | 58 kB, painted after 0.6 s | 2.6 MB, painted after 15.4 s |
| Lighthouse, mobile | 98 | 33 |
| Move a card: shown in its new lane | 252 ms, already confirmed (a marker shows the drop at once) | 481 ms, optimistic |
| Move a card: confirmed by the server | 253 ms | 712 ms |
| A second person sees the move | 199 ms | 656 ms |
| Add a comment: confirmed | 302 ms | 229 ms (shown at 90 ms, optimistic) |
| INP over the session | 64 ms | 256 ms |
| JS heap | 4.4 MB | 45.0 MB |
| On the server | one 13.5 MB binary, 28 MB RSS at rest, a 143 kB database | two containers (673 MB of images), 200 MB RAM at rest, a 10.3 MB database |

## Licence

MIT. The vendored Starbase components in `web/static/vendor/starbase` (one file each) are MIT. Those from derekr's PD rockets (`kanban-board`, `inline-edit`) are Beer-Ware. Datastar is MIT. See `web/static/vendor/VERSION`.
