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
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sanskarmishra479/findly/internal/chunk"
	"github.com/sanskarmishra479/findly/internal/embed"
	"github.com/sanskarmishra479/findly/internal/extract"
	"github.com/sanskarmishra479/findly/internal/safefs"
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
	Total   int // files this run will process (text, documents, PDFs)
	Done    int // finished: indexed, failed, skipped or postponed
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
	Scan      scan.Stats
	Indexed   int
	Failed    int
	Skipped   int      // over the free-tier limit
	Postponed int      // PDF engine busy: retried next run
	Deleted   int      // removed from the index because they no longer exist
	Thumbs    []string // thumbnails of deleted files, for the caller to remove
	Deferred  int      // PDFs with pages left for a later pass (Phase 4)
	Timing    Timing   // where the time went
}

// Timing splits a run's time by stage. Extract is summed over the parallel workers, so
// it can exceed Total.
type Timing struct {
	Extract time.Duration // reading files and pulling out text

	Save  time.Duration // chunking and database writes
	Total time.Duration // the whole store stage, from first job to last
}

// ErrEmbedder means the embedding server is not working; the run stops instead of
// marking every remaining file as failed.
var ErrEmbedder = errors.New("index: embedding server failed")

// job is one file that needs (re)indexing.
type job struct {
	id         int64
	file       scan.File
	prio       int  // lower runs first
	hadContent bool // already counted in the free-tier total
}

// AddRoot saves a folder the user chose. Credential folders are refused here, before
// they are saved: a stored one would fail every later index run and block adding its
// parent ("folders overlap").
func (ix *Indexer) AddRoot(ctx context.Context, path string) (store.Root, error) {
	// The real path: "." or a short name like PASSWO~1 must not get past the check.
	real, err := safefs.Canonical(path)
	if err != nil {
		return store.Root{}, fmt.Errorf("%w: %v", scan.ErrBadRoot, err)
	}
	path = real
	if scan.SecretFolder(filepath.Clean(path)) {
		return store.Root{}, fmt.Errorf("%w: %s", scan.ErrSecretRoot, path)
	}
	return ix.Store.AddRoot(ctx, path)
}

// IndexRoot scans root, removes files that disappeared, and indexes new or changed files.
func (ix *Indexer) IndexRoot(ctx context.Context, root store.Root) (Stats, error) {
	var st Stats
	excludes, err := ix.Store.Excludes(ctx, root.ID)
	if err != nil {
		return st, err
	}

	// 1. Scan: record every file; collect the ones that need work in this phase.
	seen := map[string]bool{}
	var jobs []job
	now := ix.now()
	st.Scan, err = scan.Root(ctx, root.Path, scan.Options{Excludes: excludes}, func(f scan.File) error {
		seen[safefs.PathKey(f.Path)] = true
		id, changed, err := ix.Store.UpsertFile(ctx, store.File{
			RootID: root.ID, Path: f.Path, Name: f.Name, Ext: f.Ext, Kind: f.Kind, Size: f.Size, MTime: f.ModTime,
		})
		if err != nil || !changed {
			return err
		}
		switch {
		case f.Kind == scan.KindOther:
			// Nothing to read: the file-name row is all there is.
			return ix.Store.SetFileResult(ctx, id, store.StatusDone, "", "", "")
		case !handled(f.Kind):
			return nil // images, audio, video: pending for a later phase
		}
		had, err := ix.Store.HasContent(ctx, id)
		if err != nil {
			return err
		}
		jobs = append(jobs, job{id: id, file: f, prio: priority(f, now), hadContent: had})
		return nil
	})
	if err != nil {
		// Includes scan.ErrBadRoot (unplugged drive): never treat that as "all deleted".
		return st, err
	}

	// 2. Files recorded before but not found now were deleted (P-5). Files inside
	// folders that could not be read this time are kept.
	if err := ix.removeDeleted(ctx, root.ID, seen, st.Scan, &st); err != nil {
		return st, err
	}

	// Files skipped earlier by the free-tier limit are queued again only when they can
	// actually be indexed (license, or free slots), not re-written on every scan.
	if err := ix.queueSkipped(ctx, root.ID, now, &jobs); err != nil {
		return st, err
	}

	// 3. Index changed files, most important first.
	sort.SliceStable(jobs, func(i, j int) bool { return jobs[i].prio < jobs[j].prio })
	err = ix.run(ctx, root.Path, jobs, &st)
	return st, err
}

func (ix *Indexer) removeDeleted(ctx context.Context, rootID int64, seen map[string]bool, sc scan.Stats, st *Stats) error {
	if sc.Errors > len(sc.FailedDirs) {
		return nil // too many unreadable folders to know which files are really gone
	}
	failed := make([]string, len(sc.FailedDirs))
	for i, d := range sc.FailedDirs {
		// Same key form as the paths it is compared with (Windows case rules).
		failed[i] = strings.TrimSuffix(safefs.PathKey(d), `\`) + `\`
	}
	known, err := ix.Store.FilesInRoot(ctx, rootID)
	if err != nil {
		return err
	}
	for _, p := range known {
		key := safefs.PathKey(p)
		if seen[key] || underAny(key, failed) {
			continue
		}
		found, thumbs, err := ix.Store.DeleteFile(ctx, p)
		if err != nil {
			return err
		}
		if found {
			st.Deleted++
			st.Thumbs = append(st.Thumbs, thumbs...)
		}
	}
	return nil
}

func underAny(key string, dirs []string) bool {
	for _, d := range dirs {
		if strings.HasPrefix(key, d) {
			return true
		}
	}
	return false
}

// priority follows plan 9.10: recent files, then the main user folders.
func priority(f scan.File, now time.Time) int {
	p := 0
	if now.Sub(time.Unix(0, f.ModTime)) > 30*24*time.Hour {
		p += 100
	}
	lower := strings.ToLower(f.Path)
	if !strings.Contains(lower, `\desktop\`) && !strings.Contains(lower, `\documents\`) && !strings.Contains(lower, `\downloads\`) {
		p += 10
	}
	return p
}

// extracted is the result of the extract stage for one file.
type extracted struct {
	job  job
	doc  extract.Doc
	hash string
	err  error
	took time.Duration // extraction time
}

// freeTier tracks how many files have content, shared by the feeder and the store stage.
type freeTier struct {
	licensed bool
	used     atomic.Int64
}

// allows reports whether j may get (new) content. Files that already had content keep
// their slot: re-indexing an edited file never pushes it out (plan L-3).
func (f *freeTier) allows(j job) bool {
	return f.licensed || j.hadContent || f.used.Load() < FreeLimit
}

func (ix *Indexer) run(ctx context.Context, rootPath string, jobs []job, st *Stats) error {
	prog := Progress{Root: rootPath, Total: len(jobs)}
	finish := func(status string) {
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
		case store.StatusPending:
			st.Postponed++
		}
		if ix.OnProgress != nil {
			ix.OnProgress(prog)
		}
	}

	count, err := ix.Store.CountContentIndexed(ctx)
	if err != nil {
		return err
	}
	tier := &freeTier{licensed: ix.Licensed != nil && ix.Licensed()}
	tier.used.Store(int64(count))

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
				if ctx.Err() != nil {
					return // cancelled: don't start another (possibly slow) extraction
				}
				r := extracted{job: j}
				start := time.Now()
				r.doc, r.err = extract.File(j.file.Path)
				r.took = time.Since(start)
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
	// The feeder skips files over the free limit without reading them. Skips are
	// reported to the store stage through results so progress stays in one goroutine.
	// It is part of wg: results must not close while the feeder can still send.
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(todo)
		for _, j := range jobs {
			if !tier.allows(j) {
				select {
				case results <- extracted{job: j, err: errOverLimit}:
				case <-ctx.Done():
					return
				}
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

	// Stage 2: store, one file at a time. Passages are keyword-searchable at once; their
	// vectors are added afterwards by EmbedPending.
	stage2 := time.Now()
	defer func() { st.Timing.Total = time.Since(stage2) }()
	var runErr error
	for r := range results {
		if runErr != nil {
			continue // draining after a fatal error so the workers can exit
		}
		st.Timing.Extract += r.took
		start := time.Now()
		status, err := ix.save(ctx, r, tier)
		st.Timing.Save += time.Since(start)
		if err != nil {
			runErr = err
			cancel()
			continue
		}
		finish(status)
	}
	if runErr != nil {
		return runErr
	}
	return ctx.Err()
}

var errOverLimit = errors.New("free version limit reached")

// save stores one extracted file and returns the status it recorded. Its passages are
// keyword-searchable as soon as this returns; vectors come later (EmbedPending). (Not
// named "store": that is the package it writes to.)
func (ix *Indexer) save(ctx context.Context, r extracted, tier *freeTier) (string, error) {
	id := r.job.id
	set := func(status, hash, msg string) (string, error) {
		return status, ix.Store.SetFileResult(ctx, id, status, "", hash, msg)
	}
	switch {
	case errors.Is(r.err, errOverLimit):
		return set(store.StatusSkipped, "", errOverLimit.Error())
	case errors.Is(r.err, extract.ErrPDFBusy):
		return store.StatusPending, nil // not the file's fault: retried on the next run
	case r.err != nil:
		return set(store.StatusFailed, "", r.err.Error())
	}
	if !tier.allows(r.job) {
		// The limit was reached while this file was being extracted.
		return set(store.StatusSkipped, "", errOverLimit.Error())
	}

	status := store.StatusDone
	if r.doc.MorePages {
		status = store.StatusPartial // remaining pages: deferred job (Phase 4)
	}

	// Identical content already indexed: copy it (with its vectors, or still waiting for
	// them) instead of embedding it again.
	if r.hash != "" {
		copied, err := ix.Store.CopyDuplicate(ctx, r.hash, status, id, false)
		if err != nil {
			return "", err
		}
		if copied > 0 {
			ix.account(tier, r.job, true)
			return set(status, r.hash, "")
		}
	}

	items := passages(r.doc)
	if err := ix.Store.ReplaceContent(ctx, id, items); err != nil {
		return "", err
	}
	ix.account(tier, r.job, len(items) > 0)
	return set(status, r.hash, "")
}

// account updates the free-tier count after a file's content was replaced.
func (ix *Indexer) account(tier *freeTier, j job, hasContent bool) {
	switch {
	case hasContent && !j.hadContent:
		tier.used.Add(1)
	case !hasContent && j.hadContent:
		tier.used.Add(-1)
	}
}

// passages chunks the document into passages that wait for their vectors.
// Mostly-numeric text (data exports) is stored for keyword search only (plan 9.3).
func passages(d extract.Doc) []store.Item {
	sections := make([]chunk.Section, len(d.Sections))
	for i, s := range d.Sections {
		sections[i] = chunk.Section{Page: s.Page, Text: s.Text}
	}
	chunks := chunk.Split(sections)
	items := make([]store.Item, len(chunks))
	for i, c := range chunks {
		items[i] = store.Item{
			Kind: store.ItemPassage, Locator: locator(c), Text: c.Text,
			Pending: !extract.MostlyNumeric(c.Text),
		}
	}
	return items
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
	// Partial PDFs (first 30 pages) get a hash too: CopyDuplicate only copies between
	// files with the same status, so a partial copy is never mixed with a complete one.
	// Two PDFs that differ only after page 30 share it until the deferred pass (Phase 4)
	// reads the rest of each file separately.
	if len(d.Sections) == 0 {
		return ""
	}
	h := sha256.New()
	h.Write([]byte(kind + "\x00"))
	for _, s := range d.Sections {
		fmt.Fprintf(h, "%d\x00%s\x00", s.Page, s.Text)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// handled reports the kinds this phase extracts.
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

// queueSkipped adds files that were skipped by the free-tier limit, as many as there is
// room for now (all of them with a license).
func (ix *Indexer) queueSkipped(ctx context.Context, rootID int64, now time.Time, jobs *[]job) error {
	room := 1 << 30
	if ix.Licensed == nil || !ix.Licensed() {
		used, err := ix.Store.CountContentIndexed(ctx)
		if err != nil {
			return err
		}
		room = FreeLimit - used
		// Files this scan already queued that would use a slot (new, or not yet indexed
		// with content) take room first.
		for _, j := range *jobs {
			if !j.hadContent {
				room--
			}
		}
	}
	if room <= 0 {
		return nil
	}
	skipped, err := ix.Store.SkippedFiles(ctx, rootID, room)
	if err != nil {
		return err
	}
	queued := make(map[int64]bool, len(*jobs))
	for _, j := range *jobs {
		queued[j.id] = true
	}
	for _, f := range skipped {
		if queued[f.ID] {
			continue
		}
		sf := scan.File{Path: f.Path, Name: f.Name, Ext: f.Ext, Kind: f.Kind, Size: f.Size, ModTime: f.MTime}
		*jobs = append(*jobs, job{id: f.ID, file: sf, prio: priority(sf, now)})
	}
	return nil
}
