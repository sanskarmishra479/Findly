//go:build windows

package extract

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"io"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/klippa-app/go-pdfium"
	pdfiumerr "github.com/klippa-app/go-pdfium/errors"
	"github.com/klippa-app/go-pdfium/requests"
	"github.com/klippa-app/go-pdfium/webassembly"
	"github.com/sanskarmishra479/findly/internal/safefs"
	"github.com/tetratelabs/wazero"
)

// Page previews for the search panel use their own small PDF engine, so a preview never
// waits behind indexing (which can keep the main engine busy for minutes). Same
// sandbox as the indexing engine (no file system, no output, memory cap, stoppable),
// with a lower memory cap and short time limits: a preview is only worth showing fast.
const (
	previewMemoryPages = 4096 // 4096 × 64 KiB = 256 MiB
	// PreviewWidth is the rendered page width in pixels: sharp at 2× in the panel's
	// ~380 px preview area.
	PreviewWidth = 760
	// previewAspect is height/width of the cropped preview (the panel's preview box).
	previewAspect = 0.62
)

var (
	previewTimeout = 10 * time.Second // rendering one page
	previewWait    = 5 * time.Second  // waiting for the preview engine

	previewMu   sync.Mutex
	previewPool pdfium.Pool
)

// PagePreview is a rendered PDF page, cropped around the first highlighted word.
type PagePreview struct {
	JPEG       []byte
	Page       int // 1-based page shown
	Pages      int // pages in the document
	Highlights int // occurrences of the words that were highlighted
}

func previewEngine() (pdfium.Pool, error) {
	previewMu.Lock()
	defer previewMu.Unlock()
	if previewPool != nil {
		return previewPool, nil
	}
	pool, err := webassembly.Init(webassembly.Config{
		MinIdle:       0,
		MaxIdle:       1,
		MaxTotal:      1,
		FSConfig:      wazero.NewFSConfig(), // empty: nothing mounted
		Stdout:        io.Discard,
		Stderr:        io.Discard,
		RuntimeConfig: runtimeConfig(previewMemoryPages),
	})
	if err != nil {
		return nil, fmt.Errorf("extract: PDF preview engine: %w", err)
	}
	previewPool = pool
	return pool, nil
}

// ClosePreviews releases the preview engine (when the panel has been idle, and on exit).
// The next preview starts it again.
func ClosePreviews() error {
	previewMu.Lock()
	defer previewMu.Unlock()
	if previewPool == nil {
		return nil
	}
	err := previewPool.Close()
	previewPool = nil
	return err
}

// RenderPDFPage renders page (1-based) of the PDF at path, highlights the given words
// (whole words, any case) and crops the picture around the first highlight. The file is
// read through safefs, read-only.
func RenderPDFPage(ctx context.Context, path string, page int, words []string) (PagePreview, error) {
	if page < 1 {
		page = 1
	}
	info, err := safefs.Stat(path)
	if err != nil {
		return PagePreview{}, err
	}
	if info.Size > MaxPDFFile {
		return PagePreview{}, fmt.Errorf("%w: %s", ErrTooLarge, path)
	}
	pool, err := previewEngine()
	if err != nil {
		return PagePreview{}, err
	}
	waitCtx, cancel := context.WithTimeout(ctx, previewWait)
	inst, err := pool.GetInstanceWithContext(waitCtx)
	cancel()
	if err != nil {
		if ctx.Err() != nil {
			return PagePreview{}, ctx.Err()
		}
		return PagePreview{}, fmt.Errorf("%w: %v", ErrPDFBusy, err)
	}
	data, err := readPDF(path)
	if err != nil {
		inst.Close()
		return PagePreview{}, err
	}

	type result struct {
		p   PagePreview
		err error
	}
	done := make(chan result, 1)
	go func() {
		defer func() {
			if p := recover(); p != nil {
				done <- result{err: fmt.Errorf("%w: %v", ErrPDFCrash, p)}
			}
		}()
		p, err := renderPage(inst, data, page, words)
		done <- result{p, err}
	}()
	select {
	case r := <-done:
		if errors.Is(r.err, ErrPDFCrash) {
			inst.Kill()
		} else {
			inst.Close()
		}
		return r.p, r.err
	case <-time.After(previewTimeout):
		inst.Kill()
		return PagePreview{}, fmt.Errorf("%w (%v): %s", ErrPDFTimeout, previewTimeout, path)
	}
}

func renderPage(inst pdfium.Pdfium, data []byte, page int, words []string) (PagePreview, error) {
	opened, err := inst.OpenDocument(&requests.OpenDocument{File: &data})
	if err != nil {
		if errors.Is(err, pdfiumerr.ErrPassword) || errors.Is(err, pdfiumerr.ErrSecurity) {
			return PagePreview{}, ErrPDFPassword
		}
		return PagePreview{}, fmt.Errorf("extract: not a readable PDF: %w", err)
	}
	defer inst.FPDF_CloseDocument(&requests.FPDF_CloseDocument{Document: opened.Document})
	count, err := inst.FPDF_GetPageCount(&requests.FPDF_GetPageCount{Document: opened.Document})
	if err != nil {
		return PagePreview{}, err
	}
	page = min(page, count.PageCount)
	if page < 1 {
		return PagePreview{}, errors.New("extract: PDF has no pages")
	}
	ref := requests.Page{ByIndex: &requests.PageByIndex{Document: opened.Document, Index: page - 1}}

	rendered, err := inst.RenderPageInPixels(&requests.RenderPageInPixels{
		Page: ref, Width: PreviewWidth, Height: PreviewWidth * 3, // height: a generous cap
	})
	if err != nil {
		return PagePreview{}, fmt.Errorf("extract: render page %d: %w", page, err)
	}
	// In WebAssembly mode the pixels live in the engine's memory until Cleanup: copy them.
	src := rendered.Result.RenderedImage
	w, h := rendered.Result.Width, rendered.Result.Height
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(img, img.Bounds(), src, src.Bounds().Min, draw.Src)
	rendered.Cleanup()

	var boxes []image.Rectangle
	if len(words) > 0 {
		text, err := inst.GetPageTextStructured(&requests.GetPageTextStructured{
			Page: ref,
			Mode: requests.GetPageTextStructuredModeChars,
			PixelPositions: requests.GetPageTextStructuredPixelPositions{
				Calculate: true, Width: w, Height: h,
			},
		})
		if err == nil { // no text layer (a scan): the page is shown without highlights
			chars := make([]pageChar, 0, len(text.Chars))
			for _, c := range text.Chars {
				if c.PixelPosition == nil {
					continue
				}
				p := c.PixelPosition
				chars = append(chars, pageChar{c.Text, image.Rect(int(p.Left), int(p.Top), int(p.Right)+1, int(p.Bottom)+1)})
			}
			boxes = findWords(chars, words)
		}
	}
	highlight(img, boxes)

	crop := cropAround(img.Bounds(), boxes)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img.SubImage(crop), &jpeg.Options{Quality: 82}); err != nil {
		return PagePreview{}, err
	}
	return PagePreview{JPEG: buf.Bytes(), Page: page, Pages: count.PageCount, Highlights: len(boxes)}, nil
}

type pageChar struct {
	text string
	box  image.Rectangle
}

// findWords returns one box per whole-word occurrence of any of the words (any case).
func findWords(chars []pageChar, words []string) []image.Rectangle {
	// The page as runes, each remembering the char it came from.
	var runes []rune
	var from []int
	for i, c := range chars {
		for _, r := range strings.ToLower(c.text) {
			runes = append(runes, r)
			from = append(from, i)
		}
	}
	isWord := func(i int) bool {
		return i >= 0 && i < len(runes) && (unicode.IsLetter(runes[i]) || unicode.IsNumber(runes[i]))
	}
	var boxes []image.Rectangle
	for _, w := range words {
		target := []rune(strings.ToLower(w))
		if len(target) == 0 {
			continue
		}
	scan:
		for i := 0; i+len(target) <= len(runes); i++ {
			for k, r := range target {
				if runes[i+k] != r {
					continue scan
				}
			}
			if isWord(i-1) || isWord(i+len(target)) {
				continue // part of a longer word
			}
			var b image.Rectangle
			for k := range target {
				cb := chars[from[i+k]].box
				if b.Empty() {
					b = cb
				} else {
					b = b.Union(cb)
				}
			}
			boxes = append(boxes, b.Inset(-2)) // a little padding, like a marker pen
			if len(boxes) >= 200 {
				return boxes // enough for any page
			}
		}
	}
	return boxes
}

// highlight paints a translucent yellow marker over each box.
func highlight(img *image.RGBA, boxes []image.Rectangle) {
	marker := color.NRGBA{R: 255, G: 214, B: 10, A: 110}
	for _, b := range boxes {
		draw.Draw(img, b.Intersect(img.Bounds()), image.NewUniform(marker), image.Point{}, draw.Over)
	}
}

// cropAround picks the preview area: the full width, previewAspect × width high, with
// the first highlight a third of the way down (or the top of the page without one).
func cropAround(page image.Rectangle, boxes []image.Rectangle) image.Rectangle {
	h := int(float64(page.Dx()) * previewAspect)
	if h >= page.Dy() {
		return page
	}
	top := page.Min.Y
	if len(boxes) > 0 {
		first := boxes[0]
		for _, b := range boxes[1:] {
			if b.Min.Y < first.Min.Y {
				first = b
			}
		}
		top = first.Min.Y - h/3
	}
	top = max(page.Min.Y, min(top, page.Max.Y-h))
	return image.Rect(page.Min.X, top, page.Max.X, top+h)
}
