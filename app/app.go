package main

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"

	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/runtime"

	"github.com/sanskarmishra479/findly/internal/appfs"
	"github.com/sanskarmishra479/findly/internal/desktop"
)

// Version is the app version shown in About.
const Version = "0.1.0-dev"

// Search panel size in pixels (plan 10.2: about 680 px wide).
const (
	panelWidth  = 680
	panelHeight = 460
)

// App is the API the UI can call. Every exported method becomes a JavaScript function
// in frontend/wailsjs/go/main/App. Keep it to what the UI needs: no method may write,
// move or delete user files (plan I-6).
type App struct {
	// ctx is set in startup. Wails may deliver a second start (secondInstance) before
	// startup has run, on another goroutine, so it is read and written atomically.
	ctx   atomic.Pointer[context.Context]
	fs    *appfs.FS
	panel panelState

	onDomReady func() // set before wails.Run; runs once the page has loaded

	// mu guards desktop and hotkeyWarning: startup runs on its own goroutine while the
	// page may already call Info.
	mu            sync.Mutex
	desktop       *desktop.Desktop // tray icon and hotkey
	hotkeyWarning string           // set when another program owns the hotkey
}

func newApp(fs *appfs.FS) *App { return &App{fs: fs} }

// context returns the Wails context, or nil before startup.
func (a *App) context() context.Context {
	if c := a.ctx.Load(); c != nil {
		return *c
	}
	return nil
}

func (a *App) startup(ctx context.Context) {
	a.ctx.Store(&ctx)
	d, err := desktop.Start(desktop.Options{
		Tooltip:      "Findly: Ctrl+Shift+Space to search",
		Hotkey:       desktop.DefaultHotkey,
		OnHotkey:     a.ShowPanel,
		OnOpen:       a.ShowPanel,
		OnQuit:       func() { runtime.Quit(ctx) },
		OnForeground: a.foreground,
	})
	switch {
	case errors.Is(err, desktop.ErrHotkeyInUse):
		// The tray still works; say why the hotkey does nothing (settings will offer
		// another combination in T7).
		const warning = "Ctrl+Shift+Space is used by another program. Open Findly from the tray icon."
		a.mu.Lock()
		a.hotkeyWarning = warning
		a.mu.Unlock()
		runtime.EventsEmit(ctx, "hotkey:warning", warning) // the page may have asked Info already
		d.SetTooltip("Findly: hotkey in use, click to search")
	case err != nil:
		// No tray and no hotkey: the app could not be reached at all, so say so and stop.
		desktop.MessageBox("Findly could not start", err.Error())
		runtime.Quit(ctx)
		return
	}
	a.mu.Lock()
	a.desktop = d
	a.mu.Unlock()
}

func (a *App) shutdown(ctx context.Context) {
	a.mu.Lock()
	d := a.desktop
	a.mu.Unlock()
	if d != nil {
		d.Close()
	}
}

// secondInstance runs when the exe is started again while Findly is running: show the
// panel instead of starting a second copy.
func (a *App) secondInstance(options.SecondInstanceData) { a.ShowPanel() }

// ShowPanel centers the search panel on the current screen and brings it to the front.
func (a *App) ShowPanel() {
	ctx := a.context()
	if ctx == nil {
		return
	}
	prev := desktop.Foreground()
	if prev.Ours() || prev.IsShell() {
		prev = 0 // the panel is already in front, or opened from the taskbar tray
	}
	a.panel.show(prev, desktop.Now())
	runtime.WindowCenter(ctx)
	runtime.WindowShow(ctx)
	runtime.EventsEmit(ctx, "panel:shown")
}

// HidePanel closes the panel (Esc) and returns to the window the user was in, so they
// can keep typing there.
func (a *App) HidePanel() {
	ctx := a.context()
	if ctx == nil {
		return
	}
	prev := a.panel.hide()
	runtime.WindowHide(ctx)
	prev.Activate()
}

// foreground runs when any window comes to the front: a click or switch to another
// program closes the panel, like other launchers.
func (a *App) foreground(w desktop.Window, at uint32) {
	if a.panel.foreground(w.Ours(), at) {
		if ctx := a.context(); ctx != nil {
			runtime.WindowHide(ctx)
		}
	}
}

// Info is shown in the UI's About section.
type Info struct {
	Version string `json:"version"`
	// HotkeyWarning is non-empty when the search hotkey could not be registered.
	HotkeyWarning string `json:"hotkeyWarning"`
	DataDir       string `json:"dataDir"`
}

// Info returns the version and the app data folder.
func (a *App) Info() Info {
	a.mu.Lock()
	defer a.mu.Unlock()
	return Info{Version: Version, DataDir: a.fs.Dir(), HotkeyWarning: a.hotkeyWarning}
}

// domReady runs when the page has loaded, i.e. the WebView2 browser process is running.
func (a *App) domReady(ctx context.Context) {
	if a.onDomReady != nil {
		a.onDomReady()
	}
}
