package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// count runs a COUNT query; used to check that deletes really remove rows.
func count(t *testing.T, s *Store, query string, args ...any) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

// matches returns how many keyword rows match an FTS5 query.
func matches(t *testing.T, s *Store, q string) int {
	t.Helper()
	return count(t, s, `SELECT count(*) FROM items_fts WHERE items_fts MATCH ?`, q)
}

func addFile(t *testing.T, s *Store, rootID int64, path string) int64 {
	t.Helper()
	id, changed, err := s.UpsertFile(context.Background(), File{
		RootID: rootID, Path: path, Name: filepath.Base(path), Ext: filepath.Ext(path),
		Kind: "text", Size: 10, MTime: 1000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("new file should be reported as changed")
	}
	return id
}

func vec(seed int8) *Vector {
	v := &Vector{Scale: 0.01}
	for i := range v.I8 {
		v.I8[i] = seed
	}
	v.Bin[0] = byte(seed)
	return v
}

func TestOpenSetsPragmasAndSchema(t *testing.T) {
	s := openTest(t)
	var mode string
	s.db.QueryRow(`PRAGMA journal_mode`).Scan(&mode)
	if mode != "wal" {
		t.Errorf("journal_mode = %q, want wal", mode)
	}
	var fk int
	s.db.QueryRow(`PRAGMA foreign_keys`).Scan(&fk)
	if fk != 1 {
		t.Error("foreign keys are off")
	}
	var v int
	s.db.QueryRow(`PRAGMA user_version`).Scan(&v)
	if v != len(migrations) {
		t.Errorf("user_version = %d, want %d", v, len(migrations))
	}
}

func TestReopenKeepsData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := s.SetMeta(ctx, "model_version", "eg2-q8"); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	v, ok, err := s.Meta(ctx, "model_version")
	if err != nil || !ok || v != "eg2-q8" {
		t.Fatalf("Meta = %q %v %v", v, ok, err)
	}
	if _, ok, _ := s.Meta(ctx, "missing"); ok {
		t.Fatal("missing key reported as present")
	}
}

func TestRefusesNewerSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s.db.Exec(`PRAGMA user_version = 999`)
	s.Close()
	if _, err := Open(path); err == nil {
		t.Fatal("expected error opening a database from a newer app version")
	}
}

func TestFileNameSearchAlwaysWorks(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	root, _ := s.AddRoot(ctx, `C:\Docs`)
	addFile(t, s, root.ID, `C:\Docs\Invoice_2024-03.pdf`)

	for _, q := range []string{"invoice", "2024", "invo*", "pdf"} {
		if n := matches(t, s, q); n != 1 {
			t.Errorf("MATCH %q = %d rows, want 1", q, n)
		}
	}
}

func TestUpsertDetectsChanges(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	root, _ := s.AddRoot(ctx, `C:\Docs`)
	f := File{RootID: root.ID, Path: `C:\Docs\a.txt`, Name: "a.txt", Ext: ".txt", Kind: "text", Size: 10, MTime: 1000}

	id, _, _ := s.UpsertFile(ctx, f)
	s.SetFileResult(ctx, id, StatusDone, "", "", "")

	if _, changed, _ := s.UpsertFile(ctx, f); changed {
		t.Error("unchanged file reported as changed")
	}
	f.MTime = 2000
	if id2, changed, _ := s.UpsertFile(ctx, f); !changed || id2 != id {
		t.Errorf("modified file: changed=%v id=%d want true, %d", changed, id2, id)
	}
	got, _, _ := s.FileByPath(ctx, `c:\docs\A.TXT`) // paths are case-insensitive, like Windows
	if got.Status != StatusPending {
		t.Errorf("status = %q, want pending", got.Status)
	}
	if n := count(t, s, `SELECT count(*) FROM items WHERE file_id = ? AND kind = ?`, id, ItemName); n != 1 {
		t.Errorf("file has %d name rows, want 1", n)
	}
}

func TestReplaceContent(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	root, _ := s.AddRoot(ctx, `C:\Docs`)
	id := addFile(t, s, root.ID, `C:\Docs\lease.txt`)

	err := s.ReplaceContent(ctx, id, []Item{
		{Kind: ItemPassage, Locator: `{"chunk":0}`, Text: "No pets are allowed in the apartment", Vector: vec(1)},
		{Kind: ItemPassage, Locator: `{"chunk":1}`, Text: "Rent is due on the first day", Vector: vec(2)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if matches(t, s, "pets") != 1 || count(t, s, `SELECT count(*) FROM vectors`) != 2 {
		t.Fatal("first content not stored")
	}

	// Replacing removes the old passages, vectors and keyword rows.
	if err := s.ReplaceContent(ctx, id, []Item{{Kind: ItemPassage, Text: "Parking space included", Vector: vec(3)}}); err != nil {
		t.Fatal(err)
	}
	if matches(t, s, "pets") != 0 {
		t.Error("old passage still searchable")
	}
	if matches(t, s, "parking") != 1 {
		t.Error("new passage not searchable")
	}
	if n := count(t, s, `SELECT count(*) FROM vectors`); n != 1 {
		t.Errorf("vectors = %d, want 1", n)
	}
	if matches(t, s, "lease") != 1 {
		t.Error("file-name row was lost")
	}

	// Vectors round-trip exactly, including negative int8 values.
	if err := s.ReplaceContent(ctx, id, []Item{{Kind: ItemPassage, Text: "x", Vector: vec(-7)}}); err != nil {
		t.Fatal(err)
	}
	var bin, i8 []byte
	var scale float64
	s.db.QueryRow(`SELECT bin, i8, scale FROM vectors`).Scan(&bin, &i8, &scale)
	if len(bin) != VectorDim/8 || len(i8) != VectorDim || int8(i8[5]) != -7 || int8(bin[0]) != -7 || float32(scale) != 0.01 {
		t.Fatalf("vector round-trip failed: bin=%d i8=%d [5]=%d scale=%v", len(bin), len(i8), int8(i8[5]), scale)
	}
}

func TestReplaceContentRejectsNameKind(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	root, _ := s.AddRoot(ctx, `C:\Docs`)
	id := addFile(t, s, root.ID, `C:\Docs\a.txt`)
	if err := s.ReplaceContent(ctx, id, []Item{{Kind: ItemName}}); err == nil {
		t.Fatal("expected error")
	}
}

// P-5: deleting a file removes its text, vectors and keyword rows immediately.
func TestDeleteFileRemovesEverything(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	root, _ := s.AddRoot(ctx, `C:\Docs`)
	id := addFile(t, s, root.ID, `C:\Docs\diary.txt`)
	keep := addFile(t, s, root.ID, `C:\Docs\other.txt`)
	s.ReplaceContent(ctx, id, []Item{{Kind: ItemPassage, Text: "my private secret thoughts", Vector: vec(1)}})
	s.ReplaceContent(ctx, keep, []Item{{Kind: ItemPassage, Text: "shopping list", Vector: vec(2)}})
	s.db.Exec(`INSERT INTO thumbs(file_id, path) VALUES(?, 'ab/1.jpg')`, id)

	ok, err := s.DeleteFile(ctx, `C:\Docs\diary.txt`)
	if err != nil || !ok {
		t.Fatalf("DeleteFile = %v, %v", ok, err)
	}
	if matches(t, s, "secret") != 0 || matches(t, s, "diary") != 0 {
		t.Error("deleted file is still searchable")
	}
	for _, q := range []string{
		`SELECT count(*) FROM items WHERE file_id = ?`,
		`SELECT count(*) FROM thumbs WHERE file_id = ?`,
		`SELECT count(*) FROM vectors v JOIN items i ON i.id = v.item_id WHERE i.file_id = ?`,
	} {
		if n := count(t, s, q, id); n != 0 {
			t.Errorf("%s = %d, want 0", q, n)
		}
	}
	if n := count(t, s, `SELECT count(*) FROM vectors`); n != 1 {
		t.Errorf("other file's vectors affected: %d, want 1", n)
	}
	if matches(t, s, "shopping") != 1 {
		t.Error("other file's text was removed")
	}
	if ok, _ := s.DeleteFile(ctx, `C:\Docs\diary.txt`); ok {
		t.Error("second delete reported success")
	}
}

// P-5: removing a folder removes everything indexed from it, and nothing else.
func TestRemoveRootCascades(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	docs, _ := s.AddRoot(ctx, `C:\Docs`)
	pics, _ := s.AddRoot(ctx, `C:\Pics`)
	a := addFile(t, s, docs.ID, `C:\Docs\tax.txt`)
	b := addFile(t, s, pics.ID, `C:\Pics\beach.jpg`)
	s.ReplaceContent(ctx, a, []Item{{Kind: ItemPassage, Text: "income tax return", Vector: vec(1)}})
	s.ReplaceContent(ctx, b, []Item{{Kind: ItemImage, Text: "sunset", Vector: vec(2)}})
	s.AddExclude(ctx, docs.ID, `C:\Docs\old`)

	if err := s.RemoveRoot(ctx, docs.ID); err != nil {
		t.Fatal(err)
	}
	if matches(t, s, "tax") != 0 {
		t.Error("removed folder still searchable")
	}
	if matches(t, s, "sunset") != 1 || matches(t, s, "beach") != 1 {
		t.Error("other folder affected")
	}
	if n := count(t, s, `SELECT count(*) FROM files`); n != 1 {
		t.Errorf("files = %d, want 1", n)
	}
	if n := count(t, s, `SELECT count(*) FROM excludes`); n != 0 {
		t.Errorf("excludes = %d, want 0", n)
	}
	if n := count(t, s, `SELECT count(*) FROM vectors`); n != 1 {
		t.Errorf("vectors = %d, want 1", n)
	}
}

func TestAddRootIsIdempotent(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	a, err := s.AddRoot(ctx, `C:\Docs`)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := s.AddRoot(ctx, `c:\docs`)
	if a.ID != b.ID {
		t.Fatal("same folder added twice")
	}
	roots, _ := s.Roots(ctx)
	if len(roots) != 1 || !roots[0].Enabled {
		t.Fatalf("roots = %+v", roots)
	}
}

func TestCountContentIndexed(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	root, _ := s.AddRoot(ctx, `C:\Docs`)
	for i, st := range []string{StatusDone, StatusPartial, StatusPending, StatusFailed, StatusSkipped, StatusDone} {
		id := addFile(t, s, root.ID, filepath.Join(`C:\Docs`, string(rune('a'+i))+".txt"))
		s.SetFileResult(ctx, id, st, "", "", "")
	}
	n, err := s.CountContentIndexed(ctx)
	if err != nil || n != 3 {
		t.Fatalf("CountContentIndexed = %d, %v; want 3", n, err)
	}
}

func TestKeywordSearchIgnoresAccents(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	root, _ := s.AddRoot(ctx, `C:\Docs`)
	id := addFile(t, s, root.ID, `C:\Docs\menu.txt`)
	s.ReplaceContent(ctx, id, []Item{{Kind: ItemPassage, Text: "Crème brûlée at the Café"}})
	if matches(t, s, "creme brulee cafe") != 1 {
		t.Error("accent-insensitive match failed")
	}
}

func TestOpenRequiresAbsolutePath(t *testing.T) {
	if _, err := Open("index.db"); err == nil {
		t.Fatal("expected error")
	}
}

// Windows user names can contain spaces and symbols, and the database lives under them.
func TestOpenPathWithSpecialCharacters(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "Sanskar M #1 100% ?x")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		// '?' is not allowed in Windows names; retry without it.
		dir = filepath.Join(t.TempDir(), "Sanskar M #1 100%")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(dir, "index.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("database not created at the exact path: %v", err)
	}
}
