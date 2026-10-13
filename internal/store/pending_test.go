package store

import (
	"context"
	"testing"
)

func TestPendingPassagesGetVectorsLater(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	root, _ := s.AddRoot(ctx, `C:\Docs`)
	a := addFile(t, s, root.ID, `C:\Docs\a.txt`)
	b := addFile(t, s, root.ID, `C:\Docs\b.txt`)
	// a is stored first, so its passages are embedded first.
	s.ReplaceContent(ctx, a, []Item{
		{Kind: ItemPassage, Text: "quarterly report", Pending: true},
		{Kind: ItemPassage, Text: "12 34 56", Pending: false}, // keyword-only by design
		{Kind: ItemPassage, Text: "second passage", Pending: true},
	})
	s.ReplaceContent(ctx, b, []Item{{Kind: ItemPassage, Text: "other file", Pending: true}})

	if n, _ := s.CountPending(ctx); n != 3 {
		t.Fatalf("pending = %d, want 3", n)
	}
	// Keyword search works before any vector exists.
	if n := matches(t, s, "quarterly"); n != 1 {
		t.Fatalf("keyword rows before embedding = %d, want 1", n)
	}
	f, ok, err := s.NextPendingFile(ctx)
	if err != nil || !ok || f.ID != a || f.Name != "a.txt" {
		t.Fatalf("NextPendingFile = %+v %v %v, want a.txt", f, ok, err)
	}
	items, err := s.PendingItems(ctx, a)
	if err != nil || len(items) != 2 || items[0].Text != "quarterly report" || items[1].Text != "second passage" {
		t.Fatalf("PendingItems = %+v %v", items, err)
	}
	n, err := s.SetVectors(ctx, []ItemVector{{ID: items[0].ID, Vector: *vec(1)}, {ID: items[1].ID, Vector: *vec(2)}})
	if err != nil || n != 2 {
		t.Fatalf("SetVectors = %d %v", n, err)
	}
	if got := count(t, s, `SELECT count(*) FROM vectors`); got != 2 {
		t.Fatalf("vectors = %d, want 2", got)
	}
	if f, _, _ := s.NextPendingFile(ctx); f.ID != b {
		t.Fatalf("next file = %d, want b (%d)", f.ID, b)
	}
}

func TestSetVectorsSkipsReplacedPassages(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	root, _ := s.AddRoot(ctx, `C:\Docs`)
	a := addFile(t, s, root.ID, `C:\Docs\a.txt`)
	s.ReplaceContent(ctx, a, []Item{{Kind: ItemPassage, Text: "old text", Pending: true}})
	old, _ := s.PendingItems(ctx, a)
	// The file changes while its old passage is being embedded.
	s.ReplaceContent(ctx, a, []Item{{Kind: ItemPassage, Text: "new text", Pending: true}})
	n, err := s.SetVectors(ctx, []ItemVector{{ID: old[0].ID, Vector: *vec(1)}})
	if err != nil || n != 0 {
		t.Fatalf("a vector was stored for a replaced passage: n=%d err=%v", n, err)
	}
	if got := count(t, s, `SELECT count(*) FROM vectors`); got != 0 {
		t.Fatalf("vectors = %d, want 0", got)
	}
	if p, _ := s.CountPending(ctx); p != 1 {
		t.Fatalf("the new passage must still wait: pending = %d", p)
	}
	// A deleted file's passages are skipped too.
	cur, _ := s.PendingItems(ctx, a)
	s.DeleteFile(ctx, `C:\Docs\a.txt`)
	if n, err := s.SetVectors(ctx, []ItemVector{{ID: cur[0].ID, Vector: *vec(1)}}); err != nil || n != 0 {
		t.Fatalf("vector for a deleted file: n=%d err=%v", n, err)
	}
}

func TestMarkEmbedFailed(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	root, _ := s.AddRoot(ctx, `C:\Docs`)
	a := addFile(t, s, root.ID, `C:\Docs\a.txt`)
	s.ReplaceContent(ctx, a, []Item{{Kind: ItemPassage, Text: "poison", Pending: true}})
	if err := s.MarkEmbedFailed(ctx, a, "the model crashed"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.NextPendingFile(ctx); ok {
		t.Fatal("a failed file is still waiting (would be retried forever)")
	}
	if n := matches(t, s, "poison"); n != 1 {
		t.Fatal("a failed passage must stay keyword-searchable")
	}
	f, _, _ := s.FileByPath(ctx, `C:\Docs\a.txt`)
	if f.Error == "" {
		t.Fatal("the reason was not recorded")
	}
}

func TestCopyDuplicateWaitingAndFinished(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	root, _ := s.AddRoot(ctx, `C:\Docs`)
	src := addFile(t, s, root.ID, `C:\Docs\report.txt`)
	dup := addFile(t, s, root.ID, `C:\Docs\report copy.txt`)
	s.ReplaceContent(ctx, src, []Item{{Kind: ItemPassage, Text: "numbers", Pending: true}})
	s.SetFileResult(ctx, src, StatusDone, "", "h", "")

	if copied, _ := s.CopyDuplicate(ctx, "h", StatusDone, dup, true); copied {
		t.Fatal("finishedOnly copied from a source still waiting for vectors")
	}
	// Without finishedOnly the copy takes the text at once, still waiting for vectors.
	if copied, err := s.CopyDuplicate(ctx, "h", StatusDone, dup, false); err != nil || !copied {
		t.Fatalf("copy = %v %v", copied, err)
	}
	if n, _ := s.CountPending(ctx); n != 2 {
		t.Fatalf("pending after copy = %d, want 2 (source and copy)", n)
	}
	// Once the source has its vectors, the copy can take them instead of embedding.
	items, _ := s.PendingItems(ctx, src)
	s.SetVectors(ctx, []ItemVector{{ID: items[0].ID, Vector: *vec(3)}})
	if copied, err := s.CopyDuplicate(ctx, "h", StatusDone, dup, true); err != nil || !copied {
		t.Fatalf("finished copy = %v %v", copied, err)
	}
	if n, _ := s.CountPending(ctx); n != 0 {
		t.Fatalf("pending after finished copy = %d, want 0", n)
	}
	if got := count(t, s, `SELECT count(*) FROM vectors`); got != 2 {
		t.Fatalf("vectors = %d, want 2", got)
	}
}
