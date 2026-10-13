package store

// migrations are applied in order; the index of each entry + 1 is its schema version.
// Once a version has been released, never edit its migration: add a new one instead.
// (Migration 1 was revised before any release, after the P1-T2 review.)
var migrations = []string{
	// 1: initial schema (plan section 8).
	`
CREATE TABLE meta (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);

-- path_key is the path upper-cased in Go (all letters, not just ASCII), used for
-- uniqueness and lookups because Windows paths are case-insensitive.
CREATE TABLE roots (
  id        INTEGER PRIMARY KEY AUTOINCREMENT,
  path      TEXT NOT NULL,
  path_key  TEXT NOT NULL UNIQUE,
  enabled   INTEGER NOT NULL DEFAULT 1,
  added_at  INTEGER NOT NULL
);

CREATE TABLE excludes (
  id        INTEGER PRIMARY KEY,
  root_id   INTEGER NOT NULL REFERENCES roots(id) ON DELETE CASCADE,
  path      TEXT NOT NULL,
  path_key  TEXT NOT NULL,
  UNIQUE (root_id, path_key)
);

-- AUTOINCREMENT: ids are never reused, so a stale reference (e.g. in the in-memory
-- vector cache) can never point at a different, newer file or item.
CREATE TABLE files (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  root_id      INTEGER NOT NULL REFERENCES roots(id) ON DELETE CASCADE,
  path         TEXT NOT NULL,
  path_key     TEXT NOT NULL UNIQUE,
  name         TEXT NOT NULL,
  ext          TEXT NOT NULL,
  kind         TEXT NOT NULL,
  size         INTEGER NOT NULL,
  mtime        INTEGER NOT NULL,
  content_hash TEXT,
  status       TEXT NOT NULL,
  quality      TEXT,
  error        TEXT,
  indexed_at   INTEGER
);
CREATE INDEX files_root   ON files(root_id);
CREATE INDEX files_status ON files(status);
CREATE INDEX files_hash   ON files(content_hash);

CREATE TABLE items (
  id        INTEGER PRIMARY KEY AUTOINCREMENT,
  file_id   INTEGER NOT NULL REFERENCES files(id) ON DELETE CASCADE,
  kind      TEXT NOT NULL,
  locator   TEXT,
  text      TEXT
);
CREATE INDEX items_file ON items(file_id, kind);

CREATE TABLE vectors (
  item_id  INTEGER PRIMARY KEY REFERENCES items(id) ON DELETE CASCADE,
  bin      BLOB NOT NULL,
  i8       BLOB NOT NULL,
  scale    REAL NOT NULL
);

-- Keyword index. Contentless (stores no copy of the text) and contentless_delete=1 so
-- rows can be deleted by rowid. rowid = items.id. prefix='2 3' adds a prefix index so
-- typing a short word prefix stays fast.
CREATE VIRTUAL TABLE items_fts USING fts5(
  name, text,
  content='',
  contentless_delete=1,
  prefix='2 3',
  tokenize='unicode61 remove_diacritics 2'
);

-- Deleting an item (directly, or through ON DELETE CASCADE from files/roots) removes its
-- keyword row too, so deleted files never stay searchable (plan P-5).
CREATE TRIGGER items_fts_delete AFTER DELETE ON items BEGIN
  DELETE FROM items_fts WHERE rowid = old.id;
END;

CREATE TABLE thumbs (
  file_id  INTEGER PRIMARY KEY REFERENCES files(id) ON DELETE CASCADE,
  path     TEXT NOT NULL
);
`,
	// 2: keyword first (P3-T3). A file's passages are stored (and keyword-searchable) as
	// soon as its text is read; vectors are added by a second pass. embed tracks that:
	// 0 = nothing to do (has a vector, or keyword-only by design), 1 = waiting for its
	// vector, 2 = the embedding model failed on it (not retried).
	`
ALTER TABLE items ADD COLUMN embed INTEGER NOT NULL DEFAULT 0;
-- Waiting passages in the order they were stored, which is the indexing priority order.
CREATE INDEX items_embed_pending ON items(id) WHERE embed = 1;
`,
}
