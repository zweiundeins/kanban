# Kanban: notes for coding agents

A collaborative Kanban board in Go, built as the case study planned in `/develop/2und1.ch/docs/case-studies/kanban.md` (with the shared rules in `README.md` there). One static binary, one SQLite file, Datastar on the client, Starbase components. Compared with PLANKA Community.

It runs on lith (`github.com/zweiundeins/lith`, replaced by `/develop/lith` in go.mod): its writer, live hub, sse and edge. The general rules are lith's skill (`skills/lith/SKILL.md` in the lith repo); this file says what is particular to kanban.

## Rules that shape every change

- **CQRS.** Actions (`POST`) send one command to the writer and answer `204`. Nothing they did travels in their response: the page learns it from its stream. Views are `view = f(state)`: the stream sends the whole board after every change.
- **No optimistic updates.** The page may show the user's own input at once (the drag preview, typed text). It shows an outcome only when the stream brings it: a card moves when the frame shows it there, an input clears when the outcome says `ok`. While an action waits, its element looks pending; a refused action says why. Never add client-side state that predicts the server.
- **One writer.** Every write is a command run by lith's writer (`a.DB.W.Do(ctx, writer.Func(...))`), batched with other commands into one transaction, each in its own savepoint. Read-before-write checks (did the card move since the page was drawn) go inside the command. A command that changes a board calls `touch`, which bumps the board's version and touches `board:<id>`: without it, pages stay stale. Never write through `DB.R`, never hold a transaction across anything slow. The writer prepares each SQL string once, so build SQL from constants, never from values. Reads outside a read transaction go through `DB.QueryRow`/`DB.Query`, which cache their statements too; inside one (`DB.View`, a view's render) use the transaction (preparing on the pool from inside a transaction can wait forever for a connection).
- **Shared renders.** The board fragment and a card's dialog are the same bytes for every viewer. Nothing about the viewer goes into them: per-user looks come from CSS (`[data-author="<me>"]` in the layout) or from the tab's own messages.
- **Op ids.** Every action carries an op id from the page; its outcome is stored in `ops`, refusals too (lith's `writer.Once` keeps successes only), so a retried request gets the first answer. The page's `$_pend[key]` holds the op, `$_acks[op]` its outcome.
- **Outcomes go through SQLite.** The command that acts also writes its outcome for the page's tab (`app.Actor.Tab`): the ack, the live region's text, the signals that empty an input, and on a refusal the tab's toasts, into `tab_outbox` and `tab_state`. The tab's view sends a message once its stream has sent a board frame of at least the message's board version, so an outcome never arrives before the change. A deploy loses none of it: the page sends the last message it got (`outbox`) with its next stream request.

## Layout

- `cmd/kanban`: the binary (`serve`, `seed`, `reset`, `backup`, `export`). `cmd/loadtest`: simulated tabs moving cards, measuring send-to-outcome. `cmd/netem`: a TCP proxy with a round trip and bandwidth limits, for the benchmarks (Chrome's emulation doesn't delay open streams).
- `benchmarks/`: `RESULTS.md` (its "On lith" section compares this code with the binary before the move), the PLANKA setup (`planka/`: compose file, `setup.sh`, `seed.mjs` loads `kanban export` output through its API), and the scripts (`compare-bench.mjs` for both apps, `compare.sh`, `summarize.py`, `lighthouse-auth.mjs`, `load.sh` and `summarize-load.py`). `go test ./internal/app -bench MoveCard` measures the writer alone.
- `internal/db`: opens the file with lith's sqlite (migrations `migrations/NNN_*.sql`, `PRAGMA user_version`), starts lith's writer, which checkpoints the WAL between batches, and caches read statements outside transactions.
- `internal/app`: queries (`read.go`; `ReadBoard`, `ReadCard`, `ReadMembers` take the caller's transaction), commands with their rules (`commands.go`), the tab outbox and toasts (`tabs.go`), positions, auth (argon2id, hashed session tokens), seed and reset.
- `internal/live`: kanban's side of lith's hub. `Board` is a board's frame: its lanes with one delta shared by every stream that had the frame before, the dialogs of the cards open in its tabs, its version; `Prepare` formats the events once. `Stream.Write` (lith's `Spec.Write`) sends a stream only the lanes it lacks, its tab's dialog and the tab's messages. `Seats` knows who is on which board (with a grace period) and which card each tab shows, in memory: the page sends its card again on every connect.
- `internal/web`: routes behind lith's edge (CSRF with Datastar-Request on `POST /boards/`, security headers with the CSP's hashes, compression, precompressed static files under `/static/`), sessions, handlers, the views of a stream (`streams.go`: the tab's outbox, the board, presence and a ping, in that order), the renderer with lith's content-addressed caches.
- `internal/views`: templ components. `expr.go` builds the Datastar expressions (`Act`, `Pending`, `Done`): use them instead of writing expressions by hand.
- `web/static`: `app.css`, the icon, and `vendor/` (Starbase components as one file each, from Starbase's `cmd/dist`, and its patched Datastar build, pinned; see `vendor/VERSION`, update with `task starbase`).
- `e2e/`: Node test runner with playwright-core against the real binary. `deploy/`: systemd units, backup timer, Caddy snippet, install script.

## Commands

- `go tool task dev` (air), `go tool task test` (vet, gofmt, `go test -race`), `go tool task e2e` (needs Chromium; `CHROME=` to point at it), `go tool task build`.
- `go tool templ generate` after editing `.templ` files; the generated `_templ.go` files are committed.
- Load test: `benchmarks/scripts/load.sh` (three runs each of 50 and 200 tabs, server and load tool on separate cores; `KANBAN=<binary>` runs another build, `CONFIGS` and `ACCEPT` pick the runs and the streams' Accept-Encoding), then `python3 benchmarks/scripts/summarize-load.py <folder>`. One run by hand: `kanban serve -action-limit 0 …`, then `go tool task load -- -base http://127.0.0.1:8080 -tabs 200 -interval 250ms`. The load tool sends Chrome's Accept-Encoding, so a new server answers zstd; it needs more CPU to decode zstd than brotli, so give it cores.

## Gotchas

- Cards must stay free of Datastar expressions: 200 cards times a few expressions is a lot of markup and compiled functions per frame. Card behaviour is delegated to the board (`openOnClick`, `openOnEnter`), and pending cards are styled by `#pending-style`, computed from the signals outside the patched regions (a frame can't strip it).
- Generated expressions are comma expressions, so they can be wrapped: `cond && (` + `Act(…)` + `)`.
- Per-stream work is the server's main cost under load: every stream compresses every frame it sends. Keep `Stream.Write` free of copies and formatting: events are formatted in the render (`Board.Prepare`, the tab view's `message`), once per frame.
- A view's `Render` may keep state between renders (the previous frame, the newest message read) because lith never renders one view twice at once; the definition the first stream gave is the one used while anyone watches it, so build it from the key alone.
- The stream opens with `retry: 'always'`: with the default, Datastar doesn't reopen a stream the server closed cleanly (a deploy). A watchdog replaces a stream that went quiet (no `_ping` for 50 s). The ping is a shared view (`pingView`, every 20 s): lith's own heartbeat is an SSE comment, which the page never sees.
- `card` and `outbox` are the page's only signals without an underscore: Datastar sends them with the stream's request (`datastar=` in the query, on every retry). Every action passes an explicit `payload` (`Act`, `UI`), so they don't go along.
- Starbase's patched Datastar build is required: reordered keyed cards containing Rocket components crash the official 1.0.4 (starfederation/datastar#1209).
- The board has `data-kanban-landing`: after a drop, Starbase's landing marker (a dashed copy in `<body>`, `data-landing-marker`) holds a gap where the card or lane goes until the frame moves it there. `LandingRelease` (board.templ) releases the marker of a refused move from the ack signals. Test selectors for cards must skip markers (`card()` in e2e/harness.mjs does).
- Lanes move through sb-kanban-board's lane support (Starbase b91a1a5): the grip, Alt and the arrows on it, and the menu's step buttons all end in one `sb-kanban-lane-move` handler. A lane carries `data-seen` (its move version): the server refuses a move from a page that hasn't seen the last one.
- The login form is rate limited (a token bucket per IP: 10 at once, then one every 6 s); the e2e harness logs each person in once.
- Inputs inside patched regions carry `data-ignore-morph` and get their value from a signal: a frame must not reset what someone is typing.
