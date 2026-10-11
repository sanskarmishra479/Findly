// Package chunk splits document text into passages for embedding (plan 9.5): about 350
// tokens each with about 50 tokens of overlap, cut at paragraph or sentence boundaries
// when possible so a passage reads as whole sentences.
package chunk

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Sizes in characters (runes). EmbeddingGemma averages ~4 characters per token for
// English; CJK text is denser, so for it these sizes give fewer tokens, which is safe.
const (
	Target  = 1400 // ≈ 350 tokens
	Overlap = 200  // ≈ 50 tokens
	// Max is a hard upper bound for one passage, far below the model's 8,192-token context.
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
		for end < len(units) && (end == start || size+units[end].n <= Target) {
			size += units[end].n
			end++
		}
		out = append(out, join(units[start:end]))
		if end == len(units) {
			break
		}
		// Step back over trailing units worth at most Overlap characters, but always
		// move forward by at least one unit so the loop ends.
		next, back := end, 0
		for next-1 > start && back+units[next-1].n <= Overlap {
			next--
			back += units[next].n
		}
		start = next
	}
	return out
}

type unit struct {
	text string
	n    int  // length in runes
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
				out = append(out, unit{text: piece, n: utf8.RuneCountInString(piece)})
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

// pieces cuts s into parts of at most limit runes, at spaces when possible.
func pieces(s string, limit int) []string {
	rs := []rune(s)
	var out []string
	for len(rs) > limit {
		cut := limit
		for i := limit; i > limit/2; i-- {
			if rs[i] == ' ' {
				cut = i
				break
			}
		}
		out = append(out, strings.TrimSpace(string(rs[:cut])))
		rs = rs[cut:]
	}
	if s := strings.TrimSpace(string(rs)); s != "" {
		out = append(out, s)
	}
	return out
}
