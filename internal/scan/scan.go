//go:build windows

// Package scan walks the folders the user chose and decides which files Findly looks at
// (plan 9.3). It reads only folder listings, never file contents, so it is fast and can
// produce counts for time estimates before any indexing starts.
package scan

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/sanskarmishra479/findly/internal/safefs"
	"golang.org/x/sys/windows"
)

// File kinds.
const (
	KindText  = "text"
	KindDoc   = "doc"
	KindPDF   = "pdf"
	KindImage = "image"
	KindAudio = "audio"
	KindVideo = "video"
	KindOther = "other" // unsupported: only its file name is searchable
)

var kindByExt = map[string]string{
	".txt": KindText, ".md": KindText, ".markdown": KindText, ".csv": KindText, ".log": KindText, ".json": KindText,
	".docx": KindDoc, ".odt": KindDoc, ".rtf": KindDoc, ".html": KindDoc, ".htm": KindDoc,
	".pdf": KindPDF,
	".jpg": KindImage, ".jpeg": KindImage, ".png": KindImage, ".webp": KindImage, ".bmp": KindImage,
	".gif": KindImage, ".tif": KindImage, ".tiff": KindImage, ".heic": KindImage,
	".mp3": KindAudio, ".m4a": KindAudio, ".wav": KindAudio, ".flac": KindAudio, ".ogg": KindAudio,
	".opus": KindAudio, ".aac": KindAudio, ".wma": KindAudio,
	".mp4": KindVideo, ".mkv": KindVideo, ".mov": KindVideo, ".avi": KindVideo, ".webm": KindVideo,
	".wmv": KindVideo, ".m4v": KindVideo,
}

// Classify returns the kind for a file name, KindOther if unsupported.
func Classify(name string) string {
	if k, ok := kindByExt[strings.ToLower(filepath.Ext(name))]; ok {
		return k
	}
	return KindOther
}

// Folders never entered (compared case-insensitively).
var skipDirs = map[string]bool{
	"appdata": true, "$recycle.bin": true, "system volume information": true,
	"node_modules": true, ".git": true, ".venv": true, "venv": true, "__pycache__": true, ".cache": true,
	// credentials (P-6)
	".ssh": true, ".gnupg": true, ".aws": true, ".azure": true, ".kube": true,
}

// A folder containing one of these is a code project: only images inside it are indexed.
var codeMarkers = map[string]bool{
	".git": true, "package.json": true, "go.mod": true, "cargo.toml": true, "pom.xml": true,
}

// Files never indexed, not even by name.
var skipFileNames = map[string]bool{"thumbs.db": true, "desktop.ini": true}

// Extensions of key, certificate, keystore, password-database and VPN files (P-6).
var secretExts = map[string]bool{
	".pem": true, ".key": true, ".pfx": true, ".p12": true, ".kdbx": true, ".ovpn": true, ".env": true,
	".ppk": true, ".jks": true, ".keystore": true, ".gpg": true, ".asc": true,
}

// Exact file names of credential stores.
var secretNames = map[string]bool{
	".netrc": true, "_netrc": true, ".git-credentials": true, ".pgpass": true, "accesskeys.csv": true,
}

// IsSecretName reports file or folder names that look like keys or secrets (P-6). Such
// files, and everything inside such folders, are skipped completely, even inside folders
// the user chose.
func IsSecretName(name string) bool {
	n := strings.ToLower(name)
	switch {
	case secretExts[filepath.Ext(n)], secretNames[n],
		n == ".env", strings.HasPrefix(n, ".env."), // .env, .env.local — but not ".envelope.txt"
		strings.HasPrefix(n, "id_rsa"), strings.HasPrefix(n, "id_ed25519"),
		strings.HasPrefix(n, "id_ecdsa"), strings.HasPrefix(n, "id_dsa"),
		strings.Contains(n, "password"), strings.Contains(n, "passwd"),
		strings.Contains(n, "credential"),
		containsSecret(n):
		return true
	}
	return false
}

// containsSecret matches "secret" but not "secretary"/"secretariat" (meeting notes etc.).
func containsSecret(n string) bool {
	for i := 0; ; {
		j := strings.Index(n[i:], "secret")
		if j < 0 {
			return false
		}
		i += j + len("secret")
		if !strings.HasPrefix(n[i:], "ar") {
			return true
		}
	}
}

// MaxFailedDirs caps Stats.FailedDirs; beyond it callers should assume anything failed.
const MaxFailedDirs = 1000

// MaxSize is the largest non-video file considered (plan 9.3: 2 GB).
const MaxSize = 2 << 30

// File is a file the indexer should handle.
type File struct {
	Path    string
	Name    string
	Ext     string // lower case with dot
	Kind    string
	Size    int64
	ModTime int64 // unix nanoseconds
}

// Stats counts what a scan found. Counts and Bytes are keyed by kind.
type Stats struct {
	Counts     map[string]int
	Bytes      map[string]int64
	Secret     int // files skipped by IsSecretName
	CloudOnly  int // files and folders skipped because they are not on this PC (I-4)
	Links      int // symlinks/junctions not followed (I-5)
	TooLarge   int
	Errors     int      // folders that could not be read (e.g. access denied)
	FailedDirs []string // those folders (first MaxFailedDirs), so callers keep their files
}

// Options for one root.
type Options struct {
	// Excludes are subfolders the user unticked. They and everything below are skipped.
	Excludes []string
}

// ErrBadRoot is returned when the root itself cannot be scanned (missing, unplugged
// drive, a link, cloud-only). Callers must NOT treat this as "the folder is empty":
// purging the index for an unplugged USB drive would lose everything indexed from it.
var ErrBadRoot = errors.New("scan: root folder is not available")

// Root scans one chosen folder and calls fn for every file to index. fn may return an
// error to stop the scan. Scanning stops early if ctx is cancelled. root must be a real
// path (safefs.Canonical); unreadable subfolders are counted in Stats.Errors and skipped.
func Root(ctx context.Context, root string, opts Options, fn func(File) error) (Stats, error) {
	s := &scanner{
		ctx:   ctx,
		fn:    fn,
		stats: Stats{Counts: map[string]int{}, Bytes: map[string]int64{}},
	}
	root = filepath.Clean(root)
	info, err := safefs.Stat(root)
	switch {
	case err != nil:
		return s.stats, fmt.Errorf("%w: %v", ErrBadRoot, err)
	case !info.IsDir(), info.IsLink(), info.IsCloudOnly():
		return s.stats, fmt.Errorf("%w: %s is not a real local folder", ErrBadRoot, root)
	}
	entries, err := safefs.ReadDir(root)
	if err != nil {
		return s.stats, fmt.Errorf("%w: %v", ErrBadRoot, err)
	}

	for _, e := range opts.Excludes {
		// Use the real long path so "C:\LONGFO~1", "\\?\C:\x" or a trailing "\" still match.
		if c, err := safefs.Canonical(e); err == nil {
			e = c
		}
		s.excludes = append(s.excludes, strings.TrimRight(filepath.Clean(e), `\`))
	}
	return s.stats, s.entries(entries, isCodeProject(entries))
}

func isCodeProject(entries []safefs.Info) bool {
	for _, e := range entries {
		if codeMarkers[strings.ToLower(e.Name)] {
			return true
		}
	}
	return false
}

type scanner struct {
	ctx      context.Context
	fn       func(File) error
	excludes []string
	stats    Stats
}

func (s *scanner) dir(path string, inCodeProject bool) error {
	if err := s.ctx.Err(); err != nil {
		return err
	}
	entries, err := safefs.ReadDir(path)
	if err != nil {
		s.stats.Errors++
		if len(s.stats.FailedDirs) < MaxFailedDirs {
			s.stats.FailedDirs = append(s.stats.FailedDirs, path)
		}
		return nil // an unreadable subfolder must not stop the scan
	}
	return s.entries(entries, inCodeProject || isCodeProject(entries))
}

func (s *scanner) entries(entries []safefs.Info, inCodeProject bool) error {
	for i, e := range entries {
		if i%256 == 255 { // big flat folders: notice cancellation without waiting for the end
			if err := s.ctx.Err(); err != nil {
				return err
			}
		}
		switch {
		case e.IsLink():
			s.stats.Links++
			continue
		case e.Attrs&windows.FILE_ATTRIBUTE_HIDDEN != 0:
			// Only HIDDEN: a visible folder can get SYSTEM just from desktop.ini customization.
			continue
		case e.IsCloudOnly():
			s.stats.CloudOnly++
			continue
		case s.excluded(e.Path):
			continue
		}

		if e.IsDir() {
			if skipDirs[strings.ToLower(e.Name)] {
				continue
			}
			if IsSecretName(e.Name) { // e.g. "Passwords", "Secrets", "credentials"
				s.stats.Secret++
				continue
			}
			if err := s.dir(e.Path, inCodeProject); err != nil {
				return err
			}
			continue
		}

		if err := s.file(e, inCodeProject); err != nil {
			return err
		}
	}
	return nil
}

func (s *scanner) file(e safefs.Info, inCodeProject bool) error {
	lower := strings.ToLower(e.Name)
	if skipFileNames[lower] {
		return nil
	}
	if IsSecretName(e.Name) {
		s.stats.Secret++
		return nil
	}
	kind := Classify(e.Name)
	if inCodeProject && kind != KindImage {
		return nil
	}
	if e.Size > MaxSize && kind != KindVideo {
		s.stats.TooLarge++
		return nil
	}
	err := s.fn(File{
		Path:    e.Path,
		Name:    e.Name,
		Ext:     filepath.Ext(lower),
		Kind:    kind,
		Size:    e.Size,
		ModTime: e.ModTime.UnixNano(),
	})
	if err != nil {
		return err
	}
	s.stats.Counts[kind]++
	s.stats.Bytes[kind] += e.Size
	return nil
}

// excluded reports whether path is an unticked folder or inside one.
func (s *scanner) excluded(path string) bool {
	for _, ex := range s.excludes {
		if len(path) < len(ex) || !strings.EqualFold(path[:len(ex)], ex) {
			continue
		}
		// Whole folder name must match: "C:\private" excludes "C:\private\x" but not "C:\privateer".
		if len(path) == len(ex) || path[len(ex)] == '\\' || strings.HasSuffix(ex, `:`) {
			return true
		}
	}
	return false
}
