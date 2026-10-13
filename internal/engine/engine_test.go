package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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
	gate   chan struct{}
	closed atomic.Bool
	calls  atomic.Int64
}

func (m *fakeModel) EmbedPassages(ctx context.Context, _ string, texts []string) ([]embed.Vector, error) {
	m.calls.Add(1)
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
	delay  time.Duration
}

func (s *starter) start(_ context.Context, low bool) (Model, error) {
	time.Sleep(s.delay)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return nil, s.err
	}
	m := &fakeModel{gate: s.gate}
	s.models = append(s.models, m)
	s.low = append(s.low, low)
	return m, nil
}

func (s *starter) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.models)
}

func newEngine(t *testing.T, s *starter, idle time.Duration) *Engine {
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
