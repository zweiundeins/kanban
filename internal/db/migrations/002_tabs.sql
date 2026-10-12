-- What one open page (a tab) is told about its own actions, in order: the
-- outcome, the live region's text, its toasts. The command that acts writes
-- it; the tab's stream sends it once a frame of the board at version
-- `after` went out, and the page reports the last id it got when its stream
-- reconnects. AUTOINCREMENT: ids never come back after a sweep.
CREATE TABLE tab_outbox (
	id         INTEGER PRIMARY KEY AUTOINCREMENT,
	tab        TEXT NOT NULL,
	user_id    INTEGER NOT NULL,
	board_id   INTEGER NOT NULL,
	after      INTEGER NOT NULL,
	signals    TEXT NOT NULL, -- a JSON merge patch
	status     TEXT,          -- the live region's text (NULL: unchanged)
	toasts     TEXT,          -- the tab's toasts as JSON (NULL: unchanged)
	created_at INTEGER NOT NULL
) STRICT;
CREATE INDEX tab_outbox_tab ON tab_outbox(tab, id);

-- A tab's own state: the messages in its toast region.
CREATE TABLE tab_state (
	tab        TEXT NOT NULL,
	user_id    INTEGER NOT NULL,
	toasts     TEXT NOT NULL DEFAULT '[]',
	toast_n    INTEGER NOT NULL DEFAULT 0,
	updated_at INTEGER NOT NULL,
	PRIMARY KEY (tab, user_id)
) STRICT, WITHOUT ROWID;
CREATE INDEX tab_state_updated ON tab_state(updated_at);
