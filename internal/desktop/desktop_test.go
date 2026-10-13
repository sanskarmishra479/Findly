//go:build windows

package desktop

import (
	"errors"
	"runtime"
	"strings"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// A combination nobody uses, so the tests never collide with the user's hotkeys.
var testHotkey = Hotkey{Ctrl: true, Shift: true, Alt: true, Key: 0x87} // F24

func waitFor(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatalf("%s: callback not called", what)
	}
}

func TestStartCallbacksClose(t *testing.T) {
	hot, open, quit := make(chan struct{}, 1), make(chan struct{}, 1), make(chan struct{}, 1)
	d, err := Start(Options{
		Tooltip:    "Findly test",
		NoTrayIcon: true,
		Hotkey:     testHotkey,
		OnHotkey:   func() { hot <- struct{}{} },
		OnOpen:     func() { open <- struct{}{} },
		OnQuit:     func() { quit <- struct{}{} },
	})
	if err != nil {
		t.Fatal(err)
	}
	// The same messages Windows sends for a key press and a tray click.
	procPostMessageW.Call(uintptr(d.hwnd), wmHotkey, hotkeyID, 0)
	waitFor(t, hot, "hotkey")
	procPostMessageW.Call(uintptr(d.hwnd), msgTray, 0, wmLButtonUp)
	waitFor(t, open, "tray click")

	d.SetTooltip("Indexing 4,210 of 12,000")
	// Longer than the 127 characters Windows allows (cut in tooltipUTF16, tested below).
	d.SetTooltip(strings.Repeat("a", 500))

	// Messages from another program must not end the tray and hotkey: WM_CLOSE, our private
	// close message, WM_DESTROY and WM_QUIT. Only Close() may.
	for _, m := range []uintptr{wmClose, msgClose, wmDestroy, 0x0012 /* WM_QUIT */} {
		procPostMessageW.Call(uintptr(d.hwnd), m, 0, 0)
	}
	procPostMessageW.Call(uintptr(d.hwnd), wmHotkey, hotkeyID, 0)
	waitFor(t, hot, "hotkey after outside close messages")

	d.Close()
	select {
	case <-d.done:
	default:
		t.Fatal("Close returned before the desktop thread ended")
	}
	// A second Start works after Close (the window class is reused).
	d2, err := Start(Options{Hotkey: testHotkey, NoTrayIcon: true})
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	d2.Close()
}

func TestHotkeyInUse(t *testing.T) {
	// Another "program" holds the hotkey: register it on a separate thread first.
	held := make(chan struct{})
	release := make(chan struct{})
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		r, _, e := procRegisterHotKey.Call(0, 7, testHotkey.mods(), uintptr(testHotkey.Key))
		if r == 0 {
			t.Errorf("setup RegisterHotKey: %v", e)
		}
		close(held)
		<-release
		procUnregisterHotKey.Call(0, 7)
	}()
	<-held
	defer close(release)

	d, err := Start(Options{Hotkey: testHotkey, NoTrayIcon: true})
	if !errors.Is(err, ErrHotkeyInUse) {
		t.Fatalf("got %v, want ErrHotkeyInUse", err)
	}
	if d == nil {
		t.Fatal("the tray must keep running when only the hotkey is taken")
	}
	d.Close()
}

func TestOnlyOneDesktop(t *testing.T) {
	d, err := Start(Options{Hotkey: testHotkey, NoTrayIcon: true})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := Start(Options{Hotkey: testHotkey, NoTrayIcon: true}); err == nil {
		t.Fatal("a second Desktop was started")
	}
}

func TestTooltipUTF16(t *testing.T) {
	if got := tooltipUTF16(strings.Repeat("a", 500), 127); len(got) != 127 {
		t.Errorf("long text: %d units, want 127", len(got))
	}
	// An emoji takes two UTF-16 units; cutting between them would leave half of it.
	got := tooltipUTF16(strings.Repeat("a", 126)+"\U0001F600", 127)
	if len(got) != 126 {
		t.Errorf("emoji at the cut: %d units, want 126 (emoji dropped whole)", len(got))
	}
	if got := tooltipUTF16("a\x00b", 127); string(rune(got[0]))+string(rune(got[1])) != "ab" || len(got) != 2 {
		t.Errorf("NUL not dropped: %v", got)
	}
}

func TestCloseTwiceAndAfterClose(t *testing.T) {
	d, err := Start(Options{Hotkey: testHotkey, NoTrayIcon: true})
	if err != nil {
		t.Fatal(err)
	}
	d.Close()
	d.Close()                // must not block or post to a destroyed window
	d.SetTooltip("too late") // likewise
}

func TestIsShell(t *testing.T) {
	c, _ := windowsUTF16Ptr("Shell_TrayWnd")
	h, _, _ := procFindWindowW.Call(uintptr(c), 0)
	if h == 0 {
		t.Skip("no taskbar (Explorer not running)")
	}
	if !Window(h).IsShell() {
		t.Error("the taskbar is not recognized")
	}
	if Window(0).IsShell() {
		t.Error("no window counted as the taskbar")
	}
}

func windowsUTF16Ptr(s string) (unsafe.Pointer, error) {
	p, err := windows.UTF16PtrFromString(s)
	return unsafe.Pointer(p), err
}
