//go:build windows

package extract

import (
	"archive/zip"
	"bytes"
	"compress/flate"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// zipFile builds a .docx/.odt-like archive from name → content.
func zipFile(t *testing.T, name string, parts map[string]string) string {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for n, c := range parts {
		w, err := zw.Create(n)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(c))
	}
	zw.Close()
	return write(t, name, buf.Bytes())
}

func text(t *testing.T, path string) string {
	t.Helper()
	d, err := File(path)
	if err != nil {
		t.Fatal(err)
	}
	return d.Text()
}

func TestPlainTextEncodings(t *testing.T) {
	cases := []struct {
		name string
		data []byte
		want string
	}{
		{"utf8.txt", []byte("Café crème"), "Café crème"},
		{"bom.txt", append([]byte{0xEF, 0xBB, 0xBF}, "naïve"...), "naïve"},
		{"utf16le.txt", []byte{0xFF, 0xFE, 'H', 0, 'i', 0, 0xE9, 0}, "Hié"},
		{"utf16be.txt", []byte{0xFE, 0xFF, 0, 'H', 0, 'i'}, "Hi"},
		{"ansi.txt", []byte("na\xefve \x93quoted\x94 \x80"), "naïve “quoted” €"},
		{"crlf.md", []byte("line1\r\nline2\r\n\r\n\r\n\r\nline3\x00"), "line1\nline2\n\nline3"},
	}
	for _, c := range cases {
		if got := text(t, write(t, c.name, c.data)); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestHTML(t *testing.T) {
	page := `<html><head><title>Lease Terms</title><style>p{color:red}</style>
<script>alert("hidden")</script></head>
<body><h1>Pets</h1><p>No pets &amp; no smoking.</p><noscript>enable js</noscript>
<ul><li>Rent: $500</li><li>Deposit</li></ul></body></html>`
	got := text(t, write(t, "lease.html", []byte(page)))
	for _, want := range []string{"Lease Terms", "Pets", "No pets & no smoking.", "Rent: $500", "Deposit"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
	for _, bad := range []string{"alert", "color:red", "enable js", "<p>"} {
		if strings.Contains(got, bad) {
			t.Errorf("unexpected %q in %q", bad, got)
		}
	}
}

func TestDOCX(t *testing.T) {
	doc := `<?xml version="1.0"?><w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body>
<w:p><w:r><w:t>The clause about</w:t></w:r><w:r><w:t xml:space="preserve"> pets</w:t></w:r></w:p>
<w:p><w:r><w:t>Col1</w:t><w:tab/><w:t>Col2</w:t></w:r></w:p>
<w:p><w:r><w:instrText>HYPERLINK junk</w:instrText><w:t>Rent &amp; fees</w:t></w:r></w:p>
</w:body></w:document>`
	got := text(t, zipFile(t, "lease.docx", map[string]string{"word/document.xml": doc}))
	want := "The clause about pets\nCol1\tCol2\nRent & fees"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestODT(t *testing.T) {
	content := `<?xml version="1.0"?><office:document-content xmlns:office="urn:oasis:names:tc:opendocument:xmlns:office:1.0" xmlns:text="urn:oasis:names:tc:opendocument:xmlns:text:1.0">
<office:automatic-styles><style>not text</style></office:automatic-styles>
<office:body><office:text><text:h>Title</text:h><text:p>Hello<text:s/>world<text:tab/>tab</text:p>
<text:p>Second <text:span>para</text:span></text:p></office:text></office:body></office:document-content>`
	got := text(t, zipFile(t, "note.odt", map[string]string{"content.xml": content}))
	want := "Title\nHello world\ttab\nSecond para"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestRTF(t *testing.T) {
	rtf := `{\rtf1\ansi\ansicpg1252\deff0{\fonttbl{\f0 Times New Roman;}}{\colortbl;\red255\green0\blue0;}
{\*\generator Riched20;}{\info{\title Secret Title}}
\f0\fs24 Payment declined: error 0x80070005.\par
Caf\'e9 \` + `u8364?5 \{braces\}\par
{\pict\pngblip 89504e47}\tab End\par}`
	got := text(t, write(t, "note.rtf", []byte(rtf)))
	want := "Payment declined: error 0x80070005.\nCafé €5 {braces}\n\tEnd"
	if got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

func TestUnsupportedAndMissing(t *testing.T) {
	if _, err := File(write(t, "x.exe", []byte("MZ"))); !errors.Is(err, ErrUnsupported) {
		t.Errorf("exe: got %v, want ErrUnsupported", err)
	}
	if _, err := File(filepath.Join(t.TempDir(), "missing.txt")); err == nil {
		t.Error("missing file: expected error")
	}
}

func TestTooLargeTextIsRefused(t *testing.T) {
	p := filepath.Join(t.TempDir(), "big.json")
	f, _ := os.Create(p)
	f.Truncate(MaxJSONFile + 1)
	f.Close()
	if _, err := File(p); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("got %v, want ErrTooLarge", err)
	}
}

// A small .docx whose document.xml declares a huge size is refused before reading.
func TestZipBombDeclaredSize(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.CreateHeader(&zip.FileHeader{Name: "word/document.xml", Method: zip.Deflate})
	zeros := make([]byte, 1<<20)
	for range (MaxXMLPart >> 20) + 5 {
		w.Write(zeros)
	}
	zw.Close()
	if buf.Len() > 1<<20 {
		t.Fatalf("bomb fixture is %d bytes, expected tiny", buf.Len())
	}
	p := write(t, "bomb.docx", buf.Bytes())
	if _, err := File(p); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("got %v, want ErrTooLarge", err)
	}
}

// A .docx that lies (claims a tiny size, really expands to more than the limit) is cut
// off at the limit instead of filling memory.
func TestZipBombLyingHeader(t *testing.T) {
	var compressed bytes.Buffer
	fw, _ := flate.NewWriter(&compressed, flate.BestCompression)
	chunk := []byte(strings.Repeat("<w:p><w:r><w:t>aaaaaaaa</w:t></w:r></w:p>", 1<<14))
	for written := 0; written <= MaxXMLPart+len(chunk); written += len(chunk) {
		fw.Write(chunk)
	}
	fw.Close()

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.CreateRaw(&zip.FileHeader{
		Name: "word/document.xml", Method: zip.Deflate,
		CompressedSize64: uint64(compressed.Len()), UncompressedSize64: 100, CRC32: 0,
	})
	if err != nil {
		t.Fatal(err)
	}
	w.Write(compressed.Bytes())
	zw.Close()

	d, err := File(write(t, "liar.docx", buf.Bytes()))
	if err != nil {
		return // refusing is fine too
	}
	if n := len(d.Text()); n > MaxOutputText {
		t.Fatalf("output %d bytes exceeds MaxOutputText", n)
	}
}

func TestNotAZip(t *testing.T) {
	if _, err := File(write(t, "fake.docx", []byte("this is not a zip"))); err == nil {
		t.Fatal("expected error")
	}
}

func TestMalformedRTFNesting(t *testing.T) {
	deep := strings.Repeat("{", 100000) + "x" + strings.Repeat("}", 100000)
	text(t, write(t, "deep.rtf", []byte(deep))) // must not panic or hang
}

func TestMostlyNumeric(t *testing.T) {
	if !MostlyNumeric("12,34,56.7\n2024-01-01;99;100") {
		t.Error("numeric export not detected")
	}
	if MostlyNumeric("Invoice 4471 for the lease of apartment 12") {
		t.Error("normal text flagged as numeric")
	}
}

func TestOutputIsCapped(t *testing.T) {
	d := newDoc(strings.Repeat("é", MaxOutputText)) // 2 bytes each → over the cap
	if !d.Truncated || len(d.Text()) > MaxOutputText || !strings.HasSuffix(d.Text(), "é") {
		t.Fatalf("truncated=%v len=%d", d.Truncated, len(d.Text()))
	}
}
