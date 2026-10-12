//go:build windows

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/sanskarmishra479/findly/internal/appfs"
	"github.com/sanskarmishra479/findly/internal/index"
	"github.com/sanskarmishra479/findly/internal/safefs"
	"github.com/sanskarmishra479/findly/internal/scan"
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
	folder, err := absFolder(fs.Arg(0)) // relative paths like "docs" work too
	if err != nil {
		return err
	}
	if info, err := safefs.Stat(folder); err != nil || !info.IsDir() {
		return fmt.Errorf("%s is not a folder", fs.Arg(0))
	}

	// Checked again when saving; here it saves waiting for the model to start.
	if scan.SecretFolder(folder) {
		return fmt.Errorf("%w: %s", scan.ErrSecretRoot, folder)
	}

	en, err := openEngine(ef)
	if err != nil {
		return err
	}
	defer en.close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	// Start the model first: if its files are missing, no empty folder entry is left behind.
	if err := en.startEmbedder(ctx, ef, true); err != nil {
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
	root, err := ix.AddRoot(ctx, folder)
	if err != nil {
		return err
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
	after := fs.String("after", "", "only files modified on or after this date (YYYY-MM-DD)")
	before := fs.String("before", "", "only files modified before this date (YYYY-MM-DD)")
	fs.Parse(args)
	if fs.NArg() == 0 {
		return errors.New(`usage: cli search [flags] "<query>"`)
	}
	// Go's flag package stops at the first non-flag argument, so in
	// `cli search "rent due" -n 1` the "-n 1" would silently become search words.
	// A word like "-5" is a query word; only names of real flags are refused.
	for _, a := range fs.Args()[1:] {
		name, _, _ := strings.Cut(strings.TrimLeft(a, "-"), "=")
		if strings.HasPrefix(a, "-") && fs.Lookup(name) != nil {
			return fmt.Errorf(`flag %s is after the query; put flags first, e.g. cli search -n 5 "your words"`, a)
		}
	}
	query := strings.Join(fs.Args(), " ")
	if *limit < 1 {
		return errors.New("-n must be at least 1")
	}
	for _, k := range strings.Split(*kinds, ",") {
		if k = strings.ToLower(strings.TrimSpace(k)); k != "" && !validKinds[k] {
			return fmt.Errorf("unknown -kind %q (use text, doc, pdf, image, audio, video, other)", k)
		}
	}
	var fl store.Filter
	// Dates are local days: -after 2025-03-01 starts at midnight here, not in UTC.
	for _, d := range []struct {
		name, val string
		dst       *int64
	}{{"after", *after, &fl.After}, {"before", *before, &fl.Before}} {
		if d.val == "" {
			continue
		}
		ns, err := dayStart(d.val, time.Local)
		if err != nil {
			return fmt.Errorf("-%s %q: use a date like 2025-03-31", d.name, d.val)
		}
		*d.dst = ns
	}
	if fl.After != 0 && fl.Before != 0 && fl.Before <= fl.After {
		return errors.New("-before must be later than -after")
	}

	en, err := openEngine(ef)
	if err != nil {
		return err
	}
	defer en.close()
	ctx := context.Background()

	s := &search.Searcher{Store: en.store}
	if !*keywordOnly {
		if err := en.startEmbedder(ctx, ef, false); err != nil {
			return err
		}
		s.Embedder = en.emb
		s.Cache = &search.VectorCache{}
		if err := s.Cache.Load(ctx, en.store); err != nil {
			return err
		}
	}
	if *folder != "" {
		if fl.Folder, err = absFolder(*folder); err != nil {
			return err
		}
	}
	for _, k := range strings.Split(*kinds, ",") {
		if k = strings.ToLower(strings.TrimSpace(k)); k != "" {
			fl.Kinds = append(fl.Kinds, k)
		}
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
	abs, err := absFolder(fs.Arg(0))
	if err != nil {
		return err
	}
	want := safefs.PathKey(abs)
	for _, r := range roots {
		if safefs.PathKey(r.Path) == want {
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

// cli reset: delete the index (database and thumbnails) so it can be rebuilt (plan I-7,
// "rebuild index"). Only files inside the app data folder are touched.
func runReset(args []string) error {
	fs := flag.NewFlagSet("reset", flag.ExitOnError)
	var ef engineFlags
	ef.register(fs)
	fs.Parse(args)
	dir, err := ef.dataPath()
	if err != nil {
		return err
	}
	afs, err := appfs.Open(dir)
	if err != nil {
		return err
	}
	defer afs.Close()
	db := filepath.Join(appfs.DirIndex, "index.db")
	for _, p := range store.DatabaseFiles(db) {
		if err := afs.Remove(p); err != nil {
			return err
		}
	}
	if err := afs.RemoveAll(appfs.DirThumbs); err != nil {
		return err
	}
	if err := afs.MkdirAll(appfs.DirThumbs); err != nil {
		return err
	}
	fmt.Println("index deleted. Add your folders again with: cli index <folder>")
	return nil
}

// cli check: read the whole index looking for damage (plan I-7). Slow on a big index,
// which is why it is not done on every start.
func runCheck(args []string) error {
	fs := flag.NewFlagSet("check", flag.ExitOnError)
	var ef engineFlags
	ef.register(fs)
	fs.Parse(args)
	en, err := openEngine(ef)
	if err != nil {
		return err
	}
	defer en.close()
	start := time.Now()
	if err := en.store.Verify(context.Background()); err != nil {
		return err
	}
	fmt.Printf("index is healthy (checked in %v)\n", time.Since(start).Round(time.Millisecond))
	return nil
}

var validKinds = map[string]bool{"text": true, "doc": true, "pdf": true, "image": true, "audio": true, "video": true, "other": true}

// dayStart returns the first moment of the day "YYYY-MM-DD" in loc, in unix nanoseconds
// (at least 1, since 0 means "no filter" in store.Filter).
func dayStart(day string, loc *time.Location) (int64, error) {
	t, err := time.ParseInLocation("2006-01-02", day, loc)
	if err != nil {
		return 0, err
	}
	// UnixNano only covers 1678–2262; outside that it returns nonsense.
	if t.Year() < 1970 || t.Year() > 2200 {
		return 0, errors.New("year out of range")
	}
	// Where clocks jump at midnight (some DST zones), Go returns a time on the day
	// before; move to the first moment that is really on the requested day.
	for t.Format("2006-01-02") != day {
		t = t.Add(15 * time.Minute)
	}
	return max(t.UnixNano(), 1), nil
}
