//go:build windows

package extract

import (
	"bytes"
	"context"
	"image"
	"image/jpeg"
	"os"
	"testing"
	"time"
)

func TestRenderPDFPageHighlights(t *testing.T) {
	t.Cleanup(func() { ClosePreviews() })
	p := write(t, "lease.pdf", makePDF("The clause about pets", "", "Rent is due; rental terms apply"))
	pv, err := RenderPDFPage(context.Background(), p, 3, []string{"rent", "DUE"})
	if err != nil {
		t.Fatal(err)
	}
	if pv.Page != 3 || pv.Pages != 3 {
		t.Fatalf("page %d of %d, want 3 of 3", pv.Page, pv.Pages)
	}
	// "Rent" and "due" are highlighted; "rental" is a longer word and is not.
	if pv.Highlights != 2 {
		t.Fatalf("highlights = %d, want 2", pv.Highlights)
	}
	img, err := jpeg.Decode(bytes.NewReader(pv.JPEG))
	if err != nil {
		t.Fatal(err)
	}
	if w, h := img.Bounds().Dx(), img.Bounds().Dy(); w != PreviewWidth || h != int(float64(PreviewWidth)*aspect()) {
		t.Fatalf("preview is %dx%d", w, h)
	}
	if yellow(img) < 200 {
		t.Fatalf("only %d highlighted pixels", yellow(img))
	}
	// Past the last page: the last page is shown.
	if pv, err := RenderPDFPage(context.Background(), p, 99, nil); err != nil || pv.Page != 3 || pv.Highlights != 0 {
		t.Fatalf("page 99: %+v %v", pv, err)
	}
}

func TestRenderPDFPageBadFile(t *testing.T) {
	t.Cleanup(func() { ClosePreviews() })
	p := write(t, "garbage.pdf", []byte("%PDF-1.4\nthis is not really a pdf"))
	if _, err := RenderPDFPage(context.Background(), p, 1, nil); err == nil {
		t.Fatal("a broken PDF rendered")
	}
}

// yellow counts pixels that look like the marker over white paper.
func yellow(img image.Image) int {
	n := 0
	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r, g, bl, _ := img.At(x, y).RGBA()
			if r>>8 > 200 && g>>8 > 180 && bl>>8 < 170 {
				n++
			}
		}
	}
	return n
}

func TestCropAround(t *testing.T) {
	page := image.Rect(0, 0, 760, 984)
	h := int(760 * aspect())
	if c := cropAround(page, nil); c.Min.Y != 0 || c.Dy() != h {
		t.Errorf("no highlight: %v, want the top", c)
	}
	// A match near the bottom: the crop ends at the page bottom, never past it.
	if c := cropAround(page, []image.Rectangle{image.Rect(10, 950, 50, 970)}); c.Max.Y != 984 || c.Dy() != h {
		t.Errorf("bottom match: %v", c)
	}
	// A match in the middle sits a third of the way down.
	if c := cropAround(page, []image.Rectangle{image.Rect(10, 500, 50, 520)}); c.Min.Y != 500-h/3 {
		t.Errorf("middle match: %v", c)
	}
}

// aspect returns the constant as a variable: Go refuses to convert the constant 471.2
// to int at compile time, but truncates a variable like the code under test does.
func aspect() float64 { return previewAspect }

func TestCompileCacheSpeedsUpStart(t *testing.T) {
	dir := t.TempDir()
	if err := SetCompileCache(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ClosePreviews()
		cacheMu.Lock()
		cache = nil
		cacheMu.Unlock()
	})
	p := write(t, "a.pdf", makePDF("hello world"))
	start := time.Now()
	if _, err := RenderPDFPage(context.Background(), p, 1, nil); err != nil {
		t.Fatal(err)
	}
	cold := time.Since(start)
	ClosePreviews()
	start = time.Now()
	if _, err := RenderPDFPage(context.Background(), p, 1, nil); err != nil {
		t.Fatal(err)
	}
	warm := time.Since(start)
	t.Logf("first start (compile): %v, second start (from cache): %v", cold, warm)
	if entries, _ := os.ReadDir(dir); len(entries) == 0 {
		t.Fatal("nothing was cached")
	}
	if warm > cold/2 {
		t.Errorf("the cache did not speed up the start: %v vs %v", warm, cold)
	}
}
