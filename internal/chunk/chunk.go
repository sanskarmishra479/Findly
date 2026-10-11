// Package chunk splits document text into passages for embedding (plan 9.5): about 350
// tokens each with about 50 tokens of overlap, cut at paragraph or sentence boundaries
// when possible so a passage reads as whole sentences.
package chunk

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Sizes in UTF-8 bytes. Measured with the real tokenizer, 1,400 *characters* were 321
// tokens of English but 940 of Chinese, 1,407 of emoji and 4,669 of rare scripts, which
// overflowed the server batch. Bytes track tokens far better: 1,400 bytes is ~320
// English tokens, ~310 CJK tokens and at most ~1,200 for rare scripts.
const (
	Target  = 1400 // ≈ 350 English tokens
	Overlap = 200  // ≈ 50 English tokens
	// Max is a hard upper bound for one passage, separators included.
	Max = 2000
)

// Chunk is one passage.
type Chunk struct {
	Index int    // 0-based position within the file
	Page  int    // 1-based PDF page, 0 if not paged
	Text  string // passage text
}

// Section is input text with its page number (0 if not paged).
type Section struct {
	Page int
	Text string
}

// Split turns sections into passages. Passages never cross a page boundary, so every
// passage can be opened at its page.
func Split(sections []Section) []Chunk {
	var out []Chunk
	for _, s := range sections {
		for _, text := range splitText(s.Text) {
			out = append(out, Chunk{Index: len(out), Page: s.Page, Text: text})
		}
	}
	return out
}

// splitText packs units (sentences or shorter pieces) into passages of about Target
// characters, starting each new passage with about Overlap characters from the end of
// the previous one.
func splitText(text string) []string {
	units := units(text)
	var out []string
	for start := 0; start < len(units); {
		size, end := 0, start
		for end < len(units) {
			add := units[end].n
			if end > start {
				add += sepLen(units[end-1]) // the space or blank line join() will insert
			}
			if end > start && size+add > Target {
				break
			}
			size += add
			end++
		}
		out = append(out, join(units[start:end]))
		if end == len(units) {
			break
		}
		// Step back over trailing units worth at most Overlap characters, but always
		// move forward by at least one unit so the loop ends.
		next, back := end, 0
		for next-1 > start && back+units[next-1].n+sepLen(units[next-1]) <= Overlap {
			next--
			back += units[next].n + sepLen(units[next])
		}
		start = next
	}
	return out
}

type unit struct {
	text string
	n    int  // length in bytes
	para bool // a paragraph ends after this unit
}

func join(us []unit) string {
	var b strings.Builder
	for i, u := range us {
		b.WriteString(u.text)
		if i < len(us)-1 {
			if u.para {
				b.WriteString("\n\n")
			} else {
				b.WriteByte(' ')
			}
		}
	}
	return b.String()
}

// units cuts text into paragraphs, paragraphs into sentences, and anything still longer
// than Target into word-boundary pieces (or plain rune pieces for text without spaces).
func units(text string) []unit {
	var out []unit
	for _, para := range strings.Split(text, "\n\n") {
		para = strings.Join(strings.Fields(para), " ")
		if para == "" {
			continue
		}
		for _, sent := range sentences(para) {
			for _, piece := range pieces(sent, Target) {
				out = append(out, unit{text: piece, n: len(piece)})
			}
		}
		out[len(out)-1].para = true
	}
	return out
}

// sentences splits after ". ", "! ", "? " and their CJK forms. It errs on the side of
// not splitting (e.g. "Dr. Smith" may split, which is harmless for retrieval).
func sentences(p string) []string {
	var out []string
	start := 0
	rs := []rune(p)
	for i, r := range rs {
		end := false
		switch r {
		case '。', '！', '？':
			end = true
		case '.', '!', '?':
			end = i+1 < len(rs) && rs[i+1] == ' ' && (i+2 >= len(rs) || !unicode.IsLower(rs[i+2]))
		}
		if end {
			s := strings.TrimSpace(string(rs[start : i+1]))
			if s != "" {
				out = append(out, s)
			}
			start = i + 1
		}
	}
	if s := strings.TrimSpace(string(rs[start:])); s != "" {
		out = append(out, s)
	}
	return out
}

// pieces cuts s into parts of at most limit bytes, at spaces when possible, never in the
// middle of a character.
func pieces(s string, limit int) []string {
	var out []string
	for len(s) > limit {
		cut := limit
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		if sp := strings.LastIndexByte(s[:cut], ' '); sp > limit/2 {
			cut = sp
		}
		if p := strings.TrimSpace(s[:cut]); p != "" {
			out = append(out, p)
		}
		s = s[cut:]
	}
	if p := strings.TrimSpace(s); p != "" {
		out = append(out, p)
	}
	return out
}

func sepLen(u unit) int {
	if u.para {
		return 2
	}
	return 1
}
