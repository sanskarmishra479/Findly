package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"os"
	"path/filepath"
	"runtime"
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

// T4 review: a tiny file can claim a huge 16-bit image (8 bytes per pixel). It must be
// refused from its header alone, before anything is decoded.
func TestImagePreviewRefusesHugeDecode(t *testing.T) {
	var b bytes.Buffer
	b.WriteString("\x89PNG\r\n\x1a\n")
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:], 7000) // width
	binary.BigEndian.PutUint32(ihdr[4:], 7000) // height
	ihdr[8], ihdr[9] = 16, 6                   // 16 bits per channel, RGBA
	binary.Write(&b, binary.BigEndian, uint32(len(ihdr)))
	chunk := append([]byte("IHDR"), ihdr...)
	b.Write(chunk)
	binary.Write(&b, binary.BigEndian, crc32.ChecksumIEEE(chunk))
	path := filepath.Join(t.TempDir(), "huge.png")
	os.WriteFile(path, b.Bytes(), 0o644)

	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	_, err := imagePreview(context.Background(), path)
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	if !errors.Is(err, extract.ErrTooLarge) {
		t.Fatalf("got %v, want ErrTooLarge", err)
	}
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 16<<20 {
		t.Fatalf("refusing it allocated %d MB", grew>>20)
	}
	// A cancelled preview does nothing.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := imagePreview(ctx, path); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled: got %v", err)
	}
}

func TestBytesPerPixel(t *testing.T) {
	if bytesPerPixel(color.NRGBA64Model) != 8 || bytesPerPixel(color.RGBAModel) != 4 || bytesPerPixel(color.GrayModel) != 1 {
		t.Fatal("wrong bytes per pixel")
	}
}
