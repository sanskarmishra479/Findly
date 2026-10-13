package main

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/runtime"

	"github.com/sanskarmishra479/findly/internal/appfs"
	"github.com/sanskarmishra479/findly/internal/desktop"
	"github.com/sanskarmishra479/findly/internal/engine"
	"github.com/sanskarmishra479/findly/internal/store"
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

	onDomReady func() // set before wails.Run; runs once the page has loaded (or startup ends)

	// mu guards desktop and hotkeyWarning: startup runs on its own goroutine while the
	// page may already call Info.
	mu            sync.Mutex
	desktop       *desktop.Desktop // tray icon and hotkey
	hotkeyWarning string           // set when another program owns the hotkey
	engine        *engine.Engine   // nil until opened, or if it could not be opened
	engineErr     string           // why the index could not be opened
	modelNote     string           // where the search model was found, or why not
	throttle      statusThrottle   // limits tooltip updates during indexing
	quitting      bool             // shutdown has started

	searches  searches     // cancels outdated searches while the user types
	previews  previewCalls // cancels an outdated preview
	pageCache pageCache    // recently rendered PDF pages
}

func newApp(fs *appfs.FS) *App {
	return &App{fs: fs, throttle: statusThrottle{interval: 250 * time.Millisecond}}
}

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
		Tooltip:       "Findly: Ctrl+Shift+Space to search",
		Hotkey:        desktop.DefaultHotkey,
		OnHotkey:      a.ShowPanel,
		OnOpen:        a.ShowPanel,
		OnQuit:        func() { runtime.Quit(ctx) },
		OnForeground:  a.foreground,
		OnTogglePause: a.togglePause,
		Paused:        a.paused,
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
		// The desktop window could not be created: no tray and no hotkey, so the app could
		// not be reached at all. Say so and stop.
		desktop.MessageBox("Findly could not start", err.Error())
		runtime.Quit(ctx)
		return
	}
	a.mu.Lock()
	a.desktop = d
	a.mu.Unlock()
	// The WebView2 browser process exists by now (it starts before startup runs), so TEMP
	// can go back to the user's value even if the page never finishes loading.
	if a.onDomReady != nil {
		a.onDomReady()
	}
	// In the background: loading a large index takes seconds, and the tray, hotkey and
	// panel must not wait for it.
	go a.openEngine(ctx)
}

// openEngine opens the index and starts keeping the chosen folders up to date. Indexing
// runs in the background; the app never waits for it.
func (a *App) openEngine(ctx context.Context) {
	start, note := modelStarter(a.fs)
	eng, err := engine.Open(engine.Config{FS: a.fs, StartModel: start, OnStatus: a.engineStatus})
	a.mu.Lock()
	a.modelNote = note
	if err != nil {
		a.engineErr = engineMessage(err)
		a.mu.Unlock()
		a.setTooltip("Findly: the index needs attention. Open search for details")
		runtime.EventsEmit(ctx, "engine:error", a.engineErr)
		return
	}
	if a.quitting {
		// The app quit while the index was opening: nobody else will close it.
		a.mu.Unlock()
		eng.Close()
		return
	}
	a.engine = eng
	a.mu.Unlock()
	eng.IndexAll()
}

// engineMessage explains why the index could not be opened, in the user's terms. The
// user's files are never affected (plan I-7).
func engineMessage(err error) string {
	switch {
	case errors.Is(err, store.ErrModelChanged):
		return "This version of Findly uses a new search model, so the index must be rebuilt. Your files are not affected."
	case store.IsCorrupt(err):
		return "The search index is damaged and must be rebuilt. Your files are not affected."
	default:
		return "The search index could not be opened: " + err.Error()
	}
}

// engineStatus runs on the engine's goroutines for every progress step.
func (a *App) engineStatus(s engine.Status) {
	a.mu.Lock()
	pass := a.throttle.pass(s, time.Now())
	a.mu.Unlock()
	if !pass {
		return
	}
	a.setTooltip(tooltip(s))
	if ctx := a.context(); ctx != nil {
		runtime.EventsEmit(ctx, "engine:status", statusText(s))
	}
}

// statusText is the tooltip without the app name, for the panel's footer.
func statusText(s engine.Status) string { return strings.TrimPrefix(tooltip(s), "Findly: ") }

func (a *App) setTooltip(text string) {
	a.mu.Lock()
	d := a.desktop
	a.mu.Unlock()
	if d != nil {
		d.SetTooltip(text)
	}
}

func (a *App) currentEngine() *engine.Engine {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.engine
}

// paused tells the tray menu which label to show. It runs on the tray thread, so it only
// reads the engine's status (no waiting).
func (a *App) paused() bool {
	e := a.currentEngine()
	return e != nil && e.Status().State == engine.StatePaused
}

func (a *App) togglePause() {
	e := a.currentEngine()
	if e == nil {
		return
	}
	if a.paused() {
		e.Resume()
	} else {
		e.Pause()
	}
}

func (a *App) shutdown(ctx context.Context) {
	a.mu.Lock()
	d, e := a.desktop, a.engine
	a.engine = nil
	a.quitting = true // an engine still opening closes itself
	a.mu.Unlock()
	if e != nil {
		e.Close() // interrupted indexing continues on the next start
	}
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
	// IndexError is non-empty when the index could not be opened (e.g. it needs a rebuild).
	IndexError string `json:"indexError"`
	// ModelNote says where the search model was found, or why meaning search is off.
	ModelNote string `json:"modelNote"`
}

// Info returns the version, the app data folder and any problem to show the user.
func (a *App) Info() Info {
	a.mu.Lock()
	defer a.mu.Unlock()
	return Info{Version: Version, DataDir: a.fs.Dir(), HotkeyWarning: a.hotkeyWarning,
		IndexError: a.engineErr, ModelNote: a.modelNote}
}

// IndexStatus describes what indexing is doing right now, as a short sentence (updates
// arrive as the "engine:status" event). Empty when the index could not be opened.
func (a *App) IndexStatus() string {
	if e := a.currentEngine(); e != nil {
		return statusText(e.Status())
	}
	return ""
}

// domReady runs when the page has loaded, i.e. the WebView2 browser process is running.
func (a *App) domReady(ctx context.Context) {
	if a.onDomReady != nil {
		a.onDomReady()
	}
}
