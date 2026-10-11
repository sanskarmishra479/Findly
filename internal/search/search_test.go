//go:build windows

package search

import (
	"context"
	"fmt"
	"math"
	"math/rand/v2"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/sanskarmishra479/findly/internal/embed"
	"github.com/sanskarmishra479/findly/internal/store"
	"github.com/sanskarmishra479/findly/internal/vecfmt"
)

// vecFor makes a deterministic 768-d vector for a "topic" so tests control similarity.
func vecFor(topic int, noise float64, r *rand.Rand) []float32 {
	v := make([]float32, 768)
	for i := range v {
		v[i] = float32(math.Sin(float64(topic*97+i)) + noise*r.NormFloat64())
	}
	return v
}

func toStore(v []float32) *store.Vector {
	b, q, err := vecfmt.Encode(v)
	if err != nil {
		panic(err)
	}
	return &store.Vector{Bin: b, I8: q.I8, Scale: q.Scale}
}

type fakeQE struct{ v []float32 }

func (f fakeQE) EmbedQuery(context.Context, string) (embed.Vector, error) {
	b, q, err := vecfmt.Encode(f.v)
	return embed.Vector{Bin: b, Q: q}, err
}

type fixture struct {
	st   *store.Store
	root store.Root
	ids  map[string]int64
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	root, _ := st.AddRoot(context.Background(), `C:\Docs`)
	return &fixture{st: st, root: root, ids: map[string]int64{}}
}

// add stores a file whose passages each get a topic vector (topic < 0 = no vector).
func (fx *fixture) add(t *testing.T, path, kind string, mtime time.Time, passages map[string]int) int64 {
	t.Helper()
	ctx := context.Background()
	id, _, err := fx.st.UpsertFile(ctx, store.File{
		RootID: fx.root.ID, Path: path, Name: filepath.Base(path), Ext: filepath.Ext(path), Kind: kind, Size: 1, MTime: mtime.UnixNano(),
	})
	if err != nil {
		t.Fatal(err)
	}
	r := rand.New(rand.NewPCG(uint64(len(path)), 7))
	var items []store.Item
	i := 0
	for text, topic := range passages {
		it := store.Item{Kind: store.ItemPassage, Locator: fmt.Sprintf(`{"page":%d,"chunk":%d}`, i+1, i), Text: text}
		if topic >= 0 {
			it.Vector = toStore(vecFor(topic, 0.3, r))
		}
		items = append(items, it)
		i++
	}
	if err := fx.st.ReplaceContent(ctx, id, items); err != nil {
		t.Fatal(err)
	}
	fx.st.SetFileResult(ctx, id, store.StatusDone, "", "", "")
	fx.ids[filepath.Base(path)] = id
	return id
}

func (fx *fixture) searcher(t *testing.T, qv []float32) *Searcher {
	t.Helper()
	c := &VectorCache{}
	if err := c.Load(context.Background(), fx.st); err != nil {
		t.Fatal(err)
	}
	s := &Searcher{Store: fx.st, Cache: c}
	if qv != nil {
		s.Embedder = fakeQE{qv}
	}
	return s
}

func names(rs []Result) []string {
	var out []string
	for _, r := range rs {
		out = append(out, r.File.Name)
	}
	return out
}

const (
	topicPets = 1
	topicTax  = 2
	topicCake = 3
)

func standard(t *testing.T) *fixture {
	fx := newFixture(t)
	now := time.Now()
	fx.add(t, `C:\Docs\lease.pdf`, "pdf", now, map[string]int{"Tenants may not keep dogs or cats in the flat.": topicPets})
	fx.add(t, `C:\Docs\tax-2024.pdf`, "pdf", now.Add(-400*24*time.Hour), map[string]int{"Income tax return for 2024, invoice INV-2024-0042.": topicTax})
	fx.add(t, `C:\Docs\Recipes\cake.docx`, "doc", now, map[string]int{"Bake the sponge for forty minutes.": topicCake})
	fx.add(t, `C:\Docs\invoice_march.txt`, "text", now, map[string]int{"Payment declined: error 0x80070005": -1})
	return fx
}

func TestKeywordFindsWordsAndNames(t *testing.T) {
	fx := standard(t)
	s := fx.searcher(t, nil)
	ctx := context.Background()
	cases := map[string]string{
		"dogs":           "lease.pdf",
		"INV-2024-0042":  "tax-2024.pdf",
		"0x80070005":     "invoice_march.txt",
		"invoice march":  "invoice_march.txt", // file name words
		"sponge minutes": "cake.docx",
	}
	for q, want := range cases {
		rs, err := s.Keyword(ctx, q, store.Filter{}, false)
		if err != nil {
			t.Fatalf("%q: %v", q, err)
		}
		if len(rs) == 0 || rs[0].File.Name != want {
			t.Errorf("%q: got %v, want %s first", q, names(rs), want)
		}
	}
}

func TestKeywordPrefixWhileTyping(t *testing.T) {
	s := standard(t).searcher(t, nil)
	rs, _ := s.Keyword(context.Background(), "spon", store.Filter{}, true)
	if len(rs) != 1 || rs[0].File.Name != "cake.docx" {
		t.Fatalf("got %v", names(rs))
	}
	if rs, _ := s.Keyword(context.Background(), "spon", store.Filter{}, false); len(rs) != 0 {
		t.Fatalf("prefix matched without typing: %v", names(rs))
	}
}

// User input must never be interpreted as FTS5 syntax (no errors, no injection).
func TestKeywordHostileInput(t *testing.T) {
	s := standard(t).searcher(t, nil)
	for _, q := range []string{
		`NEAR(dogs cats)`, `name:lease`, `"unclosed`, `***`, `dogs OR cats`, `-dogs`, `^dogs`,
		`dogs AND`, `"" ""`, `'); DROP TABLE files; --`, `{a b}`, strings.Repeat("a ", 1000),
		"\x00dogs", "😀", "",
	} {
		if _, err := s.Keyword(context.Background(), q, store.Filter{}, true); err != nil {
			t.Errorf("%q: %v", q, err)
		}
	}
	// "OR" is just a word: "dogs OR pizza" requires all three words, so nothing matches.
	rs, _ := s.Keyword(context.Background(), "dogs OR pizza", store.Filter{}, false)
	if len(rs) != 0 {
		t.Errorf("OR was treated as an operator: %v", names(rs))
	}
}

func TestFilters(t *testing.T) {
	s := standard(t).searcher(t, nil)
	ctx := context.Background()
	rs, _ := s.Keyword(ctx, "pdf", store.Filter{Kinds: []string{"doc"}}, false)
	if len(rs) != 0 {
		t.Errorf("kind filter: %v", names(rs))
	}
	rs, _ = s.Keyword(ctx, "sponge", store.Filter{Folder: `c:\docs\recipes`}, false)
	if len(rs) != 1 {
		t.Errorf("folder filter: %v", names(rs))
	}
	rs, _ = s.Keyword(ctx, "dogs", store.Filter{Folder: `C:\Docs\Rec`}, false) // partial name is not a folder
	if len(rs) != 0 {
		t.Errorf("partial folder matched: %v", names(rs))
	}
	rs, _ = s.Keyword(ctx, "tax", store.Filter{After: time.Now().Add(-30 * 24 * time.Hour).UnixNano()}, false)
	if len(rs) != 0 {
		t.Errorf("date filter: %v", names(rs))
	}
}

func TestSemanticFindsByMeaning(t *testing.T) {
	fx := standard(t)
	r := rand.New(rand.NewPCG(99, 1))
	s := fx.searcher(t, vecFor(topicPets, 0.3, r)) // "can I have a pet?" ≈ pets topic
	rs, err := s.Semantic(context.Background(), "can I have a pet in my apartment", store.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) == 0 || rs[0].File.Name != "lease.pdf" || !rs[0].Meaning {
		t.Fatalf("got %v", names(rs))
	}
	if rs[0].Locator.Page != 1 {
		t.Errorf("locator = %+v", rs[0].Locator)
	}
	// Filters apply to semantic results too.
	rs, _ = s.Semantic(context.Background(), "pets", store.Filter{Kinds: []string{"doc"}})
	for _, x := range rs {
		if x.File.Kind != "doc" {
			t.Fatalf("filter ignored: %v", names(rs))
		}
	}
}

func TestSearchFusesAndBoostsExact(t *testing.T) {
	fx := standard(t)
	r := rand.New(rand.NewPCG(5, 5))
	s := fx.searcher(t, vecFor(topicTax, 0.3, r))
	rs, err := s.Search(context.Background(), "INV-2024-0042", store.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rs) == 0 || rs[0].File.Name != "tax-2024.pdf" || !rs[0].Exact || !rs[0].Keyword || !rs[0].Meaning {
		t.Fatalf("got %+v", rs)
	}
}

func TestQuotedPhraseIsRequired(t *testing.T) {
	fx := standard(t)
	r := rand.New(rand.NewPCG(5, 6))
	s := fx.searcher(t, vecFor(topicPets, 0.3, r))
	rs, _ := s.Search(context.Background(), `pets "income tax"`, store.Filter{})
	for _, x := range rs {
		if x.File.Name == "lease.pdf" {
			t.Fatalf("semantic hit without the quoted phrase kept: %v", names(rs))
		}
	}
}

func TestSearchWithoutEmbedderStillWorks(t *testing.T) {
	s := standard(t).searcher(t, nil)
	rs, err := s.Search(context.Background(), "dogs", store.Filter{})
	if err != nil || len(rs) != 1 {
		t.Fatalf("got %v %v", names(rs), err)
	}
}

func TestDeletedFileDisappears(t *testing.T) {
	fx := standard(t)
	r := rand.New(rand.NewPCG(1, 1))
	s := fx.searcher(t, vecFor(topicPets, 0.3, r))
	fx.st.DeleteFile(context.Background(), `C:\Docs\lease.pdf`)
	// Even before the cache is reloaded, the deleted file must not be returned.
	rs, _ := s.Search(context.Background(), "dogs cats pets", store.Filter{})
	for _, x := range rs {
		if x.File.Name == "lease.pdf" {
			t.Fatal("deleted file returned")
		}
	}
}

func TestParse(t *testing.T) {
	q := Parse(`invoice "INV-2024" from march "unclosed`)
	if strings.Join(q.Words, ",") != "invoice,from,march,unclosed" || len(q.Phrases) != 1 || q.Phrases[0] != "INV 2024" {
		t.Fatalf("got %+v", q)
	}
	if m := q.Match(true); m != `"invoice" AND "from" AND "march" AND "unclosed" AND "INV 2024"` {
		t.Fatalf("Match = %s", m)
	}
}

func TestSnippet(t *testing.T) {
	text := strings.Repeat("filler words here. ", 30) + "The KEY sentence with the answer." + strings.Repeat(" more", 50)
	s := snippet(text, Parse("key"))
	if !strings.Contains(s, "KEY sentence") || !strings.HasPrefix(s, "…") || len(s) > snippetLength+10 {
		t.Fatalf("snippet = %q", s)
	}
}

// Nearest must agree with brute force on the int8 ranking of its candidates.
func TestNearestMatchesBruteForce(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 3))
	c := &VectorCache{}
	for i := range 5000 {
		b, q, _ := vecfmt.Encode(vecFor(i%50, 1.0, r))
		c.items = append(c.items, int64(i))
		c.files = append(c.files, int64(i))
		c.bins = append(c.bins, b)
		c.q = append(c.q, q)
	}
	b, q, _ := vecfmt.Encode(vecFor(7, 1.0, r))
	qv := embed.Vector{Bin: b, Q: q}
	got := c.Nearest(&qv, Candidates)
	if len(got) != Candidates {
		t.Fatalf("got %d", len(got))
	}
	// Brute-force Hamming top-200 set.
	type hd struct{ i, d int }
	all := make([]hd, len(c.bins))
	for i := range c.bins {
		all[i] = hd{i, vecfmt.Hamming(&qv.Bin, &c.bins[i])}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].d < all[j].d })
	limit := all[Candidates-1].d
	for _, s := range got {
		if vecfmt.Hamming(&qv.Bin, &c.bins[s.ItemID]) > limit {
			t.Fatalf("item %d is not among the %d nearest", s.ItemID, Candidates)
		}
	}
	for i := 1; i < len(got); i++ {
		if got[i].Score > got[i-1].Score {
			t.Fatal("not sorted by score")
		}
	}
}

// Plan 12: binary scan of 100k vectors + int8 re-rank of 200 must take < 10 ms.
func BenchmarkNearest100k(b *testing.B) {
	r := rand.New(rand.NewPCG(4, 4))
	c := &VectorCache{}
	for i := range 100_000 {
		v := make([]float32, 768)
		for j := range v {
			v[j] = float32(r.NormFloat64())
		}
		bin, q, _ := vecfmt.Encode(v)
		c.items = append(c.items, int64(i))
		c.files = append(c.files, int64(i))
		c.bins = append(c.bins, bin)
		c.q = append(c.q, q)
	}
	bin, q, _ := vecfmt.Encode(vecFor(1, 1, r))
	qv := embed.Vector{Bin: bin, Q: q}
	b.ResetTimer()
	for b.Loop() {
		c.Nearest(&qv, Candidates)
	}
}
