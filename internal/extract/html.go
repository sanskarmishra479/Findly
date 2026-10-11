//go:build windows

package extract

import (
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
	"iframe": true, "object": true,
}

// Elements that separate text (a new line before and after).
var htmlBlock = map[string]bool{
	"p": true, "div": true, "br": true, "li": true, "tr": true, "td": true, "th": true,
	"dt": true, "dd": true, "h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
	"section": true, "article": true, "header": true, "footer": true, "nav": true, "main": true,
	"aside": true, "figcaption": true, "caption": true, "blockquote": true, "pre": true,
	"table": true, "ul": true, "ol": true, "dl": true, "hr": true, "title": true,
}

const maxTitle = 1024

// htmlToText keeps visible text and the first page title. The tokenizer only reads the
// string it is given; it never loads linked pages, scripts or images.
func htmlToText(s string) string {
	z := html.NewTokenizer(strings.NewReader(s))
	var b, title strings.Builder
	skipDepth := 0
	inTitle, titleDone := false, false
	finish := func() string {
		text := b.String()
		if t := strings.TrimSpace(title.String()); t != "" && !strings.Contains(text, t) {
			text = t + "\n" + text
		}
		return text
	}
	for {
		switch z.Next() {
		case html.ErrorToken:
			if !errors.Is(z.Err(), io.EOF) && b.Len() == 0 && title.Len() == 0 {
				return ""
			}
			return finish()
		case html.StartTagToken:
			name, _ := z.TagName()
			tag := string(name)
			if tag == "title" {
				inTitle = true // later titles are read and ignored, not added to the body
			}
			if htmlSkip[tag] {
				skipDepth++
			}
			if htmlBlock[tag] {
				b.WriteByte('\n')
			}
		case html.SelfClosingTagToken:
			// <svg/> or <br/>: no content follows, so never change skipDepth here.
			name, _ := z.TagName()
			if htmlBlock[string(name)] {
				b.WriteByte('\n')
			}
		case html.EndTagToken:
			name, _ := z.TagName()
			tag := string(name)
			if tag == "title" && inTitle {
				inTitle, titleDone = false, true
			}
			if htmlSkip[tag] && skipDepth > 0 {
				skipDepth--
			}
			if htmlBlock[tag] {
				b.WriteByte('\n')
			}
		case html.TextToken:
			if inTitle {
				if !titleDone && title.Len() < maxTitle {
					title.WriteString(cutUTF8(string(z.Text()), maxTitle-title.Len()))
				}
				continue
			}
			if skipDepth == 0 {
				b.Write(z.Text()) // z.Text() already decodes entities like &amp;
			}
		}
		if b.Len() > MaxOutputText {
			return finish()
		}
	}
}
