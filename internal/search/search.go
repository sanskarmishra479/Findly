// Package search finds files for a query (plan 9.11). Two paths run for every query:
//
//   - keyword: FTS5 over file names and text, fast enough for every keystroke;
//   - semantic: the query is embedded, every stored vector is compared by Hamming
//     distance in memory, the best 200 are re-ranked with int8 dot products.
//
// Results are grouped by file (best item per file, with its page/chunk) and the two lists
// are merged with Reciprocal Rank Fusion, plus a boost for exact matches.
package search

import (
	"container/heap"
	"context"
	"encoding/json"
	"regexp"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/sanskarmishra479/findly/internal/embed"
	"github.com/sanskarmishra479/findly/internal/store"
	"github.com/sanskarmishra479/findly/internal/vecfmt"
)

const (
	KeywordLimit  = 50  // keyword hits per query (plan 9.11)
	Candidates    = 200 // Hamming candidates re-ranked with int8 (plan 9.11)
	ResultLimit   = 50
	rrfK          = 60
	exactBoost    = 0.05 // more than any single RRF rank difference near the top
	snippetLength = 200
	// MinSemantic drops semantic hits whose similarity is too low to mean anything.
	MinSemantic = 0.25
)

// QueryEmbedder embeds a search query. *embed.Server implements it.
type QueryEmbedder interface {
	EmbedQuery(ctx context.Context, q string) (embed.Vector, error)
}

// Result is one file found.
type Result struct {
	File    store.File
	ItemID  int64   // best matching item (0 for a file-name-only match)
	Locator Locator // where in the file
	Snippet string
	Score   float64
	Keyword bool // found by the keyword path
	Meaning bool // found by the semantic path
	Exact   bool // query text appears verbatim
}

// Locator says where in a file a match is.
type Locator struct {
	Page  int     `json:"page,omitempty"`
	Chunk int     `json:"chunk,omitempty"`
	T     float64 `json:"t,omitempty"` // seconds, for audio/video (Phase 6)
}

// Searcher runs searches. Safe for concurrent use.
type Searcher struct {
	Store    *store.Store
	Embedder QueryEmbedder // nil: keyword search only (e.g. model not downloaded yet)
	Cache    *VectorCache
}

// Keyword returns keyword results immediately (every keystroke). typing makes the last
// word match as a prefix.
func (s *Searcher) Keyword(ctx context.Context, raw string, fl store.Filter, typing bool) ([]Result, error) {
	q := Parse(raw)
	if q.Empty() {
		return nil, nil
	}
	hits, err := s.Store.KeywordSearch(ctx, q.Match(typing), fl, KeywordLimit)
	if err != nil {
		return nil, err
	}
	// Best hit per file, in rank order.
	var order []int64
	best := map[int64]store.KeywordHit{}
	for _, h := range hits {
		if _, ok := best[h.FileID]; !ok {
			best[h.FileID] = h
			order = append(order, h.FileID)
		}
	}
	files, err := s.Store.FilesByID(ctx, order)
	if err != nil {
		return nil, err
	}
	out := make([]Result, 0, len(order))
	for _, id := range order {
		f, ok := files[id]
		if !ok {
			continue
		}
		h := best[id]
		r := Result{File: f, Keyword: true, Snippet: snippet(h.Text, q)}
		if h.Kind != store.ItemName {
			r.ItemID = h.ItemID
			r.Locator = parseLocator(h.Locator)
		}
		out = append(out, r)
	}
	return out, nil
}

// Semantic returns results by meaning. It needs the embedder and the vector cache.
func (s *Searcher) Semantic(ctx context.Context, raw string, fl store.Filter) ([]Result, error) {
	q := Parse(raw)
	if q.Empty() || s.Embedder == nil || s.Cache == nil {
		return nil, nil
	}
	qv, err := s.Embedder.EmbedQuery(ctx, q.Text())
	if err != nil {
		return nil, err
	}
	scored := s.Cache.Nearest(&qv, Candidates)
	// Best item per file, by int8 score.
	var order []int64
	best := map[int64]Scored{}
	for _, c := range scored {
		if c.Score < MinSemantic {
			break // sorted best first
		}
		if _, ok := best[c.FileID]; !ok {
			best[c.FileID] = c
			order = append(order, c.FileID)
		}
	}
	files, err := s.Store.FilesByID(ctx, order)
	if err != nil {
		return nil, err
	}
	var out []Result
	for _, id := range order {
		f, ok := files[id]
		if !ok || !fl.Match(f) {
			continue
		}
		c := best[id]
		_, loc, text, err := s.Store.ItemByID(ctx, c.ItemID)
		if err != nil {
			continue // item deleted since the cache was loaded
		}
		out = append(out, Result{
			File: f, ItemID: c.ItemID, Locator: parseLocator(loc),
			Snippet: snippet(text, q), Score: float64(c.Score), Meaning: true,
		})
		if len(out) == ResultLimit {
			break
		}
	}
	return out, nil
}

// Search runs both paths and merges them. If the semantic path fails (for example the
// model is still loading), keyword results are still returned along with the error.
func (s *Searcher) Search(ctx context.Context, raw string, fl store.Filter) ([]Result, error) {
	kw, err := s.Keyword(ctx, raw, fl, false)
	if err != nil {
		return nil, err
	}
	sem, semErr := s.Semantic(ctx, raw, fl)
	return Fuse(Parse(raw), kw, sem), semErr
}

// Fuse merges keyword and semantic lists with Reciprocal Rank Fusion (k=60) and adds a
// boost when the query, or a quoted part of it, appears verbatim in the file name or text.
// With quoted phrases, only results containing every phrase are kept (plan 9.11).
func Fuse(q Query, keyword, semantic []Result) []Result {
	merged := map[int64]*Result{}
	var order []int64
	add := func(list []Result) {
		for rank, r := range list {
			score := 1.0 / float64(rrfK+rank+1)
			if m, ok := merged[r.File.ID]; ok {
				m.Score += score
				m.Keyword = m.Keyword || r.Keyword
				m.Meaning = m.Meaning || r.Meaning
				if m.ItemID == 0 && r.ItemID != 0 { // prefer a content location over a name match
					m.ItemID, m.Locator, m.Snippet = r.ItemID, r.Locator, r.Snippet
				}
				continue
			}
			c := r
			c.Score = score
			merged[r.File.ID] = &c
			order = append(order, r.File.ID)
		}
	}
	add(keyword)
	add(semantic)

	exact := strings.ToLower(strings.Join(q.Words, " "))
	var out []Result
	for _, id := range order {
		r := merged[id]
		hay := strings.ToLower(r.File.Name + "\n" + r.Snippet)
		if len(q.Phrases) > 0 {
			ok := true
			for _, p := range q.Phrases {
				if !strings.Contains(hay, strings.ToLower(p)) && !r.Keyword {
					ok = false
				}
			}
			if !ok {
				continue // semantic-only hit without the required exact phrase
			}
		}
		if exact != "" && strings.Contains(normalize(hay), normalize(exact)) {
			r.Exact = true
			r.Score += exactBoost
		}
		out = append(out, *r)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	if len(out) > ResultLimit {
		out = out[:ResultLimit]
	}
	return out
}

// normalize collapses punctuation to spaces so "INV-2024-001" matches "inv 2024 001".
func normalize(s string) string { return strings.Join(words(s), " ") }

func parseLocator(s string) Locator {
	var l Locator
	if s != "" {
		json.Unmarshal([]byte(s), &l)
	}
	return l
}

// snippet returns a short excerpt of text around the first query word found.
func snippet(text string, q Query) string {
	if text == "" {
		return ""
	}
	// Search the original text case-insensitively, so positions are byte offsets into
	// text itself (ToLower can change byte lengths, e.g. "İ", which would misplace the cut).
	at := -1
	for _, w := range append(append([]string{}, q.Phrases...), q.Words...) {
		re, err := regexp.Compile("(?i)" + regexp.QuoteMeta(w))
		if err != nil {
			continue
		}
		if loc := re.FindStringIndex(text); loc != nil {
			at = loc[0]
			break
		}
	}
	start := 0
	if at > snippetLength/3 {
		start = at - snippetLength/3
		for start > 0 && !utf8.RuneStart(text[start]) {
			start--
		}
	}
	end := min(len(text), start+snippetLength)
	for end < len(text) && !utf8.RuneStart(text[end]) {
		end++
	}
	s := strings.Join(strings.Fields(text[start:end]), " ")
	if start > 0 {
		s = "…" + s
	}
	if end < len(text) {
		s += "…"
	}
	return s
}

// ---- in-memory vector cache (plan 9.11) ----

// VectorCache holds every stored vector in memory: binary forms in one contiguous slice
// for the fast scan, int8 forms for re-ranking. 100k items ≈ 3.2 MB binary + 26 MB int8.
type VectorCache struct {
	mu    sync.RWMutex
	items []int64
	files []int64
	bins  []vecfmt.Bin
	q     []vecfmt.Int8
}

// Load replaces the cache with all vectors in the store.
func (c *VectorCache) Load(ctx context.Context, st *store.Store) error {
	var items, files []int64
	var bins []vecfmt.Bin
	var qs []vecfmt.Int8
	err := st.AllVectors(ctx, func(r store.VectorRow) error {
		items = append(items, r.ItemID)
		files = append(files, r.FileID)
		bins = append(bins, vecfmt.Bin(r.Vector.Bin))
		qs = append(qs, vecfmt.Int8{I8: r.Vector.I8, Scale: r.Vector.Scale})
		return nil
	})
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.items, c.files, c.bins, c.q = items, files, bins, qs
	c.mu.Unlock()
	return nil
}

// Len returns the number of cached vectors.
func (c *VectorCache) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.items)
}

// Scored is a candidate item with its int8 similarity.
type Scored struct {
	ItemID int64
	FileID int64
	Score  float32
}

// Nearest returns up to n items closest to qv: the n smallest Hamming distances, then
// sorted by int8 dot product (best first).
func (c *VectorCache) Nearest(qv *embed.Vector, n int) []Scored {
	c.mu.RLock()
	defer c.mu.RUnlock()
	h := &maxHeap{}
	for i := range c.bins {
		d := vecfmt.Hamming(&qv.Bin, &c.bins[i])
		if h.Len() < n {
			heap.Push(h, cand{i, d})
		} else if d < (*h)[0].dist {
			(*h)[0] = cand{i, d}
			heap.Fix(h, 0)
		}
	}
	out := make([]Scored, h.Len())
	for k, cd := range *h {
		out[k] = Scored{ItemID: c.items[cd.idx], FileID: c.files[cd.idx], Score: vecfmt.Dot(&qv.Q, &c.q[cd.idx])}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	return out
}

type cand struct{ idx, dist int }

// maxHeap keeps the n smallest distances; the root is the largest of them.
type maxHeap []cand

func (h maxHeap) Len() int           { return len(h) }
func (h maxHeap) Less(i, j int) bool { return h[i].dist > h[j].dist }
func (h maxHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *maxHeap) Push(x any)        { *h = append(*h, x.(cand)) }
func (h *maxHeap) Pop() any {
	old := *h
	x := old[len(old)-1]
	*h = old[:len(old)-1]
	return x
}
