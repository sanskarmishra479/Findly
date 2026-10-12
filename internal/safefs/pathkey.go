//go:build windows

package safefs

import (
	"path/filepath"
	"strings"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

var procRtlUpcaseUnicodeString = windows.NewLazySystemDLL("ntdll.dll").NewProc("RtlUpcaseUnicodeString")

// PathKey returns the form used to compare Windows paths: cleaned and upper-cased with
// Windows' own rules (RtlUpcaseUnicodeString, the case mapping NTFS uses).
//
// strings.ToUpper is not the same: it treats "σ.txt"/"ς.txt", "µ.txt"/"μ.txt" or
// "s.txt"/"ſ.txt" as equal, but NTFS keeps each pair as two different files. Two real
// files sharing one key would share one index entry.
func PathKey(path string) string {
	p := filepath.Clean(path)
	u := utf16.Encode([]rune(p))
	const maxChars = 0xFFFE / 2 // UNICODE_STRING lengths are 16-bit byte counts
	if len(u) == 0 || len(u) > maxChars {
		return strings.ToUpper(p) // never happens for real paths; keep a sane fallback
	}
	out := make([]uint16, len(u))
	src := windows.NTUnicodeString{Length: uint16(2 * len(u)), MaximumLength: uint16(2 * len(u)), Buffer: &u[0]}
	dst := windows.NTUnicodeString{Length: 0, MaximumLength: uint16(2 * len(out)), Buffer: &out[0]}
	status, _, _ := procRtlUpcaseUnicodeString.Call(uintptr(unsafe.Pointer(&dst)), uintptr(unsafe.Pointer(&src)), 0)
	if status != 0 {
		return strings.ToUpper(p)
	}
	return string(utf16.Decode(out[:dst.Length/2]))
}
