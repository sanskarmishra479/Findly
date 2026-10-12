package search

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Query is a parsed user query.
type Query struct {
	Raw     string
	Words   []string // search words, in order
	Phrases []string // "quoted parts": must appear exactly (plan 9.11)
}

// Query size limits: a huge paste must not make every keystroke slow (the embedder also
// cuts queries at 500 runes).
const (
	maxQueryRunes = 500
	maxQueryWords = 32
)

// Parse splits the user's text into words and quoted phrases. It never fails.
func Parse(raw string) (q Query) {
	if utf8.RuneCountInString(raw) > maxQueryRunes {
		raw = string([]rune(raw)[:maxQueryRunes])
	}
	q = Query{Raw: raw}
	// q is a named result, so this deferred trim changes what Parse returns.
	defer func() {
		if len(q.Words) > maxQueryWords {
			q.Words = q.Words[:maxQueryWords]
		}
		if len(q.Phrases) > maxQueryWords {
			q.Phrases = q.Phrases[:maxQueryWords]
		}
	}()
	rest := raw
	for {
		start := strings.IndexByte(rest, '"')
		if start < 0 {
			q.Words = append(q.Words, words(rest)...)
			break
		}
		q.Words = append(q.Words, words(rest[:start])...)
		end := strings.IndexByte(rest[start+1:], '"')
		if end < 0 { // unclosed quote: treat the rest as words
			q.Words = append(q.Words, words(rest[start+1:])...)
			break
		}
		if p := strings.Join(words(rest[start+1:start+1+end]), " "); p != "" {
			q.Phrases = append(q.Phrases, p)
		}
		rest = rest[start+1+end+1:]
	}
	return q
}

// words keeps runs of letters, digits and combining marks; everything else separates
// words, which drops every FTS5 operator character. Marks must stay inside the word:
// Devanagari vowel signs are marks, and splitting at them turned "किराया" (rent) into
// single letters joined by AND, which matched almost any Hindi text. Kept whole and
// quoted, FTS5 matches the word as a phrase of its own tokens.
//
// A mark only counts when it follows a letter or digit. Emoji carry marks of their own
// (the variation selector U+FE0F after "❤", the keycap U+20E3), and a word made of just
// that mark matches nothing, so "rent ❤️" would find no file at all. Variation selectors
// never belong to a word.
func words(s string) []string {
	var out []string
	start := -1 // byte offset where the current word began, or -1
	for i, r := range s {
		inWord := unicode.IsLetter(r) || unicode.IsNumber(r) ||
			(start >= 0 && unicode.IsMark(r) && !unicode.Is(unicode.Variation_Selector, r))
		switch {
		case inWord && start < 0:
			start = i
		case !inWord && start >= 0:
			out = append(out, s[start:i])
			start = -1
		}
	}
	if start >= 0 {
		out = append(out, s[start:])
	}
	return out
}

// Match builds a safe FTS5 MATCH expression: every word is quoted, so user text can
// never be read as FTS5 syntax (AND, NEAR, column filters, ...). All words must match;
// the last word also matches as a prefix while the user is still typing it. Quoted
// phrases must match as phrases. Returns "" when there is nothing to search for.
func (q Query) Match(typing bool) string {
	var parts []string
	for i, w := range q.Words {
		t := quote(w)
		// A one-letter prefix would match a huge number of words; wait for the second letter.
		if typing && i == len(q.Words)-1 && len(q.Phrases) == 0 && utf8.RuneCountInString(w) >= 2 {
			t += "*"
		}
		parts = append(parts, t)
	}
	for _, p := range q.Phrases {
		parts = append(parts, quote(p))
	}
	return strings.Join(parts, " AND ")
}

func quote(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// Text is the query as plain words, for the embedder.
func (q Query) Text() string {
	all := append(append([]string{}, q.Words...), q.Phrases...)
	return strings.Join(all, " ")
}

// Empty reports whether there is nothing to search for.
func (q Query) Empty() bool { return len(q.Words) == 0 && len(q.Phrases) == 0 }
