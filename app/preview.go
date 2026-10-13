package main

import (
	"bytes"
	"container/list"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	"image/color"
	_ "image/gif" // registered for image.Decode
	"image/jpeg"
	_ "image/png"
	"strings"
	"sync"

	"github.com/sanskarmishra479/findly/internal/extract"
	"github.com/sanskarmishra479/findly/internal/safefs"
	"github.com/sanskarmishra479/findly/internal/store"
)

// PreviewResponse is what the panel's right column shows for one result.
type PreviewResponse struct {
	Seq   int    `json:"seq"`
	Image string `json:"image"` // data: URL (PDF page or the image itself), "" if none
	Page  int    `json:"page"`  // PDF page shown (1-based)
	Pages int    `json:"pages"` // pages in the PDF
	// Text is the matching passage, for files shown as text (Word, text, PDFs whose page
	// could not be drawn).
	Text string `json:"text"`
	Err  string `json:"err"`
}

// Image previews: files above these limits get no picture (decoding them would need
// too much memory for a quick look).
const (
	maxImageFile   = 40 << 20
	maxImagePixels = 50_000_000
)

// Preview returns the preview for result id (from Search) at itemID's passage. seq is
// echoed back; a newer Preview call cancels an older one.
func (a *App) Preview(seq int, id, itemID int64, query string) PreviewResponse {
	resp := PreviewResponse{Seq: seq}
	e := a.currentEngine()
	ctx := a.context()
	if e == nil || ctx == nil {
		return resp
	}
	ctx, cancel := a.previews.start(ctx)
	defer cancel()

	files, err := e.Store().FilesByID(ctx, []int64{id})
	f, ok := files[id]
	if err != nil || !ok {
		resp.Err = "This file is no longer in the index."
		return resp
	}
	var text string
	page := 0
	if itemID != 0 {
		if fid, loc, t, err := e.Store().ItemByID(ctx, itemID); err == nil && fid == id {
			text = t
			page = pageOf(loc)
		}
	}

	switch f.Kind {
	case "pdf":
		pv, err := a.pdfPreview(ctx, f, page, queryWords(query))
		if err != nil {
			if ctx.Err() != nil {
				return resp
			}
			resp.Text = text // the passage instead of the picture
			resp.Err = previewError(err)
			return resp
		}
		resp.Image = "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(pv.JPEG)
		resp.Page, resp.Pages = pv.Page, pv.Pages
	case "image":
		img, err := imagePreview(f.Path)
		if err != nil {
			resp.Err = previewError(err)
			return resp
		}
		resp.Image = img
	default:
		resp.Text = text
	}
	return resp
}

func (a *App) pdfPreview(ctx context.Context, f store.File, page int, words []string) (extract.PagePreview, error) {
	key := fmt.Sprintf("%d|%d|%d|%s", f.ID, f.MTime, page, strings.Join(words, "\x00"))
	if pv, ok := a.pageCache.get(key); ok {
		return pv, nil
	}
	pv, err := extract.RenderPDFPage(ctx, f.Path, page, words)
	if err == nil {
		a.pageCache.put(key, pv)
	}
	return pv, err
}

// previewError explains in the user's words why there is no preview.
func previewError(err error) string {
	switch {
	case errors.Is(err, extract.ErrPDFPassword):
		return "This PDF is password-protected."
	case errors.Is(err, extract.ErrTooLarge):
		return "This file is too large to preview."
	case errors.Is(err, extract.ErrPDFBusy), errors.Is(err, extract.ErrPDFTimeout):
		return "The preview took too long."
	default:
		return "No preview for this file."
	}
}

func pageOf(locator string) int {
	var p int
	if i := strings.Index(locator, `"page":`); i >= 0 {
		fmt.Sscanf(locator[i+len(`"page":`):], "%d", &p)
	}
	return p
}

// imagePreview reads an image read-only, scales it to the preview width and returns
// it as a JPEG data URL.
func imagePreview(path string) (string, error) {
	data, err := safefs.ReadFile(path, maxImageFile)
	if err != nil {
		return "", err
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	if cfg.Width*cfg.Height > maxImagePixels {
		return "", fmt.Errorf("%w: %dx%d", extract.ErrTooLarge, cfg.Width, cfg.Height)
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	img = shrink(img, extract.PreviewWidth)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 82}); err != nil {
		return "", err
	}
	return "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(buf.Bytes()), nil
}

// shrink scales img down to at most maxW pixels wide by averaging blocks of pixels
// (good enough for a preview; no extra library). Smaller images are returned as is.
func shrink(img image.Image, maxW int) image.Image {
	b := img.Bounds()
	if b.Dx() <= maxW {
		return img
	}
	w := maxW
	h := max(1, b.Dy()*maxW/b.Dx())
	out := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		y0 := b.Min.Y + y*b.Dy()/h
		y1 := max(y0+1, b.Min.Y+(y+1)*b.Dy()/h)
		for x := 0; x < w; x++ {
			x0 := b.Min.X + x*b.Dx()/w
			x1 := max(x0+1, b.Min.X+(x+1)*b.Dx()/w)
			var r, g, bl, n uint64
			// Sample at most 4×4 points per block: enough for a preview, fast for huge images.
			sy := max(1, (y1-y0)/4)
			sx := max(1, (x1-x0)/4)
			for yy := y0; yy < y1; yy += sy {
				for xx := x0; xx < x1; xx += sx {
					cr, cg, cb, _ := img.At(xx, yy).RGBA()
					r, g, bl, n = r+uint64(cr), g+uint64(cg), bl+uint64(cb), n+1
				}
			}
			out.Set(x, y, color.RGBA{uint8(r / n >> 8), uint8(g / n >> 8), uint8(bl / n >> 8), 255})
		}
	}
	return out
}

// previewCalls cancels an outdated preview when the selection moves on.
type previewCalls struct {
	mu     sync.Mutex
	cancel context.CancelFunc
}

func (p *previewCalls) start(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	p.mu.Lock()
	if p.cancel != nil {
		p.cancel()
	}
	p.cancel = cancel
	p.mu.Unlock()
	return ctx, cancel
}

// pageCache keeps the last rendered PDF pages, so moving back and forth in the result
// list does not render them again.
type pageCache struct {
	mu    sync.Mutex
	order *list.List // most recent first; values are *pageEntry
	byKey map[string]*list.Element
}

type pageEntry struct {
	key string
	pv  extract.PagePreview
}

const pageCacheSize = 24 // ~24 × 60 KB

func (c *pageCache) get(key string) (extract.PagePreview, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.byKey == nil {
		return extract.PagePreview{}, false
	}
	el, ok := c.byKey[key]
	if !ok {
		return extract.PagePreview{}, false
	}
	c.order.MoveToFront(el)
	return el.Value.(*pageEntry).pv, true
}

func (c *pageCache) put(key string, pv extract.PagePreview) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.byKey == nil {
		c.byKey, c.order = map[string]*list.Element{}, list.New()
	}
	if el, ok := c.byKey[key]; ok {
		el.Value.(*pageEntry).pv = pv
		c.order.MoveToFront(el)
		return
	}
	c.byKey[key] = c.order.PushFront(&pageEntry{key, pv})
	for c.order.Len() > pageCacheSize {
		last := c.order.Back()
		delete(c.byKey, last.Value.(*pageEntry).key)
		c.order.Remove(last)
	}
}
