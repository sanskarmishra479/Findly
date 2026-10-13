//go:build windows

// Package desktop provides the parts of the Windows shell that Wails v2 lacks: the tray
// icon with its menu (plan 10.4) and the global search hotkey (plan 10.2).
//
// Both live on one hidden window, owned by one OS thread that runs a Windows message
// loop. Every Win32 call happens on that thread; other goroutines talk to it by posting
// messages.
package desktop

import (
	"errors"
	"fmt"
	"runtime"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// ErrHotkeyInUse means another program already owns the hotkey. The tray still works;
// the user can pick another hotkey in settings.
var ErrHotkeyInUse = errors.New("desktop: the hotkey is already used by another program")

// Hotkey is a key combination for RegisterHotKey.
type Hotkey struct {
	Ctrl, Shift, Alt bool
	Key              uint32 // virtual-key code, e.g. 0x20 for Space
}

// DefaultHotkey is Ctrl+Shift+Space (plan 10.2; Alt+Space is left to PowerToys Run).
var DefaultHotkey = Hotkey{Ctrl: true, Shift: true, Key: 0x20}

func (h Hotkey) mods() uintptr {
	m := uintptr(modNoRepeat) // holding the keys down fires once, not repeatedly
	if h.Ctrl {
		m |= modControl
	}
	if h.Shift {
		m |= modShift
	}
	if h.Alt {
		m |= modAlt
	}
	return m
}

// Options configures the tray icon and hotkey. The callbacks run on their own goroutine,
// never on the desktop thread, so they may block.
type Options struct {
	Tooltip  string
	Hotkey   Hotkey
	OnHotkey func() // the hotkey was pressed
	OnOpen   func() // tray icon clicked, or "Open search" chosen
	OnQuit   func() // "Quit Findly" chosen
	// OnForeground is called whenever another window comes to the front (any program),
	// with the time it happened (see Now). Used to close the panel on a click elsewhere.
	OnForeground func(w Window, at uint32)
}

// Desktop is a running tray icon and hotkey.
type Desktop struct {
	opts Options
	hwnd windows.HWND
	done chan struct{}

	mu      sync.Mutex
	tooltip string // the latest SetTooltip text, read on the desktop thread

	taskbarCreated uint32  // message sent when Explorer restarts: add the icon again
	winEventHook   uintptr // foreground-change hook, 0 if none
	icon           windows.Handle
}

const (
	hotkeyID      = 1
	trayID        = 1
	msgTray       = wmApp + 1 // tray icon mouse events
	msgSetTooltip = wmApp + 2

	cmdOpen = 1
	cmdQuit = 2
)

var (
	// The window procedure is a C callback. Windows callbacks are a limited resource
	// in Go, so it is created once and finds its Desktop through current.
	winEventOnce sync.Once
	winEventPtr  uintptr
	wndProcOnce  sync.Once
	wndProcPtr   uintptr
	classOnce    sync.Once
	classErr     error
	className    = windows.StringToUTF16Ptr("FindlyDesktopWindow")

	currentMu sync.Mutex
	current   *Desktop // the running Desktop; Findly has one
)

// Start creates the tray icon and registers the hotkey. If only the hotkey fails
// (ErrHotkeyInUse), the returned Desktop is still running and must be closed.
func Start(opts Options) (*Desktop, error) {
	currentMu.Lock()
	if current != nil {
		currentMu.Unlock()
		return nil, errors.New("desktop: already started")
	}
	d := &Desktop{opts: opts, tooltip: opts.Tooltip, done: make(chan struct{})}
	current = d
	currentMu.Unlock()

	ready := make(chan error, 1)
	go d.loop(ready)
	err := <-ready
	if err != nil && !errors.Is(err, ErrHotkeyInUse) {
		<-d.done
		return nil, err
	}
	return d, err
}

// SetTooltip changes the text shown when hovering the tray icon ("Indexing 4,210 of
// 12,000"). Windows shows at most 127 characters.
func (d *Desktop) SetTooltip(s string) {
	d.mu.Lock()
	d.tooltip = s
	d.mu.Unlock()
	procPostMessageW.Call(uintptr(d.hwnd), msgSetTooltip, 0, 0)
}

// Close removes the tray icon, releases the hotkey and stops the desktop thread.
func (d *Desktop) Close() {
	procPostMessageW.Call(uintptr(d.hwnd), wmClose, 0, 0)
	<-d.done
}

// loop runs on one locked OS thread for the Desktop's whole life: Windows delivers a
// window's messages to the thread that created it.
func (d *Desktop) loop(ready chan<- error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	defer func() {
		currentMu.Lock()
		current = nil
		currentMu.Unlock()
		close(d.done)
	}()

	if err := d.create(); err != nil {
		ready <- err
		return
	}
	if d.opts.OnForeground != nil {
		// Out-of-context hook: Windows calls winEventProc on this thread's message loop;
		// nothing is injected into other programs.
		winEventOnce.Do(func() { winEventPtr = windows.NewCallback(winEventProc) })
		d.winEventHook, _, _ = procSetWinEventHook.Call(eventSystemForeground, eventSystemForeground,
			0, winEventPtr, 0, 0, winEventOutOfContext)
	}
	hotkeyErr := d.registerHotkey()
	ready <- hotkeyErr

	var m msg
	for {
		r, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(r) <= 0 { // 0: WM_QUIT, -1: error
			return
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}
}

func (d *Desktop) create() error {
	wndProcOnce.Do(func() { wndProcPtr = windows.NewCallback(wndProc) })
	inst, err := moduleHandle()
	if err != nil {
		return err
	}
	classOnce.Do(func() {
		wc := wndClassEx{WndProc: wndProcPtr, Instance: inst, ClassName: className}
		wc.Size = uint32(unsafe.Sizeof(wc))
		if r, _, e := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); r == 0 {
			classErr = fmt.Errorf("desktop: RegisterClassEx: %w", e)
		}
	})
	if classErr != nil {
		return classErr
	}
	tc, _ := windows.UTF16PtrFromString("TaskbarCreated")
	r, _, _ := procRegisterWindowMessageW.Call(uintptr(unsafe.Pointer(tc)))
	d.taskbarCreated = uint32(r)

	// A hidden top-level window, never shown. (A message-only window would not receive
	// the TaskbarCreated broadcast after an Explorer restart.)
	title, _ := windows.UTF16PtrFromString("Findly")
	h, _, e := procCreateWindowExW.Call(0, uintptr(unsafe.Pointer(className)), uintptr(unsafe.Pointer(title)),
		0, 0, 0, 0, 0, 0, 0, uintptr(inst), 0)
	if h == 0 {
		return fmt.Errorf("desktop: CreateWindowEx: %w", e)
	}
	d.hwnd = windows.HWND(h)
	d.icon = loadIcon(inst)
	if err := d.notify(nimAdd); err != nil {
		procDestroyWindow.Call(h)
		return err
	}
	return nil
}

func (d *Desktop) registerHotkey() error {
	r, _, e := procRegisterHotKey.Call(uintptr(d.hwnd), hotkeyID, d.opts.Hotkey.mods(), uintptr(d.opts.Hotkey.Key))
	if r == 0 {
		if errors.Is(e, errorHotkeyAlreadyRegistered) {
			return ErrHotkeyInUse
		}
		return fmt.Errorf("desktop: RegisterHotKey: %w", e)
	}
	return nil
}

// notify adds, updates or removes the tray icon.
func (d *Desktop) notify(op uintptr) error {
	nid := notifyIconData{Wnd: d.hwnd, ID: trayID}
	nid.Size = uint32(unsafe.Sizeof(nid))
	if op != nimDelete {
		nid.Flags = nifMessage | nifIcon | nifTip
		nid.CallbackMessage = msgTray
		nid.Icon = d.icon
		d.mu.Lock()
		tip, _ := windows.UTF16FromString(d.tooltip)
		d.mu.Unlock()
		copy(nid.Tip[:len(nid.Tip)-1], tip) // always leaves a terminating zero
	}
	if r, _, e := procShellNotifyIconW.Call(op, uintptr(unsafe.Pointer(&nid))); r == 0 {
		return fmt.Errorf("desktop: Shell_NotifyIcon(%d): %w", op, e)
	}
	return nil
}

func wndProc(hwnd, message, wParam, lParam uintptr) uintptr {
	currentMu.Lock()
	d := current
	currentMu.Unlock()
	if d == nil || windows.HWND(hwnd) != d.hwnd {
		r, _, _ := procDefWindowProcW.Call(hwnd, message, wParam, lParam)
		return r
	}
	switch uint32(message) {
	case wmHotkey:
		if wParam == hotkeyID {
			d.call(d.opts.OnHotkey)
		}
		return 0
	case msgTray:
		switch uint32(lParam) {
		case wmLButtonUp:
			d.call(d.opts.OnOpen)
		case wmRButtonUp:
			d.showMenu()
		}
		return 0
	case msgSetTooltip:
		d.notify(nimModify)
		return 0
	case wmClose:
		if d.winEventHook != 0 {
			procUnhookWinEvent.Call(d.winEventHook)
			d.winEventHook = 0
		}
		procUnregisterHotKey.Call(hwnd, hotkeyID)
		d.notify(nimDelete)
		procDestroyWindow.Call(hwnd)
		return 0
	case wmDestroy:
		procPostQuitMessage.Call(0)
		return 0
	}
	if d.taskbarCreated != 0 && uint32(message) == d.taskbarCreated {
		d.notify(nimAdd) // Explorer restarted and lost all tray icons
		return 0
	}
	r, _, _ := procDefWindowProcW.Call(hwnd, message, wParam, lParam)
	return r
}

// winEventProc receives foreground changes (EVENT_SYSTEM_FOREGROUND) for all windows.
func winEventProc(hook, event, hwnd, idObject, idChild, thread, at uintptr) uintptr {
	currentMu.Lock()
	d := current
	currentMu.Unlock()
	if d != nil && d.opts.OnForeground != nil && uint32(event) == eventSystemForeground {
		w, t := Window(hwnd), uint32(at)
		go d.opts.OnForeground(w, t)
	}
	return 0
}

// call runs a callback off the desktop thread, so a slow callback never freezes the
// tray or the hotkey.
func (d *Desktop) call(f func()) {
	if f != nil {
		go f()
	}
}

func (d *Desktop) showMenu() {
	menu, _, _ := procCreatePopupMenu.Call()
	if menu == 0 {
		return
	}
	defer procDestroyMenu.Call(menu)
	appendItem(menu, cmdOpen, "Open search")
	procAppendMenuW.Call(menu, mfSeparator, 0, 0)
	appendItem(menu, cmdQuit, "Quit Findly")

	var pt point
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	// Without this the menu stays open when the user clicks elsewhere (documented
	// TrackPopupMenu behaviour).
	procSetForegroundWindow.Call(uintptr(d.hwnd))
	cmd, _, _ := procTrackPopupMenu.Call(menu, tpmRightButton|tpmReturnCmd|tpmBottomAlign,
		uintptr(pt.X), uintptr(pt.Y), 0, uintptr(d.hwnd), 0)
	procPostMessageW.Call(uintptr(d.hwnd), wmNull, 0, 0)
	switch cmd {
	case cmdOpen:
		d.call(d.opts.OnOpen)
	case cmdQuit:
		d.call(d.opts.OnQuit)
	}
}

func appendItem(menu uintptr, id int, text string) {
	t, _ := windows.UTF16PtrFromString(text)
	procAppendMenuW.Call(menu, mfString, uintptr(id), uintptr(unsafe.Pointer(t)))
}

// loadIcon returns the app icon at tray size, or the generic Windows icon when the exe
// has none (e.g. a plain `go build` or a test binary).
func loadIcon(inst windows.Handle) windows.Handle {
	cx, _, _ := procGetSystemMetrics.Call(smCxSmIcon)
	cy, _, _ := procGetSystemMetrics.Call(smCySmIcon)
	h, _, _ := procLoadImageW.Call(uintptr(inst), appIconResource, imageIcon, cx, cy, lrDefaultColor)
	if h == 0 {
		h, _, _ = procLoadIconW.Call(0, idiApplication)
	}
	return windows.Handle(h)
}

func moduleHandle() (windows.Handle, error) {
	var h windows.Handle
	if err := windows.GetModuleHandleEx(0, nil, &h); err != nil {
		return 0, fmt.Errorf("desktop: GetModuleHandle: %w", err)
	}
	return h, nil
}
