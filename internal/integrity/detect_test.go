//go:build windows

package integrity

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// The integrity check itself must notice every kind of change; otherwise a passing
// TestFileIntegrity would prove nothing.
func TestSnapshotDetectsChanges(t *testing.T) {
	cases := map[string]func(dir string){
		"content": func(d string) { os.WriteFile(filepath.Join(d, "a.txt"), []byte("hellO"), 0o644) },
		"mtime": func(d string) {
			later := time.Now().Add(time.Hour)
			os.Chtimes(filepath.Join(d, "a.txt"), later, later)
		},
		"attribute": func(d string) { setAttrs(t, filepath.Join(d, "a.txt"), windows.FILE_ATTRIBUTE_HIDDEN) },
		"deleted":   func(d string) { os.Remove(filepath.Join(d, "a.txt")) },
		"created":   func(d string) { os.WriteFile(filepath.Join(d, "new.txt"), nil, 0o644) },
	}
	for name, change := range cases {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello"), 0o644)
		before := snapshot(t, dir)
		change(dir)
		if problems := diff(before, snapshot(t, dir)); len(problems) == 0 {
			t.Errorf("%s: change not detected", name)
		}
	}
}
