//go:build windows

package opener

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf16"

	"golang.org/x/sys/windows"
)

// These tests never open windows or touch the clipboard: that would disturb the person
// running them. The parts with side effects are thin wrappers around what is tested here.

func TestPageCommand(t *testing.T) {
	path := `C:\Users\me\Docs\My Report.pdf`
	cases := map[string]string{
		`C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe`: "file:///C:/Users/me/Docs/My%20Report.pdf#page=4",
		`C:\Program Files\SumatraPDF\SumatraPDF.exe`:                   "-page 4 " + path,
		`C:\Program Files\Adobe\Acrobat DC\Acrobat\Acrobat.exe`:        "/A page=4 " + path,
	}
	for app, want := range cases {
		cmd := pageCommand(app, path, 4)
		if cmd == nil {
			t.Fatalf("%s: no page command", app)
		}
		if got := strings.Join(cmd.Args[1:], " "); got != want {
			t.Errorf("%s: args %q, want %q", filepath.Base(app), got, want)
		}
	}
	if pageCommand(`C:\Apps\SomeViewer.exe`, path, 4) != nil {
		t.Error("an unknown PDF app got a page command (it would ignore or misread it)")
	}
}

func TestOnlyKnownTypesOpen(t *testing.T) {
	// Types that run code or change the system when "opened" (the review found several
	// missing from the old blocklist): all must go to Explorer instead.
	for _, name := range []string{"x.exe", "x.BAT", "x.ps1", "x.lnk", "x.msi", "x.hta", "x.vbs",
		"x.msc", "x.chm", "x.xll", "x.xbap", "x.diagcab", "x.theme", "x.themepack", "x.scf",
		"x.website", "x.settingcontent-ms", "x.iso", "x.img", "x.vhd", "x.vhdx", "x.appinstaller",
		"x.msix", "x.appx", "x.py", "x.js", "x.jar", "x.reg", "noextension"} {
		if openable(name) {
			t.Errorf("%s would be opened", name)
		}
	}
	for _, name := range []string{"x.pdf", "x.docx", "x.txt", "x.PNG", "x.jpg", "x.mp3", "x.mp4", "x.md"} {
		if !openable(name) {
			t.Errorf("%s would not open", name)
		}
	}
}

func TestFileURL(t *testing.T) {
	cases := map[string]string{
		`C:\Docs\My Report #2.pdf`:  "file:///C:/Docs/My%20Report%20%232.pdf#page=3",
		`\\server\share\Docs\a.pdf`: "file://server/share/Docs/a.pdf#page=3",
	}
	for path, want := range cases {
		if got := fileURL(path, 3); got != want {
			t.Errorf("fileURL(%q) = %q, want %q", path, got, want)
		}
	}
}
func TestMissingFileIsNotOpened(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "gone.pdf")
	for name, f := range map[string]func(string) error{
		"Open":           func(p string) error { return Open(p, 1) },
		"ShowInExplorer": ShowInExplorer,
		"Copy":           Copy,
	} {
		if err := f(missing); !errors.Is(err, ErrNotAvailable) {
			t.Errorf("%s(missing) = %v, want ErrNotAvailable", name, err)
		}
	}
	dir := t.TempDir()
	if err := Open(dir, 0); !errors.Is(err, ErrNotAvailable) {
		t.Errorf("Open(folder) = %v, want ErrNotAvailable", err)
	}
}

func TestDropFilesLayout(t *testing.T) {
	path := `C:\Docs\Résumé ✓.pdf`
	name, _ := windows.UTF16FromString(path)
	b := dropFiles(name)
	if off := binary.LittleEndian.Uint32(b[0:]); off != 20 {
		t.Fatalf("pFiles = %d, want 20", off)
	}
	if wide := binary.LittleEndian.Uint32(b[16:]); wide != 1 {
		t.Fatalf("fWide = %d, want 1", wide)
	}
	units := make([]uint16, (len(b)-20)/2)
	for i := range units {
		units[i] = binary.LittleEndian.Uint16(b[20+2*i:])
	}
	if n := len(units); units[n-1] != 0 || units[n-2] != 0 {
		t.Fatal("the path list does not end in two NULs")
	}
	if got := string(utf16.Decode(units[:len(units)-2])); got != path {
		t.Fatalf("path = %q, want %q", got, path)
	}
}

// A real clipboard round trip, only when asked (it replaces the clipboard's content):
// FINDLY_CLIPBOARD_TEST=<file> copies that file; the caller checks the clipboard.
func TestCopyToClipboard(t *testing.T) {
	path := os.Getenv("FINDLY_CLIPBOARD_TEST")
	if path == "" {
		t.Skip("set FINDLY_CLIPBOARD_TEST to a file to run (changes the clipboard)")
	}
	if err := Copy(path); err != nil {
		t.Fatal(err)
	}
}
