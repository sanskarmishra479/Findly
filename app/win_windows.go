package main

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	user32                       = windows.NewLazySystemDLL("user32.dll")
	procAllowSetForegroundWindow = user32.NewProc("AllowSetForegroundWindow")
	procGetForegroundWindow      = user32.NewProc("GetForegroundWindow")
	procGetWindowThreadProcessID = user32.NewProc("GetWindowThreadProcessId")
)

// asfwAny is ASFW_ANY: any process may take the foreground.
const asfwAny = ^uintptr(0)

// allowForeground lets the already-running Findly bring its panel to the front when the
// exe is started a second time. Windows only lets the program the user just started
// take focus; without this the running copy's window opens behind others and loses
// focus at once. The permission ends with the user's next input.
func allowForeground() {
	procAllowSetForegroundWindow.Call(asfwAny)
}

// isForeground reports whether this process's Findly window is the active window.
func isForeground() bool {
	fg, _, _ := procGetForegroundWindow.Call()
	if fg == 0 {
		return false
	}
	var pid uint32
	procGetWindowThreadProcessID.Call(fg, uintptr(unsafe.Pointer(&pid)))
	return pid == windows.GetCurrentProcessId()
}

// fatalBox shows a startup error. A GUI build has no console, so a returned error would
// otherwise vanish.
func fatalBox(title, msg string) {
	t, _ := windows.UTF16PtrFromString(title)
	m, _ := windows.UTF16PtrFromString(msg)
	windows.MessageBox(0, m, t, windows.MB_OK|windows.MB_ICONERROR)
}
