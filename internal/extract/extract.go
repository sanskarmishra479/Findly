//go:build windows

// Package extract turns files into plain text (plan 9.4). It reads user files only
// through safefs, and treats every file as untrusted: sizes are capped, zip archives are
// checked for "zip bombs", and XML is parsed without fetching anything external.
package extract

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"unicode"
)

// Limits. Files above them are refused or cut, never read without bounds.
const (
	MaxTextFile    = 20 << 20  // .txt/.md/.csv/.log/.html read limit
	MaxJSONFile    = 5 << 20   // plan: skip JSON if huge
	MaxArchiveFile = 200 << 20 // .docx/.odt on disk
	MaxXMLPart     = 100 << 20 // uncompressed document.xml / content.xml
	MaxOutputText  = 10 << 20  // extracted text kept per file
)

var (
	// ErrUnsupported is returned for kinds this package does not read (yet).
	ErrUnsupported = errors.New("extract: unsupported file type")
	// ErrTooLarge is returned for files over the read limits.
	ErrTooLarge = errors.New("extract: file is too large")
)

// Section is a part of a document. Page is 1-based for paged formats (PDF), 0 otherwise.
type Section struct {
	Page int
	Text string
}

// Doc is the text extracted from one file.
type Doc struct {
	Sections  []Section
	Truncated bool // text was cut at MaxOutputText
}

// Text returns all sections joined by blank lines.
func (d Doc) Text() string {
	parts := make([]string, 0, len(d.Sections))
	for _, s := range d.Sections {
		parts = append(parts, s.Text)
	}
	return strings.Join(parts, "\n\n")
}

// File extracts text from the file at path, choosing the reader by extension.
func File(path string) (Doc, error) {
	var text string
	var err error
	switch strings.ToLower(filepath.Ext(path)) {
	case ".txt", ".md", ".markdown", ".csv", ".log":
		text, err = plainText(path, MaxTextFile)
	case ".json":
		text, err = plainText(path, MaxJSONFile)
	case ".html", ".htm":
		text, err = htmlFile(path)
	case ".docx":
		text, err = docx(path)
	case ".odt":
		text, err = odt(path)
	case ".rtf":
		text, err = rtfFile(path)
	default:
		return Doc{}, fmt.Errorf("%w: %s", ErrUnsupported, filepath.Ext(path))
	}
	if err != nil {
		return Doc{}, err
	}
	return newDoc(text), nil
}

// newDoc tidies text and caps its size.
func newDoc(text string) Doc {
	text = tidy(text)
	d := Doc{}
	if len(text) > MaxOutputText {
		text = cutUTF8(text, MaxOutputText)
		d.Truncated = true
	}
	if text != "" {
		d.Sections = []Section{{Text: text}}
	}
	return d
}

// tidy normalizes newlines, drops control characters and collapses runs of blank lines.
func tidy(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	var b strings.Builder
	b.Grow(len(s))
	blank := 0
	for rest, more := s, true; more; {
		// strings.Cut walks the lines without building a slice of millions of strings.
		var line string
		line, rest, more = strings.Cut(rest, "\n")
		line = strings.TrimRightFunc(strings.Map(func(r rune) rune {
			if r == '\t' || !unicode.IsControl(r) && r != 0xFEFF {
				return r
			}
			return -1 // drop NUL and other control characters
		}, line), unicode.IsSpace)
		if line == "" {
			blank++
			if blank > 1 || b.Len() == 0 {
				continue
			}
		} else {
			blank = 0
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return strings.TrimSpace(b.String())
}

// cutUTF8 shortens s to at most n bytes without splitting a character.
func cutUTF8(s string, n int) string {
	if n >= len(s) {
		return s
	}
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}

// MostlyNumeric reports text that is mostly digits and punctuation (data exports, logs of
// numbers). Plan 9.3: such text goes into the keyword index only, not the embedder.
func MostlyNumeric(text string) bool {
	var total, numeric int
	for _, r := range text {
		if unicode.IsSpace(r) {
			continue
		}
		total++
		if unicode.IsDigit(r) || unicode.IsPunct(r) || unicode.IsSymbol(r) {
			numeric++
		}
	}
	return total > 0 && numeric*100 > total*60
}
