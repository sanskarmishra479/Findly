//go:build windows

package embed

import (
	"container/list"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sanskarmishra479/findly/internal/vecfmt"
)

func fakeEmbedding(seed int) []float32 {
	v := make([]float32, 768)
	for i := range v {
		v[i] = float32(math.Sin(float64(seed*1000 + i)))
	}
	return v
}

func response(t *testing.T, order []int) []byte {
	t.Helper()
	type item struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	}
	var data []item
	for _, i := range order {
		data = append(data, item{i, fakeEmbedding(i)})
	}
	b, err := json.Marshal(map[string]any{"data": data})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestPrefixes(t *testing.T) {
	if got := QueryText("dog on a beach"); got != "task: search result | query: dog on a beach" {
		t.Errorf("QueryText = %q", got)
	}
	if got := PassageText("lease.pdf", "No pets."); got != "title: lease.pdf | text: No pets." {
		t.Errorf("PassageText = %q", got)
	}
	if got := PassageText("", "x"); got != "title: none | text: x" {
		t.Errorf("PassageText(no title) = %q", got)
	}
}

func TestParseResponseOrdersByIndex(t *testing.T) {
	vs, err := parseResponse(response(t, []int{2, 0, 1}), 3)
	if err != nil {
		t.Fatal(err)
	}
	for i, v := range vs {
		want, _, _ := vecfmt.Encode(fakeEmbedding(i))
		if v.Bin != want {
			t.Errorf("vector %d is out of order", i)
		}
	}
}

func TestParseResponseRejectsBadData(t *testing.T) {
	cases := map[string]string{
		"wrong count":   string(response(t, []int{0})),
		"dup index":     string(response(t, []int{0, 0})),
		"out of range":  string(response(t, []int{0, 5})),
		"not json":      "<html>error</html>",
		"short vector":  `{"data":[{"index":0,"embedding":[1,2,3]},{"index":1,"embedding":[1,2,3]}]}`,
		"zero vector":   `{"data":[{"index":0,"embedding":[` + strings.Repeat("0,", 767) + `0]},{"index":1,"embedding":[1]}]}`,
		"empty payload": `{}`,
	}
	for name, body := range cases {
		if _, err := parseResponse([]byte(body), 2); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestQueryCacheEvictsOldest(t *testing.T) {
	s := &Server{cache: map[string]*list.Element{}, order: list.New()}
	for i := range cacheSize + 10 {
		s.remember(fmt.Sprint(i), Vector{})
	}
	if s.order.Len() != cacheSize {
		t.Fatalf("cache size %d", s.order.Len())
	}
	if _, ok := s.cached("0"); ok {
		t.Error("oldest entry not evicted")
	}
	if _, ok := s.cached(fmt.Sprint(cacheSize + 9)); !ok {
		t.Error("newest entry missing")
	}
}

func TestStartValidatesPaths(t *testing.T) {
	if _, err := Start(context.Background(), Config{ServerExe: "llama-server.exe", Model: "m.gguf"}); err == nil {
		t.Fatal("expected error for relative paths")
	}
}

// Integration test with the real model, if Phase 0 downloads are present.
func TestRealServer(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	root, _ := filepath.Abs(filepath.Join("..", ".."))
	cfg := Config{
		WorkDir:   t.TempDir(),
		ServerExe: filepath.Join(root, "third_party", "llama", "cpu", "llama-server.exe"),
		Model:     filepath.Join(root, "third_party", "models", "embeddinggemma-2-Q8_0.gguf"),
	}
	for _, p := range []string{cfg.ServerExe, cfg.Model} {
		if _, err := os.Stat(p); err != nil {
			t.Skipf("model files not downloaded: %v", err)
		}
	}
	ctx := context.Background()
	s, err := Start(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	passages := []string{
		"Tenants may not keep dogs, cats or other pets in the apartment.",
		"The quarterly revenue grew by twelve percent compared to last year.",
		"Bake the cake at 180 degrees for forty minutes until golden.",
	}
	pv, err := s.EmbedPassages(ctx, "lease.txt", passages)
	if err != nil {
		t.Fatal(err)
	}
	for q, want := range map[string]int{
		"can I have a pet in my flat": 0,
		"company earnings growth":     1,
		"how long to bake a cake":     2,
	} {
		qv, err := s.EmbedQuery(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		best, bestScore := -1, float32(-2)
		for i := range pv {
			if d := vecfmt.Dot(&qv.Q, &pv[i].Q); d > bestScore {
				best, bestScore = i, d
			}
		}
		if best != want {
			t.Errorf("%q matched passage %d, want %d", q, best, want)
		}
	}

	// Crash recovery: kill the server; the next call restarts it.
	s.mu.Lock()
	s.proc.Stop()
	s.mu.Unlock()
	if _, err := s.EmbedQuery(ctx, "a brand new query after crash"); err != nil {
		t.Fatalf("no restart after crash: %v", err)
	}
	if s.restarts != 0 {
		t.Errorf("restarts = %d, want 0 (reset after a successful request)", s.restarts)
	}
}

func realConfig(t *testing.T) Config {
	t.Helper()
	if testing.Short() {
		t.Skip("short mode")
	}
	root, _ := filepath.Abs(filepath.Join("..", ".."))
	cfg := Config{
		WorkDir:   t.TempDir(),
		ServerExe: filepath.Join(root, "third_party", "llama", "cpu", "llama-server.exe"),
		Model:     filepath.Join(root, "third_party", "models", "embeddinggemma-2-Q8_0.gguf"),
	}
	for _, p := range []string{cfg.ServerExe, cfg.Model} {
		if _, err := os.Stat(p); err != nil {
			t.Skipf("model files not downloaded: %v", err)
		}
	}
	return cfg
}

// Review 1 (P-4): another program listening on our port must never receive anything.
func TestPortSquatterIsRefused(t *testing.T) {
	cfg := realConfig(t)
	var hits atomic.Int64
	squatter := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer squatter.Close()
	squatPort := squatter.Listener.Addr().(*net.TCPAddr).Port

	first := true
	pickPort = func() (int, error) {
		if first {
			first = false
			return squatPort, nil
		}
		return freePort()
	}
	defer func() { pickPort = freePort }()

	s, err := Start(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.EmbedQuery(context.Background(), "hello"); err != nil {
		t.Fatal(err)
	}
	if strings.HasSuffix(s.base, ":"+strconv.Itoa(squatPort)) {
		t.Fatal("using the squatted port")
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("squatter received %d requests", n)
	}
}

// Review 2: a passage far over the token limit is cut and still embedded, and one bad
// passage does not fail the rest of its batch.
func TestTooLongPassageIsCut(t *testing.T) {
	s, err := Start(context.Background(), realConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	huge := strings.Repeat("ꙮ𐍈ᚠ ", 3000) // ~10,000 tokens
	vs, err := s.EmbedPassages(context.Background(), "x.txt", []string{"normal text", huge, "more normal text"})
	if err != nil {
		t.Fatal(err)
	}
	if len(vs) != 3 {
		t.Fatalf("got %d vectors", len(vs))
	}
}

// Review 3: a cancelled request is reported as cancelled and does not use up restarts.
func TestCancelledRequestKeepsServer(t *testing.T) {
	s, err := Start(context.Background(), realConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.EmbedQuery(ctx, "cancelled query"); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
	if _, err := s.EmbedQuery(context.Background(), "next query works"); err != nil {
		t.Fatal(err)
	}
	if s.restarts != 0 {
		t.Fatalf("restarts = %d", s.restarts)
	}
}

func TestParseResponseRejectsWrongDim(t *testing.T) {
	body := `{"data":[{"index":0,"embedding":[` + strings.Repeat("0.5,", 1023) + `0.5]}]}`
	if _, err := parseResponse([]byte(body), 1); err == nil {
		t.Fatal("1024-d vector accepted")
	}
}

func TestTokensInError(t *testing.T) {
	msg := `embed: input cannot be embedded: {"error":{"message":"input (33009 tokens) is too large to process."}}`
	if n := tokensInError(msg); n != 33009 {
		t.Fatalf("got %d", n)
	}
	if n := tokensInError("something else"); n != 0 {
		t.Fatalf("got %d", n)
	}
}

// fakeServer builds testdata/fakeserver once per test run.
func fakeServer(t *testing.T) Config {
	t.Helper()
	exe := filepath.Join(t.TempDir(), "fakeserver.exe")
	if out, err := exec.Command("go", "build", "-o", exe, "./testdata/fakeserver").CombinedOutput(); err != nil {
		t.Fatalf("build fake server: %v %s", err, out)
	}
	model := filepath.Join(t.TempDir(), "model.gguf")
	os.WriteFile(model, nil, 0o644)
	return Config{ServerExe: exe, Model: model, WorkDir: t.TempDir()}
}

// High-fix review #2/#3: files that crash the server must not use up the restart limit:
// after many crashing inputs, normal requests (and searches) still work.
func TestCrashingInputsDoNotKillServer(t *testing.T) {
	s, err := Start(context.Background(), fakeServer(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	// Rounds of crashing inputs separated by successes: never runs out of restarts.
	for round := range 3 {
		for i := range maxInputCrashes - 1 {
			_, err := s.EmbedPassages(ctx, "bad.txt", []string{"poison " + strconv.Itoa(round*10+i)})
			if !errors.Is(err, ErrCrashed) || !errors.Is(err, ErrServer) {
				t.Fatalf("round %d crash %d: got %v, want ErrCrashed", round, i, err)
			}
		}
		if _, err := s.EmbedPassages(ctx, "ok.txt", []string{"a normal passage"}); err != nil {
			t.Fatalf("round %d: server dead after crashing inputs: %v", round, err)
		}
	}
	if _, err := s.EmbedQuery(ctx, "a search"); err != nil {
		t.Fatalf("search fails after crashing inputs: %v", err)
	}
}

// A server that crashes on every request is broken: after maxInputCrashes in a row it is
// not restarted again (no endless model reloads).
func TestAlwaysCrashingServerStops(t *testing.T) {
	s, err := Start(context.Background(), fakeServer(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	var last error
	for i := range maxInputCrashes + 3 {
		_, last = s.EmbedPassages(ctx, "x", []string{"poison " + strconv.Itoa(i)})
	}
	if errors.Is(last, ErrCrashed) || !errors.Is(last, ErrServer) {
		t.Fatalf("last error = %v; want a refusal (ErrServer, not another crash)", last)
	}
}
