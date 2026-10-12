//go:build windows

package extract

import (
	"archive/zip"
	"bufio"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/sanskarmishra479/findly/internal/safefs"
)

const (
	// Real documents have a few dozen parts; a zip with more is refused before Go builds
	// its in-memory directory (200k entries cost ~575 MB).
	maxZipEntries = 10000
	// Real documents nest XML maybe 30 deep. Deeper input is hostile.
	maxXMLDepth = 512
)

// openZipPart opens one file inside a .docx/.odt archive, with zip-bomb protection: the
// archive must be small enough and have few entries, the part's declared size must be
// under the limit, and reading stops at the limit even if the archive lies.
func openZipPart(path, part string) (io.ReadCloser, func(), error) {
	info, err := safefs.Stat(path)
	if err != nil {
		return nil, nil, err
	}
	if info.Size > MaxArchiveFile {
		return nil, nil, fmt.Errorf("%w: %s", ErrTooLarge, path)
	}
	f, err := safefs.Open(path)
	if err != nil {
		return nil, nil, err
	}
	if n, err := zipEntryCount(f, info.Size); err != nil || n > maxZipEntries {
		f.Close()
		if err == nil {
			err = fmt.Errorf("%w: %s has %d entries", ErrTooLarge, path, n)
		}
		return nil, nil, err
	}
	zr, err := zip.NewReader(f, info.Size)
	if err != nil {
		f.Close()
		return nil, nil, fmt.Errorf("extract: %s is not a valid document: %w", path, err)
	}
	for _, zf := range zr.File {
		if zf.Name != part {
			continue
		}
		if zf.UncompressedSize64 > MaxXMLPart {
			f.Close()
			return nil, nil, fmt.Errorf("%w: %s in %s", ErrTooLarge, part, path)
		}
		rc, err := zf.Open()
		if err != nil {
			f.Close()
			return nil, nil, err
		}
		limited := struct {
			io.Reader
			io.Closer
		}{io.LimitReader(rc, MaxXMLPart), rc}
		return limited, func() { rc.Close(); f.Close() }, nil
	}
	f.Close()
	return nil, nil, fmt.Errorf("%w: %s in %s", errNotFound, part, path)
}

// zipEntryCount counts the entries of a zip's central directory by walking the headers
// themselves, refusing more than maxZipEntries, before Go's zip.NewReader builds its
// in-memory directory. The count stored in the end-of-directory record is NOT trusted:
// Go's reader ignores it and keeps reading headers, so a forged count (real count mod
// 65536) let a 186 MB file with 1.9 million entries through (508 MB of memory).
func zipEntryCount(r io.ReaderAt, size int64) (int, error) {
	const (
		eocdLen, maxComment = 22, 65535
		eocdSig             = 0x06054b50
		headerSig           = 0x02014b50
		headerLen           = 46
	)
	n := min(size, eocdLen+maxComment)
	buf := make([]byte, n)
	if _, err := r.ReadAt(buf, size-n); err != nil && !errors.Is(err, io.EOF) {
		return 0, err
	}
	eocd := -1
	for i := len(buf) - eocdLen; i >= 0; i-- {
		if binary.LittleEndian.Uint32(buf[i:]) == eocdSig {
			eocd = i
			break
		}
	}
	if eocd < 0 {
		return 0, errors.New("extract: not a valid document (no zip directory)")
	}
	if binary.LittleEndian.Uint16(buf[eocd+10:]) == 0xFFFF ||
		binary.LittleEndian.Uint32(buf[eocd+16:]) == 0xFFFFFFFF {
		return 0, fmt.Errorf("%w: ZIP64 archives are not supported", ErrTooLarge)
	}
	// Go's reader does not simply trust the stored directory offset: when it does not
	// point at a header, it reads from (end record − directory size) instead. So the
	// stored offset and the size must agree exactly, or a forged offset could make us
	// count nothing while Go loads millions of entries. Documents never have data in
	// front of the zip (unlike self-extracting archives), so a mismatch is refused.
	dirSize := int64(binary.LittleEndian.Uint32(buf[eocd+12:]))
	dirStart := int64(binary.LittleEndian.Uint32(buf[eocd+16:]))
	eocdPos := size - n + int64(eocd)
	if dirSize == 0xFFFFFFFF || dirStart+dirSize != eocdPos {
		return 0, errors.New("extract: not a valid document (zip directory position does not match its size)")
	}

	// Walk the headers: 46 fixed bytes, then name, extra field and comment.
	br := bufio.NewReaderSize(io.NewSectionReader(r, dirStart, size-dirStart), 64<<10)
	head := make([]byte, headerLen)
	count := 0
	for {
		if _, err := io.ReadFull(br, head); err != nil {
			return count, nil // end of data: the walk is complete
		}
		if binary.LittleEndian.Uint32(head) != headerSig {
			return count, nil // the end-of-directory record (or junk) follows the headers
		}
		if count++; count > maxZipEntries {
			return count, fmt.Errorf("%w: more than %d entries", ErrTooLarge, maxZipEntries)
		}
		skip := int64(binary.LittleEndian.Uint16(head[28:])) +
			int64(binary.LittleEndian.Uint16(head[30:])) +
			int64(binary.LittleEndian.Uint16(head[32:]))
		if _, err := br.Discard(int(skip)); err != nil {
			return count, nil
		}
	}
}

// xmlRules describe how to turn one XML format into text.
type xmlRules struct {
	start  func(name string) string // text to emit when an element starts (e.g. "\t" for a tab)
	end    func(name string) string // text to emit when an element ends (e.g. "\n" after a paragraph)
	skip   map[string]bool          // elements whose whole subtree is ignored
	textIn map[string]bool          // character data inside these elements is document text
}

// xmlText walks an XML document and builds text using rules. Work per token is constant
// (counters, not stack scans), nesting is capped at maxXMLDepth, and output at
// MaxOutputText. encoding/xml never fetches external entities or DTDs and does not
// expand custom entities, so malicious XML cannot read files or the network.
func xmlText(r io.Reader, rules xmlRules) (string, error) {
	dec := xml.NewDecoder(r)
	dec.Strict = false
	var b strings.Builder
	type frame struct{ skip, text bool }
	var stack []frame
	skipDepth, textDepth := 0, 0
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return b.String(), nil
		}
		if err != nil {
			// A truncated or damaged file: keep what was read so far.
			if b.Len() > 0 {
				return b.String(), nil
			}
			return "", fmt.Errorf("extract: bad XML: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if len(stack) >= maxXMLDepth {
				return b.String(), nil // hostile nesting: stop with what we have
			}
			name := t.Name.Local
			f := frame{skip: rules.skip[name], text: rules.textIn[name]}
			stack = append(stack, f)
			if f.skip {
				skipDepth++
			}
			if f.text {
				textDepth++
			}
			if skipDepth == 0 {
				b.WriteString(rules.start(name))
			}
		case xml.EndElement:
			if len(stack) == 0 {
				continue
			}
			f := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if skipDepth == 0 {
				b.WriteString(rules.end(t.Name.Local))
			}
			if f.skip {
				skipDepth--
			}
			if f.text {
				textDepth--
			}
		case xml.CharData:
			if skipDepth == 0 && textDepth > 0 {
				b.Write(t)
			}
		}
		if b.Len() > MaxOutputText {
			return b.String(), nil
		}
	}
}

// docx reads word/document.xml: <w:t> holds text, <w:p> is a paragraph.
var docxRules = xmlRules{
	start: func(name string) string {
		switch name {
		case "tab":
			return "\t"
		case "br", "cr":
			return "\n"
		case "txbxContent": // a text box: put it on its own line
			return "\n"
		}
		return ""
	},
	end: func(name string) string {
		if name == "p" {
			return "\n"
		}
		return ""
	},
	skip: map[string]bool{
		"Fallback":  true, // mc:Fallback repeats mc:Choice content (text boxes would appear twice)
		"pPr":       true, // paragraph properties: tab stops are not tabs
		"rPr":       true,
		"instrText": true, // field codes, not visible text
		"delText":   true, // deleted text in tracked changes
	},
	textIn: map[string]bool{"t": true},
}

func docx(path string) (string, error) {
	r, closeFn, err := openZipPart(path, "word/document.xml")
	if err != nil {
		return "", err
	}
	defer closeFn()
	return xmlText(r, docxRules)
}

// odt reads content.xml: text is inside <text:p>/<text:h>; <text:s> is a space.
var odtRules = xmlRules{
	start: func(name string) string {
		switch name {
		case "s":
			return " "
		case "tab":
			return "\t"
		case "line-break", "note": // footnotes go on their own line
			return "\n"
		}
		return ""
	},
	end: func(name string) string {
		switch name {
		case "p", "h":
			return "\n"
		}
		return ""
	},
	skip: map[string]bool{
		"automatic-styles": true,
		"tracked-changes":  true, // includes deleted text
		"annotation":       true, // comments with author and date
		"note-citation":    true, // the footnote number
	},
	textIn: map[string]bool{"p": true, "h": true},
}

func odt(path string) (string, error) {
	r, closeFn, err := openZipPart(path, "content.xml")
	if err != nil {
		return "", err
	}
	defer closeFn()
	return xmlText(r, odtRules)
}
