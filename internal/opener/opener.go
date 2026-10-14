//go:build windows

// Package opener carries out the only actions Findly offers on a user's file (plan 9.15
// and I-6): open it, open it at a page, show it in Explorer, copy it to the clipboard.
// Nothing here writes, moves, renames or deletes a file.
package opener

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/sanskarmishra479/findly/internal/safefs"
	"github.com/sanskarmishra479/findly/internal/scan"
)

var (
	shell32  = windows.NewLazySystemDLL("shell32.dll")
	shlwapi  = windows.NewLazySystemDLL("shlwapi.dll")
	user32   = windows.NewLazySystemDLL("user32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")

	procShellExecuteW        = shell32.NewProc("ShellExecuteW")
	procAssocQueryStringW    = shlwapi.NewProc("AssocQueryStringW")
	procAssocIsDangerous     = shlwapi.NewProc("AssocIsDangerous")
	procOpenClipboard        = user32.NewProc("OpenClipboard")
	procCloseClipboard       = user32.NewProc("CloseClipboard")
	procCreateWindowExW      = user32.NewProc("CreateWindowExW")
	procDestroyWindow        = user32.NewProc("DestroyWindow")
	procEmptyClipboard       = user32.NewProc("EmptyClipboard")
	procSetClipboardData     = user32.NewProc("SetClipboardData")
	procRegisterClipboardFmt = user32.NewProc("RegisterClipboardFormatW")
	procGlobalAlloc          = kernel32.NewProc("GlobalAlloc")
	procGlobalLock           = kernel32.NewProc("GlobalLock")
	procGlobalUnlock         = kernel32.NewProc("GlobalUnlock")
	procGlobalFree           = kernel32.NewProc("GlobalFree")
	procRtlMoveMemory        = kernel32.NewProc("RtlMoveMemory")
)

// ErrNotAvailable means the file is gone or is not a plain file (e.g. a link).
var ErrNotAvailable = errors.New("opener: the file is no longer there")

// openable reports whether Open may hand the file to its app. Search must never start a
// program by accident, and a list of dangerous types is never complete (.msc, .chm,
// .xll, disk images, ... all run code or change the system). So it is an allowlist:
// only the document and media types Findly indexes (text, documents, PDFs, images,
// audio, video), and only if Windows itself does not mark the type as dangerous.
// Everything else is shown in Explorer instead.
func openable(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	return scan.Classify(path) != scan.KindOther && !assocIsDangerous(ext)
}

// assocIsDangerous asks Windows whether opening files of type ext is dangerous.
func assocIsDangerous(ext string) bool {
	e, err := windows.UTF16PtrFromString(ext)
	if err != nil {
		return true
	}
	r, _, _ := procAssocIsDangerous.Call(uintptr(unsafe.Pointer(e)))
	return r != 0
}

// check confirms path is still an existing plain file. It is read-only (safefs).
func check(path string) error {
	info, err := safefs.Stat(path)
	if err != nil || info.IsDir() || info.IsLink() {
		return ErrNotAvailable
	}
	return nil
}

// Open opens the file in its usual app. For a PDF with page > 0 it opens that page when
// the user's PDF app supports it (Edge, Chrome, Firefox, SumatraPDF, Adobe); any other
// app opens the file at its first page. Anything but a known document or media type
// (programs, scripts, ...) is shown in Explorer instead of being opened (openable).
func Open(path string, page int) error {
	if err := check(path); err != nil {
		return err
	}
	if !openable(path) {
		return ShowInExplorer(path)
	}
	ext := strings.ToLower(filepath.Ext(path))
	if ext == ".pdf" && page > 0 {
		if app := assocExe(".pdf"); app != "" {
			if cmd := pageCommand(app, path, page); cmd != nil {
				return cmd.Start()
			}
		}
	}
	return shellOpen(path)
}

// pageCommand returns a command that opens path at page in app, or nil if app has no
// known way to do that.
func pageCommand(app, path string, page int) *exec.Cmd {
	switch strings.ToLower(filepath.Base(app)) {
	case "msedge.exe", "chrome.exe", "firefox.exe", "brave.exe":
		return exec.Command(app, fileURL(path, page))
	case "sumatrapdf.exe":
		return exec.Command(app, "-page", fmt.Sprint(page), path)
	case "acrobat.exe", "acrord32.exe":
		return exec.Command(app, "/A", fmt.Sprintf("page=%d", page), path)
	}
	return nil
}

func shellOpen(path string) error {
	verb, _ := windows.UTF16PtrFromString("open")
	file, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	dir, _ := windows.UTF16PtrFromString(filepath.Dir(path))
	r, _, _ := procShellExecuteW.Call(0, uintptr(unsafe.Pointer(verb)), uintptr(unsafe.Pointer(file)), 0,
		uintptr(unsafe.Pointer(dir)), windows.SW_SHOWNORMAL)
	if r <= 32 { // ShellExecute reports errors as values <= 32
		return fmt.Errorf("opener: could not open the file (code %d)", r)
	}
	return nil
}

// ShowInExplorer opens the file's folder with the file selected.
func ShowInExplorer(path string) error {
	if err := check(path); err != nil {
		return err
	}
	explorer := filepath.Join(os.Getenv("SystemRoot"), "explorer.exe")
	cmd := exec.Command(explorer)
	// Explorer parses "/select," itself and needs the path quoted after the comma, which
	// Go's usual argument quoting would not produce.
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: fmt.Sprintf(`"%s" /select,"%s"`, explorer, path)}
	return cmd.Start()
}

// assocExe returns the program Windows uses to open files with ext, or "".
func assocExe(ext string) string {
	const assocStrExecutable = 2
	e, _ := windows.UTF16PtrFromString(ext)
	verb, _ := windows.UTF16PtrFromString("open")
	buf := make([]uint16, windows.MAX_PATH)
	n := uint32(len(buf))
	if r, _, _ := procAssocQueryStringW.Call(0, assocStrExecutable, uintptr(unsafe.Pointer(e)),
		uintptr(unsafe.Pointer(verb)), uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&n))); r != 0 {
		return ""
	}
	return windows.UTF16ToString(buf)
}

// Copy puts the file on the clipboard the way Explorer does, marked as a copy (never a
// cut): pasting it somewhere makes a copy and leaves the original where it is.
func Copy(path string) error {
	if err := check(path); err != nil {
		return err
	}
	name, err := windows.UTF16FromString(path)
	if err != nil {
		return err
	}
	hDrop, err := globalData(dropFiles(name))
	if err != nil {
		return err
	}
	effect, err := globalData([]byte{1, 0, 0, 0}) // DROPEFFECT_COPY
	if err != nil {
		procGlobalFree.Call(hDrop)
		return err
	}
	pref, _ := windows.UTF16PtrFromString("Preferred DropEffect")
	prefFmt, _, _ := procRegisterClipboardFmt.Call(uintptr(unsafe.Pointer(pref)))

	// The clipboard is opened per thread, and SetClipboardData fails after EmptyClipboard
	// unless the clipboard was opened with a window (documented). So: stay on one OS
	// thread, and own the clipboard through a small invisible message-only window.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	static, _ := windows.UTF16PtrFromString("STATIC")
	const hwndMessage = ^uintptr(2) // HWND_MESSAGE = (HWND)-3
	owner, _, e := procCreateWindowExW.Call(0, uintptr(unsafe.Pointer(static)), 0, 0, 0, 0, 0, 0, hwndMessage, 0, 0, 0)
	if owner == 0 {
		procGlobalFree.Call(hDrop)
		procGlobalFree.Call(effect)
		return fmt.Errorf("opener: clipboard window: %w", e)
	}
	defer procDestroyWindow.Call(owner)
	if r, _, e := procOpenClipboard.Call(owner); r == 0 {
		procGlobalFree.Call(hDrop)
		procGlobalFree.Call(effect)
		return fmt.Errorf("opener: clipboard busy: %w", e)
	}
	defer procCloseClipboard.Call() // runs before DestroyWindow (defers run in reverse)
	procEmptyClipboard.Call()
	const cfHDrop = 15
	if r, _, e := procSetClipboardData.Call(cfHDrop, hDrop); r == 0 {
		procGlobalFree.Call(hDrop)
		procGlobalFree.Call(effect)
		return fmt.Errorf("opener: clipboard: %w", e)
	}
	// From here the clipboard owns hDrop.
	if prefFmt != 0 {
		if r, _, _ := procSetClipboardData.Call(prefFmt, effect); r == 0 {
			procGlobalFree.Call(effect)
		}
	} else {
		procGlobalFree.Call(effect)
	}
	return nil
}

// globalData copies data into newly allocated movable global memory and returns its
// handle (what the clipboard takes). The copy is done by Windows (RtlMoveMemory), so no
// Go pointer is made from the address GlobalLock returns.
func globalData(data []byte) (uintptr, error) {
	const gmemMoveable = 0x0002
	h, _, e := procGlobalAlloc.Call(gmemMoveable, uintptr(len(data)))
	if h == 0 {
		return 0, fmt.Errorf("opener: memory: %w", e)
	}
	p, _, e := procGlobalLock.Call(h)
	if p == 0 {
		procGlobalFree.Call(h)
		return 0, fmt.Errorf("opener: memory: %w", e)
	}
	procRtlMoveMemory.Call(p, uintptr(unsafe.Pointer(&data[0])), uintptr(len(data)))
	procGlobalUnlock.Call(h)
	return h, nil
}

// dropFiles builds CF_HDROP data for one file: a DROPFILES header followed by the path
// list (UTF-16, each path ending in NUL, the list ending in an extra NUL). name is the
// path as UTF-16 with its NUL.
func dropFiles(name []uint16) []byte {
	const dropFilesSize = 20 // DWORD pFiles; POINT pt; BOOL fNC; BOOL fWide
	list := append(name, 0)
	drop := make([]byte, dropFilesSize+2*len(list))
	binary.LittleEndian.PutUint32(drop[0:], dropFilesSize) // pFiles: offset of the list
	binary.LittleEndian.PutUint32(drop[16:], 1)            // fWide: UTF-16 paths
	for i, c := range list {
		binary.LittleEndian.PutUint16(drop[dropFilesSize+2*i:], c)
	}
	return drop
}

// fileURL is the file: URL of path at page: file:///C:/Docs/a.pdf#page=4, or for a
// network share (\\server\share\a.pdf) file://server/share/a.pdf#page=4.
func fileURL(path string, page int) string {
	u := url.URL{Scheme: "file", Fragment: fmt.Sprintf("page=%d", page)}
	if rest, ok := strings.CutPrefix(path, `\\`); ok && !strings.HasPrefix(rest, `?\`) {
		host, share, _ := strings.Cut(rest, `\`)
		u.Host, u.Path = host, "/"+filepath.ToSlash(share)
	} else {
		u.Path = "/" + filepath.ToSlash(path)
	}
	return u.String()
}
