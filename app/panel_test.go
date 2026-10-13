package main

import "testing"

func TestPanelClosesWhenAnotherWindowIsActivated(t *testing.T) {
	var p panelState
	p.show(42, 1000)
	if p.foreground(true, 1010) {
		t.Fatal("the panel itself coming to the front closed it")
	}
	if p.foreground(false, 990) {
		t.Fatal("a stale event from before showing closed the panel")
	}
	if !p.foreground(false, 1500) {
		t.Fatal("another window came to the front, but the panel stayed open")
	}
	if p.foreground(false, 1600) {
		t.Fatal("an already closed panel reported closing again")
	}
}

func TestPanelEscReturnsToPreviousWindow(t *testing.T) {
	var p panelState
	p.show(42, 1000)
	p.show(0, 2000) // hotkey pressed again while open: keep the original window
	if got := p.hide(); got != 42 {
		t.Fatalf("hide returned window %d, want 42", got)
	}
	if got := p.hide(); got != 0 {
		t.Fatalf("second hide returned %d, want 0", got)
	}
	// After a click elsewhere there is nothing to go back to.
	p.show(7, 3000)
	p.foreground(false, 3100)
	if got := p.hide(); got != 0 {
		t.Fatalf("hide after a click elsewhere returned %d, want 0", got)
	}
}

func TestPanelTickWrap(t *testing.T) {
	// GetTickCount wraps every 49.7 days.
	var p panelState
	p.show(1, 0xFFFFFF00)
	if p.foreground(false, 0xFFFFFE00) {
		t.Fatal("stale event before the wrap closed the panel")
	}
	if !p.foreground(false, 0x00000100) {
		t.Fatal("event after the wrap did not close the panel")
	}
}
