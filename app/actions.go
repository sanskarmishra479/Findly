package main

import (
	"errors"

	"github.com/wailsapp/wails/v2/pkg/runtime"

	"github.com/sanskarmishra479/findly/internal/opener"
)

// The only actions on a user's file (plan I-6): open, open at a page, show in Explorer,
// copy. The UI passes a result's index id, never a path, so it can only act on files
// Findly indexed.

// OpenResult opens the file (a PDF at page, when its app supports that) and closes the
// panel. It returns a message for the user, or "" on success.
func (a *App) OpenResult(id int64, page int) string {
	path, msg := a.resultPath(id)
	if msg != "" {
		return msg
	}
	if err := opener.Open(path, page); err != nil {
		return actionError(err)
	}
	a.closeForAction()
	return ""
}

// ShowResultInExplorer opens the file's folder with it selected, and closes the panel.
func (a *App) ShowResultInExplorer(id int64) string {
	path, msg := a.resultPath(id)
	if msg != "" {
		return msg
	}
	if err := opener.ShowInExplorer(path); err != nil {
		return actionError(err)
	}
	a.closeForAction()
	return ""
}

// CopyResult copies the file to the clipboard (as a copy, never a cut). The panel stays.
func (a *App) CopyResult(id int64) string {
	path, msg := a.resultPath(id)
	if msg != "" {
		return msg
	}
	if err := opener.Copy(path); err != nil {
		return actionError(err)
	}
	return ""
}

func (a *App) resultPath(id int64) (string, string) {
	e := a.currentEngine()
	ctx := a.context()
	if e == nil || ctx == nil {
		return "", "The index is not open."
	}
	files, err := e.Store().FilesByID(ctx, []int64{id})
	f, ok := files[id]
	if err != nil || !ok {
		return "", "This file is no longer in the index."
	}
	return f.Path, ""
}

// closeForAction hides the panel after opening something; the opened app takes the
// focus, so there is no window to return to.
func (a *App) closeForAction() {
	a.panel.hide()
	if ctx := a.context(); ctx != nil {
		runtime.WindowHide(ctx)
	}
}

func actionError(err error) string {
	if errors.Is(err, opener.ErrNotAvailable) {
		return "This file is no longer there. It may have been moved or deleted."
	}
	return "Windows could not open this file."
}
