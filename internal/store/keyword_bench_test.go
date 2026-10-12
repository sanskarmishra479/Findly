package store

import (
	"context"
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"strings"
	"testing"
)

// Plan 12: keyword results must arrive in < 30 ms (mid laptop) / < 50 ms (low-end) per
// keystroke. This builds 5,000 files × 10 passages = 50,000 passages once and measures
// a common word, a two-letter prefix and a rare word.
func BenchmarkKeyword50k(b *testing.B) {
	s, err := Open(filepath.Join(b.TempDir(), "index.db"))
	if err != nil {
		b.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	root, _ := s.AddRoot(ctx, `C:\Bench`)
	r := rand.New(rand.NewPCG(1, 2))
	vocab := make([]string, 20000)
	for i := range vocab {
		vocab[i] = fmt.Sprintf("w%dx", i)
	}
	for f := range 5000 {
		id, _, err := s.UpsertFile(ctx, File{RootID: root.ID, Path: fmt.Sprintf(`C:\Bench\%d.txt`, f), Name: fmt.Sprintf("%d.txt", f), Ext: ".txt", Kind: "text", Size: 1, MTime: 1})
		if err != nil {
			b.Fatal(err)
		}
		items := make([]Item, 10)
		for i := range items {
			words := make([]string, 60)
			for w := range words {
				words[w] = vocab[r.IntN(len(vocab))]
			}
			if r.IntN(2) == 0 {
				words[0] = "budget" // a common word: in ~50% of passages
			}
			items[i] = Item{Kind: ItemPassage, Text: strings.Join(words, " ")}
		}
		if err := s.ReplaceContent(ctx, id, items); err != nil {
			b.Fatal(err)
		}
	}
	for _, q := range []struct {
		name, match string
		ranked      bool
	}{
		{"common ranked", `"budget"`, true},
		{"common typing", `"budget"`, false},
		{"prefix2 typing", `"w1"*`, false},
		{"rare ranked", `"w12345x"`, true},
	} {
		b.Run(q.name, func(b *testing.B) {
			for b.Loop() {
				if _, err := s.KeywordSearch(ctx, q.match, Filter{}, 50, q.ranked); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
