package appfs

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func newFS(t *testing.T) *FS {
	t.Helper()
	f, err := Open(filepath.Join(t.TempDir(), "Findly"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func TestOpenCreatesSubfolders(t *testing.T) {
	f := newFS(t)
	for _, sub := range []string{DirModels, DirIndex, DirThumbs, DirLogs, DirBin, DirLicense} {
		info, err := os.Stat(filepath.Join(f.Dir(), sub))
		if err != nil || !info.IsDir() {
			t.Fatalf("subfolder %s missing: %v", sub, err)
		}
	}
}

func TestWriteAndReadFile(t *testing.T) {
	f := newFS(t)
	if err := f.WriteFile("thumbs/ab/12.jpg", []byte("v1")); err != nil {
		t.Fatal(err)
	}
	if err := f.WriteFile("thumbs/ab/12.jpg", []byte("v2")); err != nil {
		t.Fatal(err)
	}
	b, err := f.ReadFile("thumbs/ab/12.jpg")
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "v2" {
		t.Fatalf("got %q, want v2", b)
	}
	// No temp files may be left behind.
	entries, _ := os.ReadDir(filepath.Join(f.Dir(), "thumbs", "ab"))
	if len(entries) != 1 {
		t.Fatalf("leftover files: %v", entries)
	}
}

// I-3: nothing may be written outside the app directory.
func TestRejectsPathsOutside(t *testing.T) {
	f := newFS(t)
	outside := filepath.Join(filepath.Dir(f.Dir()), "victim.txt")
	bad := []string{
		"",
		"..",
		"../victim.txt",
		`..\victim.txt`,
		"models/../../victim.txt",
		outside, // absolute
		`C:\Windows\evil.txt`,
		`\\server\share\x`,
		"NUL",
		"models/COM1",
	}
	for _, p := range bad {
		if err := f.WriteFile(p, []byte("x")); err == nil {
			t.Errorf("WriteFile(%q) succeeded, want error", p)
		}
		if _, err := f.Path(p); err == nil {
			t.Errorf("Path(%q) succeeded, want error", p)
		}
		if err := f.RemoveAll(p); err == nil {
			t.Errorf("RemoveAll(%q) succeeded, want error", p)
		}
	}
	if _, err := os.Stat(outside); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a file was written outside the app directory")
	}
}

// I-3: a junction inside the app directory that points outside must not be usable.
func TestRejectsJunctionEscape(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("junctions are Windows-only")
	}
	f := newFS(t)
	outside := t.TempDir()
	userFile := filepath.Join(outside, "user-document.txt")
	if err := os.WriteFile(userFile, []byte("precious"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(f.Dir(), "escape")
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, outside).CombinedOutput(); err != nil {
		t.Fatalf("mklink /J: %v: %s", err, out)
	}

	if err := f.WriteFile("escape/pwned.txt", []byte("x")); err == nil {
		t.Error("write through junction succeeded")
	}
	if err := f.Remove("escape/user-document.txt"); err == nil {
		t.Error("remove through junction succeeded")
	}
	if err := f.RemoveAll("escape/user-document.txt"); err == nil {
		t.Error("RemoveAll through junction succeeded")
	}
	// Removing the junction itself must delete only the link, not the folder it points to.
	if err := f.RemoveAll("escape"); err != nil {
		t.Errorf("removing the junction itself should work: %v", err)
	}

	// The user's file behind the junction must be untouched, and nothing new may appear.
	b, err := os.ReadFile(userFile)
	if err != nil || string(b) != "precious" {
		t.Fatalf("user file outside app dir was changed or deleted: %v", err)
	}
	entries, _ := os.ReadDir(outside)
	if len(entries) != 1 {
		t.Fatalf("files outside the app directory changed: %v", entries)
	}
}

// GO-2026-6604: Root.MkdirAll could follow junctions out of the root on Windows
// before Go 1.27.2. Creating folders through a junction must not create anything outside.
func TestMkdirAllJunctionEscape(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("junctions are Windows-only")
	}
	f := newFS(t)
	outside := t.TempDir()
	link := filepath.Join(f.Dir(), "escape")
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, outside).CombinedOutput(); err != nil {
		t.Fatalf("mklink /J: %v: %s", err, out)
	}

	f.MkdirAll("escape/made-by-findly")
	f.WriteFile("escape/sub/made-by-findly.txt", []byte("x"))

	entries, _ := os.ReadDir(outside)
	if len(entries) != 0 {
		t.Fatalf("folders/files were created outside the app directory: %v", entries)
	}
}

func TestRemoveAllRefusesRoot(t *testing.T) {
	f := newFS(t)
	for _, p := range []string{".", "models/.."} {
		if err := f.RemoveAll(p); err == nil {
			t.Errorf("RemoveAll(%q) succeeded, want error", p)
		}
	}
	if _, err := os.Stat(f.Dir()); err != nil {
		t.Fatal("app directory was removed")
	}
}

func TestRemoveMissingIsNotError(t *testing.T) {
	f := newFS(t)
	if err := f.Remove("thumbs/none.jpg"); err != nil {
		t.Fatal(err)
	}
}

func TestOpenRequiresAbsolute(t *testing.T) {
	if _, err := Open("relative"); err == nil {
		t.Fatal("expected error for relative root")
	}
}
