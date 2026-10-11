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

// decodeText detects the encoding: UTF-8 (with or without BOM, tolerating a few broken
// bytes), UTF-16 with or without BOM, and falls back to Windows-1252 (the classic
// Notepad "ANSI" on Western PCs).
func decodeText(b []byte) string {
	switch {
	case bytes.HasPrefix(b, []byte{0xEF, 0xBB, 0xBF}):
		return strings.ToValidUTF8(string(b[3:]), "�")
	case bytes.HasPrefix(b, []byte{0xFF, 0xFE}):
		return decodeUTF16(b[2:], false)
	case bytes.HasPrefix(b, []byte{0xFE, 0xFF}):
		return decodeUTF16(b[2:], true)
	}
	if le, ok := looksUTF16(b); ok {
		return decodeUTF16(b, !le)
	}
	valid, invalid := utf8Stats(b)
	switch {
	case invalid == 0:
		return string(b)
	case valid >= invalid:
		// Real UTF-8 with a few damaged bytes: keep it UTF-8, mark the damage. (A
		// Windows-1252 file with accents almost never forms valid UTF-8 sequences.)
		return strings.ToValidUTF8(string(b), "�")
	default:
		return decodeWindows1252(b)
	}
}

// utf8Stats counts valid multi-byte characters and invalid bytes.
func utf8Stats(b []byte) (multibyte, invalid int) {
	for len(b) > 0 {
		r, size := utf8.DecodeRune(b)
		switch {
		case r == utf8.RuneError && size == 1:
			invalid++
		case size > 1:
			multibyte++
		}
		b = b[size:]
	}
	return multibyte, invalid
}

// looksUTF16 detects UTF-16 without a BOM: in mostly-ASCII or mostly-one-script text,
// one of every two bytes is zero (or the same high byte), and the other half has no NULs.
func looksUTF16(b []byte) (littleEndian, ok bool) {
	n := min(len(b), 4096) &^ 1
	if n < 8 {
		return false, false
	}
	var zeroEven, zeroOdd int
	for i := 0; i < n; i += 2 {
		if b[i] == 0 {
			zeroEven++
		}
		if b[i+1] == 0 {
			zeroOdd++
		}
	}
	pairs := n / 2
	switch {
	case zeroOdd*10 >= pairs*4 && zeroEven*20 < pairs: // "H\0i\0" → little-endian
		return true, true
	case zeroEven*10 >= pairs*4 && zeroOdd*20 < pairs: // "\0H\0i" → big-endian
		return false, true
	}
	return false, false
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
		sb.WriteRune(cp1252Rune(c))
	}
	return sb.String()
}

var errNotFound = errors.New("extract: part not found in archive")
