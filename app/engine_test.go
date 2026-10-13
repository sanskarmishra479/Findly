package main

import (
	"strings"
	"testing"
	"time"

	"github.com/sanskarmishra479/findly/internal/engine"
)

func TestTooltip(t *testing.T) {
	cases := map[string]engine.Status{
		"Findly: reading files 120 / 4,210 in Documents": {State: engine.StateIndexing, Root: `C:\Users\x\Documents`, Done: 120, Total: 4210},
		"Findly: preparing meaning search 50%":           {State: engine.StateEmbedding, Passages: 500, Waiting: 1000},
		"Findly: preparing meaning search 99%":           {State: engine.StateEmbedding, Passages: 1000, Waiting: 1000},
		"Findly: indexing paused":                        {State: engine.StatePaused},
		"Findly: ready. Ctrl+Shift+Space to search":      {State: engine.StateIdle},
	}
	for want, s := range cases {
		if got := tooltip(s); got != want {
			t.Errorf("tooltip(%+v) = %q, want %q", s, got, want)
		}
	}
	long := tooltip(engine.Status{State: engine.StateIndexing, Root: `C:\` + strings.Repeat("x", 300), Done: 1, Total: 2})
	if len(long) < 127 {
		t.Fatal("test needs a long folder name")
	} // cut safely by the desktop package (tooltipUTF16)
}

func TestThousands(t *testing.T) {
	for n, want := range map[int]string{0: "0", 999: "999", 1000: "1,000", 4210: "4,210", 1234567: "1,234,567"} {
		if got := thousands(n); got != want {
			t.Errorf("thousands(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestStatusThrottle(t *testing.T) {
	th := statusThrottle{interval: 250 * time.Millisecond}
	t0 := time.Now()
	idx := engine.Status{State: engine.StateIndexing}
	if !th.pass(idx, t0) {
		t.Fatal("first status dropped")
	}
	if th.pass(idx, t0.Add(100*time.Millisecond)) {
		t.Fatal("progress within the interval passed")
	}
	if !th.pass(engine.Status{State: engine.StateIdle}, t0.Add(110*time.Millisecond)) {
		t.Fatal("a state change was held back")
	}
	if !th.pass(idx, t0.Add(120*time.Millisecond)) {
		t.Fatal("a state change back was held back")
	}
	if !th.pass(idx, t0.Add(400*time.Millisecond)) {
		t.Fatal("progress after the interval dropped")
	}
}
