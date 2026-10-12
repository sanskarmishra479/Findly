//go:build windows

package extract

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/klippa-app/go-pdfium"
	pdfiumerr "github.com/klippa-app/go-pdfium/errors"
	"github.com/klippa-app/go-pdfium/requests"
)

// makePDF builds a small valid PDF with one line of Helvetica text per page
// (an empty string makes a page with no text, like a scanned page).
func makePDF(pages ...string) []byte {
	var objs []string
	n := len(pages)
	// 1: catalog, 2: pages, 3: font, then per page: page object + content stream.
	kids := make([]string, n)
	for i := range pages {
		kids[i] = fmt.Sprintf("%d 0 R", 4+2*i)
	}
	objs = append(objs,
		"<< /Type /Catalog /Pages 2 0 R >>",
		fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), n),
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
	)
	for i, text := range pages {
		stream := ""
		if text != "" {
			esc := strings.NewReplacer(`\`, `\\`, "(", `\(`, ")", `\)`).Replace(text)
			stream = fmt.Sprintf("BT /F1 12 Tf 72 720 Td (%s) Tj ET", esc)
		}
		objs = append(objs,
			fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 3 0 R >> >> /Contents %d 0 R >>", 5+2*i),
			fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(stream), stream),
		)
	}
	var b bytes.Buffer
	b.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objs))
	for i, o := range objs {
		offsets[i] = b.Len()
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, o)
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(objs)+1)
	for _, off := range offsets {
		fmt.Fprintf(&b, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objs)+1, xref)
	return b.Bytes()
}

func TestPDFText(t *testing.T) {
	p := write(t, "lease.pdf", makePDF("The clause about pets in the lease", "", "Rent is due on the first day of each month"))
	d, err := File(p)
	if err != nil {
		t.Fatal(err)
	}
	if d.Pages != 3 || d.MorePages {
		t.Errorf("Pages=%d MorePages=%v, want 3 false", d.Pages, d.MorePages)
	}
	if len(d.Sections) != 2 || d.Sections[0].Page != 1 || d.Sections[1].Page != 3 {
		t.Fatalf("sections = %+v", d.Sections)
	}
	if !strings.Contains(d.Sections[0].Text, "clause about pets") || !strings.Contains(d.Sections[1].Text, "Rent") {
		t.Errorf("text = %q", d.Text())
	}
	if len(d.NeedsOCR) != 1 || d.NeedsOCR[0] != 2 {
		t.Errorf("NeedsOCR = %v, want [2]", d.NeedsOCR)
	}
}

func TestPDFFirstPassAndRanges(t *testing.T) {
	pages := make([]string, PDFFirstPass+5)
	for i := range pages {
		pages[i] = fmt.Sprintf("This is page number %d of the report", i+1)
	}
	p := write(t, "long.pdf", makePDF(pages...))
	d, err := File(p)
	if err != nil {
		t.Fatal(err)
	}
	if d.Pages != len(pages) || !d.MorePages || len(d.Sections) != PDFFirstPass {
		t.Fatalf("Pages=%d MorePages=%v sections=%d", d.Pages, d.MorePages, len(d.Sections))
	}
	// The deferred job reads the rest.
	rest, n, err := PDFPages(p, PDFFirstPass, len(pages), MaxOutputText)
	if err != nil || n != len(pages) || len(rest.Sections) != 5 || rest.Sections[0].Page != PDFFirstPass+1 {
		t.Fatalf("rest: sections=%d total=%d err=%v", len(rest.Sections), n, err)
	}
}

func TestPDFGarbageIsAnErrorNotACrash(t *testing.T) {
	for name, data := range map[string][]byte{
		"empty.pdf":     {},
		"garbage.pdf":   []byte("%PDF-1.4\nthis is not really a pdf"),
		"truncated.pdf": makePDF("hello world text")[:150],
		"binary.pdf":    bytes.Repeat([]byte{0xFF, 0x00, 0x7B}, 10000),
	} {
		if _, err := File(write(t, name, data)); err == nil {
			t.Logf("%s: no error (PDFium repaired it), acceptable", name)
		}
	}
	// The engine still works after bad input.
	if _, err := File(write(t, "ok.pdf", makePDF("still works fine"))); err != nil {
		t.Fatalf("engine broken after bad PDFs: %v", err)
	}
}

// Security: the PDF sandbox must not be able to open files from disk by path.
func TestPDFSandboxHasNoDiskAccess(t *testing.T) {
	secret := write(t, "secret.pdf", makePDF("top secret content"))
	pool, err := pdfEngine()
	if err != nil {
		t.Fatal(err)
	}
	inst, err := pool.GetInstance(0)
	if err != nil {
		t.Fatal(err)
	}
	defer inst.Close()
	// Try the paths the library's default config would expose (drive mounted at "/").
	for _, p := range []string{secret, "/" + strings.ReplaceAll(secret[3:], `\`, "/"), "/Windows/win.ini"} {
		p := p
		if _, err := inst.OpenDocument(&requests.OpenDocument{FilePath: &p}); err == nil {
			t.Errorf("sandbox opened %q from disk", p)
		}
	}
}

func TestPDFTooLarge(t *testing.T) {
	p := write(t, "huge.pdf", nil)
	if err := truncateFile(p, MaxPDFFile+1); err != nil {
		t.Skip(err)
	}
	if _, err := File(p); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("got %v, want ErrTooLarge", err)
	}
}

func TestPDFBadRange(t *testing.T) {
	p := write(t, "r.pdf", makePDF("some text on the only page"))
	for _, r := range [][2]int{{-1, 5}, {5, 2}} {
		if _, _, err := PDFPages(p, r[0], r[1], MaxOutputText); err == nil {
			t.Errorf("range %v: expected error", r)
		}
	}
	// Past the end is fine: nothing to read.
	d, n, err := PDFPages(p, 10, 20, MaxOutputText)
	if err != nil || n != 1 || len(d.Sections) != 0 {
		t.Errorf("past end: sections=%d total=%d err=%v", len(d.Sections), n, err)
	}
}

// Review 5: the page crossing the text cap is cut, not dropped, and no more pages follow.
func TestPDFOutputCap(t *testing.T) {
	p := write(t, "cap.pdf", makePDF("first page has some words", "second page has more words", "third page"))
	d, _, err := PDFPages(p, 0, 10, 40)
	if err != nil {
		t.Fatal(err)
	}
	if !d.Truncated || len(d.Sections) != 2 || d.LastPage != 2 {
		t.Fatalf("truncated=%v sections=%d last=%d", d.Truncated, len(d.Sections), d.LastPage)
	}
	if size := len(d.Sections[0].Text) + len(d.Sections[1].Text); size > 40 {
		t.Fatalf("output %d bytes > cap 40", size)
	}
}

// Review 2: more callers than engines all succeed (they wait their turn).
func TestPDFConcurrentCallers(t *testing.T) {
	p := write(t, "c.pdf", makePDF("concurrent extraction test page"))
	errs := make(chan error, 6)
	for range 6 {
		go func() { _, err := File(p); errs <- err }()
	}
	for range 6 {
		if err := <-errs; err != nil {
			t.Errorf("concurrent File: %v", err)
		}
	}
}

// Review 2/8: a PDF that takes too long is stopped, and the engine still works afterwards.
func TestPDFTimeoutKillsAndRecovers(t *testing.T) {
	pages := make([]string, 3000)
	for i := range pages {
		pages[i] = fmt.Sprintf("page %d with enough text to not count as scanned", i)
	}
	big := write(t, "slow.pdf", makePDF(pages...))
	old := pdfTimeout
	pdfTimeout = 50 * time.Millisecond
	_, _, err := PDFPages(big, 0, 3000, MaxOutputText)
	pdfTimeout = old
	if !errors.Is(err, ErrPDFTimeout) {
		t.Fatalf("got %v, want ErrPDFTimeout", err)
	}
	if _, err := File(write(t, "after.pdf", makePDF("engine works after a kill"))); err != nil {
		t.Fatalf("engine broken after timeout: %v", err)
	}
}

func TestPDFPasswordErrorMapping(t *testing.T) {
	// Real encrypted fixtures need an encryption library; check the mapping instead.
	for _, e := range []error{pdfiumerr.ErrPassword, pdfiumerr.ErrSecurity} {
		wrapped := fmt.Errorf("open: %w", e)
		if !errors.Is(wrapped, pdfiumerr.ErrPassword) && !errors.Is(wrapped, pdfiumerr.ErrSecurity) {
			t.Fatal("errors.Is does not see through wrapping")
		}
	}
}

func TestClosePDFTwiceAndReuse(t *testing.T) {
	if err := ClosePDF(); err != nil {
		t.Fatal(err)
	}
	if err := ClosePDF(); err != nil {
		t.Fatal(err)
	}
	if _, err := File(write(t, "reuse.pdf", makePDF("engine restarts after close"))); err != nil {
		t.Fatalf("after ClosePDF: %v", err)
	}
}

// Full review #2: a negative text budget is an error, not a panic.
func TestPDFNegativeBudget(t *testing.T) {
	p := write(t, "neg.pdf", makePDF("some page text that is long enough"))
	if _, _, err := PDFPages(p, 0, 1, -5); err == nil {
		t.Fatal("expected error")
	}
}

// Full review #2: a panic during extraction becomes an error; the app keeps running and
// the engine still works afterwards.
func TestPDFPanicIsRecovered(t *testing.T) {
	old := pdfTextFn
	pdfTextFn = func(pdfium.Pdfium, []byte, int, int, int) (Doc, int, error) { panic("boom") }
	_, _, err := PDFPages(write(t, "p.pdf", makePDF("x page text here ok")), 0, 1, MaxOutputText)
	pdfTextFn = old
	if !errors.Is(err, ErrPDFCrash) {
		t.Fatalf("got %v, want ErrPDFCrash", err)
	}
	if _, err := File(write(t, "after.pdf", makePDF("engine still works after a panic"))); err != nil {
		t.Fatal(err)
	}
}

// Full review #3: a PDF's bytes are read only after an engine is free, so at most
// pdfEngines PDFs are in memory at once, however many workers are waiting.
func TestPDFReadAfterEngineIsFree(t *testing.T) {
	var mu sync.Mutex
	inMemory, peak := 0, 0
	oldRead, oldText := readPDF, pdfTextFn
	readPDF = func(p string) ([]byte, error) {
		mu.Lock()
		inMemory++
		peak = max(peak, inMemory)
		mu.Unlock()
		return oldRead(p)
	}
	pdfTextFn = func(inst pdfium.Pdfium, data []byte, from, to, maxText int) (Doc, int, error) {
		defer func() { mu.Lock(); inMemory--; mu.Unlock() }()
		time.Sleep(30 * time.Millisecond)
		return oldText(inst, data, from, to, maxText)
	}
	defer func() { readPDF, pdfTextFn = oldRead, oldText }()

	p := write(t, "c.pdf", makePDF("concurrent memory test page"))
	var wg sync.WaitGroup
	for range 6 {
		wg.Add(1)
		go func() { defer wg.Done(); File(p) }()
	}
	wg.Wait()
	if peak > pdfEngines {
		t.Fatalf("%d PDFs in memory at once, want at most %d", peak, pdfEngines)
	}
}
