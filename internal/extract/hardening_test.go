//go:build windows

package extract

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf16"
)

// bounded fails the test if fn takes longer than limit or allocates more than maxAlloc bytes.
func bounded(t *testing.T, limit time.Duration, maxAlloc uint64, fn func()) {
	t.Helper()
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	start := time.Now()
	fn()
	took := time.Since(start)
	runtime.ReadMemStats(&after)
	if took > limit {
		t.Errorf("took %v, limit %v", took, limit)
	}
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > maxAlloc {
		t.Errorf("allocated %d MB, limit %d MB", alloc>>20, maxAlloc>>20)
	}
}

// u returns the RTF escape \uN (built at runtime so no tool can rewrite it).
func u(n int) string { return "\\" + "u" + fmt.Sprint(n) }

// Review 1: thousands of <title> tags must not take quadratic time.
func TestHTMLManyTitles(t *testing.T) {
	page := strings.Repeat("<title>"+strings.Repeat("x", 100)+"</title>", 4<<20/116)
	bounded(t, 3*time.Second, 400<<20, func() { htmlToText(page) })
	got := htmlToText("<title>First</title><title>Second</title><p>body</p>")
	if !strings.HasPrefix(got, "First") || strings.Contains(got, "Second") {
		t.Errorf("got %q, want only the first title", got)
	}
}

// Review 2: deeply nested XML is cut off quickly instead of eating memory.
func TestDeepXML(t *testing.T) {
	deep := strings.Repeat("<a>", 3_000_000) + "x" + strings.Repeat("</a>", 3_000_000)
	bounded(t, 5*time.Second, 300<<20, func() {
		xmlText(strings.NewReader(deep), docxRules)
	})
	odtDeep := "<office:text>" + strings.Repeat("<text:p>x", 200_000)
	bounded(t, 5*time.Second, 300<<20, func() {
		xmlText(strings.NewReader(odtDeep), odtRules)
	})
}

// Review 3: an archive with a huge number of entries is refused up front.
func TestZipManyEntries(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	zw.Create("word/document.xml")
	for i := range maxZipEntries + 10 {
		zw.Create(fmt.Sprintf("e%d", i))
	}
	zw.Close()
	p := write(t, "many.docx", buf.Bytes())
	if _, err := File(p); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("got %v, want ErrTooLarge", err)
	}
}

// Review 4: a self-closing <svg/> must not hide the rest of the page.
func TestHTMLSelfClosingSkipTag(t *testing.T) {
	got := htmlToText(`<p>before</p><svg class="i"/><iframe src="x"/><p>after</p>`)
	if !strings.Contains(got, "after") {
		t.Errorf("got %q, lost text after <svg/>", got)
	}
}

// Review 8: table cells and definition lists are separated.
func TestHTMLSeparators(t *testing.T) {
	got := htmlToText(`<table><tr><td>Name</td><td>Age</td></tr></table><dl><dt>Term</dt><dd>Def</dd></dl>`)
	for _, glued := range []string{"NameAge", "TermDef"} {
		if strings.Contains(got, glued) {
			t.Errorf("got %q: %q glued together", got, glued)
		}
	}
}

// Review 5 + 12: text boxes appear once, on their own line; tab stops are not tabs.
func TestDOCXTextBoxAndTabStops(t *testing.T) {
	doc := `<w:document xmlns:w="w" xmlns:mc="mc"><w:body>
<w:p><w:pPr><w:tabs><w:tab w:val="left"/></w:tabs></w:pPr><w:r><w:t>Body</w:t></w:r>
<w:r><mc:AlternateContent><mc:Choice><w:txbxContent><w:p><w:r><w:t>BoxText</w:t></w:r></w:p></w:txbxContent></mc:Choice>
<mc:Fallback><w:txbxContent><w:p><w:r><w:t>BoxText</w:t></w:r></w:p></w:txbxContent></mc:Fallback></mc:AlternateContent></w:r></w:p>
</w:body></w:document>`
	got := text(t, zipFile(t, "box.docx", map[string]string{"word/document.xml": doc}))
	if strings.Count(got, "BoxText") != 1 {
		t.Errorf("text box appears %d times: %q", strings.Count(got, "BoxText"), got)
	}
	if strings.Contains(got, "BodyBoxText") || strings.Contains(got, "\tBody") {
		t.Errorf("got %q: box glued to body or tab stop emitted", got)
	}
}

// Review 6: ODT comments and deleted text are not indexed; footnotes are separated.
func TestODTSkipsCommentsAndDeletions(t *testing.T) {
	content := `<office:document-content xmlns:office="o" xmlns:text="t" xmlns:dc="d"><office:body><office:text>
<text:tracked-changes><text:changed-region><text:deletion><text:p>DeletedSecret</text:p></text:deletion></text:changed-region></text:tracked-changes>
<text:p>Para<office:annotation><dc:creator>Alice Smith</dc:creator><dc:date>2024-01-01</dc:date><text:p>Comment</text:p></office:annotation> end<text:note><text:note-citation>1</text:note-citation><text:note-body><text:p>Footnote</text:p></text:note-body></text:note></text:p>
</office:text></office:body></office:document-content>`
	got := text(t, zipFile(t, "c.odt", map[string]string{"content.xml": content}))
	for _, bad := range []string{"DeletedSecret", "Alice", "2024", "Comment", "end1"} {
		if strings.Contains(got, bad) {
			t.Errorf("got %q: contains %q", got, bad)
		}
	}
	if !strings.Contains(got, "Para end") || !strings.Contains(got, "Footnote") {
		t.Errorf("got %q: missing paragraph or footnote", got)
	}
}

// Review 7: a few broken bytes keep a UTF-8 file UTF-8; UTF-16 without BOM is detected.
func TestEncodingDetection(t *testing.T) {
	stray := []byte("Café naïve résumé déjà vu \xff and more text here")
	if got := decodeText(stray); !strings.Contains(got, "Café naïve résumé") {
		t.Errorf("stray byte: got %q", got)
	}
	ru := "Привет, мир. This is a test."
	le := utf16le(ru)
	if got := decodeText(le); got != ru {
		t.Errorf("UTF-16LE no BOM: got %q", got)
	}
	be := make([]byte, len(le))
	for i := 0; i < len(le); i += 2 {
		be[i], be[i+1] = le[i+1], le[i]
	}
	if got := decodeText(be); got != ru {
		t.Errorf("UTF-16BE no BOM: got %q", got)
	}
	if got := decodeText([]byte{0xFF, 0xFE, 'A', 0, 'B'}); got != "A" { // odd length: last byte dropped
		t.Errorf("odd UTF-16: got %q", got)
	}
}

func utf16le(s string) []byte {
	var b []byte
	for _, v := range utf16.Encode([]rune(s)) {
		b = append(b, byte(v), byte(v>>8))
	}
	return b
}

// Review 9 + 10: RTF fallback counting, surrogates and binary data.
func TestRTFEdgeCases(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"huge uc", `{\rtf1` + "\\" + "uc" + "999999" + ` a` + u(8364) + `?rest of document}`, "a€ document"}, // capped to 8: skips "?rest of", not everything
		{"skip reset at group end", `{\rtf1 {` + u(8364) + `}Hello}`, "€Hello"},
		{"surrogate pair", `{\rtf1 ` + u(-10179) + `?` + u(-8704) + `?!}`, "😀!"},
		{"lone high surrogate", `{\rtf1 ` + u(-10179) + `?` + u(65) + `?B}`, "�AB"},
		// 6 bytes of binary data (all braces), then "}" closes the picture group.
		{"bin data", `{\rtf1 A{\pict\bin6 }}}}}}}B}`, "AB"},
	}
	for _, c := range cases {
		if got := rtfToText([]byte(c.in)); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

// Review 11: tidy on 20 MB of blank lines stays cheap.
func TestTidyManyLines(t *testing.T) {
	s := strings.Repeat("\n", 20<<20)
	bounded(t, 3*time.Second, 100<<20, func() { tidy(s) })
}

func TestRTFLargePictureIsCheap(t *testing.T) {
	rtf := []byte(`{\rtf1 A{\pict ` + strings.Repeat("ab", 10<<20) + `}B}`)
	bounded(t, 2*time.Second, 80<<20, func() {
		if got := rtfToText(rtf); got != "AB" {
			t.Errorf("got %q", got)
		}
	})
}
