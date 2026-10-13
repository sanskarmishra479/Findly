package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sanskarmishra479/findly/internal/appfs"
	"github.com/sanskarmishra479/findly/internal/embed"
	"github.com/sanskarmishra479/findly/internal/safefs"
	"github.com/sanskarmishra479/findly/internal/store"
)

// fakeModel embeds instantly (or waits on gate, if set).
type fakeModel struct {
	gate    chan struct{}
	failOn  string // passages containing this get a server error (not the input's fault)
	failAll bool   // a broken model: every request fails
	closed  atomic.Bool
	calls   atomic.Int64
}

func (m *fakeModel) EmbedPassages(ctx context.Context, _ string, texts []string) ([]embed.Vector, error) {
	m.calls.Add(1)
	for _, t := range texts {
		if m.failAll || m.failOn != "" && strings.Contains(t, m.failOn) {
			return nil, errors.New("embed: request: connection reset")
		}
	}
	if m.gate != nil {
		select {
		case <-m.gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	out := make([]embed.Vector, len(texts))
	for i := range out {
		out[i].Q.Scale = 0.01
		out[i].Q.I8[0] = 1
	}
	return out, nil
}

func (m *fakeModel) EmbedQuery(context.Context, string) (embed.Vector, error) {
	return embed.Vector{}, nil
}

func (m *fakeModel) Close() { m.closed.Store(true) }

// starter hands out fake models and records how they were started.
type starter struct {
	mu     sync.Mutex
	models []*fakeModel
	low    []bool
	err    error
	gate   chan struct{}
	failOn string
	broken int // the first this-many models started are broken
	delay  time.Duration
}

func (s *starter) start(ctx context.Context, low bool) (Model, error) {
	select {
	case <-time.After(s.delay):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return nil, s.err
	}
	m := &fakeModel{gate: s.gate, failOn: s.failOn, failAll: len(s.models) < s.broken}
	s.models = append(s.models, m)
	s.low = append(s.low, low)
	return m, nil
}

func (s *starter) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.models)
}

func newEngine(t *testing.T, s *starter, idle time.Duration, opts ...func(*Config)) *Engine {
	t.Helper()
	fs, err := appfs.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { fs.Close() })
	cfg := Config{FS: fs, IndexIdle: idle, SearchIdle: idle, idleTick: 10 * time.Millisecond,
		retryAfter: time.Hour, Licensed: func() bool { return true }}
	if s != nil {
		cfg.StartModel = s.start
	}
	for _, o := range opts {
		o(&cfg)
	}
	e, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.Close)
	return e
}

func folder(t *testing.T, files map[string]string) string {
	t.Helper()
	dir, err := safefs.Canonical(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for name, text := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for: %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func pending(t *testing.T, e *Engine) int {
	n, err := e.Store().CountPending(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestIndexesThenEmbedsThenStopsModel(t *testing.T) {
	s := &starter{}
	e := newEngine(t, s, 50*time.Millisecond)
	dir := folder(t, map[string]string{"a.txt": "quarterly budget report", "b.txt": "meeting notes"})
	if _, err := e.AddFolder(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "vectors added", func() bool { return e.Status().State == StateIdle && pending(t, e) == 0 && s.count() == 1 })
	if !s.low[0] {
		t.Error("the model was started for indexing at normal priority")
	}
	hits, _ := e.Store().KeywordSearch(context.Background(), `"quarterly"`, store.Filter{}, 10, true)
	if len(hits) != 1 {
		t.Fatalf("keyword hits = %d", len(hits))
	}
	if e.Cache().Len() == 0 {
		t.Fatal("the vector cache was not reloaded")
	}
	// IndexIdle after the last use the model is stopped (never kept loaded for good).
	waitUntil(t, "model stopped", func() bool { return s.models[0].closed.Load() && !e.Status().Model })
}

func TestKeywordOnlyWithoutModel(t *testing.T) {
	e := newEngine(t, nil, time.Minute)
	dir := folder(t, map[string]string{"a.txt": "rent agreement"})
	e.AddFolder(context.Background(), dir)
	waitUntil(t, "indexed", func() bool {
		hits, _ := e.Store().KeywordSearch(context.Background(), `"rent"`, store.Filter{}, 10, true)
		return len(hits) == 1 && e.Status().State == StateIdle
	})
	if pending(t, e) == 0 {
		t.Fatal("passages must keep waiting for a model")
	}
	if _, _, err := e.QueryModel(context.Background()); !errors.Is(err, ErrNoModel) {
		t.Fatalf("QueryModel = %v, want ErrNoModel", err)
	}
}

func TestQueryModelStartsOnceForConcurrentSearches(t *testing.T) {
	s := &starter{delay: 50 * time.Millisecond}
	e := newEngine(t, s, time.Minute)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m, release, err := e.QueryModel(context.Background())
			if err != nil || m == nil {
				t.Errorf("QueryModel: %v", err)
				return
			}
			release()
		}()
	}
	wg.Wait()
	if s.count() != 1 {
		t.Fatalf("model started %d times, want once", s.count())
	}
	if s.low[0] {
		t.Error("a model started for search must not run at low priority")
	}
}

func TestModelStartFailureIsReportedAndKeepsPassages(t *testing.T) {
	s := &starter{err: errors.New("llama-server.exe not found")}
	e := newEngine(t, s, time.Minute)
	dir := folder(t, map[string]string{"a.txt": "some text"})
	e.AddFolder(context.Background(), dir)
	waitUntil(t, "error state", func() bool { return e.Status().State == StateError })
	if pending(t, e) == 0 {
		t.Fatal("passages were given up because the model did not start")
	}
	// Once the model can start, the next trigger finishes the job.
	s.mu.Lock()
	s.err = nil
	s.mu.Unlock()
	e.IndexAll()
	waitUntil(t, "recovered", func() bool { return pending(t, e) == 0 && e.Status().State == StateIdle })
}

func TestPauseStopsAndResumeContinues(t *testing.T) {
	gate := make(chan struct{})
	s := &starter{gate: gate}
	e := newEngine(t, s, time.Minute)
	dir := folder(t, map[string]string{"a.txt": "alpha text", "b.txt": "beta text"})
	e.AddFolder(context.Background(), dir)
	waitUntil(t, "embedding started", func() bool { return e.Status().State == StateEmbedding && s.count() == 1 })
	e.Pause()
	waitUntil(t, "paused", func() bool { return e.Status().State == StatePaused })
	close(gate) // the model would answer now, but the engine is paused
	time.Sleep(50 * time.Millisecond)
	if e.Status().State != StatePaused {
		t.Fatalf("status after pause = %s", e.Status().State)
	}
	e.Resume()
	waitUntil(t, "finished after resume", func() bool { return pending(t, e) == 0 && e.Status().State == StateIdle })
}

func TestNewFolderInterruptsEmbedding(t *testing.T) {
	gate := make(chan struct{})
	s := &starter{gate: gate}
	e := newEngine(t, s, time.Minute)
	first := folder(t, map[string]string{"a.txt": "first folder text"})
	e.AddFolder(context.Background(), first)
	waitUntil(t, "embedding the first folder", func() bool { return e.Status().State == StateEmbedding && s.count() == 1 })
	// While vectors are being added (the model is stuck on gate), a new folder arrives:
	// its files must become keyword-searchable without waiting for the model.
	second := folder(t, map[string]string{"b.txt": "urgent invoice"})
	e.AddFolder(context.Background(), second)
	waitUntil(t, "second folder keyword-searchable", func() bool {
		hits, _ := e.Store().KeywordSearch(context.Background(), `"urgent"`, store.Filter{}, 10, true)
		return len(hits) == 1
	})
	close(gate)
	waitUntil(t, "all vectors", func() bool { return pending(t, e) == 0 && e.Status().State == StateIdle })
}

// T3 review #1: a copy of an already-embedded file arrives with its vectors (nothing to
// embed); the vector cache must still be reloaded so it is searchable by meaning.
func TestCopiedDuplicateIsSearchableByMeaning(t *testing.T) {
	s := &starter{}
	e := newEngine(t, s, time.Minute)
	a := folder(t, map[string]string{"report.txt": "the same quarterly report"})
	e.AddFolder(context.Background(), a)
	waitUntil(t, "first folder embedded", func() bool { return pending(t, e) == 0 && e.Cache().Len() == 1 && e.Status().State == StateIdle })
	b := folder(t, map[string]string{"report copy.txt": "the same quarterly report"})
	e.AddFolder(context.Background(), b)
	waitUntil(t, "copy in the vector cache", func() bool { return e.Cache().Len() == 2 })
}

// T3 review: a file the model keeps failing on is given up after maxStuck passes, so it
// cannot block meaning search for the files stored after it.
func TestStuckFileIsGivenUp(t *testing.T) {
	s := &starter{failOn: "unlucky"}
	e := newEngine(t, s, time.Minute, func(c *Config) { c.retryAfter = 10 * time.Millisecond })
	dir := folder(t, map[string]string{"a.txt": "an unlucky passage", "b.txt": "a normal passage"})
	e.AddFolder(context.Background(), dir)
	waitUntil(t, "other file embedded, bad file given up", func() bool {
		f, ok, _ := e.Store().FileByPath(context.Background(), filepath.Join(dir, "a.txt"))
		return ok && f.Error != "" && pending(t, e) == 0 && e.Cache().Len() == 1
	})
	f, ok, err := e.Store().FileByPath(context.Background(), filepath.Join(dir, "a.txt"))
	if err != nil || !ok || f.Error == "" {
		t.Fatalf("the given-up file has no reason recorded: %+v %v %v (starts %d)", f, ok, err, s.count())
	}
	if e.Cache().Len() != 1 {
		t.Fatalf("cache = %d vectors, want 1 (the normal file)", e.Cache().Len())
	}
}

// T3 review: after Close nothing may start a model, and Close does not wait for a slow
// model start.
func TestCloseDuringStartAndAfter(t *testing.T) {
	s := &starter{delay: 10 * time.Second}
	fs, _ := appfs.Open(t.TempDir())
	defer fs.Close()
	e, err := Open(Config{FS: fs, StartModel: s.start, idleTick: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	e.WarmUp() // a 10 s start begins
	time.Sleep(20 * time.Millisecond)
	start := time.Now()
	e.Close()
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("Close waited %v for the model start", d)
	}
	s.delay = 0
	e.WarmUp() // after Close: must not start anything
	time.Sleep(50 * time.Millisecond)
	if s.count() != 0 {
		t.Fatalf("%d models started after Close", s.count())
	}
}

// T3 verification review: a broken model (every request fails for a while) must never
// get a good file blamed; once it works again, every file gets its vectors.
func TestBrokenModelBlamesNoFile(t *testing.T) {
	s := &starter{broken: 4}
	e := newEngine(t, s, time.Minute, func(c *Config) { c.retryAfter = 10 * time.Millisecond })
	dir := folder(t, map[string]string{"a.txt": "first good passage", "b.txt": "second good passage"})
	e.AddFolder(context.Background(), dir)
	waitUntil(t, "all vectors after the model recovered", func() bool { return pending(t, e) == 0 && e.Cache().Len() == 2 })
	for _, name := range []string{"a.txt", "b.txt"} {
		if f, _, _ := e.Store().FileByPath(context.Background(), filepath.Join(dir, name)); f.Error != "" {
			t.Errorf("%s was blamed for a broken model: %q", name, f.Error)
		}
	}
}
