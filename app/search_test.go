package main

import (
	"context"
	"image"
	"strings"
	"testing"

	"github.com/sanskarmishra479/findly/internal/extract"
)

func TestShortFolder(t *testing.T) {
	home := `C:\Users\sansk`
	cases := map[string]string{
		`C:\Users\sansk\Documents\Home`: `~\Documents\Home`,
		`C:\Users\sansk`:                `~`,
		`D:\Work`:                       `D:\Work`,
		`C:\Users\other\Documents`:      `C:\Users\other\Documents`,
	}
	for dir, want := range cases {
		if got := shortFolder(dir, home); got != want {
			t.Errorf("shortFolder(%q) = %q, want %q", dir, got, want)
		}
	}
	if got := shortFolder(`D:\Work`, ""); got != `D:\Work` {
		t.Errorf("no home: %q", got)
	}
}

func TestPageOf(t *testing.T) {
	for loc, want := range map[string]int{`{"chunk":2,"page":7}`: 7, `{"chunk":0}`: 0, ``: 0, `{"page":12}`: 12} {
		if got := pageOf(loc); got != want {
			t.Errorf("pageOf(%q) = %d, want %d", loc, got, want)
		}
	}
}

func TestQueryWords(t *testing.T) {
	if got := strings.Join(queryWords(`rent "due date" may`), "|"); got != "rent|may|due date" {
		t.Errorf("queryWords = %q", got)
	}
}

func TestShrink(t *testing.T) {
	big := image.NewRGBA(image.Rect(0, 0, 4000, 3000))
	if b := shrink(big, 760).Bounds(); b.Dx() != 760 || b.Dy() != 570 {
		t.Errorf("shrunk to %v", b)
	}
	small := image.NewRGBA(image.Rect(0, 0, 300, 200))
	if shrink(small, 760) != image.Image(small) {
		t.Error("a small image was scaled")
	}
}

func TestPageCacheKeepsRecent(t *testing.T) {
	var c pageCache
	for i := range pageCacheSize + 5 {
		c.put(string(rune('a'+i)), extract.PagePreview{Page: i})
	}
	if _, ok := c.get("a"); ok {
		t.Error("the oldest entry was kept beyond the cache size")
	}
	if pv, ok := c.get(string(rune('a' + pageCacheSize + 4))); !ok || pv.Page != pageCacheSize+4 {
		t.Error("the newest entry is missing")
	}
}

func TestNewSearchCancelsOldOfSameKind(t *testing.T) {
	var s searches
	words1, _ := s.start(context.Background(), false)
	meaning1, _ := s.start(context.Background(), true)
	words2, _ := s.start(context.Background(), false)
	if words1.Err() == nil {
		t.Error("an outdated word search was not cancelled")
	}
	if meaning1.Err() != nil {
		t.Error("a word search cancelled the meaning search")
	}
	if words2.Err() != nil {
		t.Error("the newest search was cancelled")
	}
}
