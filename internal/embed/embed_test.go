//go:build windows

package embed

import (
	"container/list"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
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
	if s.restarts != 1 {
		t.Errorf("restarts = %d, want 1", s.restarts)
	}
}
