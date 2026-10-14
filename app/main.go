// Command Findly is the desktop app: a tray program with a hotkey search panel (plan
// sections 5 and 10). It is built with Wails v2: Go here, the UI in app/frontend.
package main

import (
	"embed"
	"fmt"
	"os"
	"sync"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/logger"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/windows"

	"github.com/sanskarmishra479/findly/internal/appfs"
	"github.com/sanskarmishra479/findly/internal/desktop"
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
	// A second start hands over to the running copy inside wails.Run and exits. Before
	// that, let the running copy (only that process) bring its panel to the front.
	desktop.AllowForeground("wails-app-"+singleInstanceID+"-sic", "wails-app-"+singleInstanceID+"-siw")
	if err := run(); err != nil {
		// No console in a GUI build: show the error in a message box instead.
		desktop.MessageBox("Findly could not start", err.Error())
		os.Exit(1)
	}
}

// run keeps the work before wails.Run small: it also runs for every second start (which
// exits inside wails.Run) and when `wails build` runs the exe to generate bindings.
// Opening the index and other heavy setup belong in App.startup.
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
	// WebView2's browser process creates temporary files in %TEMP%. It copies the
	// environment when it starts, so TEMP points into the app folder (plan I-3) only until
	// the page is up. Restoring it then matters: programs Findly opens later (Word, Edge,
	// Explorer via ShellExecute) inherit the environment and must use the user's TEMP.
	tmp, err := afs.Path(appfs.DirTmp)
	if err != nil {
		return err
	}
	restoreTemp := setTemp(tmp)

	app := newApp(afs)
	app.onDomReady = restoreTemp
	err = wails.Run(&options.App{
		Title:  "Findly",
		Width:  panelWidth,
		Height: barHeight,
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
		OnDomReady:        app.domReady,
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
			Theme:               windows.SystemDefault, // light or dark, like Windows
			// Glass look (docs/design/search-panel.md): a see-through page over Windows 11's
			// acrylic blur. Frameless windows keep the rounded corners and shadow.
			WebviewIsTransparent: true,
			WindowIsTranslucent:  true,
			BackdropType:         windows.Acrylic,
			DisablePinchZoom:     true,
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

// setTemp points TEMP and TMP at dir and returns a function that restores the previous
// values (safe to call more than once).
func setTemp(dir string) (restore func()) {
	oldTemp, hadTemp := os.LookupEnv("TEMP")
	oldTmp, hadTmp := os.LookupEnv("TMP")
	os.Setenv("TEMP", dir)
	os.Setenv("TMP", dir)
	var once sync.Once
	return func() {
		once.Do(func() {
			reset("TEMP", oldTemp, hadTemp)
			reset("TMP", oldTmp, hadTmp)
		})
	}
}

func reset(key, val string, had bool) {
	if had {
		os.Setenv(key, val)
	} else {
		os.Unsetenv(key)
	}
}
