package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"

	"github.com/sanskarmishra479/findly/internal/embed"
	"github.com/sanskarmishra479/findly/internal/engine"
	"github.com/sanskarmishra479/findly/internal/search"
	"github.com/sanskarmishra479/findly/internal/store"
)

// SearchResult is one file in the panel's result list (docs/design/search-panel.md).
type SearchResult struct {
	ID       int64   `json:"id"`
	ItemID   int64   `json:"itemId"` // best matching passage, for the preview
	Name     string  `json:"name"`
	Folder   string  `json:"folder"` // shortened: the user folder becomes "~"
	Kind     string  `json:"kind"`   // text | doc | pdf | image | audio | video | other
	Ext      string  `json:"ext"`
	Size     int64   `json:"size"`
	Modified int64   `json:"modified"` // unix milliseconds
	Page     int     `json:"page"`     // 1-based PDF page of the best match, 0 if none
	Snippet  string  `json:"snippet"`
	Keyword  bool    `json:"keyword"` // has the query's words
	Meaning  bool    `json:"meaning"` // matches its meaning
	Exact    bool    `json:"exact"`
	Score    float64 `json:"score"`
}

// SearchResponse answers one Search call.
type SearchResponse struct {
	Seq     int            `json:"seq"` // echoes the request, so the UI drops stale answers
	Results []SearchResult `json:"results"`
	// Words are the query's words and phrases, for showing them in bold.
	Words []string `json:"words"`
	// MeaningOff says why meaning search is not available (no model yet); empty if it is.
	MeaningOff string `json:"meaningOff"`
	Err        string `json:"err"`
}

// searches cancels the previous search of the same kind when a new one starts: while
// the user types, only the latest query matters.
type searches struct {
	mu     sync.Mutex
	cancel [2]context.CancelFunc // [0] words, [1] meaning
}

func (s *searches) start(parent context.Context, meaning bool) (context.Context, context.CancelFunc) {
	i := 0
	if meaning {
		i = 1
	}
	ctx, cancel := context.WithCancel(parent)
	s.mu.Lock()
	if s.cancel[i] != nil {
		s.cancel[i]()
	}
	s.cancel[i] = cancel
	s.mu.Unlock()
	return ctx, cancel
}

// queryModel gives search the engine's model on demand: each query leases it (starting
// it if needed), and it stays loaded SearchIdle after the last search.
type queryModel struct{ e *engine.Engine }

func (q queryModel) EmbedQuery(ctx context.Context, text string) (embed.Vector, error) {
	m, release, err := q.e.QueryModel(ctx)
	if err != nil {
		return embed.Vector{}, err
	}
	defer release()
	return m.EmbedQuery(ctx, text)
}

// Search runs a search for the panel. With meaning false it returns word matches only
// (fast enough to run on every keystroke); with meaning true it returns words and
// meaning merged. seq is echoed back so the UI can ignore answers to old queries.
func (a *App) Search(seq int, query string, meaning bool) SearchResponse {
	resp := SearchResponse{Seq: seq, Words: queryWords(query)}
	e := a.currentEngine()
	if e == nil {
		a.mu.Lock()
		resp.Err = a.engineErr
		a.mu.Unlock()
		if resp.Err == "" {
			resp.Err = "The index is still opening."
		}
		return resp
	}
	ctx := a.context()
	if ctx == nil {
		return resp
	}
	ctx, cancel := a.searches.start(ctx, meaning)
	defer cancel()

	s := &search.Searcher{Store: e.Store(), Cache: e.Cache(), Embedder: queryModel{e}}
	var (
		rs  []search.Result
		err error
	)
	if meaning {
		rs, err = s.Search(ctx, query, store.Filter{})
	} else {
		rs, err = s.Keyword(ctx, query, store.Filter{}, true)
	}
	switch {
	case errors.Is(err, engine.ErrNoModel):
		a.mu.Lock()
		resp.MeaningOff = a.modelNote
		a.mu.Unlock()
	case errors.Is(err, context.Canceled):
		return resp // a newer query replaced this one; the UI ignores it
	case err != nil:
		resp.Err = err.Error()
	}
	home, _ := os.UserHomeDir()
	resp.Results = make([]SearchResult, 0, len(rs))
	for _, r := range rs {
		resp.Results = append(resp.Results, SearchResult{
			ID: r.File.ID, ItemID: r.ItemID, Name: r.File.Name, Folder: shortFolder(filepath.Dir(r.File.Path), home),
			Kind: r.File.Kind, Ext: r.File.Ext, Size: r.File.Size, Modified: r.File.MTime / 1e6,
			Page: r.Locator.Page, Snippet: r.Snippet,
			Keyword: r.Keyword, Meaning: r.Meaning, Exact: r.Exact, Score: r.Score,
		})
	}
	return resp
}

// queryWords lists the words and quoted phrases of a query, for bold text and page
// highlights.
func queryWords(query string) []string {
	q := search.Parse(query)
	return append(append([]string{}, q.Words...), q.Phrases...)
}

// shortFolder shows a folder inside the user's profile as "~\Documents\Home".
func shortFolder(dir, home string) string {
	if home == "" {
		return dir
	}
	rel, err := filepath.Rel(home, dir)
	switch {
	case err != nil:
		return dir
	case rel == ".":
		return "~"
	case filepath.IsLocal(rel):
		return `~\` + rel
	}
	return dir
}
