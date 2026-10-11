//go:build windows

package extract

import (
	"bytes"
	"errors"
	"io"
	"strings"

	"golang.org/x/net/html"
)

func htmlFile(path string) (string, error) {
	b, err := readLimited(path, MaxTextFile)
	if err != nil {
		return "", err
	}
	return htmlToText(decodeText(b)), nil
}

// Elements whose content is never visible text.
var htmlSkip = map[string]bool{
	"script": true, "style": true, "noscript": true, "template": true, "svg": true,
	"head": true, "iframe": true, "object": true,
}

// Elements that start a new line.
var htmlBlock = map[string]bool{
	"p": true, "div": true, "br": true, "li": true, "tr": true, "h1": true, "h2": true, "h3": true,
	"h4": true, "h5": true, "h6": true, "section": true, "article": true, "blockquote": true,
	"pre": true, "table": true, "ul": true, "ol": true, "hr": true, "title": true,
}

// htmlToText keeps visible text and the page title. The tokenizer only reads the string
// it is given; it never loads linked pages, scripts or images.
func htmlToText(s string) string {
	z := html.NewTokenizer(strings.NewReader(s))
	var b bytes.Buffer
	skipDepth := 0
	var title string
	inTitle := false
	for {
		switch z.Next() {
		case html.ErrorToken:
			if errors.Is(z.Err(), io.EOF) || b.Len() > 0 {
				text := b.String()
				if title != "" && !strings.Contains(text, title) {
					text = title + "\n" + text
				}
				return text
			}
			return title
		case html.StartTagToken, html.SelfClosingTagToken:
			name, _ := z.TagName()
			tag := string(name)
			if tag == "title" {
				inTitle = true
			}
			if htmlSkip[tag] && tag != "head" {
				skipDepth++
			}
			if htmlBlock[tag] {
				b.WriteByte('\n')
			}
		case html.EndTagToken:
			name, _ := z.TagName()
			tag := string(name)
			if tag == "title" {
				inTitle = false
			}
			if htmlSkip[tag] && tag != "head" && skipDepth > 0 {
				skipDepth--
			}
			if htmlBlock[tag] {
				b.WriteByte('\n')
			}
		case html.TextToken:
			if inTitle {
				title += strings.TrimSpace(string(z.Text()))
				continue
			}
			if skipDepth == 0 {
				b.Write(z.Text()) // z.Text() already decodes entities like &amp;
			}
		}
		if b.Len() > MaxOutputText {
			return b.String()
		}
	}
}
