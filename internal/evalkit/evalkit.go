// Package evalkit builds the evaluation testbed: it turns plain-text sources in
// evals/corpus into real .pdf, .docx and .odt files so the evals exercise the same
// extractors users' files go through. It also scores search results (Recall@5, MRR@10).
package evalkit

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// PageBreak separates pages in a *.pdf.txt source.
const PageBreak = "<<PAGE>>"

// BuildTestbed converts every file under corpus into out:
//
//	name.pdf.txt  → name.pdf  (pages split by PageBreak)
//	name.docx.txt → name.docx (one paragraph per line)
//	name.odt.txt  → name.odt
//	anything else → copied unchanged
func BuildTestbed(corpus, out string) (int, error) {
	n := 0
	err := filepath.WalkDir(corpus, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(corpus, p)
		if err != nil {
			return err
		}
		src, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		text := strings.ReplaceAll(string(src), "\r\n", "\n")
		dst := filepath.Join(out, rel)
		var data []byte
		switch {
		case strings.HasSuffix(rel, ".pdf.txt"):
			dst = strings.TrimSuffix(dst, ".txt")
			var pages []string
			for _, pg := range strings.Split(text, PageBreak) {
				pages = append(pages, strings.TrimSpace(pg))
			}
			data = MakePDF(pages...)
		case strings.HasSuffix(rel, ".docx.txt"):
			dst = strings.TrimSuffix(dst, ".txt")
			data, err = MakeDOCX(lines(text))
		case strings.HasSuffix(rel, ".odt.txt"):
			dst = strings.TrimSuffix(dst, ".txt")
			data, err = MakeODT(lines(text))
		default:
			data = src
		}
		if err != nil {
			return fmt.Errorf("%s: %w", rel, err)
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		n++
		return os.WriteFile(dst, data, 0o644)
	})
	return n, err
}

func lines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

// MakePDF builds a valid PDF with Helvetica text, one page per string; long pages are
// wrapped into lines. Text must be Latin-1 (the eval PDFs are English).
func MakePDF(pages ...string) []byte {
	var objs []string
	kids := make([]string, len(pages))
	for i := range pages {
		kids[i] = fmt.Sprintf("%d 0 R", 4+2*i)
	}
	objs = append(objs,
		"<< /Type /Catalog /Pages 2 0 R >>",
		fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), len(pages)),
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
	)
	esc := strings.NewReplacer(`\`, `\\`, "(", `\(`, ")", `\)`)
	for i, text := range pages {
		var stream strings.Builder
		y := 740
		for _, line := range wrap(text, 90) {
			fmt.Fprintf(&stream, "BT /F1 10 Tf 50 %d Td (%s) Tj ET\n", y, esc.Replace(line))
			y -= 14
		}
		s := stream.String()
		objs = append(objs,
			fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Resources << /Font << /F1 3 0 R >> >> /Contents %d 0 R >>", 5+2*i),
			fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(s), s),
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

func wrap(text string, width int) []string {
	var out []string
	for _, para := range strings.Split(text, "\n") {
		line := ""
		for _, w := range strings.Fields(para) {
			if line != "" && len(line)+1+len(w) > width {
				out = append(out, line)
				line = ""
			}
			if line != "" {
				line += " "
			}
			line += w
		}
		out = append(out, line)
	}
	return out
}

func xmlEscape(s string) string {
	var b bytes.Buffer
	xml.EscapeText(&b, []byte(s))
	return b.String()
}

// MakeDOCX builds a minimal Word document, one paragraph per entry.
func MakeDOCX(paragraphs []string) ([]byte, error) {
	var body strings.Builder
	for _, p := range paragraphs {
		fmt.Fprintf(&body, `<w:p><w:r><w:t xml:space="preserve">%s</w:t></w:r></w:p>`, xmlEscape(p))
	}
	return zipParts([]part{
		{"[Content_Types].xml", `<?xml version="1.0" encoding="UTF-8"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/><Default Extension="xml" ContentType="application/xml"/><Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/></Types>`, false},
		{"_rels/.rels", `<?xml version="1.0" encoding="UTF-8"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="word/document.xml"/></Relationships>`, false},
		{"word/document.xml", `<?xml version="1.0" encoding="UTF-8"?><w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body>` + body.String() + `</w:body></w:document>`, false},
	})
}

// MakeODT builds a minimal OpenDocument text file, one paragraph per entry.
func MakeODT(paragraphs []string) ([]byte, error) {
	var body strings.Builder
	for _, p := range paragraphs {
		fmt.Fprintf(&body, `<text:p>%s</text:p>`, xmlEscape(p))
	}
	// OpenDocument rules: "mimetype" must be the first entry and stored uncompressed, and
	// META-INF/manifest.xml lists the parts (LibreOffice rejects files without them).
	return zipParts([]part{
		{"mimetype", "application/vnd.oasis.opendocument.text", true},
		{"META-INF/manifest.xml", `<?xml version="1.0" encoding="UTF-8"?><manifest:manifest xmlns:manifest="urn:oasis:names:tc:opendocument:xmlns:manifest:1.0" manifest:version="1.2"><manifest:file-entry manifest:full-path="/" manifest:media-type="application/vnd.oasis.opendocument.text"/><manifest:file-entry manifest:full-path="content.xml" manifest:media-type="text/xml"/></manifest:manifest>`, false},
		{"content.xml", `<?xml version="1.0" encoding="UTF-8"?><office:document-content xmlns:office="urn:oasis:names:tc:opendocument:xmlns:office:1.0" xmlns:text="urn:oasis:names:tc:opendocument:xmlns:text:1.0" office:version="1.2"><office:body><office:text>` + body.String() + `</office:text></office:body></office:document-content>`, false},
	})
}

type part struct {
	name, content string
	stored        bool // no compression
}

// zipParts writes the parts in the given order (ranging over a map would randomize it).
func zipParts(parts []part) ([]byte, error) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, p := range parts {
		method := zip.Deflate
		if p.stored {
			method = zip.Store
		}
		w, err := zw.CreateHeader(&zip.FileHeader{Name: p.name, Method: method})
		if err != nil {
			return nil, err
		}
		if _, err := w.Write([]byte(p.content)); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ---- queries and scoring ----

// Case is one line of evals/queries.jsonl.
type Case struct {
	Query  string   `json:"query"`
	Expect []string `json:"expect"` // "folder/file.pdf" or "folder/file.pdf#page=3"; empty = should find nothing
}

// LoadCases reads a JSONL file of cases.
func LoadCases(path string) ([]Case, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var out []Case
	for i, line := range strings.Split(string(b), "\n") {
		if line = strings.TrimSpace(line); line == "" {
			continue
		}
		var c Case
		if err := json.Unmarshal([]byte(line), &c); err != nil {
			return nil, fmt.Errorf("%s line %d: %w", path, i+1, err)
		}
		if c.Query == "" || c.Expect == nil {
			return nil, fmt.Errorf("%s line %d: query and expect are required (expect: [] for a query that should find nothing)", path, i+1)
		}
		out = append(out, c)
	}
	return out, nil
}

// Hit is one search result in eval form: path relative to the testbed, plus page.
type Hit struct {
	Rel  string
	Page int
}

// Matches reports whether a hit satisfies an expectation like "a/b.pdf#page=2".
// Without "#page", any page of the file counts.
func Matches(expect string, h Hit) bool {
	path, frag, _ := strings.Cut(expect, "#")
	if !strings.EqualFold(filepath.ToSlash(h.Rel), path) {
		return false
	}
	if p, ok := strings.CutPrefix(frag, "page="); ok {
		return fmt.Sprint(h.Page) == p
	}
	return true
}

// Score returns recall@5 (share of expectations found in the top 5) and the reciprocal
// rank of the first expectation found in the top 10 (0 if none).
func Score(c Case, hits []Hit) (recall5, rr10 float64) {
	found := 0
	for _, e := range c.Expect {
		for i, h := range hits {
			if i >= 5 {
				break
			}
			if Matches(e, h) {
				found++
				break
			}
		}
	}
	recall5 = float64(found) / float64(len(c.Expect))
	for i, h := range hits {
		if i >= 10 {
			break
		}
		for _, e := range c.Expect {
			if Matches(e, h) {
				return recall5, 1 / float64(i+1)
			}
		}
	}
	return recall5, 0
}
