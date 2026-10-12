package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
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

	ok, thumbs, err := s.DeleteFile(ctx, `C:\Docs\diary.txt`)
	if err != nil || !ok {
		t.Fatalf("DeleteFile = %v, %v", ok, err)
	}
	if len(thumbs) != 1 || thumbs[0] != "ab/1.jpg" {
		t.Errorf("thumbs = %v, want [ab/1.jpg]", thumbs)
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
	if ok, _, _ := s.DeleteFile(ctx, `C:\Docs\diary.txt`); ok {
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

	if _, err := s.RemoveRoot(ctx, docs.ID); err != nil {
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
	var ids []int64
	for i := range 4 {
		ids = append(ids, addFile(t, s, root.ID, filepath.Join(`C:\Docs`, string(rune('a'+i))+".txt")))
	}
	// Three files get content; the fourth only has its name.
	for _, id := range ids[:3] {
		s.ReplaceContent(ctx, id, []Item{{Kind: ItemPassage, Text: "hello"}, {Kind: ItemPassage, Text: "more"}})
		s.SetFileResult(ctx, id, StatusDone, "", "", "")
	}
	if n, err := s.CountContentIndexed(ctx); err != nil || n != 3 {
		t.Fatalf("CountContentIndexed = %d, %v; want 3", n, err)
	}

	// Review finding 4: a modified file waiting for re-index still uses its slot…
	f, _, _ := s.FileByPath(ctx, `C:\Docs\a.txt`)
	f.MTime++
	s.UpsertFile(ctx, f)
	if n, _ := s.CountContentIndexed(ctx); n != 3 {
		t.Errorf("after modify: %d, want 3", n)
	}
	// …and if re-indexing fails, its stale content is removed and the slot is freed.
	s.SetFileResult(ctx, ids[0], StatusFailed, "", "", "read error")
	if n, _ := s.CountContentIndexed(ctx); n != 2 {
		t.Errorf("after failure: %d, want 2", n)
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

// Review finding 1: many connections opening at once must not fail with "database is locked".
func TestConcurrentConnections(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.db.SetMaxIdleConns(0) // force a brand-new connection for every operation
	ctx := context.Background()
	root, _ := s.AddRoot(ctx, `C:\Docs`)

	errs := make(chan error, 8*40)
	var wg sync.WaitGroup
	for w := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 40 {
				p := filepath.Join(`C:\Docs`, strconv.Itoa(w), strconv.Itoa(i)+".txt")
				id, _, err := s.UpsertFile(ctx, File{RootID: root.ID, Path: p, Name: filepath.Base(p), Ext: ".txt", Kind: "text", Size: 1, MTime: 1})
				if err == nil {
					err = s.ReplaceContent(ctx, id, []Item{{Kind: ItemPassage, Text: "word"}})
				}
				if err == nil {
					_, _, err = s.FileByPath(ctx, p)
				}
				errs <- err
			}
		}()
	}
	// A second Store on the same file at the same time (like a second app process).
	other, err := Open(path)
	if err != nil {
		t.Errorf("second Open: %v", err)
	} else {
		other.Close()
	}
	wg.Wait()
	close(errs)
	failed := 0
	for err := range errs {
		if err != nil {
			failed++
			if failed == 1 {
				t.Errorf("first error: %v", err)
			}
		}
	}
	if failed > 0 {
		t.Fatalf("%d of %d operations failed", failed, 8*40)
	}
}

// Review finding 3: moving a file to another root, or renaming it by case, updates the row.
func TestUpsertUpdatesRootAndCaseRename(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	a, _ := s.AddRoot(ctx, `C:\A`)
	b, _ := s.AddRoot(ctx, `D:\B`)
	f := File{RootID: a.ID, Path: `C:\A\report.txt`, Name: "report.txt", Ext: ".txt", Kind: "text", Size: 1, MTime: 1}
	id, _, _ := s.UpsertFile(ctx, f)

	f.RootID, f.Path, f.Name = b.ID, `C:\A\REPORT.txt`, "REPORT.txt"
	id2, _, err := s.UpsertFile(ctx, f)
	if err != nil || id2 != id {
		t.Fatalf("upsert: id=%d err=%v", id2, err)
	}
	got, _, _ := s.FileByPath(ctx, `c:\a\report.TXT`)
	if got.RootID != b.ID || got.Path != `C:\A\REPORT.txt` || got.Name != "REPORT.txt" {
		t.Fatalf("row not updated: %+v", got)
	}
	if n := count(t, s, `SELECT count(*) FROM items WHERE file_id = ? AND kind = ?`, id, ItemName); n != 1 {
		t.Errorf("name rows = %d, want 1", n)
	}
	if matches(t, s, "report") != 1 {
		t.Error("renamed file not searchable exactly once")
	}
}

// Review finding 3: nested roots are refused, so every file has exactly one root.
func TestAddRootRefusesNesting(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if _, err := s.AddRoot(ctx, `C:\Docs`); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{`C:\Docs\Sub`, `c:\docs\sub\deeper`, `C:\`} {
		if _, err := s.AddRoot(ctx, p); !errors.Is(err, ErrNestedRoot) {
			t.Errorf("AddRoot(%q) = %v, want ErrNestedRoot", p, err)
		}
	}
	for _, p := range []string{`C:\Docs2`, `D:\Docs`} {
		if _, err := s.AddRoot(ctx, p); err != nil {
			t.Errorf("AddRoot(%q) = %v", p, err)
		}
	}
}

// Review finding 6: non-ASCII names are matched case-insensitively, like Windows.
func TestNonASCIICaseInsensitive(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	root, _ := s.AddRoot(ctx, `C:\Docs`)
	id := addFile(t, s, root.ID, `C:\Docs\Ärger.txt`)
	s.SetFileResult(ctx, id, StatusDone, "", "", "")
	f, ok, _ := s.FileByPath(ctx, `C:\DOCS\ärger.TXT`)
	if !ok || f.ID != id {
		t.Fatal("non-ASCII path lookup is case-sensitive")
	}
	id2, changed, _ := s.UpsertFile(ctx, File{RootID: root.ID, Path: `C:\Docs\ÄRGER.txt`, Name: "ÄRGER.txt", Ext: ".txt", Kind: "text", Size: 10, MTime: 1000})
	if id2 != id || changed {
		t.Errorf("case-only rename created a new row or reported a change: id=%d changed=%v", id2, changed)
	}
}

// Review finding 5: ids are never reused, so stale references can't point at new data.
func TestIDsNotReused(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	root, _ := s.AddRoot(ctx, `C:\Docs`)
	id := addFile(t, s, root.ID, `C:\Docs\old.txt`)
	s.ReplaceContent(ctx, id, []Item{{Kind: ItemPassage, Text: "alpha"}})
	var maxItem int64
	s.db.QueryRow(`SELECT max(id) FROM items`).Scan(&maxItem)
	s.DeleteFile(ctx, `C:\Docs\old.txt`)

	id2 := addFile(t, s, root.ID, `C:\Docs\new.txt`)
	if id2 <= id {
		t.Errorf("file id reused: %d after %d", id2, id)
	}
	var minNew int64
	s.db.QueryRow(`SELECT min(id) FROM items`).Scan(&minNew)
	if minNew <= maxItem {
		t.Errorf("item id reused: %d <= %d", minNew, maxItem)
	}
	if matches(t, s, "alpha") != 0 {
		t.Error("deleted text still searchable")
	}
}

func TestRemoveRootReturnsThumbs(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	root, _ := s.AddRoot(ctx, `C:\Pics`)
	id := addFile(t, s, root.ID, `C:\Pics\a.jpg`)
	s.db.Exec(`INSERT INTO thumbs(file_id, path) VALUES(?, '00/1.jpg')`, id)
	thumbs, err := s.RemoveRoot(ctx, root.ID)
	if err != nil || len(thumbs) != 1 || thumbs[0] != "00/1.jpg" {
		t.Fatalf("RemoveRoot = %v, %v", thumbs, err)
	}
}

func TestOpenRefusesNetworkPaths(t *testing.T) {
	for _, p := range []string{`\server\share\index.db`, `\?\C:\x\index.db`} {
		if _, err := Open(p); err == nil {
			t.Errorf("Open(%q) succeeded", p)
		}
	}
}

func TestFilesInRoot(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	a, _ := s.AddRoot(ctx, `C:\A`)
	b, _ := s.AddRoot(ctx, `C:\B`)
	addFile(t, s, a.ID, `C:\A\1.txt`)
	addFile(t, s, a.ID, `C:\A\2.txt`)
	addFile(t, s, b.ID, `C:\B\3.txt`)
	got, err := s.FilesInRoot(ctx, a.ID)
	if err != nil || len(got) != 2 {
		t.Fatalf("FilesInRoot = %v, %v", got, err)
	}
}

func TestDedupeCopyContent(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	root, _ := s.AddRoot(ctx, `C:\Docs`)
	orig := addFile(t, s, root.ID, `C:\Docs\report.txt`)
	dup := addFile(t, s, root.ID, `C:\Docs\report (copy).txt`)
	s.ReplaceContent(ctx, orig, []Item{
		{Kind: ItemPassage, Locator: `{"chunk":0}`, Text: "quarterly numbers", Vector: vec(4)},
		{Kind: ItemPassage, Locator: `{"chunk":1}`, Text: "keyword only"},
	})
	s.SetFileResult(ctx, orig, StatusDone, "", "hash1", "")

	if copied, _ := s.CopyDuplicate(ctx, "hash1", orig); copied {
		t.Fatal("copied from itself")
	}
	if copied, err := s.CopyDuplicate(ctx, "hash1", dup); err != nil || !copied {
		t.Fatalf("CopyDuplicate = %v %v", copied, err)
	}
	if n := count(t, s, `SELECT count(*) FROM items WHERE file_id = ? AND kind <> ?`, dup, ItemName); n != 2 {
		t.Errorf("copied items = %d, want 2", n)
	}
	if n := count(t, s, `SELECT count(*) FROM vectors`); n != 2 {
		t.Errorf("vectors = %d, want 2", n)
	}
	if n := matches(t, s, "quarterly"); n != 2 {
		t.Errorf("keyword rows = %d, want 2", n)
	}
	// Deleting the original leaves the copy intact.
	s.DeleteFile(ctx, `C:\Docs\report.txt`)
	if n := matches(t, s, "quarterly"); n != 1 {
		t.Errorf("after delete: %d, want 1", n)
	}
	if copied, _ := s.CopyDuplicate(ctx, "unknown", dup); copied {
		t.Error("unknown hash copied")
	}
}

func TestSkippedFilesAreRetried(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	root, _ := s.AddRoot(ctx, `C:\Docs`)
	f := File{RootID: root.ID, Path: `C:\Docs\a.txt`, Name: "a.txt", Ext: ".txt", Kind: "text", Size: 1, MTime: 1}
	id, _, _ := s.UpsertFile(ctx, f)
	s.SetFileResult(ctx, id, StatusSkipped, "", "", "free tier limit")
	// Not re-offered on every scan (that cost a write per skipped file per rescan)…
	if _, changed, _ := s.UpsertFile(ctx, f); changed {
		t.Fatal("unchanged skipped file offered again on every scan")
	}
	// …but listed by SkippedFiles, for when a license or free slots are available.
	skipped, err := s.SkippedFiles(ctx, root.ID, 10)
	if err != nil || len(skipped) != 1 || skipped[0].ID != id || skipped[0].Size != 1 {
		t.Fatalf("SkippedFiles = %+v, %v", skipped, err)
	}
}

func TestHasContent(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	root, _ := s.AddRoot(ctx, `C:\Docs`)
	id := addFile(t, s, root.ID, `C:\Docs\a.txt`)
	if has, _ := s.HasContent(ctx, id); has {
		t.Fatal("name-only file reported as having content")
	}
	s.ReplaceContent(ctx, id, []Item{{Kind: ItemPassage, Text: "x"}})
	if has, _ := s.HasContent(ctx, id); !has {
		t.Fatal("content not detected")
	}
}

func TestFailedFilesRetriedLater(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	root, _ := s.AddRoot(ctx, `C:\Docs`)
	f := File{RootID: root.ID, Path: `C:\Docs\locked.docx`, Name: "locked.docx", Ext: ".docx", Kind: "doc", Size: 1, MTime: 1}
	id, _, _ := s.UpsertFile(ctx, f)
	s.SetFileResult(ctx, id, StatusFailed, "", "", "file locked")
	if _, changed, _ := s.UpsertFile(ctx, f); changed {
		t.Fatal("failed file retried immediately")
	}
	old := time.Now().Add(-RetryFailedAfter - time.Hour).UnixNano()
	s.db.Exec(`UPDATE files SET indexed_at = ? WHERE id = ?`, old, id)
	if _, changed, _ := s.UpsertFile(ctx, f); !changed {
		t.Fatal("failed file not retried after the wait")
	}
}

// Review: one file with many matching passages must not hide other files, in either path.
func TestKeywordOneBigFileDoesNotCrowdOut(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	root, _ := s.AddRoot(ctx, `C:\Docs`)
	big := addFile(t, s, root.ID, `C:\Docs\big.txt`)
	var items []Item
	for range 700 {
		items = append(items, Item{Kind: ItemPassage, Text: "budget numbers again"})
	}
	s.ReplaceContent(ctx, big, items)
	for i := range 19 {
		id := addFile(t, s, root.ID, filepath.Join(`C:\Docs`, fmt.Sprintf("small%d.txt", i)))
		s.ReplaceContent(ctx, id, []Item{{Kind: ItemPassage, Text: "a small budget note"}})
	}
	for _, ranked := range []bool{true, false} {
		hits, err := s.KeywordSearch(ctx, `"budget"`, Filter{}, 50, ranked)
		if err != nil {
			t.Fatal(err)
		}
		if len(hits) != 20 {
			t.Errorf("ranked=%v: %d files, want 20", ranked, len(hits))
		}
	}
	// A folder filter that excludes the big file still finds the others.
	hits, _ := s.KeywordSearch(ctx, `"budget"`, Filter{Folder: `C:\Docs`}, 50, false)
	if len(hits) != 20 {
		t.Errorf("filtered: %d files", len(hits))
	}
}

// Review: folder filter with non-ASCII folder names (SQLite counts characters).
func TestFolderFilterNonASCII(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	root, _ := s.AddRoot(ctx, `C:\Users\José\Документы`)
	id := addFile(t, s, root.ID, `C:\Users\José\Документы\отчёт.txt`)
	s.ReplaceContent(ctx, id, []Item{{Kind: ItemPassage, Text: "quarterly report"}})
	for _, ranked := range []bool{true, false} {
		hits, err := s.KeywordSearch(ctx, `"quarterly"`, Filter{Folder: `c:\users\josé\документы`}, 50, ranked)
		if err != nil || len(hits) != 1 {
			t.Errorf("ranked=%v: %d hits, %v", ranked, len(hits), err)
		}
	}
	if hits, _ := s.KeywordSearch(ctx, `"quarterly"`, Filter{Folder: `C:\Users\José\Док`}, 50, true); len(hits) != 0 {
		t.Error("partial folder name matched")
	}
}

// Full review #5: names NTFS keeps apart (σ/ς, µ/μ) must get separate rows.
func TestDistinctNTFSNamesGetSeparateRows(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	root, _ := s.AddRoot(ctx, `C:\Docs`)
	a := addFile(t, s, root.ID, `C:\Docs\σ.txt`)
	b := addFile(t, s, root.ID, `C:\Docs\ς.txt`)
	if a == b {
		t.Fatal("σ.txt and ς.txt share one index row")
	}
	if ok, _, _ := s.DeleteFile(ctx, `C:\Docs\σ.txt`); !ok {
		t.Fatal("delete failed")
	}
	if _, ok, _ := s.FileByPath(ctx, `C:\Docs\ς.txt`); !ok {
		t.Fatal("deleting σ.txt removed ς.txt")
	}
}

// Full review #6 (I-7): a damaged index is detected at open with ErrCorrupt.
func TestCorruptDatabaseIsDetected(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "index.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	root, _ := s.AddRoot(ctx, `C:\Docs`)
	for i := range 300 {
		id := addFile(t, s, root.ID, fmt.Sprintf(`C:\Docs\%d.txt`, i))
		s.ReplaceContent(ctx, id, []Item{{Kind: ItemPassage, Text: strings.Repeat("words ", 200), Vector: vec(int8(i % 100))}})
	}
	s.Close()

	// Overwrite a stretch in the middle of the file with garbage.
	b, _ := os.ReadFile(path)
	for i := len(b) / 2; i < len(b)/2+16384 && i < len(b); i++ {
		b[i] = 0xA5
	}
	os.WriteFile(path, b, 0o644)
	// Damage may or may not be visible at open; Verify must always find it.
	if s2, err := Open(path); err == nil {
		err = s2.Verify(ctx)
		s2.Close()
		if !errors.Is(err, ErrCorrupt) {
			t.Fatalf("damaged index: Verify = %v, want ErrCorrupt", err)
		}
	} else if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("damaged index: Open = %v, want ErrCorrupt", err)
	}

	// A file that is not a database at all.
	junk := filepath.Join(dir, "junk.db")
	os.WriteFile(junk, []byte(strings.Repeat("not sqlite ", 1000)), 0o644)
	if _, err := Open(junk); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("junk file: got %v, want ErrCorrupt", err)
	}
	if got := DatabaseFiles(path); len(got) != 3 || got[1] != path+"-wal" {
		t.Fatalf("DatabaseFiles = %v", got)
	}
}

// IsCorrupt must not mistake ordinary errors for damage.
func TestIsCorruptOnlyForDamage(t *testing.T) {
	for _, err := range []error{
		errors.New("fts5: malformed MATCH expression"),
		errors.New(`open C:\not a database\x.db: access denied`),
	} {
		if IsCorrupt(err) {
			t.Errorf("IsCorrupt(%q) = true", err)
		}
	}
	s := openTest(t)
	if err := s.Verify(context.Background()); err != nil {
		t.Fatalf("healthy index: %v", err)
	}
}

// Medium #14: the model is recorded and a different model is refused.
func TestEnsureModel(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if err := s.EnsureModel(ctx, "model-a"); err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureModel(ctx, "model-a"); err != nil {
		t.Fatalf("same model refused: %v", err)
	}
	if err := s.EnsureModel(ctx, "model-b"); !errors.Is(err, ErrModelChanged) {
		t.Fatalf("got %v, want ErrModelChanged", err)
	}
	if v, ok, _ := s.Meta(ctx, "created_at"); !ok || v == "" {
		t.Fatal("created_at not recorded")
	}
}

// Review round 2: a second app with another model cannot overwrite the first one's record.
func TestEnsureModelFirstWriterWins(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if err := s.EnsureModel(ctx, "model-a"); err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureModel(ctx, "model-b"); !errors.Is(err, ErrModelChanged) {
		t.Fatalf("got %v", err)
	}
	if v, _, _ := s.Meta(ctx, "model_version"); v != "model-a" {
		t.Fatalf("model_version = %q", v)
	}
}
