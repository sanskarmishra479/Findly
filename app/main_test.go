package main

import (
	"os"
	"testing"
)

func TestSetTempRestores(t *testing.T) {
	t.Setenv("TEMP", `C:\user-temp`)
	t.Setenv("TMP", `C:\user-tmp`)
	restore := setTemp(`C:\app\tmp`)
	if os.Getenv("TEMP") != `C:\app\tmp` || os.Getenv("TMP") != `C:\app\tmp` {
		t.Fatal("TEMP/TMP not redirected")
	}
	restore()
	restore() // twice is fine
	if os.Getenv("TEMP") != `C:\user-temp` || os.Getenv("TMP") != `C:\user-tmp` {
		t.Fatalf("not restored: TEMP=%q TMP=%q", os.Getenv("TEMP"), os.Getenv("TMP"))
	}
}
