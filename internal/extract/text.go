//go:build windows

package extract

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/sanskarmishra479/findly/internal/safefs"
)

// readLimited reads a user file through safefs, refusing files over limit.
func readLimited(path string, limit int64) ([]byte, error) {
	info, err := safefs.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.Size > limit {
		return nil, fmt.Errorf("%w: %s (%d bytes)", ErrTooLarge, path, info.Size)
	}
	b, err := safefs.ReadFile(path, limit)
	if err != nil {
		return nil, err
	}
	return b, nil
}

func plainText(path string, limit int64) (string, error) {
	b, err := readLimited(path, limit)
	if err != nil {
		return "", err
	}
	return decodeText(b), nil
}

// decodeText detects the encoding: UTF-8 (with or without BOM), UTF-16 with BOM, and
// falls back to Windows-1252 (the classic Notepad "ANSI" on Western PCs).
func decodeText(b []byte) string {
	switch {
	case bytes.HasPrefix(b, []byte{0xEF, 0xBB, 0xBF}):
		return strings.ToValidUTF8(string(b[3:]), "�")
	case bytes.HasPrefix(b, []byte{0xFF, 0xFE}):
		return decodeUTF16(b[2:], false)
	case bytes.HasPrefix(b, []byte{0xFE, 0xFF}):
		return decodeUTF16(b[2:], true)
	case utf8.Valid(b):
		return string(b)
	default:
		return decodeWindows1252(b)
	}
}

func decodeUTF16(b []byte, bigEndian bool) string {
	u := make([]uint16, len(b)/2)
	for i := range u {
		if bigEndian {
			u[i] = uint16(b[2*i])<<8 | uint16(b[2*i+1])
		} else {
			u[i] = uint16(b[2*i+1])<<8 | uint16(b[2*i])
		}
	}
	return string(utf16.Decode(u))
}

// cp1252 maps bytes 0x80–0x9F; all other bytes equal their Unicode code point (Latin-1).
var cp1252 = [32]rune{
	'€', '�', '‚', 'ƒ', '„', '…', '†', '‡', 'ˆ', '‰', 'Š', '‹', 'Œ', '�', 'Ž', '�',
	'�', '‘', '’', '“', '”', '•', '–', '—', '˜', '™', 'š', '›', 'œ', '�', 'ž', 'Ÿ',
}

func decodeWindows1252(b []byte) string {
	var sb strings.Builder
	sb.Grow(len(b))
	for _, c := range b {
		if c >= 0x80 && c <= 0x9F {
			sb.WriteRune(cp1252[c-0x80])
		} else {
			sb.WriteRune(rune(c))
		}
	}
	return sb.String()
}

var errNotFound = errors.New("extract: part not found in archive")
