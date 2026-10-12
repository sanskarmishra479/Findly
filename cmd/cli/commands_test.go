package main

import (
	"testing"
	"time"
)

func TestDayStart(t *testing.T) {
	// Chile moves clocks from 00:00 to 01:00 on 2024-09-08: that day starts at 01:00.
	scl, err := time.LoadLocation("America/Santiago")
	if err != nil {
		t.Skip("no time zone data:", err)
	}
	ns, err := dayStart("2024-09-08", scl)
	if err != nil {
		t.Fatal(err)
	}
	if got := time.Unix(0, ns).In(scl); got.Format("2006-01-02 15:04") != "2024-09-08 01:00" {
		t.Errorf("got %v", got)
	}
	if ns, _ := dayStart("1970-01-01", time.UTC); ns != 1 {
		t.Errorf("1970-01-01 UTC = %d, want 1 (0 means no filter)", ns)
	}
	if apia, err := time.LoadLocation("Pacific/Apia"); err == nil {
		if _, err := dayStart("2011-12-30", apia); err == nil { // Samoa skipped this day
			t.Error("a skipped day was accepted")
		}
	}
	for _, bad := range []string{"2025-02-30", "2025-3-1", "1969-12-31", "2201-01-01", "yesterday"} {
		if _, err := dayStart(bad, time.UTC); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
