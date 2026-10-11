package search

import (
	"strings"
	"unicode"
)

// Query is a parsed user query.
type Query struct {
	Raw     string
	Words   []string // search words, in order
	Phrases []string // "quoted parts": must appear exactly (plan 9.11)
}

// Parse splits the user's text into words and quoted phrases. It never fails.
func Parse(raw string) Query {
	q := Query{Raw: raw}
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

// words keeps runs of letters and digits; everything else separates words. This matches
// how FTS5's unicode61 tokenizer splits text, and drops every FTS5 operator character.
func words(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r)
	})
}

// Match builds a safe FTS5 MATCH expression: every word is quoted, so user text can
// never be read as FTS5 syntax (AND, NEAR, column filters, ...). All words must match;
// the last word also matches as a prefix while the user is still typing it. Quoted
// phrases must match as phrases. Returns "" when there is nothing to search for.
func (q Query) Match(typing bool) string {
	var parts []string
	for i, w := range q.Words {
		t := quote(w)
		if typing && i == len(q.Words)-1 && len(q.Phrases) == 0 {
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
