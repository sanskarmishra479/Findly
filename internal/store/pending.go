package store

import (
	"context"
	"database/sql"
	"errors"
)

// The embedding pass (keyword first, P3-T3): passages are stored as soon as a file is
// read, marked as waiting (Item.Pending); this pass adds their vectors later.

// PendingFile is a file with passages waiting for their vectors.
type PendingFile struct {
	ID     int64
	Name   string // file name, the embedding title
	Hash   string // content hash, for copying vectors from an identical file
	Status string
}

// PendingItem is a passage waiting for its vector.
type PendingItem struct {
	ID   int64
	Text string
}

// ItemVector is the vector computed for a waiting passage.
type ItemVector struct {
	ID     int64
	Vector Vector
}

// NextPendingFile returns the file whose waiting passages were stored first, which
// follows the indexing priority order (recent files, Desktop/Documents first). ok is
// false when nothing is waiting.
func (s *Store) NextPendingFile(ctx context.Context) (f PendingFile, ok bool, err error) {
	var hash sql.NullString
	err = s.db.QueryRowContext(ctx, `
		SELECT f.id, f.name, f.content_hash, f.status FROM files f
		WHERE f.id = (SELECT file_id FROM items WHERE embed = ? ORDER BY id LIMIT 1)`,
		embedPending).Scan(&f.ID, &f.Name, &hash, &f.Status)
	if errors.Is(err, sql.ErrNoRows) {
		return PendingFile{}, false, nil
	}
	if err != nil {
		return PendingFile{}, false, err
	}
	f.Hash = hash.String
	return f, true, nil
}

// PendingItems returns the waiting passages of a file, in order.
func (s *Store) PendingItems(ctx context.Context, fileID int64) ([]PendingItem, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, coalesce(text, '') FROM items WHERE file_id = ? AND embed = ? ORDER BY id`,
		fileID, embedPending)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PendingItem
	for rows.Next() {
		var it PendingItem
		if err := rows.Scan(&it.ID, &it.Text); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// SetVectors stores vectors for waiting passages in one transaction. Passages that no
// longer exist or are no longer waiting (the file was deleted or changed meanwhile, and
// its passages replaced) are skipped, so a slow embedding never resurrects old content.
// It returns how many vectors were stored.
func (s *Store) SetVectors(ctx context.Context, vecs []ItemVector) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	mark, err := tx.PrepareContext(ctx, `UPDATE items SET embed = ? WHERE id = ? AND embed = ?`)
	if err != nil {
		return 0, err
	}
	defer mark.Close()
	ins, err := tx.PrepareContext(ctx, `INSERT OR REPLACE INTO vectors(item_id, bin, i8, scale) VALUES(?, ?, ?, ?)`)
	if err != nil {
		return 0, err
	}
	defer ins.Close()

	n := 0
	for _, iv := range vecs {
		res, err := mark.ExecContext(ctx, embedNone, iv.ID, embedPending)
		if err != nil {
			return 0, err
		}
		if k, err := res.RowsAffected(); err != nil {
			return 0, err
		} else if k == 0 {
			continue // gone or replaced meanwhile
		}
		i8 := make([]byte, VectorDim)
		for i, x := range iv.Vector.I8 {
			i8[i] = byte(x)
		}
		if _, err := ins.ExecContext(ctx, iv.ID, iv.Vector.Bin[:], i8, float64(iv.Vector.Scale)); err != nil {
			return 0, err
		}
		n++
	}
	return n, tx.Commit()
}

// MarkEmbedFailed gives up on a file's waiting passages (the embedding model failed on
// them): they stay keyword-searchable but are not retried, and the reason is recorded
// on the file.
func (s *Store) MarkEmbedFailed(ctx context.Context, fileID int64, reason string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE items SET embed = ? WHERE file_id = ? AND embed = ?`,
		embedFailed, fileID, embedPending); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE files SET error = ? WHERE id = ?`,
		"meaning search unavailable: "+reason, fileID); err != nil {
		return err
	}
	return tx.Commit()
}

// CountPending returns how many passages wait for their vectors (for progress).
func (s *Store) CountPending(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM items WHERE embed = ?`, embedPending).Scan(&n)
	return n, err
}
