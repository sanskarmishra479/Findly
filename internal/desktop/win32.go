//go:build windows

package desktop

import (
	"golang.org/x/sys/windows"
)

// Win32 functions not in x/sys/windows. Window, menu, tray, hotkey and hook calls run on
// the desktop thread; PostMessage and the foreground helpers (foreground.go) are safe
// from any goroutine.
var (
	user32  = windows.NewLazySystemDLL("user32.dll")
	shell32 = windows.NewLazySystemDLL("shell32.dll")

	procRegisterClassExW       = user32.NewProc("RegisterClassExW")
	procCreateWindowExW        = user32.NewProc("CreateWindowExW")
	procDefWindowProcW         = user32.NewProc("DefWindowProcW")
	procDestroyWindow          = user32.NewProc("DestroyWindow")
	procGetMessageW            = user32.NewProc("GetMessageW")
	procTranslateMessage       = user32.NewProc("TranslateMessage")
	procDispatchMessageW       = user32.NewProc("DispatchMessageW")
	procPostMessageW           = user32.NewProc("PostMessageW")
	procPostQuitMessage        = user32.NewProc("PostQuitMessage")
	procRegisterHotKey         = user32.NewProc("RegisterHotKey")
	procUnregisterHotKey       = user32.NewProc("UnregisterHotKey")
	procRegisterWindowMessageW = user32.NewProc("RegisterWindowMessageW")
	procLoadImageW             = user32.NewProc("LoadImageW")
	procLoadIconW              = user32.NewProc("LoadIconW")
	procGetSystemMetrics       = user32.NewProc("GetSystemMetrics")
	procCreatePopupMenu        = user32.NewProc("CreatePopupMenu")
	procAppendMenuW            = user32.NewProc("AppendMenuW")
	procTrackPopupMenu         = user32.NewProc("TrackPopupMenu")
	procDestroyMenu            = user32.NewProc("DestroyMenu")
	procGetCursorPos           = user32.NewProc("GetCursorPos")
	procSetForegroundWindow    = user32.NewProc("SetForegroundWindow")
	procShellNotifyIconW       = shell32.NewProc("Shell_NotifyIconW")
)

const (
	wmNull      = 0x0000
	wmDestroy   = 0x0002
	wmClose     = 0x0010
	wmCommand   = 0x0111
	wmHotkey    = 0x0312
	wmTimer     = 0x0113
	wmLButtonUp = 0x0202
	wmRButtonUp = 0x0205
	wmApp       = 0x8000

	modControl  = 0x0002
	modShift    = 0x0004
	modAlt      = 0x0001
	modNoRepeat = 0x4000

	nimAdd    = 0
	nimModify = 1
	nimDelete = 2

	nifMessage = 0x01
	nifIcon    = 0x02
	nifTip     = 0x04

	mfString    = 0x0000
	mfSeparator = 0x0800

	tpmRightButton = 0x0002
	tpmReturnCmd   = 0x0100
	tpmBottomAlign = 0x0020

	imageIcon       = 1
	lrShared        = 0x8000 // Windows owns the handle: no DestroyIcon needed
	smCxSmIcon      = 49
	smCySmIcon      = 50
	idiApplication  = 32512
	appIconResource = 3 // Wails stores the app icon (build/windows/icon.ico) as resource 3

	errorHotkeyAlreadyRegistered = windows.Errno(1409)
)

type point struct{ X, Y int32 }

type msg struct {
	Hwnd    windows.HWND
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      point
	Private uint32
}

type wndClassEx struct {
	Size       uint32
	Style      uint32
	WndProc    uintptr
	ClsExtra   int32
	WndExtra   int32
	Instance   windows.Handle
	Icon       windows.Handle
	Cursor     windows.Handle
	Background windows.Handle
	MenuName   *uint16
	ClassName  *uint16
	IconSm     windows.Handle
}

// notifyIconData is NOTIFYICONDATAW (Vista and later layout).
type notifyIconData struct {
	Size            uint32
	Wnd             windows.HWND
	ID              uint32
	Flags           uint32
	CallbackMessage uint32
	Icon            windows.Handle
	Tip             [128]uint16
	State           uint32
	StateMask       uint32
	Info            [256]uint16
	Version         uint32
	InfoTitle       [64]uint16
	InfoFlags       uint32
	GUIDItem        windows.GUID
	BalloonIcon     windows.Handle
}

var (
	procSetWinEventHook     = user32.NewProc("SetWinEventHook")
	procUnhookWinEvent      = user32.NewProc("UnhookWinEvent")
	procGetForegroundWindow = user32.NewProc("GetForegroundWindow")
	procGetWindowThreadPID  = user32.NewProc("GetWindowThreadProcessId")
	procIsWindow            = user32.NewProc("IsWindow")
	procAllowSetForeground  = user32.NewProc("AllowSetForegroundWindow")
	procFindWindowW         = user32.NewProc("FindWindowW")
	procGetClassNameW       = user32.NewProc("GetClassNameW")
	procSetTimer            = user32.NewProc("SetTimer")
	procKillTimer           = user32.NewProc("KillTimer")
	procGetTickCount        = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetTickCount")
)

const (
	eventSystemForeground = 0x0003
	winEventOutOfContext  = 0x0000
)
