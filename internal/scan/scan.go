//go:build windows

// Package scan walks the folders the user chose and decides which files Findly looks at
// (plan 9.3). It reads only folder listings, never file contents, so it is fast and can
// produce counts for time estimates before any indexing starts.
package scan

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/sanskarmishra479/findly/internal/safefs"
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

// Extensions of key, certificate, password-database and VPN files (P-6).
var secretExts = map[string]bool{
	".pem": true, ".key": true, ".pfx": true, ".p12": true, ".kdbx": true, ".ovpn": true, ".env": true,
}

// IsSecretName reports file names that look like keys or secrets (P-6). Such files are
// skipped completely, even inside folders the user chose.
func IsSecretName(name string) bool {
	n := strings.ToLower(name)
	switch {
	case secretExts[filepath.Ext(n)],
		strings.HasPrefix(n, ".env"),
		strings.HasPrefix(n, "id_rsa"), strings.HasPrefix(n, "id_ed25519"),
		strings.HasPrefix(n, "id_ecdsa"), strings.HasPrefix(n, "id_dsa"),
		strings.Contains(n, "password"), strings.Contains(n, "secret"):
		return true
	}
	return false
}

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
	Counts    map[string]int
	Bytes     map[string]int64
	Secret    int // files skipped by IsSecretName
	CloudOnly int // files and folders skipped because they are not on this PC (I-4)
	Links     int // symlinks/junctions not followed (I-5)
	TooLarge  int
	Errors    int // folders that could not be read (e.g. access denied)
}

// Options for one root.
type Options struct {
	// Excludes are subfolders the user unticked. They and everything below are skipped.
	Excludes []string
}

// Root scans one chosen folder and calls fn for every file to index. fn may return an
// error to stop the scan. Scanning stops early if ctx is cancelled.
func Root(ctx context.Context, root string, opts Options, fn func(File) error) (Stats, error) {
	s := &scanner{
		ctx:   ctx,
		fn:    fn,
		stats: Stats{Counts: map[string]int{}, Bytes: map[string]int64{}},
	}
	for _, e := range opts.Excludes {
		s.excludes = append(s.excludes, strings.ToLower(filepath.Clean(e)))
	}
	err := s.dir(filepath.Clean(root), false)
	return s.stats, err
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
		return nil // an unreadable folder must not stop the scan
	}
	if !inCodeProject {
		for _, e := range entries {
			if codeMarkers[strings.ToLower(e.Name)] {
				inCodeProject = true
				break
			}
		}
	}

	for _, e := range entries {
		switch {
		case e.IsLink():
			s.stats.Links++
			continue
		case e.IsHidden():
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
	s.stats.Counts[kind]++
	s.stats.Bytes[kind] += e.Size
	return s.fn(File{
		Path:    e.Path,
		Name:    e.Name,
		Ext:     filepath.Ext(lower),
		Kind:    kind,
		Size:    e.Size,
		ModTime: e.ModTime.UnixNano(),
	})
}

// excluded reports whether path is an unticked folder or inside one.
func (s *scanner) excluded(path string) bool {
	p := strings.ToLower(path)
	for _, ex := range s.excludes {
		if p == ex || strings.HasPrefix(p, ex+`\`) {
			return true
		}
	}
	return false
}
