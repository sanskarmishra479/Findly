package main

import (
	"sync"

	"github.com/sanskarmishra479/findly/internal/desktop"
)

// panelState decides when the search panel closes. It holds no Windows calls, so the
// rules can be tested. Safe for concurrent use: hotkey, tray, UI and foreground events
// all arrive on different goroutines.
type panelState struct {
	mu      sync.Mutex
	visible bool
	shownAt uint32         // desktop.Now() when shown
	prev    desktop.Window // the window the user was in; Esc returns there
}

// show records that the panel was shown at now, over window prev. Showing an open panel
// again keeps the original window to return to.
func (p *panelState) show(prev desktop.Window, now uint32) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.visible || p.prev == 0 {
		p.prev = prev
	}
	p.visible = true
	p.shownAt = now
}

// hide records that the panel was closed with Esc, and returns the window to go back to
// (0 if none).
func (p *panelState) hide() desktop.Window {
	p.mu.Lock()
	defer p.mu.Unlock()
	prev := p.prev
	p.visible, p.prev = false, 0
	return prev
}

// foreground handles "a window came to the front at time at". It reports whether the
// panel must close: another program's window was activated after the panel was shown
// (the user clicked or switched elsewhere). Events stamped before the panel was shown
// are stale and ignored, so a late event cannot close a panel that just opened.
func (p *panelState) foreground(ours bool, at uint32) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.visible || ours || int32(at-p.shownAt) < 0 { // tick counts wrap; compare as a difference
		return false
	}
	p.visible, p.prev = false, 0
	return true
}
