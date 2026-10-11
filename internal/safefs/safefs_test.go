//go:build windows

package safefs

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func writeTemp(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestOpenReadsContent(t *testing.T) {
	p := writeTemp(t, t.TempDir(), "a.txt", "hello")
	f, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	b, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "hello" {
		t.Fatalf("got %q, want %q", b, "hello")
	}
}

// I-2: while we hold the file open, other programs must still be able to
// write, rename and delete it.
func TestOpenDoesNotBlockOthers(t *testing.T) {
	dir := t.TempDir()
	p := writeTemp(t, dir, "doc.txt", "original")

	f, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	if err := os.WriteFile(p, []byte("edited by another program"), 0o644); err != nil {
		t.Fatalf("write while open: %v", err)
	}
	renamed := filepath.Join(dir, "renamed.txt")
	if err := os.Rename(p, renamed); err != nil {
		t.Fatalf("rename while open: %v", err)
	}
	if err := os.Remove(renamed); err != nil {
		t.Fatalf("delete while open: %v", err)
	}
}

// I-1: reading must not change the file's content or modification time.
func TestReadDoesNotModify(t *testing.T) {
	p := writeTemp(t, t.TempDir(), "keep.txt", "same")
	before, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ReadFile(p, 1<<20); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if !before.ModTime().Equal(after.ModTime()) || before.Size() != after.Size() {
		t.Fatal("file changed after reading")
	}
}

func TestReadFileLimit(t *testing.T) {
	p := writeTemp(t, t.TempDir(), "big.txt", strings.Repeat("x", 100))
	if _, err := ReadFile(p, 50); err == nil {
		t.Fatal("expected error for file over the limit")
	}
	if b, err := ReadFile(p, 100); err != nil || len(b) != 100 {
		t.Fatalf("ReadFile at exact limit: len=%d err=%v", len(b), err)
	}
}

func TestOpenRejectsRelativePath(t *testing.T) {
	if _, err := Open("relative.txt"); !errors.Is(err, ErrNotAbsolute) {
		t.Fatalf("got %v, want ErrNotAbsolute", err)
	}
}

func TestOpenRejectsDirectory(t *testing.T) {
	if _, err := Open(t.TempDir()); !errors.Is(err, ErrIsDir) {
		t.Fatalf("got %v, want ErrIsDir", err)
	}
}

// I-4: a file marked offline (how cloud placeholders look) must never be opened.
func TestOpenRejectsOfflineFile(t *testing.T) {
	p := writeTemp(t, t.TempDir(), "cloud.txt", "x")
	setAttrs(t, p, windows.FILE_ATTRIBUTE_OFFLINE)
	t.Cleanup(func() { setAttrs(t, p, windows.FILE_ATTRIBUTE_NORMAL) })

	if _, err := Open(p); !errors.Is(err, ErrCloudOnly) {
		t.Fatalf("got %v, want ErrCloudOnly", err)
	}
	info, err := Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsCloudOnly() {
		t.Fatal("Stat did not report cloud-only")
	}
}

// I-5: symlinks are never followed. Creating one needs Developer Mode or admin, so skip otherwise.
func TestOpenRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := writeTemp(t, dir, "target.txt", "secret")
	link := filepath.Join(dir, "link.txt")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("cannot create symlinks here: %v", err)
	}
	if _, err := Open(link); !errors.Is(err, ErrLink) {
		t.Fatalf("got %v, want ErrLink", err)
	}
}

// I-5: WalkDir must not descend into a junction, and reports it as a link.
func TestWalkDirSkipsJunction(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	writeTemp(t, outside, "outside.txt", "must not be seen")
	writeTemp(t, root, "inside.txt", "ok")
	if err := os.Mkdir(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTemp(t, filepath.Join(root, "sub"), "nested.txt", "ok")
	mklinkJunction(t, filepath.Join(root, "jump"), outside)

	var seen []string
	var linkSeen bool
	err := WalkDir(root, func(info Info, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, info.Path)
		seen = append(seen, rel)
		if info.Name == "jump" {
			linkSeen = info.IsLink()
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range seen {
		if strings.Contains(s, "outside.txt") {
			t.Fatalf("walked through junction: %v", seen)
		}
	}
	if !linkSeen {
		t.Fatalf("junction not reported as link: %v", seen)
	}
	want := map[string]bool{"inside.txt": true, "sub": true, filepath.Join("sub", "nested.txt"): true, "jump": true}
	if len(seen) != len(want) {
		t.Fatalf("got %v, want %v", seen, want)
	}
	for _, s := range seen {
		if !want[s] {
			t.Fatalf("unexpected entry %q in %v", s, seen)
		}
	}
}

func TestWalkDirSkipDir(t *testing.T) {
	root := t.TempDir()
	os.Mkdir(filepath.Join(root, "skipme"), 0o755)
	writeTemp(t, filepath.Join(root, "skipme"), "hidden.txt", "x")
	writeTemp(t, root, "keep.txt", "x")

	var seen []string
	err := WalkDir(root, func(info Info, err error) error {
		if err != nil {
			return err
		}
		seen = append(seen, info.Name)
		if info.IsDir() && info.Name == "skipme" {
			return SkipDir
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range seen {
		if s == "hidden.txt" {
			t.Fatalf("SkipDir ignored: %v", seen)
		}
	}
}

func TestStatReportsSizeAndDir(t *testing.T) {
	dir := t.TempDir()
	p := writeTemp(t, dir, "five.txt", "12345")
	info, err := Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size != 5 || info.IsDir() || info.Name != "five.txt" {
		t.Fatalf("unexpected info: %+v", info)
	}
	dinfo, err := Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !dinfo.IsDir() {
		t.Fatal("directory not reported as dir")
	}
}

func TestStatDriveRoot(t *testing.T) {
	vol := filepath.VolumeName(t.TempDir()) + `\`
	info, err := Stat(vol)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() {
		t.Fatalf("%s not reported as dir", vol)
	}
}

// Paths longer than the old 260-character limit must work.
func TestLongPath(t *testing.T) {
	dir := t.TempDir()
	deep := dir
	for len(deep) < 300 {
		deep = filepath.Join(deep, strings.Repeat("d", 50))
	}
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	p := writeTemp(t, deep, "long.txt", "deep")
	b, err := ReadFile(p, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "deep" {
		t.Fatalf("got %q", b)
	}
}

func setAttrs(t *testing.T, path string, attrs uint32) {
	t.Helper()
	p16, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetFileAttributes(p16, attrs); err != nil {
		t.Fatal(err)
	}
}

// mklinkJunction creates a directory junction (no admin rights needed).
func mklinkJunction(t *testing.T, link, target string) {
	t.Helper()
	out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput()
	if err != nil {
		t.Fatalf("mklink /J: %v: %s", err, out)
	}
}
