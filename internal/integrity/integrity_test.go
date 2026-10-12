//go:build windows

// Package integrity holds the file-integrity test (plan 14.3, invariant I-9): Findly
// must never change, move, delete, lock or download any user file. It runs the whole
// engine over a tricky folder and proves every file and folder is bit-for-bit the same.
package integrity

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sanskarmishra479/findly/internal/appfs"
	"github.com/sanskarmishra479/findly/internal/embed"
	"github.com/sanskarmishra479/findly/internal/evalkit"
	"github.com/sanskarmishra479/findly/internal/index"
	"github.com/sanskarmishra479/findly/internal/safefs"
	"github.com/sanskarmishra479/findly/internal/search"
	"github.com/sanskarmishra479/findly/internal/store"
	"github.com/sanskarmishra479/findly/internal/vecfmt"
	"golang.org/x/sys/windows"
)

// fakeEmbedder gives deterministic vectors so the test runs without the model.
type fakeEmbedder struct{}

func (fakeEmbedder) EmbedPassages(_ context.Context, _ string, texts []string) ([]embed.Vector, error) {
	out := make([]embed.Vector, len(texts))
	for i, t := range texts {
		out[i] = vec(len(t))
	}
	return out, nil
}

func (fakeEmbedder) EmbedQuery(_ context.Context, q string) (embed.Vector, error) {
	return vec(len(q)), nil
}

func vec(seed int) embed.Vector {
	v := make([]float32, 768)
	for j := range v {
		v[j] = float32(math.Sin(float64(seed*31 + j)))
	}
	b, q, _ := vecfmt.Encode(v)
	return embed.Vector{Bin: b, Q: q}
}

// entry is everything we record about one file or folder.
type entry struct {
	IsDir bool
	Size  int64
	MTime time.Time
	Attrs uint32
	SHA   string
}

// snapshot records every file and folder under dir, without following links. It reads
// with os (not safefs) so the check does not depend on the code under test.
func snapshot(t *testing.T, dir string) map[string]entry {
	t.Helper()
	out := map[string]entry{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		e := entry{IsDir: d.IsDir(), Size: info.Size(), MTime: info.ModTime(), Attrs: attrs(t, p)}
		if info.Mode().IsRegular() && e.Attrs&windows.FILE_ATTRIBUTE_OFFLINE == 0 {
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(b)
			e.SHA = hex.EncodeToString(sum[:])
		}
		out[rel] = e
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func attrs(t *testing.T, p string) uint32 {
	p16, _ := windows.UTF16PtrFromString(p)
	a, err := windows.GetFileAttributes(p16)
	if err != nil {
		t.Fatalf("attributes of %s: %v", p, err)
	}
	return a
}

// diff lists every difference between two snapshots (sorted).
func diff(before, after map[string]entry) []string {
	var problems []string
	for p, b := range before {
		a, ok := after[p]
		switch {
		case !ok:
			problems = append(problems, "DELETED "+p)
		case a != b:
			problems = append(problems, fmt.Sprintf("CHANGED %s: %+v → %+v", p, b, a))
		}
	}
	for p := range after {
		if _, ok := before[p]; !ok {
			problems = append(problems, "CREATED "+p)
		}
	}
	sort.Strings(problems)
	return problems
}

func compare(t *testing.T, label string, before, after map[string]entry) {
	t.Helper()
	for _, p := range diff(before, after) {
		t.Errorf("%s: %s", label, p)
	}
}

func setAttrs(t *testing.T, p string, a uint32) {
	t.Helper()
	p16, _ := windows.UTF16PtrFromString(p)
	if err := windows.SetFileAttributes(p16, a); err != nil {
		t.Fatal(err)
	}
}

// buildTree makes the user folder: the eval corpus as real PDF/DOCX/ODT files plus
// awkward cases. It returns the user folder and a separate "outside" folder that a
// junction inside the user folder points to.
func buildTree(t *testing.T) (user, outside string) {
	t.Helper()
	base := t.TempDir()
	user = filepath.Join(base, "User Docs ünïcødé")
	outside = filepath.Join(base, "outside")
	if _, err := evalkit.BuildTestbed(filepath.Join("..", "..", "evals", "corpus"), user); err != nil {
		t.Fatal(err)
	}
	write := func(rel, content string) string {
		p := filepath.Join(user, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	write("empty.txt", "")
	write("zzz-last.txt", "sorts after every other entry, including the junction")
	write("日本語のメモ.txt", "東京で会議があります。")
	ro := write("readonly.txt", "this file is read-only")
	setAttrs(t, ro, windows.FILE_ATTRIBUTE_READONLY)
	hidden := write("hidden-note.txt", "hidden file")
	setAttrs(t, hidden, windows.FILE_ATTRIBUTE_HIDDEN)
	cloud := write("cloud-only.txt", "pretend this lives only in OneDrive")
	setAttrs(t, cloud, windows.FILE_ATTRIBUTE_OFFLINE)
	write("secrets/api.key", "sk-not-a-real-key")
	write("project/package.json", "{}")
	write("project/src/index.js", "console.log('hi')")
	deep := strings.Repeat("deep folder name/", 14)
	write(deep+"long-path.txt", "a file under a very long path")
	// Spike files: real images and audio (not read in Phase 1, but must stay unchanged).
	for _, f := range []string{"Golden Retriever at Sunset Beach.png", "Sunset Tabby on the Terrace.png"} {
		if b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "spike", f)); err == nil {
			write("photos/"+f, string(b))
		}
	}

	os.MkdirAll(outside, 0o755)
	os.WriteFile(filepath.Join(outside, "elsewhere.txt"), []byte("must never be indexed or touched"), 0o644)
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", filepath.Join(user, "jump"), outside).CombinedOutput(); err != nil {
		t.Fatalf("mklink: %v %s", err, out)
	}
	t.Cleanup(func() {
		setAttrs(t, ro, windows.FILE_ATTRIBUTE_NORMAL)
		setAttrs(t, cloud, windows.FILE_ATTRIBUTE_NORMAL)
	})
	return user, outside
}

// TestFileIntegrity runs indexing, searches, folder removal and an index rebuild, then
// proves no user file or folder changed in any way (plan 14.3).
func TestFileIntegrity(t *testing.T) {
	user, outside := buildTree(t)
	before, beforeOut := snapshot(t, user), snapshot(t, outside)
	// The snapshot must really cover the whole tree. (A SkipDir on the junction once made
	// WalkDir skip everything after it, about 60% of the files, without failing.)
	for _, want := range []string{"zzz-last.txt", "work", filepath.Join("money", "invoice_INV-2024-0042.txt"), "日本語のメモ.txt", "readonly.txt", "jump"} {
		if _, ok := before[want]; !ok {
			t.Fatalf("snapshot is missing %s (has %d entries)", want, len(before))
		}
	}

	dataDir := filepath.Join(t.TempDir(), "Findly")
	afs, err := appfs.Open(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer afs.Close()
	ctx := context.Background()
	open := func() *store.Store {
		p, err := afs.Path("index/index.db")
		if err != nil {
			t.Fatal(err)
		}
		st, err := store.Open(p)
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	runAll := func(st *store.Store) {
		real, err := safefs.Canonical(user)
		if err != nil {
			t.Fatal(err)
		}
		root, err := st.AddRoot(ctx, real)
		if err != nil {
			t.Fatal(err)
		}
		ix := &index.Indexer{Store: st, Embedder: fakeEmbedder{}, Licensed: func() bool { return true }}
		stats, err := ix.IndexRoot(ctx, root)
		if err != nil {
			t.Fatal(err)
		}
		if stats.Indexed == 0 || stats.Failed != 0 {
			t.Fatalf("index stats: %+v", stats)
		}
		// P-6 and I-4: the secret file and the cloud-only file must not be in the index at all.
		for _, rel := range []string{filepath.Join("secrets", "api.key"), "cloud-only.txt", "hidden-note.txt"} {
			if _, ok, _ := st.FileByPath(ctx, filepath.Join(real, rel)); ok {
				t.Errorf("%s was indexed", rel)
			}
		}
		// No handle may stay open after indexing: every file must open exclusively.
		assertNoOpenHandles(t, real)
		// Twice: the second run is the "nothing changed" path.
		if _, err := ix.IndexRoot(ctx, root); err != nil {
			t.Fatal(err)
		}
		cache := &search.VectorCache{}
		if err := cache.Load(ctx, st); err != nil {
			t.Fatal(err)
		}
		s := &search.Searcher{Store: st, Embedder: fakeEmbedder{}, Cache: cache}
		for _, q := range []string{"pets", "INV-2024-0042", "मानसून", "東京", `"brake pads"`, "elsewhere"} {
			rs, err := s.Search(ctx, q, store.Filter{})
			if err != nil {
				t.Fatal(err)
			}
			for _, r := range rs {
				if strings.Contains(r.File.Path, "outside") || strings.Contains(r.File.Path, "elsewhere") {
					t.Fatalf("file outside the chosen folder was indexed: %s", r.File.Path)
				}
			}
		}
		thumbs, err := st.RemoveRoot(ctx, root.ID)
		if err != nil {
			t.Fatal(err)
		}
		_ = thumbs
	}

	st := open()
	runAll(st)
	// "Rebuild index": delete the whole index and start over.
	st.Close()
	if err := afs.RemoveAll(appfs.DirIndex); err != nil {
		t.Fatal(err)
	}
	if err := afs.MkdirAll(appfs.DirIndex); err != nil {
		t.Fatal(err)
	}
	st = open()
	runAll(st)
	st.Close()

	compare(t, "user folder", before, snapshot(t, user))
	compare(t, "outside folder", beforeOut, snapshot(t, outside))
}

// TestUserCanChangeFilesWhileIndexing: other programs must be able to edit, rename and
// delete files while Findly is indexing them (I-2), and indexing must not fail because
// of it.
func TestUserCanChangeFilesWhileIndexing(t *testing.T) {
	dir := t.TempDir()
	var files []string
	for i := range 300 {
		p := filepath.Join(dir, fmt.Sprintf("note-%03d.txt", i))
		os.WriteFile(p, []byte(strings.Repeat(fmt.Sprintf("note %d about many things. ", i), 200)), 0o644)
		files = append(files, p)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	real, _ := safefs.Canonical(dir)
	root, _ := st.AddRoot(ctx, real)

	stop := make(chan struct{})
	var wg sync.WaitGroup
	var userErrs []error
	var mu sync.Mutex
	opCount := 0
	wg.Add(1)
	go func() { // the "user": keeps editing, renaming and deleting files
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			p := files[i%len(files)]
			var err error
			switch i % 3 {
			case 0:
				err = os.WriteFile(p, []byte(fmt.Sprintf("edited %d", i)), 0o644)
			case 1:
				if err = os.Rename(p, p+".renamed"); err == nil {
					err = os.Rename(p+".renamed", p)
				}
			case 2:
				if err = os.Remove(p); err == nil {
					err = os.WriteFile(p, []byte("recreated"), 0o644)
				}
			}
			mu.Lock()
			opCount++
			if err != nil {
				userErrs = append(userErrs, err)
			}
			mu.Unlock()
		}
	}()

	ix := &index.Indexer{Store: st, Embedder: fakeEmbedder{}, Licensed: func() bool { return true }}
	for run := range 3 {
		mu.Lock()
		ops := opCount
		mu.Unlock()
		_, err := ix.IndexRoot(ctx, root)
		mu.Lock()
		during := opCount - ops
		mu.Unlock()
		if during == 0 {
			t.Errorf("run %d: no user operation overlapped with indexing; the test proved nothing", run)
		}
		if err != nil {
			close(stop)
			wg.Wait()
			t.Fatalf("indexing failed while files changed: %v", err)
		}
	}
	close(stop)
	wg.Wait()
	for _, err := range userErrs {
		t.Errorf("Findly blocked the user's program: %v", err)
	}
}

// assertNoOpenHandles opens every regular file with share mode 0 (exclusive). That fails
// with a sharing violation if anything (Findly) still holds the file open.
func assertNoOpenHandles(t *testing.T, dir string) {
	t.Helper()
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return nil
		}
		if attrs(t, p)&windows.FILE_ATTRIBUTE_OFFLINE != 0 {
			return nil
		}
		// Long-path prefix (two backslashes, '?', backslash), built from bytes so no editor
		// or tool can drop a backslash from it.
		prefix := string([]byte{'\\', '\\', '?', '\\'})
		p16, _ := windows.UTF16PtrFromString(prefix + p)
		h, err := windows.CreateFile(p16, windows.GENERIC_READ, 0, nil, windows.OPEN_EXISTING, 0, 0)
		if err != nil {
			t.Errorf("file still open after indexing (%v): %s", err, p)
			return nil
		}
		windows.CloseHandle(h)
		return nil
	})
}
