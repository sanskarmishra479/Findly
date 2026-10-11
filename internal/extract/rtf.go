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

// rtfToText is a small RTF reader: it keeps plain text, paragraph breaks, \'hh
// (Windows-1252) and \uN (Unicode) characters, and skips formatting groups.
func rtfToText(b []byte) string {
	type state struct {
		skip bool
		uc   int // characters to skip after \uN
	}
	stack := []state{{uc: 1}}
	var out strings.Builder
	var pendingHigh uint16 // first half of a UTF-16 surrogate pair from \uN
	skipChars := 0         // fallback characters still to skip after \uN

	emit := func(r rune) {
		if !stack[len(stack)-1].skip {
			out.WriteRune(r)
		}
	}

	for i := 0; i < len(b); i++ {
		c := b[i]
		switch c {
		case '{':
			top := stack[len(stack)-1]
			stack = append(stack, top)
			if len(stack) > 1000 { // malformed or hostile nesting
				return out.String()
			}
			// "{\*\dest ...}" marks an optional destination readers may ignore.
			if i+2 < len(b) && b[i+1] == '\\' && b[i+2] == '*' {
				stack[len(stack)-1].skip = true
			}
		case '}':
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
						emit([]rune(decodeWindows1252([]byte{byte(v)}))[0])
					}
				}
				i += 3
			case n == '\\' || n == '{' || n == '}':
				emit(rune(n))
				i++
			case n == '~':
				emit(' ')
				i++
			case n == '\n' || n == '\r':
				emit('\n')
				i++
			case isLetter(n):
				j := i + 1
				for j < len(b) && isLetter(b[j]) {
					j++
				}
				word := string(b[i+1 : j])
				k := j
				if k < len(b) && (b[k] == '-' || isDigit(b[k])) {
					k++
					for k < len(b) && isDigit(b[k]) {
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
				case rtfSkipDest[word]:
					stack[len(stack)-1].skip = true
				case word == "par" || word == "line" || word == "sect" || word == "row":
					emit('\n')
				case word == "tab" || word == "cell":
					emit('\t')
				case word == "uc" && hasParam:
					stack[len(stack)-1].uc = param
				case word == "u" && hasParam:
					u := uint16(param) // negative values are UTF-16 units above 32767
					switch {
					case utf16.IsSurrogate(rune(u)) && pendingHigh == 0:
						pendingHigh = u
					case pendingHigh != 0:
						emit(utf16.DecodeRune(rune(pendingHigh), rune(u)))
						pendingHigh = 0
					default:
						emit(rune(u))
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
			emit([]rune(decodeWindows1252([]byte{c}))[0])
		}
	}
	return out.String()
}

func isLetter(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }
func isDigit(c byte) bool  { return c >= '0' && c <= '9' }
