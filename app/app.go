package main

import (
	"context"
	"sync/atomic"

	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/runtime"

	"github.com/sanskarmishra479/findly/internal/appfs"
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
	ctx    context.Context // set in startup; Wails runtime calls need it
	active atomic.Bool     // the panel was really the active window since it was shown
	fs     *appfs.FS
}

func newApp(fs *appfs.FS) *App { return &App{fs: fs} }

func (a *App) startup(ctx context.Context) { a.ctx = ctx }

func (a *App) shutdown(ctx context.Context) {}

// secondInstance runs when the exe is started again while Findly is running: show the
// panel instead of starting a second copy.
func (a *App) secondInstance(options.SecondInstanceData) { a.ShowPanel() }

// ShowPanel centers the search panel on the current screen and brings it to the front.
func (a *App) ShowPanel() {
	if a.ctx == nil {
		return
	}
	a.active.Store(false)
	runtime.WindowCenter(a.ctx)
	runtime.WindowShow(a.ctx)
	runtime.EventsEmit(a.ctx, "panel:shown")
}

// HidePanel hides the panel (Esc).
func (a *App) HidePanel() {
	if a.ctx != nil {
		runtime.WindowHide(a.ctx)
	}
}

// PanelFocused is called by the UI when the panel gets keyboard focus. It records
// whether Windows really made the panel the active window.
func (a *App) PanelFocused() {
	if isForeground() {
		a.active.Store(true)
	}
}

// PanelBlurred is called by the UI when the panel loses focus. Clicking elsewhere closes
// the panel, but only if it was really active: when Windows refuses to bring it to the
// front (it may for programs in the background), the panel stays open until the user
// clicks it, instead of disappearing at once.
func (a *App) PanelBlurred() {
	if a.active.Load() && !isForeground() {
		a.HidePanel()
	}
}

// Info is shown in the UI's About section.
type Info struct {
	Version string `json:"version"`
	DataDir string `json:"dataDir"`
}

// Info returns the version and the app data folder.
func (a *App) Info() Info { return Info{Version: Version, DataDir: a.fs.Dir()} }
