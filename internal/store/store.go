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
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ncruces/go-sqlite3"
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

// ErrNestedRoot is returned by AddRoot when the folder is inside, or contains, a folder
// that is already chosen. Each file must belong to exactly one root, or removing one
// root would delete files the other still covers.
var ErrNestedRoot = errors.New("store: folder overlaps a folder that is already added")

// Store is an open index database. It is safe for concurrent use.
type Store struct {
	db *sql.DB
}

// pathKey is how paths are compared: Windows ignores case for all letters (not just
// A–Z, which is all SQLite's NOCASE handles).
func pathKey(p string) string { return strings.ToUpper(filepath.Clean(p)) }

// Open opens (or creates) the index database at path and brings its schema up to date.
// path must be an absolute local path; get it from appfs.FS.Path("index/index.db").
func Open(path string) (*Store, error) {
	if !filepath.IsAbs(path) || strings.HasPrefix(path, `\\`) {
		return nil, fmt.Errorf("store: path %q must be an absolute local path like C:\\...", path)
	}
	q := url.Values{}
	// busy_timeout must come first: the driver runs pragmas in order, and anything that
	// needs a lock before it would fail immediately with "database is locked".
	q.Add("_pragma", "busy_timeout(10000)")
	q.Add("_pragma", "synchronous(NORMAL)")
	q.Add("_pragma", "foreign_keys(ON)")
	q.Set("_txlock", "immediate") // take the write lock at BEGIN, avoiding deadlocks between writers
	// "file:C:/dir/index.db?..." — characters with meaning in a URI are escaped.
	esc := strings.NewReplacer("%", "%25", "?", "%3f", "#", "%23").Replace(filepath.ToSlash(path))
	dsn := "file:" + esc + "?" + q.Encode()

	db, err := driver.Open(dsn, fts5.Register)
	if err != nil {
		return nil, err
	}
	s := &Store{db: db}

	// WAL mode is stored in the database file, so it is set once here, not on every
	// connection. Switching to WAL can return BUSY without waiting if another process
	// has the file open, so retry for a while.
	ctx := context.Background()
	deadline := time.Now().Add(10 * time.Second)
	for {
		err = s.setup(ctx)
		if err == nil || !isBusy(err) || time.Now().After(deadline) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) setup(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `PRAGMA journal_mode = WAL`); err != nil {
		return err
	}
	return s.migrate(ctx)
}

func isBusy(err error) bool {
	return errors.Is(err, sqlite3.BUSY) || errors.Is(err, sqlite3.LOCKED) ||
		strings.Contains(err.Error(), "database is locked")
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
// Pass the real path from safefs.Canonical. Folders inside or around an existing root
// are refused with ErrNestedRoot.
func (s *Store) AddRoot(ctx context.Context, path string) (Root, error) {
	path = filepath.Clean(path)
	key := pathKey(path)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Root{}, err
	}
	defer tx.Rollback()

	roots, err := queryRoots(ctx, tx)
	if err != nil {
		return Root{}, err
	}
	for _, r := range roots {
		rk := pathKey(r.Path)
		if rk == key {
			return r, nil
		}
		if isUnder(key, rk) || isUnder(rk, key) {
			return Root{}, fmt.Errorf("%w: %s and %s", ErrNestedRoot, path, r.Path)
		}
	}
	now := time.Now()
	res, err := tx.ExecContext(ctx, `INSERT INTO roots(path, path_key, added_at) VALUES(?, ?, ?)`,
		path, key, now.UnixNano())
	if err != nil {
		return Root{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Root{}, err
	}
	return Root{ID: id, Path: path, Enabled: true, AddedAt: time.Unix(0, now.UnixNano())}, tx.Commit()
}

// isUnder reports whether key is strictly inside dir (both already pathKey'd).
func isUnder(key, dir string) bool {
	if !strings.HasSuffix(dir, `\`) {
		dir += `\`
	}
	return len(key) > len(dir) && strings.HasPrefix(key, dir)
}

// Roots lists all chosen folders.
func (s *Store) Roots(ctx context.Context) ([]Root, error) {
	return queryRoots(ctx, s.db)
}

// querier is satisfied by both *sql.DB and *sql.Tx.
type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func queryRoots(ctx context.Context, q querier) ([]Root, error) {
	rows, err := q.QueryContext(ctx, `SELECT id, path, enabled, added_at FROM roots ORDER BY id`)
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

// RemoveRoot removes a folder and everything indexed from it: files, text, vectors and
// keyword rows (plan P-5). It returns the thumbnail paths (relative to the app's thumbs
// folder) that the caller must now delete from disk.
func (s *Store) RemoveRoot(ctx context.Context, id int64) (thumbs []string, err error) {
	return s.deleteWithThumbs(ctx,
		`SELECT t.path FROM thumbs t JOIN files f ON f.id = t.file_id WHERE f.root_id = ?`,
		`DELETE FROM roots WHERE id = ?`, id)
}

// AddExclude records a subfolder the user unticked.
func (s *Store) AddExclude(ctx context.Context, rootID int64, path string) error {
	path = filepath.Clean(path)
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO excludes(root_id, path, path_key) VALUES(?, ?, ?) ON CONFLICT DO NOTHING`,
		rootID, path, pathKey(path))
	return err
}

// Excludes lists unticked subfolders of a root.
func (s *Store) Excludes(ctx context.Context, rootID int64) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT path FROM excludes WHERE root_id = ? ORDER BY path_key`, rootID)
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
// (re)indexing. Its old content stays searchable until ReplaceContent swaps it.
// Every file has a file-name keyword row, kept in sync with the current name.
func (s *Store) UpsertFile(ctx context.Context, f File) (id int64, changed bool, err error) {
	f.Path = filepath.Clean(f.Path)
	key := pathKey(f.Path)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, err
	}
	defer tx.Rollback()

	var old File
	var indexedAt sql.NullInt64
	err = tx.QueryRowContext(ctx, `SELECT id, size, mtime, status, name, indexed_at FROM files WHERE path_key = ?`, key).
		Scan(&old.ID, &old.Size, &old.MTime, &old.Status, &old.Name, &indexedAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		res, err := tx.ExecContext(ctx,
			`INSERT INTO files(root_id, path, path_key, name, ext, kind, size, mtime, status) VALUES(?,?,?,?,?,?,?,?,?)`,
			f.RootID, f.Path, key, f.Name, f.Ext, f.Kind, f.Size, f.MTime, StatusPending)
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
	changed = old.Size != f.Size || old.MTime != f.MTime ||
		old.Status == StatusPending || old.Status == StatusSkipped || // skipped (free tier): retried, e.g. after purchase
		// Failures can be temporary (file locked, engine busy): retry once a day.
		old.Status == StatusFailed && time.Since(time.Unix(0, indexedAt.Int64)) > RetryFailedAfter
	status := old.Status
	if changed {
		status = StatusPending
	}
	// Always refresh what the scanner knows: root (folders can be re-added), and the
	// exact path/name casing after a case-only rename.
	_, err = tx.ExecContext(ctx, `
		UPDATE files SET root_id = ?, path = ?, name = ?, ext = ?, kind = ?, size = ?, mtime = ?,
		                 status = ?, error = CASE WHEN ? THEN NULL ELSE error END
		WHERE id = ?`,
		f.RootID, f.Path, f.Name, f.Ext, f.Kind, f.Size, f.MTime, status, changed, id)
	if err != nil {
		return 0, false, err
	}
	if old.Name != f.Name {
		// Replace the file-name row; the trigger removes the old keyword row.
		if _, err := tx.ExecContext(ctx, `DELETE FROM items WHERE file_id = ? AND kind = ?`, id, ItemName); err != nil {
			return 0, false, err
		}
		if err := insertNameItem(ctx, tx, id, f.Name); err != nil {
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

// FileByPath returns the file at path (case-insensitive). ok is false if it is not indexed.
func (s *Store) FileByPath(ctx context.Context, path string) (f File, ok bool, err error) {
	var hash, quality, errMsg sql.NullString
	var indexed sql.NullInt64
	err = s.db.QueryRowContext(ctx, `
		SELECT id, root_id, path, name, ext, kind, size, mtime, content_hash, status, quality, error, indexed_at
		FROM files WHERE path_key = ?`, pathKey(path)).
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

// SetFileResult records the outcome of indexing a file. A failed or skipped file loses
// any older content, so out-of-date text of a changed file never stays searchable.
func (s *Store) SetFileResult(ctx context.Context, id int64, status, quality, contentHash, errMsg string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if status == StatusFailed || status == StatusSkipped {
		if _, err := tx.ExecContext(ctx, `DELETE FROM items WHERE file_id = ? AND kind <> ?`, id, ItemName); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `
		UPDATE files SET status = ?, quality = NULLIF(?, ''), content_hash = NULLIF(?, ''),
		                 error = NULLIF(?, ''), indexed_at = ?
		WHERE id = ?`, status, quality, contentHash, errMsg, time.Now().UnixNano(), id)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// DeleteFile removes a file and everything indexed from it (plan P-5). It returns the
// thumbnail paths the caller must delete from disk; found is false if the file was not
// in the index.
func (s *Store) DeleteFile(ctx context.Context, path string) (found bool, thumbs []string, err error) {
	key := pathKey(path)
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM files WHERE path_key = ?`, key).Scan(&n); err != nil {
		return false, nil, err
	}
	if n == 0 {
		return false, nil, nil
	}
	thumbs, err = s.deleteWithThumbs(ctx,
		`SELECT t.path FROM thumbs t JOIN files f ON f.id = t.file_id WHERE f.path_key = ?`,
		`DELETE FROM files WHERE path_key = ?`, key)
	return err == nil, thumbs, err
}

// deleteWithThumbs collects thumbnail paths and runs the delete in one transaction, so
// no thumbnail can be added in between and then forgotten.
func (s *Store) deleteWithThumbs(ctx context.Context, selectThumbs, del string, arg any) ([]string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, selectThumbs, arg)
	if err != nil {
		return nil, err
	}
	var thumbs []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			rows.Close()
			return nil, err
		}
		thumbs = append(thumbs, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, del, arg); err != nil {
		return nil, err
	}
	return thumbs, tx.Commit()
}

// CountContentIndexed counts files that have content in the index (text, images…), not
// just a file name. This is the number the free-tier limit of 300 files is checked
// against; a modified file keeps counting while it waits to be re-indexed.
func (s *Store) CountContentIndexed(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT count(DISTINCT file_id) FROM items WHERE kind <> ?`, ItemName).Scan(&n)
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
	for _, it := range items {
		if it.Kind == ItemName || it.Kind == "" {
			return fmt.Errorf("store: invalid item kind %q", it.Kind)
		}
	}
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

// ---- used by the indexer ----

// FilesInRoot returns the paths of all files recorded under a root. The indexer compares
// them with a fresh scan to find files deleted while Findly was not running (P-5).
func (s *Store) FilesInRoot(ctx context.Context, rootID int64) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT path FROM files WHERE root_id = ?`, rootID)
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

// RetryFailedAfter is how long a failed file waits before UpsertFile offers it again.
const RetryFailedAfter = 24 * time.Hour

// HasContent reports whether a file has content items (not just its name row).
func (s *Store) HasContent(ctx context.Context, fileID int64) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM items WHERE file_id = ? AND kind <> ?)`, fileID, ItemName).Scan(&n)
	return n == 1, err
}

// CopyDuplicate looks for another fully indexed file with the same content hash and, if
// one exists, copies its items, vectors and keyword rows to file `to` — all in one
// transaction, so the source cannot disappear halfway. Identical files are embedded only
// once (plan 9.10). copied is false if there is no usable source.
func (s *Store) CopyDuplicate(ctx context.Context, hash string, to int64) (copied bool, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	var from int64
	err = tx.QueryRowContext(ctx, `
		SELECT f.id FROM files f
		WHERE f.content_hash = ? AND f.status = ? AND f.id <> ?
		  AND EXISTS(SELECT 1 FROM items i WHERE i.file_id = f.id AND i.kind <> ?)
		LIMIT 1`, hash, StatusDone, to, ItemName).Scan(&from)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM items WHERE file_id = ? AND kind <> ?`, to, ItemName); err != nil {
		return false, err
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT i.kind, i.locator, i.text, v.bin, v.i8, v.scale
		FROM items i LEFT JOIN vectors v ON v.item_id = i.id
		WHERE i.file_id = ? AND i.kind <> ? ORDER BY i.id`, from, ItemName)
	if err != nil {
		return false, err
	}
	type src struct {
		kind          string
		locator, text sql.NullString
		bin, i8       []byte
		scale         sql.NullFloat64
	}
	var items []src
	for rows.Next() {
		var it src
		if err := rows.Scan(&it.kind, &it.locator, &it.text, &it.bin, &it.i8, &it.scale); err != nil {
			rows.Close()
			return false, err
		}
		items = append(items, it)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, err
	}
	for _, it := range items {
		res, err := tx.ExecContext(ctx, `INSERT INTO items(file_id, kind, locator, text) VALUES(?, ?, ?, ?)`,
			to, it.kind, it.locator, it.text)
		if err != nil {
			return false, err
		}
		newID, err := res.LastInsertId()
		if err != nil {
			return false, err
		}
		if it.text.Valid && it.text.String != "" {
			if _, err := tx.ExecContext(ctx, `INSERT INTO items_fts(rowid, name, text) VALUES(?, '', ?)`, newID, it.text.String); err != nil {
				return false, err
			}
		}
		if it.bin != nil {
			if _, err := tx.ExecContext(ctx, `INSERT INTO vectors(item_id, bin, i8, scale) VALUES(?, ?, ?, ?)`,
				newID, it.bin, it.i8, it.scale.Float64); err != nil {
				return false, err
			}
		}
	}
	return len(items) > 0, tx.Commit()
}

// ---- used by search ----

// Filter narrows search results (plan 9.11). Zero values mean "no filter".
type Filter struct {
	Kinds  []string // file kinds, e.g. "pdf", "doc"
	Folder string   // only files under this folder
	After  int64    // modified at or after (unix ns)
	Before int64    // modified before (unix ns)
}

// sql returns a WHERE fragment over the files table aliased f, and its arguments.
func (fl Filter) sql() (string, []any) {
	var parts []string
	var args []any
	if len(fl.Kinds) > 0 {
		parts = append(parts, "f.kind IN ("+strings.TrimSuffix(strings.Repeat("?,", len(fl.Kinds)), ",")+")")
		for _, k := range fl.Kinds {
			args = append(args, k)
		}
	}
	if fl.Folder != "" {
		prefix := pathKey(fl.Folder)
		if !strings.HasSuffix(prefix, `\`) {
			prefix += `\`
		}
		// substr comparison instead of LIKE: paths may contain % and _.
		parts = append(parts, "substr(f.path_key, 1, ?) = ?")
		args = append(args, len(prefix), prefix)
	}
	if fl.After != 0 {
		parts = append(parts, "f.mtime >= ?")
		args = append(args, fl.After)
	}
	if fl.Before != 0 {
		parts = append(parts, "f.mtime < ?")
		args = append(args, fl.Before)
	}
	if len(parts) == 0 {
		return "1", nil
	}
	return strings.Join(parts, " AND "), args
}

// Match reports whether a file passes the filter (for results found outside SQL).
func (fl Filter) Match(f File) bool {
	if len(fl.Kinds) > 0 && !slices.Contains(fl.Kinds, f.Kind) {
		return false
	}
	if fl.Folder != "" {
		prefix := pathKey(fl.Folder)
		if !strings.HasSuffix(prefix, `\`) {
			prefix += `\`
		}
		if !strings.HasPrefix(pathKey(f.Path), prefix) {
			return false
		}
	}
	if fl.After != 0 && f.MTime < fl.After {
		return false
	}
	if fl.Before != 0 && f.MTime >= fl.Before {
		return false
	}
	return true
}

// KeywordHit is one keyword-index match.
type KeywordHit struct {
	ItemID  int64
	FileID  int64
	Kind    string // item kind; "name" for a file-name match
	Locator string
	Text    string  // item text (empty for name matches)
	Rank    float64 // bm25: lower is better
}

// KeywordSearch runs an FTS5 MATCH expression (built by the search package, never raw
// user input) and returns the best hit of each matching file, best files first, up to
// maxFiles files.
//
// Ranking happens inside the FTS index first (a subquery with LIMIT), and only those
// rows are joined to items/files: joining every match before ranking took ~270 ms for a
// common word in 50k passages. The candidate limit is larger with filters, because many
// candidates may be filtered out.
func (s *Store) KeywordSearch(ctx context.Context, match string, fl Filter, maxFiles int) ([]KeywordHit, error) {
	where, args := fl.sql()
	candidates := 500
	if where != "1" {
		candidates = 5000
	}
	q := `
		SELECT i.id, i.file_id, i.kind, coalesce(i.locator, ''), coalesce(i.text, ''), k.r
		FROM (SELECT rowid AS id, bm25(items_fts, 2.0, 1.0) AS r
		      FROM items_fts WHERE items_fts MATCH ? ORDER BY r LIMIT ?) k
		JOIN items i ON i.id = k.id
		JOIN files f ON f.id = i.file_id
		WHERE ` + where + `
		ORDER BY k.r`
	all := append([]any{match, candidates}, args...)
	rows, err := s.db.QueryContext(ctx, q, all...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []KeywordHit
	seen := map[int64]bool{}
	for rows.Next() {
		var h KeywordHit
		if err := rows.Scan(&h.ItemID, &h.FileID, &h.Kind, &h.Locator, &h.Text, &h.Rank); err != nil {
			return nil, err
		}
		if seen[h.FileID] {
			continue // one hit per file, so one big file cannot crowd out the others
		}
		seen[h.FileID] = true
		out = append(out, h)
		if len(out) == maxFiles {
			break
		}
	}
	return out, rows.Err()
}

// VectorRow is one stored vector with its item and file.
type VectorRow struct {
	ItemID int64
	File   File // ID, Path, Kind and MTime are filled (for filtering in memory)
	Vector Vector
}

// AllVectors streams every stored vector to fn (for the in-memory search cache).
func (s *Store) AllVectors(ctx context.Context, fn func(VectorRow) error) error {
	rows, err := s.db.QueryContext(ctx, `
		SELECT v.item_id, i.file_id, f.path, f.kind, f.mtime, v.bin, v.i8, v.scale
		FROM vectors v JOIN items i ON i.id = v.item_id JOIN files f ON f.id = i.file_id
		ORDER BY v.item_id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	var bin, i8 []byte
	for rows.Next() {
		var r VectorRow
		var scale float64
		if err := rows.Scan(&r.ItemID, &r.File.ID, &r.File.Path, &r.File.Kind, &r.File.MTime, &bin, &i8, &scale); err != nil {
			return err
		}
		if len(bin) != VectorDim/8 || len(i8) != VectorDim {
			continue // damaged row: skip rather than crash (I-7)
		}
		copy(r.Vector.Bin[:], bin)
		for j, b := range i8 {
			r.Vector.I8[j] = int8(b)
		}
		r.Vector.Scale = float32(scale)
		if err := fn(r); err != nil {
			return err
		}
	}
	return rows.Err()
}

// ItemByID returns an item's file id, locator and text.
func (s *Store) ItemByID(ctx context.Context, id int64) (fileID int64, locator, text string, err error) {
	err = s.db.QueryRowContext(ctx,
		`SELECT file_id, coalesce(locator, ''), coalesce(text, '') FROM items WHERE id = ?`, id).
		Scan(&fileID, &locator, &text)
	return
}

// FilesByID returns files by id (missing ids are left out).
func (s *Store) FilesByID(ctx context.Context, ids []int64) (map[int64]File, error) {
	out := make(map[int64]File, len(ids))
	for start := 0; start < len(ids); start += 500 {
		batch := ids[start:min(start+500, len(ids))]
		args := make([]any, len(batch))
		for i, id := range batch {
			args[i] = id
		}
		rows, err := s.db.QueryContext(ctx, `
			SELECT id, root_id, path, name, ext, kind, size, mtime, status
			FROM files WHERE id IN (`+strings.TrimSuffix(strings.Repeat("?,", len(batch)), ",")+`)`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var f File
			if err := rows.Scan(&f.ID, &f.RootID, &f.Path, &f.Name, &f.Ext, &f.Kind, &f.Size, &f.MTime, &f.Status); err != nil {
				rows.Close()
				return nil, err
			}
			out[f.ID] = f
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Counts summarizes the index for `cli stats` and the settings screen.
type Counts struct {
	Roots    int
	ByStatus map[string]int
	ByKind   map[string]int
	Items    int // content items (passages, pages…)
	Vectors  int
}

// Count returns index statistics.
func (s *Store) Count(ctx context.Context) (Counts, error) {
	c := Counts{ByStatus: map[string]int{}, ByKind: map[string]int{}}
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM roots`).Scan(&c.Roots); err != nil {
		return c, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM items WHERE kind <> ?`, ItemName).Scan(&c.Items); err != nil {
		return c, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM vectors`).Scan(&c.Vectors); err != nil {
		return c, err
	}
	for _, q := range []struct {
		sql string
		m   map[string]int
	}{
		{`SELECT status, count(*) FROM files GROUP BY status`, c.ByStatus},
		{`SELECT kind, count(*) FROM files GROUP BY kind`, c.ByKind},
	} {
		rows, err := s.db.QueryContext(ctx, q.sql)
		if err != nil {
			return c, err
		}
		for rows.Next() {
			var k string
			var n int
			if err := rows.Scan(&k, &n); err != nil {
				rows.Close()
				return c, err
			}
			q.m[k] = n
		}
		rows.Close()
	}
	return c, nil
}

// Empty reports whether the filter lets everything through.
func (fl Filter) Empty() bool {
	return len(fl.Kinds) == 0 && fl.Folder == "" && fl.After == 0 && fl.Before == 0
}
