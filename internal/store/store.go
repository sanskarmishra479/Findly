// Package store keeps Findly's index in SQLite: chosen folders, files, the passages and
// images inside them, their vectors, and the keyword (FTS5) index. It never touches user
// files; it only stores what other packages extracted from them.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ncruces/go-sqlite3/driver"
	"github.com/ncruces/go-sqlite3/ext/fts5"
)

// VectorDim is the stored vector size (EmbeddingGemma 2 truncated to 256, plan section 7).
const VectorDim = 256

// File statuses.
const (
	StatusPending = "pending" // found, not indexed yet (or changed since)
	StatusPartial = "partial" // some content indexed (e.g. first 30 PDF pages)
	StatusDone    = "done"
	StatusFailed  = "failed"
	StatusSkipped = "skipped" // e.g. over the free-tier limit or unsupported
)

// Item kinds.
const (
	ItemName    = "name"    // file-name-only keyword row; every file has exactly one
	ItemPassage = "passage" // text chunk
	ItemPage    = "page"
	ItemImage   = "image"
	ItemFrame   = "frame"
	ItemAudio   = "audio"
)

// Store is an open index database. It is safe for concurrent use.
type Store struct {
	db *sql.DB
}

// Open opens (or creates) the index database at path and brings its schema up to date.
// path must be absolute; get it from appfs.FS.Path("index/index.db").
func Open(path string) (*Store, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("store: path %q must be absolute", path)
	}
	q := url.Values{}
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(NORMAL)")
	q.Add("_pragma", "foreign_keys(ON)")
	q.Add("_pragma", "busy_timeout(10000)")
	q.Set("_txlock", "immediate") // take the write lock at BEGIN, avoiding deadlocks between writers
	// "file:C:/dir/index.db?..." — characters with meaning in a URI are escaped.
	esc := strings.NewReplacer("%", "%25", "?", "%3f", "#", "%23").Replace(filepath.ToSlash(path))
	dsn := "file:" + esc + "?" + q.Encode()

	db, err := driver.Open(dsn, fts5.Register)
	if err != nil {
		return nil, err
	}
	s := &Store{db: db}
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var version int
	if err := tx.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	if version > len(migrations) {
		return fmt.Errorf("store: database schema v%d is newer than this app (v%d)", version, len(migrations))
	}
	for i := version; i < len(migrations); i++ {
		if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
			return fmt.Errorf("store: migration %d: %w", i+1, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `PRAGMA user_version = `+strconv.Itoa(len(migrations))); err != nil {
		return err
	}
	return tx.Commit()
}

// ---- meta ----

// Meta returns a stored setting such as "model_version". ok is false if it is not set.
func (s *Store) Meta(ctx context.Context, key string) (value string, ok bool, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return value, err == nil, err
}

// SetMeta stores a setting.
func (s *Store) SetMeta(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO meta(key, value) VALUES(?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		key, value)
	return err
}

// ---- roots and excludes ----

// Root is a folder the user chose to index.
type Root struct {
	ID      int64
	Path    string
	Enabled bool
	AddedAt time.Time
}

// AddRoot adds a folder (or returns the existing one with the same path).
// Pass the real path from safefs.Canonical.
func (s *Store) AddRoot(ctx context.Context, path string) (Root, error) {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO roots(path, added_at) VALUES(?, ?) ON CONFLICT(path) DO NOTHING`,
		path, time.Now().UnixNano())
	if err != nil {
		return Root{}, err
	}
	var r Root
	var added int64
	err = s.db.QueryRowContext(ctx, `SELECT id, path, enabled, added_at FROM roots WHERE path = ?`, path).
		Scan(&r.ID, &r.Path, &r.Enabled, &added)
	r.AddedAt = time.Unix(0, added)
	return r, err
}

// Roots lists all chosen folders.
func (s *Store) Roots(ctx context.Context) ([]Root, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, path, enabled, added_at FROM roots ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Root
	for rows.Next() {
		var r Root
		var added int64
		if err := rows.Scan(&r.ID, &r.Path, &r.Enabled, &added); err != nil {
			return nil, err
		}
		r.AddedAt = time.Unix(0, added)
		out = append(out, r)
	}
	return out, rows.Err()
}

// RemoveRoot removes a folder and everything indexed from it: files, text, vectors,
// keyword rows and thumbnail records (plan P-5). Thumbnail image files on disk must be
// deleted by the caller.
func (s *Store) RemoveRoot(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM roots WHERE id = ?`, id)
	return err
}

// AddExclude records a subfolder the user unticked.
func (s *Store) AddExclude(ctx context.Context, rootID int64, path string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO excludes(root_id, path) VALUES(?, ?) ON CONFLICT DO NOTHING`, rootID, path)
	return err
}

// Excludes lists unticked subfolders of a root.
func (s *Store) Excludes(ctx context.Context, rootID int64) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT path FROM excludes WHERE root_id = ? ORDER BY path`, rootID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ---- files ----

// File is one file found under a root.
type File struct {
	ID          int64
	RootID      int64
	Path        string
	Name        string
	Ext         string // lower case, with dot: ".pdf"
	Kind        string // text | doc | pdf | image | audio | video | other
	Size        int64
	MTime       int64 // unix nanoseconds
	ContentHash string
	Status      string
	Quality     string
	Error       string
	IndexedAt   int64 // unix nanoseconds, 0 if never
}

// UpsertFile records a file found by the scanner. A new file, or one whose size or
// modification time changed, gets status pending; changed reports that it needs
// (re)indexing. Every file also gets a file-name keyword row, so name search always works.
func (s *Store) UpsertFile(ctx context.Context, f File) (id int64, changed bool, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, err
	}
	defer tx.Rollback()

	var old File
	err = tx.QueryRowContext(ctx, `SELECT id, size, mtime, status, name FROM files WHERE path = ?`, f.Path).
		Scan(&old.ID, &old.Size, &old.MTime, &old.Status, &old.Name)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		res, err := tx.ExecContext(ctx,
			`INSERT INTO files(root_id, path, name, ext, kind, size, mtime, status) VALUES(?,?,?,?,?,?,?,?)`,
			f.RootID, f.Path, f.Name, f.Ext, f.Kind, f.Size, f.MTime, StatusPending)
		if err != nil {
			return 0, false, err
		}
		if id, err = res.LastInsertId(); err != nil {
			return 0, false, err
		}
		if err := insertNameItem(ctx, tx, id, f.Name); err != nil {
			return 0, false, err
		}
		return id, true, tx.Commit()
	case err != nil:
		return 0, false, err
	}

	id = old.ID
	changed = old.Size != f.Size || old.MTime != f.MTime || old.Status == StatusPending
	if changed {
		_, err = tx.ExecContext(ctx,
			`UPDATE files SET size = ?, mtime = ?, status = ?, error = NULL WHERE id = ?`,
			f.Size, f.MTime, StatusPending, id)
		if err != nil {
			return 0, false, err
		}
	}
	return id, changed, tx.Commit()
}

func insertNameItem(ctx context.Context, tx *sql.Tx, fileID int64, name string) error {
	res, err := tx.ExecContext(ctx, `INSERT INTO items(file_id, kind) VALUES(?, ?)`, fileID, ItemName)
	if err != nil {
		return err
	}
	itemID, err := res.LastInsertId()
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO items_fts(rowid, name, text) VALUES(?, ?, '')`,
		itemID, nameForSearch(name))
	return err
}

// nameForSearch makes "Invoice_2024-03.final.pdf" searchable as separate words.
func nameForSearch(name string) string {
	return strings.NewReplacer("_", " ", "-", " ", ".", " ").Replace(name)
}

// FileByPath returns the file at path. ok is false if it is not in the index.
func (s *Store) FileByPath(ctx context.Context, path string) (f File, ok bool, err error) {
	var hash, quality, errMsg sql.NullString
	var indexed sql.NullInt64
	err = s.db.QueryRowContext(ctx, `
		SELECT id, root_id, path, name, ext, kind, size, mtime, content_hash, status, quality, error, indexed_at
		FROM files WHERE path = ?`, path).
		Scan(&f.ID, &f.RootID, &f.Path, &f.Name, &f.Ext, &f.Kind, &f.Size, &f.MTime,
			&hash, &f.Status, &quality, &errMsg, &indexed)
	if errors.Is(err, sql.ErrNoRows) {
		return File{}, false, nil
	}
	if err != nil {
		return File{}, false, err
	}
	f.ContentHash, f.Quality, f.Error, f.IndexedAt = hash.String, quality.String, errMsg.String, indexed.Int64
	return f, true, nil
}

// SetFileResult records the outcome of indexing a file.
func (s *Store) SetFileResult(ctx context.Context, id int64, status, quality, contentHash, errMsg string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE files SET status = ?, quality = NULLIF(?, ''), content_hash = NULLIF(?, ''),
		                 error = NULLIF(?, ''), indexed_at = ?
		WHERE id = ?`, status, quality, contentHash, errMsg, time.Now().UnixNano(), id)
	return err
}

// DeleteFile removes a file and everything indexed from it (plan P-5).
// It returns false if the file was not in the index.
func (s *Store) DeleteFile(ctx context.Context, path string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM files WHERE path = ?`, path)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// CountContentIndexed counts files whose content is (at least partly) indexed.
// This is the number the free-tier limit of 300 files is checked against.
func (s *Store) CountContentIndexed(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM files WHERE status IN (?, ?)`, StatusDone, StatusPartial).Scan(&n)
	return n, err
}

// ---- items and vectors ----

// Vector is a stored embedding: 256 sign bits for fast search, and int8 values with a
// scale for accurate re-ranking (plan 9.9).
type Vector struct {
	Bin   [VectorDim / 8]byte
	I8    [VectorDim]int8
	Scale float32
}

// Item is one searchable piece of a file: a text passage, a PDF page, an image, a video
// frame or an audio window.
type Item struct {
	Kind    string
	Locator string  // JSON such as {"page":3} or {"t":125.0}; empty if none
	Text    string  // passage or OCR text; empty if none
	Vector  *Vector // nil if this item has no embedding (keyword-only)
}

// ReplaceContent replaces all content items of a file (keeping its file-name row) in one
// transaction, so search never sees a half-updated file.
func (s *Store) ReplaceContent(ctx context.Context, fileID int64, items []Item) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// The trigger removes the matching keyword rows; ON DELETE CASCADE removes vectors.
	if _, err := tx.ExecContext(ctx, `DELETE FROM items WHERE file_id = ? AND kind <> ?`, fileID, ItemName); err != nil {
		return err
	}

	insItem, err := tx.PrepareContext(ctx, `INSERT INTO items(file_id, kind, locator, text) VALUES(?, ?, NULLIF(?, ''), NULLIF(?, ''))`)
	if err != nil {
		return err
	}
	defer insItem.Close()
	insFTS, err := tx.PrepareContext(ctx, `INSERT INTO items_fts(rowid, name, text) VALUES(?, '', ?)`)
	if err != nil {
		return err
	}
	defer insFTS.Close()
	insVec, err := tx.PrepareContext(ctx, `INSERT INTO vectors(item_id, bin, i8, scale) VALUES(?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer insVec.Close()

	for _, it := range items {
		if it.Kind == ItemName || it.Kind == "" {
			return fmt.Errorf("store: invalid item kind %q", it.Kind)
		}
		res, err := insItem.ExecContext(ctx, fileID, it.Kind, it.Locator, it.Text)
		if err != nil {
			return err
		}
		itemID, err := res.LastInsertId()
		if err != nil {
			return err
		}
		if it.Text != "" {
			if _, err := insFTS.ExecContext(ctx, itemID, it.Text); err != nil {
				return err
			}
		}
		if v := it.Vector; v != nil {
			i8 := make([]byte, VectorDim)
			for i, x := range v.I8 {
				i8[i] = byte(x)
			}
			if _, err := insVec.ExecContext(ctx, itemID, v.Bin[:], i8, float64(v.Scale)); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}
