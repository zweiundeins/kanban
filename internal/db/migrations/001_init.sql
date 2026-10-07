CREATE TABLE users (
	id            INTEGER PRIMARY KEY,
	email         TEXT NOT NULL UNIQUE COLLATE NOCASE,
	name          TEXT NOT NULL,
	color         TEXT NOT NULL,
	password_hash TEXT NOT NULL,
	created_at    INTEGER NOT NULL
) STRICT;

-- The cookie carries a random token; only its SHA-256 is stored.
CREATE TABLE sessions (
	token_hash BLOB PRIMARY KEY,
	user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	created_at INTEGER NOT NULL,
	expires_at INTEGER NOT NULL
) STRICT, WITHOUT ROWID;
CREATE INDEX sessions_user ON sessions(user_id);

CREATE TABLE projects (
	id         INTEGER PRIMARY KEY,
	name       TEXT NOT NULL,
	created_at INTEGER NOT NULL
) STRICT;

-- version goes up with every command that changes anything on the board:
-- a frame shows one version, an acknowledgement waits for it.
CREATE TABLE boards (
	id         INTEGER PRIMARY KEY,
	project_id INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
	name       TEXT NOT NULL,
	position   INTEGER NOT NULL,
	version    INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL
) STRICT;
CREATE INDEX boards_project ON boards(project_id, position);

CREATE TABLE board_members (
	board_id INTEGER NOT NULL REFERENCES boards(id) ON DELETE CASCADE,
	user_id  INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	PRIMARY KEY (board_id, user_id)
) STRICT, WITHOUT ROWID;
CREATE INDEX board_members_user ON board_members(user_id);

-- Positions are integers with gaps (1024, 2048, ...); a list is renumbered
-- in the same command when a gap runs out.
CREATE TABLE lists (
	id          INTEGER PRIMARY KEY,
	board_id    INTEGER NOT NULL REFERENCES boards(id) ON DELETE CASCADE,
	name        TEXT NOT NULL,
	position    INTEGER NOT NULL,
	archived_at INTEGER,
	moved_by    INTEGER REFERENCES users(id),
	move_version INTEGER NOT NULL DEFAULT 0
) STRICT;
CREATE INDEX lists_board ON lists(board_id, position) WHERE archived_at IS NULL;

-- version: any change to the card; move_version: moves only, which is what
-- a move from a stale page conflicts with.
CREATE TABLE cards (
	id           INTEGER PRIMARY KEY,
	board_id     INTEGER NOT NULL REFERENCES boards(id) ON DELETE CASCADE,
	list_id      INTEGER NOT NULL REFERENCES lists(id),
	title        TEXT NOT NULL,
	description  TEXT NOT NULL DEFAULT '',
	position     INTEGER NOT NULL,
	due_on       TEXT,
	due_done     INTEGER NOT NULL DEFAULT 0,
	archived_at  INTEGER,
	version      INTEGER NOT NULL DEFAULT 0,
	move_version INTEGER NOT NULL DEFAULT 0,
	moved_by     INTEGER REFERENCES users(id),
	created_by   INTEGER NOT NULL REFERENCES users(id),
	created_at   INTEGER NOT NULL
) STRICT;
CREATE INDEX cards_list ON cards(list_id, position) WHERE archived_at IS NULL;
CREATE INDEX cards_board ON cards(board_id) WHERE archived_at IS NULL;

CREATE TABLE labels (
	id       INTEGER PRIMARY KEY,
	board_id INTEGER NOT NULL REFERENCES boards(id) ON DELETE CASCADE,
	name     TEXT NOT NULL,
	color    TEXT NOT NULL
) STRICT;
CREATE INDEX labels_board ON labels(board_id);

CREATE TABLE card_labels (
	card_id  INTEGER NOT NULL REFERENCES cards(id) ON DELETE CASCADE,
	label_id INTEGER NOT NULL REFERENCES labels(id) ON DELETE CASCADE,
	PRIMARY KEY (card_id, label_id)
) STRICT, WITHOUT ROWID;
CREATE INDEX card_labels_label ON card_labels(label_id);

CREATE TABLE comments (
	id         INTEGER PRIMARY KEY,
	card_id    INTEGER NOT NULL REFERENCES cards(id) ON DELETE CASCADE,
	user_id    INTEGER NOT NULL REFERENCES users(id),
	body       TEXT NOT NULL,
	created_at INTEGER NOT NULL
) STRICT;
CREATE INDEX comments_card ON comments(card_id, id);

-- Every action carries an op id from the page. Its outcome is kept for a
-- day, so a retried request gets the same answer instead of acting twice.
CREATE TABLE ops (
	user_id    INTEGER NOT NULL,
	op         TEXT NOT NULL,
	board_id   INTEGER NOT NULL,
	result     TEXT NOT NULL,
	version    INTEGER NOT NULL,
	created_at INTEGER NOT NULL,
	PRIMARY KEY (user_id, op)
) STRICT, WITHOUT ROWID;
CREATE INDEX ops_created ON ops(created_at);
