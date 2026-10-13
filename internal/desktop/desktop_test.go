//go:build windows

package desktop

import (
	"errors"
	"runtime"
	"testing"
	"time"
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
		Tooltip:  "Findly test",
		Hotkey:   testHotkey,
		OnHotkey: func() { hot <- struct{}{} },
		OnOpen:   func() { open <- struct{}{} },
		OnQuit:   func() { quit <- struct{}{} },
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
	// Longer than the 127 characters Windows allows: must be cut, not overflow.
	d.SetTooltip(string(make([]rune, 500)))

	d.Close()
	select {
	case <-d.done:
	default:
		t.Fatal("Close returned before the desktop thread ended")
	}
	// A second Start works after Close (the window class is reused).
	d2, err := Start(Options{Hotkey: testHotkey})
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

	d, err := Start(Options{Hotkey: testHotkey})
	if !errors.Is(err, ErrHotkeyInUse) {
		t.Fatalf("got %v, want ErrHotkeyInUse", err)
	}
	if d == nil {
		t.Fatal("the tray must keep running when only the hotkey is taken")
	}
	d.Close()
}

func TestOnlyOneDesktop(t *testing.T) {
	d, err := Start(Options{Hotkey: testHotkey})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := Start(Options{Hotkey: testHotkey}); err == nil {
		t.Fatal("a second Desktop was started")
	}
}
