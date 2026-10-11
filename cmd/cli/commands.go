//go:build windows

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strings"
	"time"

	"github.com/sanskarmishra479/findly/internal/index"
	"github.com/sanskarmishra479/findly/internal/safefs"
	"github.com/sanskarmishra479/findly/internal/search"
	"github.com/sanskarmishra479/findly/internal/store"
)

// cli index <folder>: add the folder (if new) and index it.
func runIndex(args []string) error {
	fs := flag.NewFlagSet("index", flag.ExitOnError)
	var ef engineFlags
	ef.register(fs)
	unlimited := fs.Bool("unlimited", false, "DEV ONLY: ignore the free-version limit (the CLI is not shipped)")
	fs.Parse(args)
	if fs.NArg() != 1 {
		return errors.New("usage: cli index [flags] <folder>")
	}
	folder, err := safefs.Canonical(fs.Arg(0))
	if err != nil {
		return fmt.Errorf("folder %s: %w", fs.Arg(0), err)
	}

	en, err := openEngine(ef)
	if err != nil {
		return err
	}
	defer en.close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	root, err := en.store.AddRoot(ctx, folder)
	if err != nil {
		return err
	}
	if err := en.startEmbedder(ef, true); err != nil {
		return err
	}

	ix := &index.Indexer{
		Store:    en.store,
		Embedder: en.emb,
		Licensed: func() bool { return *unlimited },
		OnProgress: func(p index.Progress) {
			fmt.Fprintf(os.Stderr, "\rindexing %d / %d  (failed %d, over free limit %d)   ", p.Done, p.Total, p.Failed, p.Skipped)
		},
	}
	start := time.Now()
	st, err := ix.IndexRoot(ctx, root)
	fmt.Fprintln(os.Stderr)
	en.removeThumbs(st.Thumbs)
	if err != nil {
		return err
	}
	fmt.Printf("folder:   %s\n", root.Path)
	fmt.Printf("found:    %s\n", formatCounts(st.Scan.Counts))
	fmt.Printf("indexed:  %d   failed: %d   over free limit: %d   postponed: %d   removed: %d\n",
		st.Indexed, st.Failed, st.Skipped, st.Postponed, st.Deleted)
	if st.Deferred > 0 {
		fmt.Printf("long PDFs with pages left for later: %d\n", st.Deferred)
	}
	fmt.Printf("skipped:  %d secret-looking, %d cloud-only, %d links, %d too large, %d unreadable folders\n",
		st.Scan.Secret, st.Scan.CloudOnly, st.Scan.Links, st.Scan.TooLarge, st.Scan.Errors)
	fmt.Printf("time:     %v\n", time.Since(start).Round(time.Millisecond))
	return nil
}

// cli search "<query>": keyword + semantic search over everything indexed.
func runSearch(args []string) error {
	fs := flag.NewFlagSet("search", flag.ExitOnError)
	var ef engineFlags
	ef.register(fs)
	keywordOnly := fs.Bool("keyword", false, "keyword search only (no model needed)")
	kinds := fs.String("kind", "", "only these kinds, comma-separated: text,doc,pdf,image,audio,video")
	folder := fs.String("folder", "", "only files under this folder")
	limit := fs.Int("n", 10, "results to show")
	fs.Parse(args)
	if fs.NArg() == 0 {
		return errors.New(`usage: cli search [flags] "<query>"`)
	}
	query := strings.Join(fs.Args(), " ")

	en, err := openEngine(ef)
	if err != nil {
		return err
	}
	defer en.close()
	ctx := context.Background()

	s := &search.Searcher{Store: en.store}
	if !*keywordOnly {
		if err := en.startEmbedder(ef, false); err != nil {
			return err
		}
		s.Embedder = en.emb
		s.Cache = &search.VectorCache{}
		if err := s.Cache.Load(ctx, en.store); err != nil {
			return err
		}
	}
	fl := store.Filter{Folder: *folder}
	if *kinds != "" {
		fl.Kinds = strings.Split(*kinds, ",")
	}

	start := time.Now()
	results, err := s.Search(ctx, query, fl)
	took := time.Since(start)
	if err != nil && len(results) == 0 {
		return err
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "note: meaning search unavailable:", err)
	}
	if len(results) == 0 {
		fmt.Println("no results")
		return nil
	}
	for i, r := range results {
		if i == *limit {
			break
		}
		how := []string{}
		if r.Keyword {
			how = append(how, "words")
		}
		if r.Meaning {
			how = append(how, "meaning")
		}
		if r.Exact {
			how = append(how, "exact")
		}
		where := ""
		if r.Locator.Page > 0 {
			where = fmt.Sprintf("  (page %d)", r.Locator.Page)
		}
		fmt.Printf("%2d. %s%s\n    [%s]  %s\n", i+1, r.File.Path, where, strings.Join(how, "+"), r.Snippet)
	}
	fmt.Fprintf(os.Stderr, "%d results in %v\n", len(results), took.Round(time.Millisecond))
	return nil
}

// cli stats: what is in the index.
func runStats(args []string) error {
	fs := flag.NewFlagSet("stats", flag.ExitOnError)
	var ef engineFlags
	ef.register(fs)
	fs.Parse(args)
	en, err := openEngine(ef)
	if err != nil {
		return err
	}
	defer en.close()
	ctx := context.Background()
	roots, err := en.store.Roots(ctx)
	if err != nil {
		return err
	}
	c, err := en.store.Count(ctx)
	if err != nil {
		return err
	}
	used, err := en.store.CountContentIndexed(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("index:    %s\n", en.fs.Dir())
	fmt.Println("folders:")
	for _, r := range roots {
		fmt.Printf("  [%d] %s\n", r.ID, r.Path)
	}
	fmt.Printf("files:    by kind %s\n", formatCounts(c.ByKind))
	fmt.Printf("          by status %s\n", formatCounts(c.ByStatus))
	fmt.Printf("content:  %d passages, %d vectors, %d files with content (free version: %d)\n",
		c.Items, c.Vectors, used, index.FreeLimit)
	return nil
}

// cli remove <folder>: remove a folder and everything indexed from it (P-5).
func runRemove(args []string) error {
	fs := flag.NewFlagSet("remove", flag.ExitOnError)
	var ef engineFlags
	ef.register(fs)
	fs.Parse(args)
	if fs.NArg() != 1 {
		return errors.New("usage: cli remove <folder>")
	}
	en, err := openEngine(ef)
	if err != nil {
		return err
	}
	defer en.close()
	ctx := context.Background()
	roots, err := en.store.Roots(ctx)
	if err != nil {
		return err
	}
	want := strings.ToUpper(strings.TrimRight(fs.Arg(0), `\`))
	if c, err := safefs.Canonical(fs.Arg(0)); err == nil {
		want = strings.ToUpper(c)
	}
	for _, r := range roots {
		if strings.ToUpper(r.Path) == want {
			thumbs, err := en.store.RemoveRoot(ctx, r.ID)
			if err != nil {
				return err
			}
			en.removeThumbs(thumbs)
			fmt.Println("removed", r.Path, "and everything indexed from it")
			return nil
		}
	}
	return fmt.Errorf("%s is not an indexed folder (see cli stats)", fs.Arg(0))
}

func formatCounts(m map[string]int) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s %d", k, m[k]))
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, ", ")
}
