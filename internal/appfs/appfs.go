// Package appfs is the only way Findly writes to disk. Every path is relative to the app
// data directory (%LOCALAPPDATA%\Findly) and cannot escape it: no "..", no absolute
// paths, no symlinks or junctions that lead outside (plan I-3).
//
// The guarantee comes from os.Root (Go standard library), which resolves every path
// inside the root directory and refuses anything that would leave it.
package appfs

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// AppID is the frozen internal name used for the data folder (see plan.md, top).
const AppID = "findly"

// Folder name under %LOCALAPPDATA%. Windows paths are case-insensitive, so the
// display casing does not affect existing installs.
const dataDirName = "Findly"

// Standard subfolders (plan 9.2).
const (
	DirModels  = "models"
	DirIndex   = "index"
	DirThumbs  = "thumbs"
	DirLogs    = "logs"
	DirBin     = "bin"
	DirLicense = "license"
	// DirGPU is the working and cache folder for helper programs (llama-server): GPU
	// drivers write logs and shader caches relative to it, and they must stay inside the
	// app data folder (plan I-3), not in the install folder.
	DirGPU = "gpu"
	// DirWebview holds WebView2's browser data for the app window. WebView2 writes it
	// itself; without this setting it would go to %APPDATA%, outside the app folder.
	DirWebview = "webview"
)

// ErrOutside is returned for paths that are absolute or leave the app directory.
var ErrOutside = errors.New("appfs: path is outside the app data directory")

// FS writes only inside one directory.
type FS struct {
	root *os.Root
	path string
}

// DefaultDir returns %LOCALAPPDATA%\Findly.
func DefaultDir() (string, error) {
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		return "", errors.New("appfs: LOCALAPPDATA is not set")
	}
	return filepath.Join(base, dataDirName), nil
}

// Open creates dir (and the standard subfolders) if needed and returns an FS rooted there.
func Open(dir string) (*FS, error) {
	if !filepath.IsAbs(dir) {
		return nil, fmt.Errorf("appfs: root %q must be absolute", dir)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	f := &FS{root: root, path: filepath.Clean(dir)}
	for _, sub := range []string{DirModels, DirIndex, DirThumbs, DirLogs, DirBin, DirLicense, DirGPU, DirWebview} {
		if err := root.MkdirAll(sub, 0o700); err != nil {
			root.Close()
			return nil, err
		}
	}
	return f, nil
}

// Close releases the root directory handle.
func (f *FS) Close() error { return f.root.Close() }

// Dir returns the absolute app data directory.
func (f *FS) Dir() string { return f.path }

// Path returns the absolute path for rel, for code that must open files itself (for
// example SQLite). Unlike the other methods, the caller's later use of the path is not
// protected by os.Root, so Path also refuses rel if any existing folder or file along it
// is a symlink or junction. A link created after this check is not caught; only pass
// paths under folders Findly itself created (such as index/).
func (f *FS) Path(rel string) (string, error) {
	if err := check(rel); err != nil {
		return "", err
	}
	rel = filepath.FromSlash(rel)
	parts := strings.Split(rel, string(filepath.Separator))
	for i := range parts {
		info, err := f.root.Lstat(filepath.Join(parts[:i+1]...))
		if errors.Is(err, fs.ErrNotExist) {
			break // the rest does not exist yet, so it cannot be a link
		}
		if err != nil {
			return "", err
		}
		if info.Mode()&(fs.ModeSymlink|fs.ModeIrregular) != 0 {
			return "", fmt.Errorf("%w: %q goes through a link", ErrOutside, rel)
		}
	}
	return filepath.Join(f.path, rel), nil
}

// WriteFile writes data to rel atomically: the old content stays intact until the new
// content is fully on disk, so a crash never leaves a half-written file.
func (f *FS) WriteFile(rel string, data []byte) error {
	if err := check(rel); err != nil {
		return err
	}
	rel = filepath.FromSlash(rel)
	if dir := filepath.Dir(rel); dir != "." {
		if err := f.root.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}

	suffix, err := randomSuffix()
	if err != nil {
		return err
	}
	tmp := rel + ".tmp-" + suffix
	tf, err := f.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, werr := tf.Write(data)
	if werr == nil {
		werr = tf.Sync() // make sure the bytes reach the disk before the rename
	}
	if cerr := tf.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		f.root.Remove(tmp)
		return werr
	}
	if err := f.root.Rename(tmp, rel); err != nil {
		f.root.Remove(tmp)
		return err
	}
	return nil
}

// ReadFile reads a file inside the app directory.
func (f *FS) ReadFile(rel string) ([]byte, error) {
	if err := check(rel); err != nil {
		return nil, err
	}
	return f.root.ReadFile(filepath.FromSlash(rel))
}

// MkdirAll creates a folder (and parents) inside the app directory.
func (f *FS) MkdirAll(rel string) error {
	if err := check(rel); err != nil {
		return err
	}
	return f.root.MkdirAll(filepath.FromSlash(rel), 0o700)
}

// Remove deletes one file or empty folder inside the app directory.
// A missing file is not an error.
func (f *FS) Remove(rel string) error {
	if err := check(rel); err != nil {
		return err
	}
	err := f.root.Remove(filepath.FromSlash(rel))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// RemoveAll deletes a folder tree inside the app directory. It refuses to delete the
// app directory itself; use a subfolder name.
func (f *FS) RemoveAll(rel string) error {
	if err := check(rel); err != nil {
		return err
	}
	if filepath.Clean(rel) == "." {
		return fmt.Errorf("appfs: refusing to remove the app directory root")
	}
	return f.root.RemoveAll(filepath.FromSlash(rel))
}

// check rejects absolute paths, "..", empty names and Windows device names (NUL, COM1…).
// os.Root enforces the same rules again when the path is used; this gives a clear error early.
func check(rel string) error {
	rel = filepath.FromSlash(rel)
	if rel == "" || !filepath.IsLocal(rel) {
		return fmt.Errorf("%w: %q", ErrOutside, rel)
	}
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if isDeviceName(part) {
			return fmt.Errorf("%w: %q uses a Windows device name", ErrOutside, rel)
		}
	}
	return nil
}

// isDeviceName reports names like NUL, nul.txt or COM1.db. filepath.IsLocal allows
// "NUL.txt", but on Windows 10 that still opens the NUL device when used as a plain path.
func isDeviceName(name string) bool {
	base, _, _ := strings.Cut(name, ".")
	base = strings.ToUpper(strings.TrimRight(base, " "))
	switch base {
	case "CON", "PRN", "AUX", "NUL", "CONIN$", "CONOUT$":
		return true
	}
	if len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) {
		return base[3] >= '0' && base[3] <= '9'
	}
	if rest, ok := strings.CutPrefix(base, "COM"); ok {
		return rest == "¹" || rest == "²" || rest == "³"
	}
	if rest, ok := strings.CutPrefix(base, "LPT"); ok {
		return rest == "¹" || rest == "²" || rest == "³"
	}
	return false
}

func randomSuffix() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
