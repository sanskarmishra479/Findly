//go:build windows

package index

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sanskarmishra479/findly/internal/embed"
	"github.com/sanskarmishra479/findly/internal/safefs"
	"github.com/sanskarmishra479/findly/internal/scan"
	"github.com/sanskarmishra479/findly/internal/store"
	"github.com/sanskarmishra479/findly/internal/vecfmt"
)

// fakeEmbedder returns deterministic vectors and records what it was asked to embed.
type fakeEmbedder struct {
	mu    sync.Mutex
	texts []string
	err   error
}

func (f *fakeEmbedder) EmbedPassages(_ context.Context, _ string, texts []string) ([]embed.Vector, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	f.texts = append(f.texts, texts...)
	out := make([]embed.Vector, len(texts))
	for i, t := range texts {
		v := make([]float32, 768)
		for j := range v {
			v[j] = float32(math.Sin(float64(len(t)*31 + j)))
		}
		bin, q, _ := vecfmt.Encode(v)
		out[i] = embed.Vector{Bin: bin, Q: q}
	}
	return out, nil
}

func (f *fakeEmbedder) count() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.texts) }

type env struct {
	t    *testing.T
	dir  string
	st   *store.Store
	emb  *fakeEmbedder
	ix   *Indexer
	root store.Root
}

func newEnv(t *testing.T) *env {
	t.Helper()
	dir, err := safefs.Canonical(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	root, err := st.AddRoot(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	emb := &fakeEmbedder{}
	return &env{t: t, dir: dir, st: st, emb: emb, root: root,
		ix: &Indexer{Store: st, Embedder: emb, Workers: 2}}
}

func (e *env) write(rel, content string) string {
	e.t.Helper()
	p := filepath.Join(e.dir, filepath.FromSlash(rel))
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		e.t.Fatal(err)
	}
	return p
}

func (e *env) run() Stats {
	e.t.Helper()
	st, err := e.ix.IndexRoot(context.Background(), e.root)
	if err != nil {
		e.t.Fatal(err)
	}
	return st
}

func (e *env) file(rel string) store.File {
	e.t.Helper()
	f, ok, err := e.st.FileByPath(context.Background(), filepath.Join(e.dir, filepath.FromSlash(rel)))
	if err != nil || !ok {
		e.t.Fatalf("%s not in index: %v", rel, err)
	}
	return f
}

func TestIndexesTextFiles(t *testing.T) {
	e := newEnv(t)
	e.write("lease.txt", "The tenant may not keep pets in the apartment.")
	e.write("notes.md", "# Shopping\n\nBuy milk and bread.")
	e.write("page.html", "<p>Payment declined: error 0x80070005</p>")
	e.write("photo.jpg", "not really a jpeg")
	e.write("tool.exe", "MZ")

	st := e.run()
	if st.Indexed != 3 || st.Failed != 0 {
		t.Fatalf("stats = %+v", st)
	}
	for _, rel := range []string{"lease.txt", "notes.md", "page.html", "tool.exe"} {
		if f := e.file(rel); f.Status != store.StatusDone {
			t.Errorf("%s status = %s", rel, f.Status)
		}
	}
	if f := e.file("photo.jpg"); f.Status != store.StatusPending {
		t.Errorf("image status = %s, want pending (Phase 2)", f.Status)
	}
	if e.emb.count() != 3 {
		t.Errorf("embedded %d passages, want 3", e.emb.count())
	}
	if n, _ := e.st.CountContentIndexed(context.Background()); n != 3 {
		t.Errorf("content-indexed = %d, want 3", n)
	}
}

func TestUnchangedFilesAreNotReindexed(t *testing.T) {
	e := newEnv(t)
	p := e.write("a.txt", "first version of the text")
	e.run()
	before := e.emb.count()
	if st := e.run(); st.Indexed != 0 || e.emb.count() != before {
		t.Fatalf("second run re-indexed: %+v", st)
	}
	// Modify → indexed again.
	os.WriteFile(p, []byte("second, longer version of the text"), 0o644)
	os.Chtimes(p, time.Now(), time.Now().Add(time.Second))
	if st := e.run(); st.Indexed != 1 {
		t.Fatalf("modified file not re-indexed: %+v", st)
	}
}

// P-5: a file deleted while Findly was closed disappears from the index on the next run.
func TestDeletedFilesAreRemoved(t *testing.T) {
	e := newEnv(t)
	p := e.write("diary.txt", "very private words")
	e.write("keep.txt", "keep this")
	e.run()
	os.Remove(p)
	st := e.run()
	if st.Deleted != 1 {
		t.Fatalf("Deleted = %d, want 1", st.Deleted)
	}
	if _, ok, _ := e.st.FileByPath(context.Background(), p); ok {
		t.Fatal("deleted file still in index")
	}
}

// If a subfolder cannot be read, its files must NOT be treated as deleted.
func TestUnreadableFolderIsNotPurged(t *testing.T) {
	e := newEnv(t)
	e.write("locked/inside.txt", "content in a folder that becomes unreadable")
	e.run()
	locked := filepath.Join(e.dir, "locked")
	user := os.Getenv("USERNAME")
	if out, err := exec.Command("icacls", locked, "/deny", user+":(RD)").CombinedOutput(); err != nil {
		t.Skipf("icacls: %v %s", err, out)
	}
	t.Cleanup(func() { exec.Command("icacls", locked, "/remove:d", user).Run() })
	if st := e.run(); st.Deleted != 0 {
		t.Fatalf("purged %d files from an unreadable folder", st.Deleted)
	}
}

// An unplugged drive (missing root) is an error, and nothing is deleted.
func TestMissingRootDeletesNothing(t *testing.T) {
	e := newEnv(t)
	e.write("a.txt", "on the usb drive")
	e.run()
	gone := e.root
	gone.Path = filepath.Join(e.dir, "unplugged")
	if _, err := e.ix.IndexRoot(context.Background(), gone); err == nil {
		t.Fatal("expected error for missing root")
	}
	if paths, _ := e.st.FilesInRoot(context.Background(), e.root.ID); len(paths) != 1 {
		t.Fatalf("files after failed run = %v", paths)
	}
}

func TestDuplicateFilesAreEmbeddedOnce(t *testing.T) {
	e := newEnv(t)
	text := "Exactly the same contract text in two places."
	e.write("contract.txt", text)
	e.ix.Workers = 1
	e.run()
	e.write("backup/contract copy.txt", text)
	e.run()
	if e.emb.count() != 1 {
		t.Fatalf("embedded %d times, want 1", e.emb.count())
	}
	if f := e.file("backup/contract copy.txt"); f.Status != store.StatusDone {
		t.Fatalf("copy status = %s", f.Status)
	}
	if n, _ := e.st.CountContentIndexed(context.Background()); n != 2 {
		t.Fatalf("content-indexed = %d, want 2", n)
	}
}

func TestMostlyNumericIsKeywordOnly(t *testing.T) {
	e := newEnv(t)
	e.write("export.csv", strings.Repeat("2024-01-01,1234.56,7890,12.5\n", 50))
	e.run()
	if e.emb.count() != 0 {
		t.Fatalf("numeric export was embedded")
	}
	if f := e.file("export.csv"); f.Status != store.StatusDone {
		t.Fatalf("status = %s", f.Status)
	}
}

// Free tier: content indexing stops at FreeLimit; everything else stays name-searchable.
func TestFreeTierLimit(t *testing.T) {
	e := newEnv(t)
	for i := range FreeLimit + 5 {
		e.write(fmt.Sprintf("f/%03d.txt", i), fmt.Sprintf("document number %d with unique words %d", i, i*7))
	}
	st := e.run()
	if st.Indexed != FreeLimit || st.Skipped != 5 {
		t.Fatalf("Indexed=%d Skipped=%d, want %d and 5", st.Indexed, st.Skipped, FreeLimit)
	}
	if n, _ := e.st.CountContentIndexed(context.Background()); n != FreeLimit {
		t.Fatalf("content-indexed = %d", n)
	}
	// After purchase the skipped files are indexed.
	e.ix.Licensed = func() bool { return true }
	if st := e.run(); st.Indexed != 5 {
		t.Fatalf("after license: Indexed=%d, want 5", st.Indexed)
	}
}

// If the embedding server dies, the run stops; files are not wrongly marked failed.
func TestEmbedderFailureStopsRun(t *testing.T) {
	e := newEnv(t)
	e.write("a.txt", "some text to embed")
	e.emb.err = fmt.Errorf("%w: crashed 3 times", embed.ErrServer)
	if _, err := e.ix.IndexRoot(context.Background(), e.root); !errors.Is(err, ErrEmbedder) {
		t.Fatalf("got %v, want ErrEmbedder", err)
	}
	if f := e.file("a.txt"); f.Status == store.StatusFailed {
		t.Fatal("file marked failed because the server died")
	}
	e.emb.err = nil
	if st := e.run(); st.Indexed != 1 {
		t.Fatalf("not indexed after recovery: %+v", st)
	}
}

func TestBrokenFileIsFailedNotFatal(t *testing.T) {
	e := newEnv(t)
	e.write("broken.docx", "not a zip at all")
	e.write("good.txt", "this one is fine")
	st := e.run()
	if st.Failed != 1 || st.Indexed != 1 {
		t.Fatalf("stats = %+v", st)
	}
	if f := e.file("broken.docx"); f.Status != store.StatusFailed || f.Error == "" {
		t.Fatalf("broken file: %+v", f)
	}
}

func TestPriority(t *testing.T) {
	now := time.Now()
	recent, old := now.Add(-time.Hour).UnixNano(), now.Add(-90*24*time.Hour).UnixNano()
	mk := func(path, kind string, mtime int64) int {
		return priority(scanFile(path, kind, mtime), now)
	}
	a := mk(`C:\Users\x\Documents\a.txt`, "text", recent)
	b := mk(`C:\Data\b.txt`, "text", recent)
	c := mk(`C:\Users\x\Documents\c.txt`, "text", old)
	if !(a < b && b < c) {
		t.Fatalf("priorities a=%d b=%d c=%d, want a<b<c", a, b, c)
	}
}

func TestProgressIsReported(t *testing.T) {
	e := newEnv(t)
	e.write("a.txt", "one")
	e.write("b.txt", "two")
	var last Progress
	e.ix.OnProgress = func(p Progress) { last = p }
	e.run()
	if last.Total != 2 || last.Done != 2 {
		t.Fatalf("progress = %+v", last)
	}
}

func scanFile(path, kind string, mtime int64) scan.File {
	return scan.File{Path: path, Name: filepath.Base(path), Kind: kind, ModTime: mtime}
}

func (e *env) fill(n int) {
	for i := range n {
		e.write(fmt.Sprintf("f/%03d.txt", i), fmt.Sprintf("document number %d with unique words %d", i, i*7))
	}
}

func touch(t *testing.T, p, content string) {
	t.Helper()
	os.WriteFile(p, []byte(content), 0o644)
	later := time.Now().Add(time.Minute)
	os.Chtimes(p, later, later)
}

// Review 1 (L-3): editing an already-indexed file at the free limit keeps it searchable.
func TestEditAtFreeLimitKeepsContent(t *testing.T) {
	e := newEnv(t)
	e.fill(FreeLimit)
	e.run()
	touch(t, filepath.Join(e.dir, "f", "007.txt"), "edited text for document seven")
	st := e.run()
	if st.Indexed != 1 || st.Skipped != 0 {
		t.Fatalf("stats = %+v", st)
	}
	if f := e.file("f/007.txt"); f.Status != store.StatusDone {
		t.Fatalf("edited file status = %s", f.Status)
	}
	if n, _ := e.st.CountContentIndexed(context.Background()); n != FreeLimit {
		t.Fatalf("content-indexed = %d, want %d", n, FreeLimit)
	}
}

// Review 2: re-indexed files do not use extra free slots.
func TestReindexDoesNotDoubleCount(t *testing.T) {
	e := newEnv(t)
	e.fill(10)
	e.run()
	for i := range 10 {
		touch(t, filepath.Join(e.dir, "f", fmt.Sprintf("%03d.txt", i)), fmt.Sprintf("edited %d", i))
	}
	for i := range FreeLimit - 10 {
		e.write(fmt.Sprintf("g/%03d.txt", i), fmt.Sprintf("new file %d words %d", i, i*3))
	}
	st := e.run()
	if st.Indexed != FreeLimit || st.Skipped != 0 {
		t.Fatalf("Indexed=%d Skipped=%d, want %d and 0", st.Indexed, st.Skipped, FreeLimit)
	}
}

// Review 3: a transient server error stops the run and leaves files pending (not failed).
func TestTransientEmbedErrorLeavesPending(t *testing.T) {
	e := newEnv(t)
	e.write("a.txt", "text to embed")
	e.emb.err = errors.New("embed: request: connection reset")
	if _, err := e.ix.IndexRoot(context.Background(), e.root); !errors.Is(err, ErrEmbedder) {
		t.Fatalf("got %v, want ErrEmbedder", err)
	}
	if f := e.file("a.txt"); f.Status != store.StatusPending {
		t.Fatalf("status = %s, want pending", f.Status)
	}
	// An error about the input itself fails only that file.
	e.emb.err = fmt.Errorf("%w: too long", embed.ErrInput)
	st := e.run()
	if st.Failed != 1 {
		t.Fatalf("stats = %+v", st)
	}
}

// Review 5: an unreadable folder keeps its own files but does not stop cleanup elsewhere.
func TestUnreadableFolderOnlyProtectsItself(t *testing.T) {
	e := newEnv(t)
	e.write("locked/inside.txt", "inside a folder that becomes unreadable")
	gone := e.write("other/gone.txt", "will be deleted")
	e.run()
	locked := filepath.Join(e.dir, "locked")
	user := os.Getenv("USERNAME")
	if out, err := exec.Command("icacls", locked, "/deny", user+":(RD)").CombinedOutput(); err != nil {
		t.Skipf("icacls: %v %s", err, out)
	}
	t.Cleanup(func() { exec.Command("icacls", locked, "/remove:d", user).Run() })
	os.Remove(gone)
	st := e.run()
	if st.Deleted != 1 {
		t.Fatalf("Deleted = %d, want 1 (only other/gone.txt)", st.Deleted)
	}
	if _, ok, _ := e.st.FileByPath(context.Background(), filepath.Join(locked, "inside.txt")); !ok {
		t.Fatal("file in unreadable folder was purged")
	}
}

// Review 6: progress reaches Total even with images and other files present.
func TestProgressCompletes(t *testing.T) {
	e := newEnv(t)
	e.write("a.txt", "text one")
	e.write("b.md", "text two")
	e.write("pic.png", "x")
	e.write("app.exe", "MZ")
	var last Progress
	e.ix.OnProgress = func(p Progress) { last = p }
	e.run()
	if last.Total != 2 || last.Done != last.Total {
		t.Fatalf("progress = %+v", last)
	}
	// Images stay pending but are not re-queued as work for this phase.
	last = Progress{}
	if st := e.run(); st.Indexed != 0 || last.Total != 0 {
		t.Fatalf("second run did work: %+v %+v", st, last)
	}
}

// Review 7: cancelling a run returns promptly.
func TestCancelStopsQuickly(t *testing.T) {
	e := newEnv(t)
	e.fill(200)
	ctx, cancel := context.WithCancel(context.Background())
	n := 0
	e.ix.OnProgress = func(Progress) {
		if n++; n == 5 {
			cancel()
		}
	}
	start := time.Now()
	_, err := e.ix.IndexRoot(ctx, e.root)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want Canceled", err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("cancel took %v", d)
	}
}

// crashingEmbedder crashes on any passage containing "poison"; it counts calls.
type crashingEmbedder struct {
	fakeEmbedder
	calls int
}

func (c *crashingEmbedder) EmbedPassages(ctx context.Context, title string, texts []string) ([]embed.Vector, error) {
	c.calls++
	for _, t := range texts {
		if strings.Contains(t, "poison") {
			return nil, fmt.Errorf("%w: %w: connection reset", embed.ErrServer, embed.ErrCrashed)
		}
	}
	return c.fakeEmbedder.EmbedPassages(ctx, title, texts)
}

// Full review #8 (plan 5): a file that crashes the server is retried once, then marked
// failed, and indexing continues with the other files.
func TestCrashingFileIsSkipped(t *testing.T) {
	e := newEnv(t)
	emb := &crashingEmbedder{}
	e.ix.Embedder = emb
	e.write("a.txt", "fine text one")
	e.write("bad.txt", "poison pill content")
	e.write("c.txt", "fine text two")
	st := e.run()
	if st.Indexed != 2 || st.Failed != 1 {
		t.Fatalf("stats = %+v", st)
	}
	if f := e.file("bad.txt"); f.Status != store.StatusFailed || !strings.Contains(f.Error, "crashed") {
		t.Fatalf("bad file: %+v", f)
	}
}

// …but if several files in a row crash it, the server is broken: stop, don't fail them all.
func TestServerCrashingOnEverythingStopsRun(t *testing.T) {
	e := newEnv(t)
	e.ix.Embedder = &crashingEmbedder{}
	for i := range 6 {
		e.write(fmt.Sprintf("p%d.txt", i), "poison everywhere")
	}
	_, err := e.ix.IndexRoot(context.Background(), e.root)
	if !errors.Is(err, ErrEmbedder) {
		t.Fatalf("got %v, want ErrEmbedder", err)
	}
	failed := 0
	for i := range 6 {
		if e.file(fmt.Sprintf("p%d.txt", i)).Status == store.StatusFailed {
			failed++
		}
	}
	if failed >= maxCrashFiles {
		t.Fatalf("%d files marked failed; the run should stop before blaming them all", failed)
	}
}

// Full review #10: cancelling during embedding is reported as cancellation.
func TestCancelDuringEmbedIsCancellation(t *testing.T) {
	e := newEnv(t)
	e.write("a.txt", "text")
	ctx, cancel := context.WithCancel(context.Background())
	e.ix.Embedder = cancelEmbedder{cancel}
	_, err := e.ix.IndexRoot(ctx, e.root)
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrEmbedder) {
		t.Fatalf("got %v, want context.Canceled (not ErrEmbedder)", err)
	}
}

type cancelEmbedder struct{ cancel context.CancelFunc }

func (c cancelEmbedder) EmbedPassages(ctx context.Context, _ string, _ []string) ([]embed.Vector, error) {
	c.cancel()
	return nil, fmt.Errorf("embed: request: %w", ctx.Err())
}

// High-fix review #6: an unreadable folder whose name contains a character where Go's
// ToUpper and Windows disagree (ς, ı, µ…) still protects its files from being purged.
func TestUnreadableFolderNonASCIIName(t *testing.T) {
	e := newEnv(t)
	e.write("Προσωπικά ς/inside.txt", "content in a Greek-named folder")
	e.run()
	locked := filepath.Join(e.dir, "Προσωπικά ς")
	user := os.Getenv("USERNAME")
	if out, err := exec.Command("icacls", locked, "/deny", user+":(RD)").CombinedOutput(); err != nil {
		t.Skipf("icacls: %v %s", err, out)
	}
	t.Cleanup(func() { exec.Command("icacls", locked, "/remove:d", user).Run() })
	if st := e.run(); st.Deleted != 0 {
		t.Fatalf("purged %d files from an unreadable folder", st.Deleted)
	}
}
