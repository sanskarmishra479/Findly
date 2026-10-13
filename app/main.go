// Command Findly is the desktop app: a tray program with a hotkey search panel (plan
// sections 5 and 10). It is built with Wails v2: Go here, the UI in app/frontend.
package main

import (
	"embed"
	"fmt"
	"os"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/logger"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/windows"

	"github.com/sanskarmishra479/findly/internal/appfs"
)

// The built UI (app/frontend/dist) is compiled into the exe: the window never loads
// anything from the network (plan P-2).
//
//go:embed all:frontend/dist
var assets embed.FS

// singleInstanceID names the lock that keeps one Findly running per user. Starting the
// exe again only shows the running app's window.
const singleInstanceID = "findly-2869cc4d-271b-4b33-8ae8-0763dfc7eabc"

func main() {
	// Must run before wails.Run, which hands a second start over to the running copy.
	allowForeground()
	if err := run(); err != nil {
		// No console in a GUI build: show the error in a message box instead.
		fatalBox("Findly could not start", err.Error())
		os.Exit(1)
	}
}

func run() error {
	dir, err := appfs.DefaultDir()
	if err != nil {
		return err
	}
	afs, err := appfs.Open(dir)
	if err != nil {
		return err
	}
	defer afs.Close()
	webview, err := afs.Path(appfs.DirWebview)
	if err != nil {
		return err
	}

	app := newApp(afs)
	err = wails.Run(&options.App{
		Title:  "Findly",
		Width:  panelWidth,
		Height: panelHeight,
		// A small search panel, not a normal window: no title bar, fixed size, above other
		// windows, hidden until the hotkey or tray shows it.
		Frameless:         true,
		DisableResize:     true,
		AlwaysOnTop:       true,
		StartHidden:       true,
		HideWindowOnClose: true,
		BackgroundColour:  &options.RGBA{R: 0, G: 0, B: 0, A: 0},
		AssetServer:       &assetserver.Options{Assets: assets},
		OnStartup:         app.startup,
		OnShutdown:        app.shutdown,
		Bind:              []any{app},
		SingleInstanceLock: &options.SingleInstanceLock{
			UniqueId:               singleInstanceID,
			OnSecondInstanceLaunch: app.secondInstance,
		},
		LogLevel:           logger.INFO,
		LogLevelProduction: logger.ERROR,
		Windows: &windows.Options{
			// WebView2 writes its browser data here, inside the app folder (plan I-3).
			WebviewUserDataPath: webview,
			Theme:               windows.SystemDefault,
			DisablePinchZoom:    true,
		},
		// Privacy (plan P-1): SmartScreen URL checks would send addresses to Microsoft.
		// The window only shows bundled pages, so there is nothing to check.
		EnableFraudulentWebsiteDetection: false,
	})
	if err != nil {
		return fmt.Errorf("window: %w", err)
	}
	return nil
}
