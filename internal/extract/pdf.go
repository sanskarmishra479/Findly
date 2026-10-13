//go:build windows

package extract

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/klippa-app/go-pdfium"
	pdfiumerr "github.com/klippa-app/go-pdfium/errors"
	"github.com/klippa-app/go-pdfium/requests"
	"github.com/klippa-app/go-pdfium/webassembly"
	"github.com/sanskarmishra479/findly/internal/safefs"
	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental"
)

// PDF limits (plan 9.4).
const (
	// MaxPDFFile keeps peak memory near 640 MB: the bytes in Go plus PDFium's 512 MiB
	// sandbox, with one PDF engine at a time (fine on an 8 GB PC).
	MaxPDFFile      = 128 << 20
	PDFFirstPass    = 30   // pages extracted in the first pass; the rest are deferred
	MaxPDFPages     = 5000 // pages ever extracted from one file
	ScannedPageText = 20   // a page with less text than this is probably a scan (needs OCR)
	pdfMemoryPages  = 8192 // WebAssembly memory cap: 8192 × 64 KiB = 512 MiB
	pdfEngines      = 1    // PDF engines running at once
	maxPageFailures = 3    // consecutive page errors that mean the engine itself is broken
)

// Variables so tests can shorten them.
var (
	pdfTimeout = 2 * time.Minute // extraction time for one call, after an engine is free
	pdfWait    = 5 * time.Minute // time to wait for a free engine
)

var (
	// ErrPDFPassword is returned for password-protected PDFs, which Findly does not open.
	ErrPDFPassword = errors.New("extract: PDF is password-protected or encrypted")
	// ErrPDFBusy means no PDF engine became free in time; retry later. It says nothing
	// about the file itself.
	ErrPDFBusy = errors.New("extract: PDF engine busy")
	// ErrPDFTimeout means this PDF took too long and its engine was stopped.
	ErrPDFTimeout = errors.New("extract: PDF took too long")
	// ErrPDFCrash means extraction panicked; the file is marked failed, the app keeps running.
	ErrPDFCrash = errors.New("extract: PDF engine crashed on this file")
)

var (
	pdfMu   sync.Mutex
	pdfPool pdfium.Pool
)

// pdfEngine starts PDFium (compiled to WebAssembly) on first use. The sandbox gets:
// no file system at all (we pass PDF bytes read through safefs), no console output,
// a memory cap, and a runtime that stops when its context is cancelled, so a hostile
// PDF cannot read files, spam logs, exhaust RAM or hang forever.
func pdfEngine() (pdfium.Pool, error) {
	pdfMu.Lock()
	defer pdfMu.Unlock()
	if pdfPool != nil {
		return pdfPool, nil
	}
	pool, err := webassembly.Init(webassembly.Config{
		MinIdle:       0,
		MaxIdle:       pdfEngines,
		MaxTotal:      pdfEngines,
		FSConfig:      wazero.NewFSConfig(), // empty: nothing mounted (the default mounts the whole drive)
		Stdout:        io.Discard,
		Stderr:        io.Discard,
		RuntimeConfig: runtimeConfig(pdfMemoryPages),
	})
	if err != nil {
		return nil, fmt.Errorf("extract: PDF engine: %w", err)
	}
	pdfPool = pool
	return pdfPool, nil
}

// ClosePDF releases the PDF engine (call on app exit). Safe to call more than once; a
// later PDF starts a new engine.
func ClosePDF() error {
	pdfMu.Lock()
	defer pdfMu.Unlock()
	if pdfPool == nil {
		return nil
	}
	err := pdfPool.Close()
	pdfPool = nil
	return err
}

// PDFPages extracts the text of pages [from, to) (0-based) of the PDF at path, up to
// maxText bytes in total. Sections get 1-based page numbers. total is the number of
// pages in the file; doc.LastPage is the last page processed (1-based), so a deferred
// job can continue from there.
func PDFPages(path string, from, to, maxText int) (doc Doc, total int, err error) {
	if from < 0 || to < from || maxText < 0 {
		return Doc{}, 0, fmt.Errorf("extract: bad page range %d..%d or text budget %d", from, to, maxText)
	}
	info, err := safefs.Stat(path)
	if err != nil {
		return Doc{}, 0, err
	}
	if info.Size > MaxPDFFile {
		return Doc{}, 0, fmt.Errorf("%w: %s (%d bytes)", ErrTooLarge, path, info.Size)
	}

	pool, err := pdfEngine()
	if err != nil {
		return Doc{}, 0, err
	}
	// Wait for a free engine BEFORE reading the file: otherwise every queued worker holds
	// up to 128 MB of PDF bytes while waiting (measured 979 MB with 6 workers). Waiting
	// has its own deadline, so a queued PDF still gets the full extraction time.
	waitCtx, cancelWait := context.WithTimeout(context.Background(), pdfWait)
	inst, err := pool.GetInstanceWithContext(waitCtx)
	cancelWait()
	if err != nil {
		return Doc{}, 0, fmt.Errorf("%w: %v", ErrPDFBusy, err)
	}
	data, err := readPDF(path)
	if err != nil {
		inst.Close()
		return Doc{}, 0, err
	}

	type result struct {
		doc   Doc
		total int
		err   error
	}
	done := make(chan result, 1) // buffered: the goroutine never blocks if we time out
	go func() {
		// A panic here (a bug, or a library failing on a hostile PDF) would otherwise
		// crash the whole app, because no caller can recover a panic in this goroutine.
		defer func() {
			if p := recover(); p != nil {
				done <- result{err: fmt.Errorf("%w: %v", ErrPDFCrash, p)}
			}
		}()
		d, n, err := pdfTextFn(inst, data, from, to, maxText)
		done <- result{d, n, err}
	}()
	select {
	case r := <-done:
		if errors.Is(r.err, ErrPDFCrash) {
			inst.Kill() // its state is unknown after a panic: never reuse it
		} else {
			inst.Close()
		}
		return r.doc, r.total, r.err
	case <-time.After(pdfTimeout):
		inst.Kill() // stops the hung WebAssembly call; the pool replaces the instance
		return Doc{}, 0, fmt.Errorf("%w (%v): %s", ErrPDFTimeout, pdfTimeout, path)
	}
}

func pdfText(inst pdfium.Pdfium, data []byte, from, to, maxText int) (Doc, int, error) {
	opened, err := inst.OpenDocument(&requests.OpenDocument{File: &data})
	if err != nil {
		if errors.Is(err, pdfiumerr.ErrPassword) || errors.Is(err, pdfiumerr.ErrSecurity) {
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
	to = min(to, total, MaxPDFPages)

	var doc Doc
	size, failures := 0, 0
	for i := from; i < to; i++ {
		doc.LastPage = i + 1
		page, err := inst.GetPageText(&requests.GetPageText{
			Page: requests.Page{ByIndex: &requests.PageByIndex{Document: opened.Document, Index: i}},
		})
		if err != nil {
			// One broken page is recorded and skipped; several in a row mean the engine
			// itself died (trap, out of memory), and continuing would silently lose pages.
			doc.FailedPages = append(doc.FailedPages, i+1)
			if failures++; failures >= maxPageFailures {
				return Doc{}, total, fmt.Errorf("extract: PDF engine failed at page %d: %w", i+1, err)
			}
			continue
		}
		failures = 0
		text := tidy(page.Text)
		if len([]rune(text)) < ScannedPageText {
			doc.NeedsOCR = append(doc.NeedsOCR, i+1)
		}
		if text == "" {
			continue
		}
		if size+len(text) > maxText {
			text = cutUTF8(text, maxText-size)
			doc.Truncated = true
		}
		size += len(text)
		if text != "" {
			doc.Sections = append(doc.Sections, Section{Page: i + 1, Text: text})
		}
		if doc.Truncated {
			break
		}
	}
	return doc, total, nil
}

// Test hooks (variables so tests can observe ordering and inject failures).
var (
	readPDF   = func(path string) ([]byte, error) { return safefs.ReadFile(path, MaxPDFFile) }
	pdfTextFn = pdfText
)

var (
	cacheMu sync.Mutex
	cache   wazero.CompilationCache
)

// SetCompileCache makes the PDF engines keep their compiled machine code in dir (inside
// the app folder), so starting an engine takes well under a second instead of ~6 s
// (WebAssembly is compiled once per version, not on every start). Call it before the
// first PDF; without it every start compiles from scratch.
func SetCompileCache(dir string) error {
	c, err := wazero.NewCompilationCacheWithDir(dir)
	if err != nil {
		return fmt.Errorf("extract: PDF compile cache: %w", err)
	}
	cacheMu.Lock()
	cache = c
	cacheMu.Unlock()
	return nil
}

// runtimeConfig is the sandbox configuration shared by both PDF engines.
func runtimeConfig(memoryPages uint32) wazero.RuntimeConfig {
	rc := wazero.NewRuntimeConfig().
		WithCoreFeatures(api.CoreFeaturesV2 | experimental.CoreFeaturesExceptionHandling).
		WithMemoryLimitPages(memoryPages).
		WithCloseOnContextDone(true)
	cacheMu.Lock()
	defer cacheMu.Unlock()
	if cache != nil {
		rc = rc.WithCompilationCache(cache)
	}
	return rc
}
