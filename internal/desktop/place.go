//go:build windows

package desktop

import (
	"errors"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	shcore                = windows.NewLazySystemDLL("shcore.dll")
	procMonitorFromPoint  = user32.NewProc("MonitorFromPoint")
	procMonitorFromWindow = user32.NewProc("MonitorFromWindow")
	procGetMonitorInfoW   = user32.NewProc("GetMonitorInfoW")
	procSetWindowPos      = user32.NewProc("SetWindowPos")
	procFindWindowExW     = user32.NewProc("FindWindowExW")
	procGetDpiForMonitor  = shcore.NewProc("GetDpiForMonitor")
	procGetWindowRect     = user32.NewProc("GetWindowRect")
)

const (
	monitorDefaultToNearest = 2
	mdtEffectiveDPI         = 0
	swpNoZOrder             = 0x0004
	swpNoActivate           = 0x0010
)

type rect struct{ Left, Top, Right, Bottom int32 }

type monitorInfo struct {
	Size    uint32
	Monitor rect
	Work    rect
	Flags   uint32
}

// FindOwnWindow returns this process's top-level window of the given class (e.g. Wails'
// "wailsWindow"), or 0.
func FindOwnWindow(class string) Window {
	c, err := windows.UTF16PtrFromString(class)
	if err != nil {
		return 0
	}
	var h uintptr
	for {
		h, _, _ = procFindWindowExW.Call(0, h, uintptr(unsafe.Pointer(c)), 0)
		if h == 0 {
			return 0
		}
		if Window(h).Ours() {
			return Window(h)
		}
	}
}

// Place sizes w to width × height (in device-independent pixels, as at 100% scaling) and
// puts it horizontally centered on a monitor's work area (the screen minus the
// taskbar), with its top at fromTop of the work area's height. With onCursor it uses
// the monitor under the mouse (when the panel opens), else the window's current monitor
// (when it grows). Position and size are set in one step, in that monitor's own scaling,
// so the panel lands right on mixed-DPI setups.
func (w Window) Place(width, height int, fromTop float64, onCursor bool) error {
	if w == 0 {
		return errors.New("desktop: no window")
	}
	var mon uintptr
	if onCursor {
		var pt point
		procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
		// MonitorFromPoint takes the POINT by value: on amd64 its two int32 fields are
		// packed into one 64-bit argument.
		mon, _, _ = procMonitorFromPoint.Call(uintptr(uint32(pt.X))|uintptr(uint32(pt.Y))<<32, monitorDefaultToNearest)
	} else {
		mon, _, _ = procMonitorFromWindow.Call(uintptr(w), monitorDefaultToNearest)
	}
	mi := monitorInfo{}
	mi.Size = uint32(unsafe.Sizeof(mi))
	if r, _, e := procGetMonitorInfoW.Call(mon, uintptr(unsafe.Pointer(&mi))); r == 0 {
		return e
	}
	dpiX, dpiY := uint32(96), uint32(96)
	procGetDpiForMonitor.Call(mon, mdtEffectiveDPI, uintptr(unsafe.Pointer(&dpiX)), uintptr(unsafe.Pointer(&dpiY)))
	scale := float64(dpiX) / 96

	work := mi.Work
	pw := int32(float64(width) * scale)
	ph := int32(float64(height) * scale)
	ww, wh := work.Right-work.Left, work.Bottom-work.Top
	pw, ph = min(pw, ww), min(ph, wh) // never larger than the screen
	x := work.Left + (ww-pw)/2
	y := work.Top + int32(float64(wh)*fromTop)
	y = min(y, work.Bottom-ph) // grow downward, but stay on screen
	if r, _, e := procSetWindowPos.Call(uintptr(w), 0, uintptr(x), uintptr(y), uintptr(pw), uintptr(ph),
		swpNoZOrder|swpNoActivate); r == 0 {
		return e
	}
	return nil
}

// Rect returns the window's position and size in physical pixels.
func (w Window) Rect() (x, y, width, height int) {
	var r rect
	procGetWindowRect.Call(uintptr(w), uintptr(unsafe.Pointer(&r)))
	return int(r.Left), int(r.Top), int(r.Right - r.Left), int(r.Bottom - r.Top)
}
