//go:build windows

// Package index runs the indexing pipeline (plan 9.10):
//
//	scan → (changed files, by priority) → extract → chunk → embed → store
//
// Extraction runs on several workers; embedding is a single worker because llama-server
// is the bottleneck and batches internally. A failing file is marked failed and never
// stops the pipeline. Phase 1 handles text, documents and PDFs; images, audio and video
// stay pending for later phases (their file names are searchable already).
package index

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sanskarmishra479/findly/internal/chunk"
	"github.com/sanskarmishra479/findly/internal/embed"
	"github.com/sanskarmishra479/findly/internal/extract"
	"github.com/sanskarmishra479/findly/internal/scan"
	"github.com/sanskarmishra479/findly/internal/store"
)

// FreeLimit is the number of files whose content is indexed without a license.
const FreeLimit = 300

// Embedder turns passages into vectors. *embed.Server implements it.
type Embedder interface {
	EmbedPassages(ctx context.Context, title string, texts []string) ([]embed.Vector, error)
}

// Progress is reported while a root is indexed.
type Progress struct {
	Root    string
	Total   int // files that need work in this run
	Done    int // finished (indexed, failed or skipped)
	Failed  int
	Skipped int // over the free-tier limit
}

// Indexer indexes roots into a store.
type Indexer struct {
	Store    *store.Store
	Embedder Embedder
	// Licensed reports whether the full version is active. nil means unlicensed.
	Licensed func() bool
	// OnProgress, if set, is called after each file. It must return quickly.
	OnProgress func(Progress)
	// Workers is the number of extraction workers; 0 = half the CPU cores (at least 1).
	Workers int
	// Now is the clock (tests replace it).
	Now func() time.Time
}

// Stats summarize one run over a root.
type Stats struct {
	Scan     scan.Stats
	Indexed  int
	Failed   int
	Skipped  int
	Deleted  int // removed from the index because they no longer exist
	Thumbs   []string
	Deferred int // PDFs with pages left for a later pass
}

// ErrEmbedder means the embedding server is not working; the run stops instead of
// marking every remaining file as failed.
var ErrEmbedder = errors.New("index: embedding server failed")

// job is one file that needs (re)indexing.
type job struct {
	id   int64
	file scan.File
	prio int // lower runs first
}

// IndexRoot scans root, removes files that disappeared, and indexes new or changed files.
func (ix *Indexer) IndexRoot(ctx context.Context, root store.Root) (Stats, error) {
	var st Stats
	excludes, err := ix.Store.Excludes(ctx, root.ID)
	if err != nil {
		return st, err
	}

	// 1. Scan: record every file; collect the ones that need work.
	seen := map[string]bool{}
	var jobs []job
	now := ix.now()
	st.Scan, err = scan.Root(ctx, root.Path, scan.Options{Excludes: excludes}, func(f scan.File) error {
		seen[strings.ToUpper(f.Path)] = true
		id, changed, err := ix.Store.UpsertFile(ctx, store.File{
			RootID: root.ID, Path: f.Path, Name: f.Name, Ext: f.Ext, Kind: f.Kind, Size: f.Size, MTime: f.ModTime,
		})
		if err != nil {
			return err
		}
		if changed {
			jobs = append(jobs, job{id: id, file: f, prio: priority(f, now)})
		}
		return nil
	})
	if err != nil {
		// Includes scan.ErrBadRoot (unplugged drive): never treat that as "all deleted".
		return st, err
	}

	// 2. Files recorded before but not found now were deleted (P-5). Only after a
	// complete scan; folders that could not be read keep their files.
	if st.Scan.Errors == 0 {
		known, err := ix.Store.FilesInRoot(ctx, root.ID)
		if err != nil {
			return st, err
		}
		for _, p := range known {
			if seen[strings.ToUpper(p)] {
				continue
			}
			found, thumbs, err := ix.Store.DeleteFile(ctx, p)
			if err != nil {
				return st, err
			}
			if found {
				st.Deleted++
				st.Thumbs = append(st.Thumbs, thumbs...)
			}
		}
	}

	// 3. Index changed files, most important first.
	sort.SliceStable(jobs, func(i, j int) bool { return jobs[i].prio < jobs[j].prio })
	err = ix.run(ctx, root.Path, jobs, &st)
	return st, err
}

// priority follows plan 9.10: recent files, then the main user folders, then documents.
// Images, audio and video are not processed in Phase 1.
func priority(f scan.File, now time.Time) int {
	p := 0
	if now.Sub(time.Unix(0, f.ModTime)) > 30*24*time.Hour {
		p += 100
	}
	lower := strings.ToLower(f.Path)
	if !strings.Contains(lower, `\desktop\`) && !strings.Contains(lower, `\documents\`) && !strings.Contains(lower, `\downloads\`) {
		p += 10
	}
	switch f.Kind {
	case scan.KindText, scan.KindDoc, scan.KindPDF:
	default:
		p += 1
	}
	return p
}

// extracted is the result of the extract stage for one file.
type extracted struct {
	job  job
	doc  extract.Doc
	hash string
	err  error
}

func (ix *Indexer) run(ctx context.Context, rootPath string, jobs []job, st *Stats) error {
	prog := Progress{Root: rootPath, Total: len(jobs)}
	report := func() {
		if ix.OnProgress != nil {
			ix.OnProgress(prog)
		}
	}

	count, err := ix.Store.CountContentIndexed(ctx)
	if err != nil {
		return err
	}
	var used, feederSkipped atomic.Int64 // shared by the feeder and the store stage
	used.Store(int64(count))
	licensed := ix.Licensed != nil && ix.Licensed()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Stage 1: extraction workers.
	todo := make(chan job)
	results := make(chan extracted)
	var wg sync.WaitGroup
	for range ix.workers() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range todo {
				r := extracted{job: j}
				r.doc, r.err = extract.File(j.file.Path)
				if r.err == nil {
					r.hash = contentHash(j.file.Kind, r.doc)
				}
				select {
				case results <- r:
				case <-ctx.Done():
					return
				}
			}
		}()
	}
	go func() {
		defer close(todo)
		for _, j := range jobs {
			if j.file.Kind == scan.KindOther {
				// Nothing to read: the file-name row is all there is.
				ix.Store.SetFileResult(ctx, j.id, store.StatusDone, "", "", "")
				continue
			}
			if !handled(j.file.Kind) {
				continue // images, audio, video: pending for a later phase
			}
			if !licensed && used.Load() >= FreeLimit {
				feederSkipped.Add(1)
				// Mark the rest skipped without reading them; already indexed files stay
				// searchable (L-3).
				ix.Store.SetFileResult(ctx, j.id, store.StatusSkipped, "", "", "free version limit reached")
				continue
			}
			select {
			case todo <- j:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() { wg.Wait(); close(results) }()

	// Stage 2: embed and store, one file at a time.
	for r := range results {
		status, err := ix.store(ctx, r, licensed, &used)
		if err != nil {
			cancel()
			for range results { // let the workers finish
			}
			return err
		}
		prog.Done++
		switch status {
		case store.StatusDone, store.StatusPartial:
			st.Indexed++
			if status == store.StatusPartial {
				st.Deferred++
			}
		case store.StatusFailed:
			st.Failed++
			prog.Failed++
		case store.StatusSkipped:
			st.Skipped++
			prog.Skipped++
		}
		report()
	}
	st.Skipped += int(feederSkipped.Load())
	return ctx.Err()
}

// store handles one extracted file and returns the status it recorded.
func (ix *Indexer) store(ctx context.Context, r extracted, licensed bool, used *atomic.Int64) (string, error) {
	id := r.job.id
	fail := func(msg string) (string, error) {
		return store.StatusFailed, ix.Store.SetFileResult(ctx, id, store.StatusFailed, "", "", msg)
	}
	if r.err != nil {
		if errors.Is(r.err, extract.ErrPDFBusy) {
			return store.StatusPending, nil // not the file's fault: retried on the next run
		}
		return fail(r.err.Error())
	}
	if !licensed && used.Load() >= FreeLimit {
		return store.StatusSkipped, ix.Store.SetFileResult(ctx, id, store.StatusSkipped, "", "", "free version limit reached")
	}

	status := store.StatusDone
	if r.doc.MorePages {
		status = store.StatusPartial
	}

	// Identical content already indexed: copy instead of embedding again.
	if r.hash != "" && status == store.StatusDone {
		if src, ok, err := ix.Store.FileWithContent(ctx, r.hash, id); err != nil {
			return "", err
		} else if ok {
			if err := ix.Store.CopyContent(ctx, src, id); err != nil {
				return "", err
			}
			used.Add(1)
			return status, ix.Store.SetFileResult(ctx, id, status, "", r.hash, "")
		}
	}

	items, err := ix.items(ctx, r)
	if err != nil {
		if errors.Is(err, ErrEmbedder) {
			return "", err
		}
		return fail(err.Error())
	}
	if err := ix.Store.ReplaceContent(ctx, id, items); err != nil {
		return "", err
	}
	if len(items) > 0 {
		used.Add(1)
	}
	return status, ix.Store.SetFileResult(ctx, id, status, "", r.hash, "")
}

// items chunks the document and embeds the passages. Mostly-numeric text (data
// exports) is stored for keyword search only (plan 9.3).
func (ix *Indexer) items(ctx context.Context, r extracted) ([]store.Item, error) {
	sections := make([]chunk.Section, len(r.doc.Sections))
	for i, s := range r.doc.Sections {
		sections[i] = chunk.Section{Page: s.Page, Text: s.Text}
	}
	chunks := chunk.Split(sections)
	if len(chunks) == 0 {
		return nil, nil
	}
	items := make([]store.Item, len(chunks))
	var toEmbed []string
	var embedIdx []int
	for i, c := range chunks {
		items[i] = store.Item{Kind: store.ItemPassage, Locator: locator(c), Text: c.Text}
		if !extract.MostlyNumeric(c.Text) {
			toEmbed = append(toEmbed, c.Text)
			embedIdx = append(embedIdx, i)
		}
	}
	if len(toEmbed) > 0 {
		vecs, err := ix.Embedder.EmbedPassages(ctx, r.job.file.Name, toEmbed)
		if err != nil {
			if errors.Is(err, embed.ErrServer) || errors.Is(err, context.Canceled) {
				return nil, fmt.Errorf("%w: %v", ErrEmbedder, err)
			}
			return nil, err
		}
		if len(vecs) != len(toEmbed) {
			return nil, fmt.Errorf("%w: got %d vectors for %d passages", ErrEmbedder, len(vecs), len(toEmbed))
		}
		for k, i := range embedIdx {
			v := vecs[k]
			items[i].Vector = &store.Vector{Bin: v.Bin, I8: v.Q.I8, Scale: v.Q.Scale}
		}
	}
	return items, nil
}

func locator(c chunk.Chunk) string {
	m := map[string]int{"chunk": c.Index}
	if c.Page > 0 {
		m["page"] = c.Page
	}
	b, _ := json.Marshal(m)
	return string(b)
}

// contentHash identifies identical extracted content (for dedupe), not raw bytes, so
// it costs no extra file read.
func contentHash(kind string, d extract.Doc) string {
	if len(d.Sections) == 0 || d.MorePages {
		return ""
	}
	h := sha256.New()
	h.Write([]byte(kind + "\x00"))
	for _, s := range d.Sections {
		fmt.Fprintf(h, "%d\x00%s\x00", s.Page, s.Text)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// handled reports the kinds this phase extracts. "other" files need nothing beyond
// their name row, which UpsertFile already created.
func handled(kind string) bool {
	switch kind {
	case scan.KindText, scan.KindDoc, scan.KindPDF:
		return true
	}
	return false
}

func (ix *Indexer) workers() int {
	if ix.Workers > 0 {
		return ix.Workers
	}
	return max(1, runtime.NumCPU()/2)
}

func (ix *Indexer) now() time.Time {
	if ix.Now != nil {
		return ix.Now()
	}
	return time.Now()
}
