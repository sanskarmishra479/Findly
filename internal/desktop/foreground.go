//go:build windows

package desktop

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// Window is a window handle. 0 means none.
type Window uintptr

// Foreground returns the window the user is working in.
func Foreground() Window {
	h, _, _ := procGetForegroundWindow.Call()
	return Window(h)
}

// Ours reports whether w belongs to this process (the search panel or another Findly
// window).
func (w Window) Ours() bool {
	if w == 0 {
		return false
	}
	var pid uint32
	procGetWindowThreadPID.Call(uintptr(w), uintptr(unsafe.Pointer(&pid)))
	return pid == windows.GetCurrentProcessId()
}

// shellClasses are the taskbar and its tray overflow. When the panel was opened from the
// tray icon, one of them is "the window the user was in", but returning focus there
// would be pointless.
var shellClasses = map[string]bool{
	"Shell_TrayWnd": true, "Shell_SecondaryTrayWnd": true,
	"NotifyIconOverflowWindow": true, "TopLevelWindowForOverflowXamlIsland": true,
}

// IsShell reports whether w is the taskbar or the tray overflow.
func (w Window) IsShell() bool {
	if w == 0 {
		return false
	}
	var buf [64]uint16
	n, _, _ := procGetClassNameW.Call(uintptr(w), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	return n > 0 && shellClasses[windows.UTF16ToString(buf[:n])]
}

// Activate brings w back to the front, if it still exists. Windows allows it only while
// this process is in the foreground, which is the case right after the user pressed Esc
// in the panel.
func (w Window) Activate() {
	if w == 0 {
		return
	}
	if ok, _, _ := procIsWindow.Call(uintptr(w)); ok == 0 {
		return // closed in the meantime
	}
	procSetForegroundWindow.Call(uintptr(w))
}

// Now is the Windows tick count in milliseconds, the clock that foreground events are
// stamped with.
func Now() uint32 {
	t, _, _ := procGetTickCount.Call()
	return uint32(t)
}

// AllowForeground lets the running Findly (found by its single-instance window class and
// title) bring its panel to the front. Called by a second start, which Windows still lets
// take the foreground because the user just launched it. Only that one process gets the
// right, not every process (ASFW_ANY). Returns false if no running copy was found.
func AllowForeground(class, title string) bool {
	c, err := windows.UTF16PtrFromString(class)
	if err != nil {
		return false
	}
	t, err := windows.UTF16PtrFromString(title)
	if err != nil {
		return false
	}
	h, _, _ := procFindWindowW.Call(uintptr(unsafe.Pointer(c)), uintptr(unsafe.Pointer(t)))
	if h == 0 {
		return false
	}
	var pid uint32
	procGetWindowThreadPID.Call(h, uintptr(unsafe.Pointer(&pid)))
	if pid == 0 || pid == windows.GetCurrentProcessId() {
		return false
	}
	r, _, _ := procAllowSetForeground.Call(uintptr(pid))
	return r != 0
}

// MessageBox shows an error message; for startup failures, when there is no other UI.
func MessageBox(title, text string) {
	t, _ := windows.UTF16PtrFromString(title)
	m, _ := windows.UTF16PtrFromString(text)
	windows.MessageBox(0, m, t, windows.MB_OK|windows.MB_ICONERROR)
}
