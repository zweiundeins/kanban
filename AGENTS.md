# Kanban: notes for coding agents

A collaborative Kanban board in Go, built as the case study planned in `/develop/2und1.ch/docs/case-studies/kanban.md` (with the shared rules in `README.md` there). One static binary, one SQLite file, Datastar on the client, Starbase components. Compared with PLANKA Community.

## Rules that shape every change

- **CQRS.** Actions (`POST`) send one command to the writer and answer `204`. Nothing they did travels in their response: the page learns it from its stream. Views are `view = f(state)`: the stream sends the whole board after every change.
- **No optimistic updates.** The page may show the user's own input at once (the drag preview, typed text). It shows an outcome only when the stream brings it: a card moves when the frame shows it there, an input clears when the outcome says `ok`. While an action waits, its element looks pending; a refused action says why. Never add client-side state that predicts the server.
- **One writer.** Every write is a `db.Command` run by `db.Writer`, batched with other commands into one transaction, each in its own savepoint. Read-before-write checks (did the card move since the page was drawn) go inside the command. Never write through `DB.Read`, never hold a transaction across anything slow.
- **Shared renders.** The board fragment and a card's dialog are the same bytes for every viewer. Nothing about the viewer goes into them: per-user looks come from CSS (`[data-author="<me>"]` in the layout) or from the tab's own messages.
- **Op ids.** Every action carries an op id from the page; its outcome is stored in `ops`, so a retried request gets the first answer. The page's `$_pend[key]` holds the op, `$_acks[op]` its outcome.

## Layout

- `cmd/kanban`: the binary (`serve`, `seed`, `reset`, `backup`, `export`). `cmd/loadtest`: simulated tabs moving cards, measuring send-to-outcome. `cmd/netem`: a TCP proxy with a round trip and bandwidth limits, for the benchmarks (Chrome's emulation doesn't delay open streams).
- `benchmarks/`: `RESULTS.md`, the PLANKA setup (`planka/`: compose file, `setup.sh`, `seed.mjs` loads `kanban export` output through its API), and the scripts (`compare-bench.mjs` for both apps, `compare.sh`, `summarize.py`, `lighthouse-auth.mjs`, `load.sh` and `summarize-load.py`). `go test ./internal/app -bench MoveCard` measures the writer alone.
- `internal/db`: open (one write connection, a read pool), migrations (`migrations/NNN_*.sql`, `PRAGMA user_version`), the writer.
- `internal/app`: queries (`read.go`), commands with their rules (`commands.go`), positions, auth (argon2id, hashed session tokens), seed and reset.
- `internal/live`: tabs, rooms, the render loop (throttled per board), per-stream delivery with latest-frame-wins and the tab outbox (an outcome waits for a frame of at least its version). A frame's Datastar events (`Event`) are formatted once and shared by every stream; only the compression is per stream, and `Hub.Slots` lets half the CPUs compress at once.
- `internal/web`: routes, middleware (CSP with hashes, Origin checks, sessions, compression), handlers, the renderer with content-addressed caches, the event stream (`sse.go`: brotli or gzip, one window per stream).
- `internal/views`: templ components. `expr.go` builds the Datastar expressions (`Act`, `Pending`, `Done`): use them instead of writing expressions by hand.
- `web/static`: `app.css`, the icon, and `vendor/` (Starbase components as one file each, from Starbase's `cmd/dist`, and its patched Datastar build, pinned; see `vendor/VERSION`, update with `task starbase`).
- `e2e/`: Node test runner with playwright-core against the real binary. `deploy/`: systemd units, backup timer, Caddy snippet, install script.

## Commands

- `go tool task dev` (air), `go tool task test` (vet, gofmt, `go test -race`), `go tool task e2e` (needs Chromium; `CHROME=` to point at it), `go tool task build`.
- `go tool templ generate` after editing `.templ` files; the generated `_templ.go` files are committed.
- Load test: `benchmarks/scripts/load.sh` (three runs each of 50 and 200 tabs, server and load tool on separate cores), then `python3 benchmarks/scripts/summarize-load.py benchmarks/results/<yyyy-mm>`. One run by hand: `kanban serve -action-limit 0 …`, then `go tool task load -- -base http://127.0.0.1:8080 -tabs 200 -interval 250ms`.

## Gotchas

- Cards must stay free of Datastar expressions: 200 cards times a few expressions is a lot of markup and compiled functions per frame. Card behaviour is delegated to the board (`openOnClick`, `openOnEnter`), and pending cards are styled by `#pending-style`, computed from the signals outside the patched regions (a frame can't strip it).
- Generated expressions are comma expressions, so they can be wrapped: `cond && (` + `Act(…)` + `)`.
- Per-stream work is the server's main cost under load: every stream compresses every frame it sends. Keep it free of copies and formatting (they belong in `Frame.prepare`, once per frame).
- The stream opens with `retry: 'always'`: with the default, Datastar doesn't reopen a stream the server closed cleanly (a deploy). A watchdog replaces a stream that went quiet (no `_ping` for 50 s).
- Starbase's patched Datastar build is required: reordered keyed cards containing Rocket components crash the official 1.0.4 (starfederation/datastar#1209).
- The board has `data-kanban-landing`: after a drop, Starbase's landing marker (a dashed copy in `<body>`, `data-landing-marker`) holds a gap where the card or lane goes until the frame moves it there. `LandingRelease` (board.templ) releases the marker of a refused move from the ack signals. Test selectors for cards must skip markers (`card()` in e2e/harness.mjs does).
- Lanes move through sb-kanban-board's lane support (Starbase b91a1a5): the grip, Alt and the arrows on it, and the menu's step buttons all end in one `sb-kanban-lane-move` handler. A lane carries `data-seen` (its move version): the server refuses a move from a page that hasn't seen the last one.
- The login form is rate limited (10 a minute per IP); the e2e harness logs each person in once.
- Inputs inside patched regions carry `data-ignore-morph` and get their value from a signal: a frame must not reset what someone is typing.
