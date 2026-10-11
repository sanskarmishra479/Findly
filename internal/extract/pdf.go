//go:build windows

package extract

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/klippa-app/go-pdfium"
	"github.com/klippa-app/go-pdfium/requests"
	"github.com/klippa-app/go-pdfium/webassembly"
	"github.com/sanskarmishra479/findly/internal/safefs"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental"
)

// PDF limits (plan 9.4).
const (
	MaxPDFFile      = 256 << 20 // bytes read from disk
	PDFFirstPass    = 30        // pages extracted in the first pass; the rest are deferred
	MaxPDFPages     = 5000      // pages ever extracted from one file
	ScannedPageText = 20        // a page with less text than this is probably a scan (needs OCR)
	pdfTimeout      = 2 * time.Minute
	pdfMemoryPages  = 8192 // WebAssembly memory cap: 8192 × 64 KiB = 512 MiB
)

// ErrPDFPassword is returned for password-protected PDFs, which Findly does not open.
var ErrPDFPassword = errors.New("extract: PDF is password-protected")

var (
	pdfOnce sync.Once
	pdfPool pdfium.Pool
	pdfErr  error
)

// pdfEngine starts PDFium (compiled to WebAssembly) on first use. The sandbox gets:
// no file system at all (we pass PDF bytes read through safefs), no console output,
// a memory cap, and a runtime that stops when its context is cancelled, so a hostile
// PDF cannot read files, spam logs, exhaust RAM or hang forever.
func pdfEngine() (pdfium.Pool, error) {
	pdfOnce.Do(func() {
		pdfPool, pdfErr = webassembly.Init(webassembly.Config{
			MinIdle:  0,
			MaxIdle:  1,
			MaxTotal: 2,
			FSConfig: wazero.NewFSConfig(), // empty: nothing mounted (the default mounts the whole drive)
			Stdout:   io.Discard,
			Stderr:   io.Discard,
			RuntimeConfig: wazero.NewRuntimeConfig().
				WithCoreFeatures(api.CoreFeaturesV2 | experimental.CoreFeaturesExceptionHandling).
				WithMemoryLimitPages(pdfMemoryPages).
				WithCloseOnContextDone(true),
		})
	})
	return pdfPool, pdfErr
}

// ClosePDF releases the PDF engine (call on app exit).
func ClosePDF() error {
	if pdfPool == nil {
		return nil
	}
	return pdfPool.Close()
}

// PDFPages extracts the text of pages [from, to) (0-based) of the PDF at path.
// Sections get 1-based page numbers. NeedsOCR lists 1-based pages with almost no text.
// total is the number of pages in the file.
func PDFPages(path string, from, to int) (doc Doc, total int, err error) {
	info, err := safefs.Stat(path)
	if err != nil {
		return Doc{}, 0, err
	}
	if info.Size > MaxPDFFile {
		return Doc{}, 0, fmt.Errorf("%w: %s (%d bytes)", ErrTooLarge, path, info.Size)
	}
	data, err := safefs.ReadFile(path, MaxPDFFile)
	if err != nil {
		return Doc{}, 0, err
	}

	pool, err := pdfEngine()
	if err != nil {
		return Doc{}, 0, fmt.Errorf("extract: PDF engine: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), pdfTimeout)
	defer cancel()
	inst, err := pool.GetInstanceWithContext(ctx)
	if err != nil {
		return Doc{}, 0, fmt.Errorf("extract: PDF engine busy: %w", err)
	}

	type result struct {
		doc   Doc
		total int
		err   error
	}
	done := make(chan result, 1)
	go func() {
		d, n, err := pdfText(inst, data, from, to)
		done <- result{d, n, err}
	}()
	select {
	case r := <-done:
		inst.Close()
		return r.doc, r.total, r.err
	case <-ctx.Done():
		inst.Kill() // stops the hung WebAssembly call
		return Doc{}, 0, fmt.Errorf("extract: PDF took longer than %v: %s", pdfTimeout, path)
	}
}

func pdfText(inst pdfium.Pdfium, data []byte, from, to int) (Doc, int, error) {
	opened, err := inst.OpenDocument(&requests.OpenDocument{File: &data})
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "password") {
			return Doc{}, 0, ErrPDFPassword
		}
		return Doc{}, 0, fmt.Errorf("extract: not a readable PDF: %w", err)
	}
	defer inst.FPDF_CloseDocument(&requests.FPDF_CloseDocument{Document: opened.Document})

	count, err := inst.FPDF_GetPageCount(&requests.FPDF_GetPageCount{Document: opened.Document})
	if err != nil {
		return Doc{}, 0, err
	}
	total := count.PageCount
	from = max(0, from)
	to = min(to, total, MaxPDFPages)

	var doc Doc
	size := 0
	for i := from; i < to; i++ {
		page, err := inst.GetPageText(&requests.GetPageText{
			Page: requests.Page{ByIndex: &requests.PageByIndex{Document: opened.Document, Index: i}},
		})
		if err != nil {
			continue // one broken page must not lose the rest
		}
		text := tidy(page.Text)
		if len([]rune(text)) < ScannedPageText {
			doc.NeedsOCR = append(doc.NeedsOCR, i+1)
		}
		if text == "" {
			continue
		}
		if size+len(text) > MaxOutputText {
			doc.Truncated = true
			break
		}
		size += len(text)
		doc.Sections = append(doc.Sections, Section{Page: i + 1, Text: text})
	}
	return doc, total, nil
}
