//go:build windows

package extract

import (
	"archive/zip"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/sanskarmishra479/findly/internal/safefs"
)

// openZipPart opens one file inside a .docx/.odt archive, with zip-bomb protection: the
// archive and the part must be under the size limits, and reading stops at the limit even
// if the archive lies about the uncompressed size.
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

// xmlText walks an XML document and builds text. onStart/onEnd return text to emit for an
// element (e.g. a newline at the end of a paragraph); keep decides whether character
// data inside the current element is document text. encoding/xml never fetches external
// entities or DTDs, so malicious XML cannot make it read other files or the network.
func xmlText(r io.Reader, onStart func(xml.StartElement) string, onEnd func(xml.EndElement) string, keep func(stack []string) bool) (string, error) {
	dec := xml.NewDecoder(r)
	dec.Strict = false
	var b strings.Builder
	var stack []string
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
			stack = append(stack, t.Name.Local)
			b.WriteString(onStart(t))
		case xml.EndElement:
			b.WriteString(onEnd(t))
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		case xml.CharData:
			if keep(stack) {
				b.Write(t)
			}
		}
		if b.Len() > MaxOutputText {
			return b.String(), nil
		}
	}
}

// docx reads word/document.xml: <w:t> holds text, <w:p> is a paragraph.
func docx(path string) (string, error) {
	r, closeFn, err := openZipPart(path, "word/document.xml")
	if err != nil {
		return "", err
	}
	defer closeFn()
	return xmlText(r,
		func(e xml.StartElement) string {
			switch e.Name.Local {
			case "tab":
				return "\t"
			case "br", "cr":
				return "\n"
			}
			return ""
		},
		func(e xml.EndElement) string {
			if e.Name.Local == "p" {
				return "\n"
			}
			return ""
		},
		func(stack []string) bool { return len(stack) > 0 && stack[len(stack)-1] == "t" },
	)
}

// odt reads content.xml: text is anything inside <office:text>; <text:p>/<text:h> are
// paragraphs, <text:s> is a space, <text:tab> a tab.
func odt(path string) (string, error) {
	r, closeFn, err := openZipPart(path, "content.xml")
	if err != nil {
		return "", err
	}
	defer closeFn()
	return xmlText(r,
		func(e xml.StartElement) string {
			switch e.Name.Local {
			case "s":
				return " "
			case "tab":
				return "\t"
			case "line-break":
				return "\n"
			}
			return ""
		},
		func(e xml.EndElement) string {
			switch e.Name.Local {
			case "p", "h":
				return "\n"
			}
			return ""
		},
		func(stack []string) bool {
			// Text inside a paragraph or heading of the office:text body. Whitespace
			// between paragraphs (outside p/h) is source formatting, not content.
			inBody, inPara := false, false
			for _, name := range stack {
				switch name {
				case "text":
					inBody = true
				case "p", "h":
					inPara = true
				}
			}
			return inBody && inPara
		},
	)
}
