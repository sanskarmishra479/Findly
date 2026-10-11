//go:build windows

package extract

import (
	"strconv"
	"strings"
	"unicode/utf16"
)

func rtfFile(path string) (string, error) {
	b, err := readLimited(path, MaxTextFile)
	if err != nil {
		return "", err
	}
	return rtfToText(b), nil
}

// Groups whose content is not document text (fonts, colors, styles, metadata, pictures).
var rtfSkipDest = map[string]bool{
	"fonttbl": true, "colortbl": true, "stylesheet": true, "info": true, "pict": true,
	"header": true, "footer": true, "headerl": true, "headerr": true, "footerl": true, "footerr": true,
	"object": true, "themedata": true, "colorschememapping": true, "datastore": true,
	"latentstyles": true, "listtable": true, "listoverridetable": true, "rsidtbl": true,
	"generator": true, "xmlnstbl": true, "filetbl": true, "revtbl": true, "fldinst": true,
}

const (
	maxRTFDepth = 1000
	maxRTFUC    = 8 // \ucN: real files use 0–2; a huge N would swallow the rest of the text
)

// rtfToText is a small RTF reader: it keeps plain text, paragraph breaks, \'hh
// (Windows-1252) and \uN (Unicode) characters, and skips formatting groups and binary
// data. \ansicpg is not honored: non-Western \'hh text decodes as Windows-1252 (modern
// Word writes \uN for those characters, which works).
func rtfToText(b []byte) string {
	type state struct {
		skip bool
		uc   int // fallback characters that follow each \uN
	}
	stack := []state{{uc: 1}}
	var out strings.Builder
	var pendingHigh rune // first half of a UTF-16 surrogate pair from \uN, or 0
	skipChars := 0       // fallback characters still to skip after \uN

	emit := func(r rune) {
		if stack[len(stack)-1].skip {
			return
		}
		if pendingHigh != 0 { // a high surrogate not followed by a low one
			out.WriteRune('�')
			pendingHigh = 0
		}
		out.WriteRune(r)
	}

	for i := 0; i < len(b); i++ {
		c := b[i]
		switch c {
		case '{':
			skipChars = 0
			stack = append(stack, stack[len(stack)-1])
			if len(stack) > maxRTFDepth { // malformed or hostile nesting
				return out.String()
			}
			// "{\*\dest ...}" marks an optional destination readers may ignore.
			if i+2 < len(b) && b[i+1] == '\\' && b[i+2] == '*' {
				stack[len(stack)-1].skip = true
			}
		case '}':
			skipChars = 0
			if len(stack) > 1 {
				stack = stack[:len(stack)-1]
			}
		case '\\':
			if i+1 >= len(b) {
				break
			}
			n := b[i+1]
			switch {
			case n == '\'' && i+3 < len(b): // \'hh
				if v, err := strconv.ParseUint(string(b[i+2:i+4]), 16, 8); err == nil {
					if skipChars > 0 {
						skipChars--
					} else {
						emit(cp1252Rune(byte(v)))
					}
				}
				i += 3
			case n == '\\' || n == '{' || n == '}':
				emit(rune(n))
				i++
			case n == '~':
				emit(0xA0) // non-breaking space
				i++
			case n == '\n' || n == '\r':
				emit('\n')
				i++
			case isLetter(n):
				j := i + 1
				for j < len(b) && j-i <= 32 && isLetter(b[j]) {
					j++
				}
				word := string(b[i+1 : j])
				k := j
				if k < len(b) && (b[k] == '-' || isDigit(b[k])) {
					k++
					for k < len(b) && k-j <= 11 && isDigit(b[k]) {
						k++
					}
				}
				param, hasParam := 0, k > j
				if hasParam {
					param, _ = strconv.Atoi(string(b[j:k]))
				}
				if k < len(b) && b[k] == ' ' { // a single space ends a control word
					k++
				}
				i = k - 1

				switch {
				case word == "bin" && hasParam:
					// Raw binary data follows; it may contain braces, so jump over it.
					i += max(0, min(param, len(b)-1-i))
				case rtfSkipDest[word]:
					stack[len(stack)-1].skip = true
				case word == "par" || word == "line" || word == "sect" || word == "row":
					emit('\n')
				case word == "tab" || word == "cell":
					emit('\t')
				case word == "uc" && hasParam:
					stack[len(stack)-1].uc = max(0, min(param, maxRTFUC))
				case word == "u" && hasParam:
					r := rune(uint16(param)) // negative values are UTF-16 units above 32767
					switch {
					case utf16.IsSurrogate(r) && r < 0xDC00: // high surrogate: wait for the low one
						if pendingHigh != 0 {
							emit('�')
						}
						if !stack[len(stack)-1].skip {
							pendingHigh = r
						}
					case utf16.IsSurrogate(r) && pendingHigh != 0: // low surrogate completing a pair
						hi := pendingHigh
						pendingHigh = 0
						emit(utf16.DecodeRune(hi, r))
					case utf16.IsSurrogate(r): // lone low surrogate
						emit('�')
					default:
						emit(r)
					}
					skipChars = stack[len(stack)-1].uc
				}
			default:
				i++ // other control symbols: ignore
			}
		case '\r', '\n':
			// Raw newlines in RTF source are not text.
		default:
			if skipChars > 0 {
				skipChars--
				continue
			}
			if !stack[len(stack)-1].skip {
				emit(cp1252Rune(c))
			}
		}
	}
	return out.String()
}

// cp1252Rune decodes one Windows-1252 byte without allocating.
func cp1252Rune(c byte) rune {
	if c >= 0x80 && c <= 0x9F {
		return cp1252[c-0x80]
	}
	return rune(c)
}

func isLetter(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }
func isDigit(c byte) bool  { return c >= '0' && c <= '9' }
