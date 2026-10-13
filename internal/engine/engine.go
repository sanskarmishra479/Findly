// Package engine runs Findly's index in the background for the app (plan section 5):
// it owns the index database, indexes the chosen folders, adds vectors, manages the
// embedding model's lifetime and reports progress.
//
// Work order: new and changed files are stored first (keyword search works at once);
// adding vectors runs when no folder is waiting, and gives way whenever one is.
package engine

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"github.com/sanskarmishra479/findly/internal/appfs"
	"github.com/sanskarmishra479/findly/internal/embed"
	"github.com/sanskarmishra479/findly/internal/extract"
	"github.com/sanskarmishra479/findly/internal/index"
	"github.com/sanskarmishra479/findly/internal/search"
	"github.com/sanskarmishra479/findly/internal/store"
)

// Model idle times (plan section 5; user decision 2026-10-13: never kept loaded for good).
const (
	DefaultIndexIdle  = 60 * time.Second
	DefaultSearchIdle = 2 * time.Minute
)

// State is what the engine is doing.
type State string

const (
	StateIdle      State = "idle"      // everything indexed
	StateIndexing  State = "indexing"  // reading files (keyword search grows)
	StateEmbedding State = "embedding" // adding vectors (meaning search grows)
	StatePaused    State = "paused"
	StateError     State = "error" // see Status.Err; retried on the next trigger
)

// Status is reported to the UI (tray tooltip, progress).
type Status struct {
	State State  `json:"state"`
	Root  string `json:"root"`  // folder being indexed
	Done  int    `json:"done"`  // files finished in this folder
	Total int    `json:"total"` // files to process in this folder
	// Embedding progress: passages that got vectors so far, out of those waiting.
	Passages int    `json:"passages"`
	Waiting  int    `json:"waiting"`
	Model    bool   `json:"model"` // the embedding model is loaded
	Err      string `json:"err"`   // last error, for StateError
}

// Config configures an Engine.
type Config struct {
	FS         *appfs.FS
	StartModel StartModel // nil: keyword search only (e.g. model not downloaded yet)
	Licensed   func() bool
	IndexIdle  time.Duration // 0 = DefaultIndexIdle
	SearchIdle time.Duration // 0 = DefaultSearchIdle
	OnStatus   func(Status)  // must return quickly
	Workers    int           // extraction workers; 0 = index default

	// For tests: how often to check whether the model is idle, and how long to wait
	// before retrying after an error. 0 = 5 s and 5 min.
	idleTick, retryAfter time.Duration
}

// Engine is the app's indexing and search backend.
type Engine struct {
	cfg   Config
	store *store.Store
	cache *search.VectorCache
	pool  *modelPool

	ctx    context.Context
	cancel context.CancelFunc
	wake   chan struct{} // there is new work
	done   chan struct{} // the worker ended

	mu      sync.Mutex
	queue   []int64 // root ids waiting to be indexed
	all     bool    // index every root
	paused  bool
	runStop context.CancelFunc // stops the current run (pause, or new folder work during embedding)
	// embedding is true while the current run adds vectors; new folder work interrupts
	// only that (tracked here, not via the displayed status, which can lag).
	embedding bool
	status    Status
	// dirty: files were stored or removed since the vector cache was last loaded (an
	// identical file can be copied with its vectors, leaving nothing to embed).
	dirty bool
	// Errors: how often the embedding pass stopped on each file, and the next retry delay.
	stuck      map[int64]int
	retryDelay time.Duration
}

// Open opens (or creates) the index and starts the background worker. The index must
// belong to the model the app uses (store.ErrModelChanged otherwise).
func Open(cfg Config) (*Engine, error) {
	if cfg.IndexIdle == 0 {
		cfg.IndexIdle = DefaultIndexIdle
	}
	if cfg.SearchIdle == 0 {
		cfg.SearchIdle = DefaultSearchIdle
	}
	if cfg.idleTick == 0 {
		cfg.idleTick = 5 * time.Second
	}
	if cfg.retryAfter == 0 {
		cfg.retryAfter = 5 * time.Minute
	}
	dbPath, err := cfg.FS.Path(filepath.Join(appfs.DirIndex, "index.db"))
	if err != nil {
		return nil, err
	}
	st, err := store.Open(dbPath)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := st.EnsureModel(ctx, embed.ModelVersion); err != nil {
		cancel()
		st.Close()
		return nil, err
	}
	e := &Engine{
		cfg:    cfg,
		store:  st,
		cache:  &search.VectorCache{},
		ctx:    ctx,
		cancel: cancel,
		wake:   make(chan struct{}, 1),
		done:   make(chan struct{}),
		status: Status{State: StateIdle},
	}
	e.pool = newModelPool(cfg.StartModel)
	e.stuck = map[int64]int{}
	e.retryDelay = cfg.retryAfter
	if err := e.cache.Load(ctx, st); err != nil {
		cancel()
		st.Close()
		return nil, err
	}
	go e.worker()
	go e.idleLoop()
	return e, nil
}

// Close stops all work, the model and the database. Interrupted work continues on the
// next start.
func (e *Engine) Close() {
	e.cancel()
	e.pool.close() // also cancels a model start in progress, so quitting never waits for it
	<-e.done
	extract.ClosePDF()
	e.store.Close()
}

// Store gives search access to the index.
func (e *Engine) Store() *store.Store { return e.store }

// Cache is the in-memory vector cache used for meaning search.
func (e *Engine) Cache() *search.VectorCache { return e.cache }

// Status returns the current status.
func (e *Engine) Status() Status {
	e.mu.Lock()
	defer e.mu.Unlock()
	s := e.status
	s.Model = e.pool.running()
	return s
}

// IndexAll queues every chosen folder: unchanged files are skipped quickly, so this is
// cheap. Called at startup and on Resume.
func (e *Engine) IndexAll() {
	e.mu.Lock()
	e.all = true
	e.mu.Unlock()
	e.kick()
}

// AddFolder adds a folder (refusing credential folders and overlaps) and indexes it.
func (e *Engine) AddFolder(ctx context.Context, path string) (store.Root, error) {
	ix := &index.Indexer{Store: e.store}
	root, err := ix.AddRoot(ctx, path)
	if err != nil {
		return store.Root{}, err
	}
	e.mu.Lock()
	e.queue = append(e.queue, root.ID)
	e.mu.Unlock()
	e.kick()
	return root, nil
}

// Pause stops indexing (and adding vectors) until Resume.
func (e *Engine) Pause() {
	e.mu.Lock()
	e.paused = true
	if e.runStop != nil {
		e.runStop()
	}
	e.mu.Unlock()
	e.setStatus(Status{State: StatePaused})
}

// Resume continues after Pause.
func (e *Engine) Resume() {
	e.mu.Lock()
	e.paused = false
	e.mu.Unlock()
	e.IndexAll()
}

// QueryModel returns the model for embedding a search query, starting it if needed, and
// a function to call when done. It is kept loaded SearchIdle after the last search.
func (e *Engine) QueryModel(ctx context.Context) (Model, func(), error) {
	if e.cfg.StartModel == nil {
		return nil, nil, ErrNoModel
	}
	return e.pool.get(ctx, e.cfg.SearchIdle, false)
}

// WarmUp starts loading the model in the background (e.g. when the search panel opens),
// so the first meaning search does not wait for it.
func (e *Engine) WarmUp() {
	if e.cfg.StartModel != nil {
		e.pool.startAsync(e.cfg.SearchIdle, false)
	}
}

// ErrNoModel means no embedding model is available: keyword search only.
var ErrNoModel = errors.New("engine: no embedding model available")

func (e *Engine) kick() {
	select {
	case e.wake <- struct{}{}:
	default: // a wake-up is already pending
	}
	// New folder work interrupts adding vectors, so new files become searchable first.
	e.mu.Lock()
	if e.runStop != nil && e.embedding {
		e.runStop()
	}
	e.mu.Unlock()
}

func (e *Engine) setStatus(s Status) {
	e.mu.Lock()
	if e.paused && s.State != StatePaused {
		// A run winding down after Pause still reports progress; "paused" must stay.
		e.mu.Unlock()
		return
	}
	e.status = s
	e.mu.Unlock()
	e.notify(s)
}

// notify reports a status to the UI without storing it.
func (e *Engine) notify(s Status) {
	if e.cfg.OnStatus != nil {
		s.Model = e.pool.running()
		e.cfg.OnStatus(s)
	}
}

// worker does all indexing work, one thing at a time.
func (e *Engine) worker() {
	defer close(e.done)
	for {
		select {
		case <-e.ctx.Done():
			return
		case <-e.wake:
		}
		for e.ctx.Err() == nil && e.step() {
		}
	}
}

// step does one unit of work and reports whether more may be waiting.
func (e *Engine) step() bool {
	e.mu.Lock()
	if e.paused {
		e.mu.Unlock()
		return false
	}
	var roots []int64
	all := e.all
	if !all && len(e.queue) > 0 {
		roots = []int64{e.queue[0]}
		e.queue = e.queue[1:]
	}
	e.all = false
	runCtx, stop := context.WithCancel(e.ctx)
	e.runStop = stop
	e.embedding = !all && len(roots) == 0 // set before the run starts, so kick sees it
	e.mu.Unlock()
	defer func() {
		stop()
		e.mu.Lock()
		e.runStop = nil
		e.embedding = false
		e.mu.Unlock()
	}()

	switch {
	case all || len(roots) > 0:
		err := e.indexRoots(runCtx, all, roots)
		return e.handle(err)
	default:
		err := e.embedPending(runCtx)
		e.handle(err)
		if err == nil {
			e.setStatus(Status{State: StateIdle})
		}
		return false
	}
}

// handle records an error. Interruptions (pause, new folder work, quit) are not errors.
// It reports whether the worker should continue.
func (e *Engine) handle(err error) bool {
	switch {
	case err == nil:
		return true
	case errors.Is(err, context.Canceled):
		e.mu.Lock()
		paused := e.paused
		e.mu.Unlock()
		return !paused && e.ctx.Err() == nil // new work arrived: go on with it
	default:
		if errors.Is(err, index.ErrEmbedder) {
			e.pool.drop() // a broken model: the next attempt starts a fresh one
		}
		e.setStatus(Status{State: StateError, Err: err.Error()})
		// Try again later (the cause may be temporary: a busy disk, a GPU driver reset),
		// waiting longer after each failure in a row: 5 min, 10, 20, ... up to 1 hour.
		e.mu.Lock()
		delay := e.retryDelay
		e.retryDelay = min(2*e.retryDelay, maxRetryDelay)
		e.mu.Unlock()
		time.AfterFunc(delay, func() {
			if e.ctx.Err() == nil {
				e.IndexAll()
			}
		})
		return false
	}
}

func (e *Engine) indexRoots(ctx context.Context, all bool, ids []int64) error {
	roots, err := e.store.Roots(ctx)
	if err != nil {
		return err
	}
	want := map[int64]bool{}
	for _, id := range ids {
		want[id] = true
	}
	for _, r := range roots {
		if !r.Enabled || !(all || want[r.ID]) {
			continue
		}
		if err := e.indexRoot(ctx, r); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) indexRoot(ctx context.Context, r store.Root) error {
	started := false
	ix := &index.Indexer{
		Store:    e.store,
		Licensed: e.cfg.Licensed,
		Workers:  e.cfg.Workers,
		OnProgress: func(p index.Progress) {
			// The first stored file means vectors will be needed: start the model now, in
			// parallel with reading the remaining files.
			// (Only files actually stored count: failed or over-limit files need no model.)
			if !started && p.Done-p.Failed-p.Skipped-p.Postponed > 0 && e.cfg.StartModel != nil {
				started = true
				e.pool.startAsync(e.cfg.IndexIdle, true)
			}
			e.setStatus(Status{State: StateIndexing, Root: r.Path, Done: p.Done, Total: p.Total})
		},
	}
	e.setStatus(Status{State: StateIndexing, Root: r.Path})
	st, err := ix.IndexRoot(ctx, r)
	e.removeThumbs(st.Thumbs)
	if st.Deleted > 0 {
		e.reloadCache() // deleted files must leave meaning search too
	} else if st.Indexed > 0 {
		e.mu.Lock()
		e.dirty = true // e.g. a copy of an identical file arrived with its vectors
		e.mu.Unlock()
	}
	return err
}

func (e *Engine) embedPending(ctx context.Context) error {
	if n, err := e.store.CountPending(ctx); err != nil || n == 0 || e.cfg.StartModel == nil {
		if err == nil {
			e.reloadIfDirty() // copies of identical files may have arrived with vectors
			e.resetRetry()
		}
		return err // with no model yet: keyword search only, passages wait
	}
	e.setStatus(Status{State: StateEmbedding})
	last := time.Now()
	var retried int64 // a suspected file being given one more try on a model just proven to work
	for {
		m, release, err := e.pool.get(ctx, e.cfg.IndexIdle, true)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("%w: %w", index.ErrEmbedder, err)
		}
		ix := &index.Indexer{Store: e.store, Embedder: m}
		st, err := ix.EmbedPending(ctx, func(p index.EmbedProgress) {
			e.setStatus(Status{State: StateEmbedding, Passages: p.Passages, Waiting: p.Waiting})
			// Make new vectors searchable now and then, not only at the end.
			if time.Since(last) > 30*time.Second {
				e.reloadCache()
				last = time.Now()
			}
		})
		release()

		if err != nil && st.Stuck != 0 {
			switch {
			case st.Stuck == retried:
				// It failed again right after the model handled a test passage: the file
				// itself breaks the model. Give up on it (it stays keyword-searchable).
				if merr := e.store.MarkEmbedFailed(ctx, st.Stuck,
					"the embedding model fails on this file"); merr != nil {
					return merr
				}
				e.forget(st.Stuck)
				retried = 0
				e.pool.drop()
				continue
			case e.strike(st.Stuck) >= maxStuck && e.modelWorks(ctx):
				// The same file stopped the pass maxStuck times, yet a fresh model handles a
				// test passage: try the file once more on it.
				retried = st.Stuck
				continue
			}
			// Otherwise the model itself is broken (or the file is still getting its
			// strikes): nobody is blamed; everything keeps waiting for the retry.
		}
		e.reloadCache()
		if err == nil {
			e.resetRetry()
		}
		return err
	}
}

// modelWorks restarts the model and checks it on a test passage. A pass can stop on a
// file because of the file or because the model is broken; this tells them apart, so a
// broken model never gets a good file blamed.
func (e *Engine) modelWorks(ctx context.Context) bool {
	e.pool.drop()
	m, release, err := e.pool.get(ctx, e.cfg.IndexIdle, true)
	if err != nil {
		return false
	}
	defer release()
	_, err = m.EmbedPassages(ctx, "check", []string{"A short test sentence to check the search model."})
	return err == nil
}

// maxStuck: a file the embedding pass stopped on this many times in a row is checked (see
// modelWorks). Otherwise, because the pass always resumes at the first waiting file, one
// bad file would block meaning search for every file after it.
const maxStuck = 3

// maxRetryDelay caps the wait between retries after errors.
const maxRetryDelay = time.Hour

// strike counts a failed pass on file id and returns its count.
func (e *Engine) strike(id int64) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.stuck[id]++
	return e.stuck[id]
}

func (e *Engine) forget(id int64) {
	e.mu.Lock()
	delete(e.stuck, id)
	e.mu.Unlock()
}

// resetRetry: work succeeded, so the next error waits the shortest time again.
func (e *Engine) resetRetry() {
	e.mu.Lock()
	e.retryDelay = e.cfg.retryAfter
	clear(e.stuck)
	e.mu.Unlock()
}

func (e *Engine) reloadIfDirty() {
	e.mu.Lock()
	dirty := e.dirty
	e.mu.Unlock()
	if dirty {
		e.reloadCache()
	}
}

func (e *Engine) reloadCache() {
	if err := e.cache.Load(e.ctx, e.store); err != nil {
		if e.ctx.Err() == nil {
			e.setStatus(Status{State: StateError, Err: "loading vectors: " + err.Error()})
		}
		return // still dirty: retried with the next reload
	}
	e.mu.Lock()
	e.dirty = false
	e.mu.Unlock()
}

// removeThumbs deletes thumbnails of removed index entries (P-5). Paths come from the
// database: anything not a plain relative path inside the thumbnails folder is ignored.
func (e *Engine) removeThumbs(paths []string) {
	for _, p := range paths {
		rel := filepath.FromSlash(p)
		if !filepath.IsLocal(rel) {
			continue
		}
		e.cfg.FS.Remove(filepath.Join(appfs.DirThumbs, rel)) // already gone is fine
	}
}

// idleLoop stops the model when it has been idle long enough.
func (e *Engine) idleLoop() {
	t := time.NewTicker(e.cfg.idleTick)
	defer t.Stop()
	for {
		select {
		case <-e.ctx.Done():
			return
		case <-t.C:
			if e.pool.stopIfIdle() {
				e.notify(e.Status()) // report "model unloaded" without rewriting the status
			}
		}
	}
}
