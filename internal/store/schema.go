package store

// migrations are applied in order; the index of each entry + 1 is its schema version.
// Never edit a migration that has shipped: add a new one instead.
var migrations = []string{
	// 1: initial schema (plan section 8).
	`
CREATE TABLE meta (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);

CREATE TABLE roots (
  id        INTEGER PRIMARY KEY,
  path      TEXT NOT NULL UNIQUE COLLATE NOCASE,
  enabled   INTEGER NOT NULL DEFAULT 1,
  added_at  INTEGER NOT NULL
);

CREATE TABLE excludes (
  id       INTEGER PRIMARY KEY,
  root_id  INTEGER NOT NULL REFERENCES roots(id) ON DELETE CASCADE,
  path     TEXT NOT NULL COLLATE NOCASE,
  UNIQUE (root_id, path)
);

CREATE TABLE files (
  id           INTEGER PRIMARY KEY,
  root_id      INTEGER NOT NULL REFERENCES roots(id) ON DELETE CASCADE,
  path         TEXT NOT NULL UNIQUE COLLATE NOCASE,
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
  id        INTEGER PRIMARY KEY,
  file_id   INTEGER NOT NULL REFERENCES files(id) ON DELETE CASCADE,
  kind      TEXT NOT NULL,
  locator   TEXT,
  text      TEXT
);
CREATE INDEX items_file ON items(file_id);

CREATE TABLE vectors (
  item_id  INTEGER PRIMARY KEY REFERENCES items(id) ON DELETE CASCADE,
  bin      BLOB NOT NULL,
  i8       BLOB NOT NULL,
  scale    REAL NOT NULL
);

-- Keyword index. Contentless (stores no copy of the text) and contentless_delete=1 so
-- rows can be deleted by rowid. rowid = items.id.
CREATE VIRTUAL TABLE items_fts USING fts5(
  name, text,
  content='',
  contentless_delete=1,
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
}
