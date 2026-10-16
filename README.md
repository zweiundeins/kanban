# Kanban

A collaborative Kanban board, rendered on the server. Several people move cards on the same board and see each other's changes as they happen. Nothing on the page claims more than the server has confirmed.

Live demo: [kanban.zweiundeins.gmbh](https://kanban.zweiundeins.gmbh), with the demo accounts below; it goes back to its seed every night at 03:00 UTC.

It is one static Go binary (17.1 MB) with one SQLite file, Datastar in the browser and [Starbase](https://starbase.zweiundeins.gmbh) components for dragging, renaming in place, the card dialog and messages. It runs on lith, the runtime our Go and SQLite apps share: its single writer, its render loop with shared views, its compressed streams and its request edge. It is a case study by [zwei und eins](https://zweiundeins.gmbh), compared with [PLANKA](https://planka.app) Community (React, Redux, Sails.js, PostgreSQL, WebSockets).

## What it does

- Log in with email and password. A project holds boards.
- Lists: add, rename in place, move by dragging their grip, with Alt and the arrow keys on it, or from the list menu, archive.
- Cards: add, rename, move by dragging or with the keyboard (focus a card, hold Alt, use the arrow keys, release Alt), archive.
- Card dialog: Markdown description (raw HTML stays off), labels from the board's set, a due date and whether it's done, comments (delete your own). Every card has its own URL.
- Collaboration: everyone on a board sees every change, and who else is on the board or has a card open. When two people move the same card or list, the server takes the first move and tells the second person who moved it.

Compared with PLANKA, there are no card members, search or filters, other board views, activity history, comment editing, card deletion (only archiving), stopwatch, attachments, task lists, custom fields, notifications, webhooks, REST API or two-factor login.

## How it works

- **Commands and views (CQRS).** Each action is a `POST` that runs one command and answers `204`. The page learns the outcome from its own stream, which sends the whole board (`view = f(state)`) after every change. A board is one view, rendered at most every 50 ms, once for everyone on it, in one read transaction with the dialogs of the cards open in its tabs. Unchanged cards come from a cache keyed by their content. A slow connection gets the newest frame instead of a backlog.
- **Compressed streams.** A stream compresses with zstd when the browser accepts it, else brotli. The window is larger than a frame and a frame carries only the lists that changed, so a frame after one move costs about half a kilobyte on the wire. The window is per connection, so each stream compresses for itself; everything else about a frame is done once, and at most half the CPUs compress at a time, never while waiting for the network, so actions don't wait behind a burst of frames.
- **One writer, batched.** All writes go through one goroutine and one SQLite connection. While a batch commits, new commands queue up. The next transaction takes all of them, each in its own savepoint, so one refusal undoes only itself. Checks like "did this card move since the page was drawn" run inside the command, in microseconds, with no lock held across the network. This follows Anders Murphy's findings on SQLite and contention. The writer prepares each statement once on its connection and keeps it, since the driver would parse every statement again on every call, and it checkpoints the WAL between batches. Reads use their own pool. SQLite runs with `synchronous=FULL`, so it is as durable as PostgreSQL's defaults.
- **No optimistic updates.** A dropped card stays where the server last put it, looking pending, until the frame that shows it in its new place arrives. Meanwhile the board (Starbase's landing marker) opens a gap where the card was dropped and shows a dashed copy there: what the person did, not what the server has done. Then the move's outcome follows on the same stream. A refused move is marked and explained. Every action carries an op id, so a retried request gets the first answer instead of acting twice.
- **Robust streams.** The stream reconnects after a deploy or restart and always starts with the whole board. An action's outcome is written to SQLite by the same command, so it survives a deploy: the page says which outcome it got last, and the new process sends the rest, after a frame that shows them. A watchdog replaces a connection that went quiet. A banner says when the page may be out of date.

## Run it

Needs Go 1.27 or later. Chromium and Node 24 are only needed for the end-to-end tests.

```sh
go tool task dev        # seed data/kanban.db if needed, run with live reload on http://127.0.0.1:8080
go tool task test       # go vet, gofmt, go test -race
go tool task e2e        # the real binary in Chromium: honest pending, two people, conflicts, restarts, security
go tool task build      # ./kanban, static, everything embedded
```

The demo accounts are `anna@example.com`, `ben@example.com`, `chiara@example.com` and `dev@example.com`, all with the password `demo`. Open two browsers to see each other.

`kanban serve` takes flags (or `KANBAN_*` environment variables). The main ones:
- `-addr` and `-db`
- `-metrics`, where the metrics for Prometheus listen, never on the public listener: `auto` (the default) is 127.0.0.1 at `-addr`'s port + 100, `off` is none, and an address off loopback is refused
- `-secure`, behind a TLS proxy (the client's address comes from the proxy's headers when the request arrives over loopback)
- `-origins` for allowed `Origin` values
- `-reset-at 03:00` to put a public demo back to the seed every night, in process, keeping the accounts
- `-synchronous NORMAL` (faster, but can lose the last commits on power loss)
- `-brotli-level` and `-brotli-lgwin`, for the streams that get brotli
- `-zstd-level` (`fastest`, lith's default, `default`, `better` or `best`), for the streams that get zstd

`kanban backup -db … -to …` writes a consistent copy while the server runs.

## Deploy

`deploy/` has a sandboxed systemd unit, a backup timer (every six hours, fourteen days kept), the Caddy site block, the host's monitoring drop-ins and an install script:

```sh
go tool task build
scp -r kanban deploy/* root@host:/tmp/kanban-deploy/
ssh root@host sh /tmp/kanban-deploy/install.sh
```

On SIGTERM the server stops accepting connections, ends the streams and commits what is queued. The pages reconnect to the new process.

The unit listens on 127.0.0.1:8095 for Caddy and serves the metrics on 127.0.0.1:8195 for Prometheus. It may bind those two ports and no other, and reaches no host but its own. A port changes in the unit's flags and `SocketBindAllow` together, and in `kanban.caddy` or `observability/kanban.metrics.yml`; `TestMetricsPort` checks that the unit and the scrape file agree.

### On a shared host

The host keeps its own configs for Caddy, Prometheus and Alloy, and each loads a folder of files that the apps install: lith's guide [Metrics and monitoring](https://lith.zweiundeins.gmbh/docs/guides/monitoring#drop-in-files-for-a-shared-host) has the folders and the labels. Kanban's:

- `deploy/kanban.caddy` as `/etc/caddy/sites/kanban.caddy`: the site block, with its JSON access log in `/var/log/caddy/kanban-access.log`.
- `deploy/observability/kanban.metrics.yml` as `/etc/prometheus/apps/kanban.yml`: Prometheus scrapes `127.0.0.1:8195`, labelled `app: kanban` and `slot: prod`, and the host's "lith apps" dashboard and alert rules show kanban: `kanban_writer_*`, `kanban_live_*`, `kanban_sse_budget_*`, `kanban_db_*`, `go_*` and `process_*`. Prometheus watches the folder.
- `deploy/observability/kanban.targets.yml` as `/etc/prometheus/targets/kanban.yml`: blackbox probes of `/`, a redirect (the host's `http_redirect` module), and `/login` (`http_2xx_kanban`, which wants "Kanban" in the body). Prometheus watches the folder.
- `deploy/observability/kanban.alloy` as `/etc/alloy/kanban.alloy`: Caddy's access log as `{job="caddy", site="kanban"}`, through the host's pipeline for access logs.

`deploy/install-dropins.sh` installs the scrape target, the probes and the Alloy file, each only where its service is installed, reloads Alloy, and puts the old file back if Alloy refuses the new one. `install.sh` runs it last; on its own it changes nothing else (no seed, no restart), so run it alone after changing them:

```sh
scp -r deploy root@host:/tmp/kanban-dropins
ssh root@host bash /tmp/kanban-dropins/install-dropins.sh
```

The Caddy block goes in by hand, since Caddy asks for the certificate as it loads it. As root, from the copied folder:

```sh
install -m 0644 kanban.caddy /etc/caddy/sites/
runuser -u caddy -- env HOME=/var/lib/caddy caddy validate --config /etc/caddy/Caddyfile --adapter caddyfile
systemctl reload caddy
```

Validate as Caddy's user: `caddy validate`, run as root, creates the log file owned by root, and the reload then fails.

## Measured

`cmd/loadtest` opens real streams (compressed as a browser asks) and moves random cards on the 200-card board, on schedule. It measures what a person waits for: from sending a move to its outcome arriving on their own stream.

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

These are the numbers from before the move onto lith; [benchmarks/RESULTS.md](benchmarks/RESULTS.md#on-lith-2026-10-13) compares the two (2026-10-13, two rounds). In the second round on lith, at 200 tabs, the outcome arrives after 97 ms at p95 with brotli (967 bytes per frame, 900 MB peak RSS, 5.1 cores) and 102 ms with zstd (3,175 bytes, 821 MB, 2.7 cores), and a move's 204 takes 13.1 ms at p95 with brotli, as with the binary before the move (12.9 ms).

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

## Maintainer

Maintained by [zwei und eins gmbh](https://zweiundeins.gmbh), Aarberg, Switzerland.
