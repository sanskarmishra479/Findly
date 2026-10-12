//go:build windows

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sanskarmishra479/findly/internal/evalkit"
	"github.com/sanskarmishra479/findly/internal/index"
	"github.com/sanskarmishra479/findly/internal/safefs"
	"github.com/sanskarmishra479/findly/internal/search"
	"github.com/sanskarmishra479/findly/internal/store"
)

// cli eval: build the testbed from evals/corpus, index it into a temporary index, run
// evals/queries.jsonl, print Recall@5 and MRR@10 for several semantic cutoffs, and save
// the results to evals/results/<date>.json (plan 14.5).
func runEval(args []string) error {
	fs := flag.NewFlagSet("eval", flag.ExitOnError)
	var ef engineFlags
	ef.register(fs)
	corpus := fs.String("corpus", filepath.Join("evals", "corpus"), "testbed sources")
	queries := fs.String("queries", filepath.Join("evals", "queries.jsonl"), "queries file")
	resultsDir := fs.String("results", filepath.Join("evals", "results"), "where to save the results JSON")
	cutoffs := fs.String("cutoffs", "0.55,0.60,0.65", "absolute semantic cutoffs to compare")
	gaps := fs.String("gaps", "0,0.08", "relative cutoffs to compare (drop hits this far below the best; 0 = off)")
	verbose := fs.Bool("v", false, "show every query's top results")
	fs.Parse(args)

	cases, err := evalkit.LoadCases(*queries)
	if err != nil {
		return err
	}
	levels, err := floats(*cutoffs)
	if err != nil {
		return err
	}
	gapList, err := floats(*gaps)
	if err != nil {
		return err
	}

	// Everything happens in a temporary folder: the real index is never touched.
	tmp, err := os.MkdirTemp("", "findly-eval-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	bed := filepath.Join(tmp, "testbed")
	n, err := evalkit.BuildTestbed(*corpus, bed)
	if err != nil {
		return err
	}
	bed, err = safefs.Canonical(bed)
	if err != nil {
		return err
	}
	ef.dataDir = filepath.Join(tmp, "data")
	en, err := openEngine(ef)
	if err != nil {
		return err
	}
	defer en.close()
	ctx := context.Background()
	if err := en.startEmbedder(ctx, ef, false); err != nil {
		return err
	}

	root, err := en.store.AddRoot(ctx, bed)
	if err != nil {
		return err
	}
	start := time.Now()
	ix := &index.Indexer{Store: en.store, Embedder: en.emb, Licensed: func() bool { return true }}
	st, err := ix.IndexRoot(ctx, root)
	if err != nil {
		return err
	}
	if st.Failed > 0 {
		return fmt.Errorf("%d testbed files failed to index", st.Failed)
	}
	fmt.Printf("testbed: %d files, indexed %d in %v\n", n, st.Indexed, time.Since(start).Round(time.Millisecond))

	cache := &search.VectorCache{}
	if err := cache.Load(ctx, en.store); err != nil {
		return err
	}
	// Run each query once with no cutoff; cutoffs are then applied to the same results.
	s := &search.Searcher{Store: en.store, Embedder: en.emb, Cache: cache, MinScore: -1, MaxBelowBest: -1}
	type run struct {
		kw, sem []search.Result
	}
	runs := make([]run, len(cases))
	var latency time.Duration
	for i, c := range cases {
		t0 := time.Now()
		kw, err := s.Keyword(ctx, c.Query, store.Filter{}, false)
		if err != nil {
			return fmt.Errorf("%q: %w", c.Query, err)
		}
		sem, err := s.Semantic(ctx, c.Query, store.Filter{})
		if err != nil {
			return fmt.Errorf("%q: %w", c.Query, err)
		}
		latency += time.Since(t0)
		runs[i] = run{kw, sem}
	}

	type levelResult struct {
		Cutoff   float64  `json:"cutoff"`
		Gap      float64  `json:"gap"`
		Recall5  float64  `json:"recall_at_5"`
		MRR10    float64  `json:"mrr_at_10"`
		Returned float64  `json:"avg_results"`   // how many results a query shows
		Noise    float64  `json:"avg_unrelated"` // of those, how many are not expected
		Misses   []string `json:"misses,omitempty"`
	}
	type setting struct{ cutoff, gap float64 }
	var settings []setting
	for _, lv := range levels {
		for _, g := range gapList {
			settings = append(settings, setting{lv, g})
		}
	}
	var report []levelResult
	fmt.Printf("\n%-8s %-6s %-10s %-8s %-12s %s\n", "cutoff", "gap", "Recall@5", "MRR@10", "avg results", "avg unrelated")
	for _, set := range settings {
		lv, gap := set.cutoff, set.gap
		lr := levelResult{Cutoff: lv, Gap: gap}
		for i, c := range cases {
			// Same rule as Searcher.Semantic: absolute floor, raised to best-gap.
			floor := lv
			if gap > 0 && len(runs[i].sem) > 0 {
				floor = max(floor, runs[i].sem[0].Score-gap)
			}
			var sem []search.Result
			for _, r := range runs[i].sem {
				if r.Score >= floor {
					sem = append(sem, r)
				}
			}
			fused := search.Fuse(search.Parse(c.Query), runs[i].kw, sem)
			hits := toHits(bed, fused)
			r5, rr := evalkit.Score(c, hits)
			lr.Recall5 += r5
			lr.MRR10 += rr
			lr.Returned += float64(len(hits))
			for _, h := range hits {
				related := false
				for _, e := range c.Expect {
					if evalkit.Matches(strings.Split(e, "#")[0], h) {
						related = true
					}
				}
				if !related {
					lr.Noise++
				}
			}
			if r5 < 1 {
				lr.Misses = append(lr.Misses, c.Query)
			}
			if *verbose && lv == levels[0] && gap == gapList[0] {
				fmt.Printf("  %q →", c.Query)
				for k, h := range hits {
					if k == 3 {
						break
					}
					fmt.Printf(" %s#%d", h.Rel, h.Page)
				}
				fmt.Println()
			}
		}
		q := float64(len(cases))
		lr.Recall5, lr.MRR10, lr.Returned, lr.Noise = lr.Recall5/q, lr.MRR10/q, lr.Returned/q, lr.Noise/q
		report = append(report, lr)
		fmt.Printf("%-8.2f %-6.2f %-10.3f %-8.3f %-12.1f %.1f\n", lv, gap, lr.Recall5, lr.MRR10, lr.Returned, lr.Noise)
	}
	for _, lr := range report {
		if len(lr.Misses) > 0 {
			fmt.Printf("\nmisses at cutoff %.2f gap %.2f: %s\n", lr.Cutoff, lr.Gap, strings.Join(lr.Misses, " | "))
		}
	}
	fmt.Printf("\naverage search time (keyword + query embedding + semantic): %v\n",
		(latency / time.Duration(len(cases))).Round(time.Millisecond))

	if err := os.MkdirAll(*resultsDir, 0o755); err != nil {
		return err
	}
	out := filepath.Join(*resultsDir, time.Now().Format("2006-01-02")+"-"+ef.backend+".json")
	b, err := json.MarshalIndent(map[string]any{
		"date": time.Now().Format(time.RFC3339), "backend": ef.backend, "queries": len(cases),
		"files": n, "levels": report,
	}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(out, b, 0o644); err != nil {
		return err
	}
	fmt.Println("saved", out)
	return nil
}

func toHits(bed string, rs []search.Result) []evalkit.Hit {
	out := make([]evalkit.Hit, 0, len(rs))
	for _, r := range rs {
		rel, err := filepath.Rel(bed, r.File.Path)
		if err != nil {
			continue
		}
		out = append(out, evalkit.Hit{Rel: filepath.ToSlash(rel), Page: r.Locator.Page})
	}
	return out
}

func floats(list string) ([]float64, error) {
	var out []float64
	for _, s := range strings.Split(list, ",") {
		var f float64
		if _, err := fmt.Sscan(strings.TrimSpace(s), &f); err != nil {
			return nil, fmt.Errorf("bad number %q", s)
		}
		out = append(out, f)
	}
	return out, nil
}
