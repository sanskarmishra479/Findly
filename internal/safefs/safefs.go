//go:build windows

// Package safefs is the only way Findly touches user files. It is read-only by design:
// it has no function that writes, renames, moves, deletes or truncates anything (plan I-1).
//
// Every open uses GENERIC_READ with FILE_SHARE_READ|WRITE|DELETE, so other programs can
// still save, rename or delete a file while Findly is reading it (I-2). Cloud-only
// placeholders are never opened, so reading never triggers a download (I-4). Symlinks,
// junctions and mount points are never followed (I-5).
package safefs

import (
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	// ErrNotAbsolute is returned for relative paths; callers always pass full paths.
	ErrNotAbsolute = errors.New("safefs: path must be absolute")
	// ErrCloudOnly is returned for files whose content lives only in the cloud (I-4).
	ErrCloudOnly = errors.New("safefs: file is cloud-only; reading would download it")
	// ErrLink is returned for symlinks, junctions and mount points (I-5).
	ErrLink = errors.New("safefs: path is a symlink or junction")
	// ErrIsDir is returned when Open is given a directory.
	ErrIsDir = errors.New("safefs: path is a directory")
)

// Info describes a file or folder without opening it.
type Info struct {
	Path       string
	Name       string
	Size       int64
	ModTime    time.Time
	Attrs      uint32 // FILE_ATTRIBUTE_* bits
	ReparseTag uint32 // IO_REPARSE_TAG_*, only meaningful when Attrs has FILE_ATTRIBUTE_REPARSE_POINT
}

func (i Info) IsDir() bool { return i.Attrs&windows.FILE_ATTRIBUTE_DIRECTORY != 0 }

// IsLink reports whether the entry is a symlink, junction or mount point.
func (i Info) IsLink() bool { return isNameSurrogate(i.Attrs, i.ReparseTag) }

// IsCloudOnly reports whether reading the entry would fetch it from the cloud.
func (i Info) IsCloudOnly() bool { return IsCloudOnly(i.Attrs) }

// IsHidden reports whether the entry has the hidden or system attribute.
func (i Info) IsHidden() bool {
	return i.Attrs&(windows.FILE_ATTRIBUTE_HIDDEN|windows.FILE_ATTRIBUTE_SYSTEM) != 0
}

// IsCloudOnly reports whether attrs mark a file whose data is not on this PC (I-4).
func IsCloudOnly(attrs uint32) bool {
	const cloud = windows.FILE_ATTRIBUTE_RECALL_ON_DATA_ACCESS |
		windows.FILE_ATTRIBUTE_RECALL_ON_OPEN |
		windows.FILE_ATTRIBUTE_OFFLINE
	return attrs&cloud != 0
}

// isNameSurrogate reports whether a reparse point redirects to another path
// (symlink, junction, mount point). Other reparse points, such as OneDrive files that
// are already downloaded or Windows-compressed files, are normal files and are allowed.
func isNameSurrogate(attrs, tag uint32) bool {
	const nameSurrogateBit = 0x20000000 // IsReparseTagNameSurrogate in winnt.h
	return attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 && tag&nameSurrogateBit != 0
}

// Stat describes path without opening it and without following links.
// It uses FindFirstFile, which reads directory metadata only, so it never triggers
// a cloud download and never locks the file.
func Stat(path string) (Info, error) {
	p, err := longPath(path)
	if err != nil {
		return Info{}, err
	}
	p16, err := windows.UTF16PtrFromString(p)
	if err != nil {
		return Info{}, err
	}
	if clean := filepath.Clean(path); clean == filepath.VolumeName(clean)+`\` {
		// A drive root like D:\ has no directory entry for FindFirstFile to return.
		attrs, err := windows.GetFileAttributes(p16)
		if err != nil {
			return Info{}, &os.PathError{Op: "stat", Path: path, Err: err}
		}
		return Info{Path: clean, Name: clean, Attrs: attrs}, nil
	}
	var fd windows.Win32finddata
	h, err := windows.FindFirstFile(p16, &fd)
	if err != nil {
		return Info{}, &os.PathError{Op: "stat", Path: path, Err: err}
	}
	windows.FindClose(h)
	return infoFromFindData(filepath.Clean(path), &fd), nil
}

// Open opens a regular file for reading only. The caller must Close it.
func Open(path string) (*os.File, error) {
	info, err := Stat(path)
	if err != nil {
		return nil, err
	}
	switch {
	case info.IsDir():
		return nil, &os.PathError{Op: "open", Path: path, Err: ErrIsDir}
	case info.IsLink():
		return nil, &os.PathError{Op: "open", Path: path, Err: ErrLink}
	case info.IsCloudOnly():
		return nil, &os.PathError{Op: "open", Path: path, Err: ErrCloudOnly}
	}

	// Open the path itself, never what it points to. If it was swapped for a link after
	// the Stat above, we catch it here instead of following it.
	h, err := createFile(path, windows.FILE_FLAG_OPEN_REPARSE_POINT)
	if err != nil {
		return nil, err
	}
	fail := func(h windows.Handle, err error) (*os.File, error) {
		windows.CloseHandle(h)
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	attrs, tag, err := attributeTag(h)
	switch {
	case err != nil:
		return fail(h, err)
	case isNameSurrogate(attrs, tag):
		return fail(h, ErrLink)
	case IsCloudOnly(attrs):
		return fail(h, ErrCloudOnly)
	case attrs&windows.FILE_ATTRIBUTE_DIRECTORY != 0:
		return fail(h, ErrIsDir)
	}
	// The file itself is not a link, but a folder earlier in the path could be a
	// junction. The real path of the handle must equal the path we were given.
	if err := checkFinalPath(h, path); err != nil {
		return fail(h, err)
	}

	if attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		// A non-link reparse point (downloaded OneDrive file, Windows-compressed file):
		// its data is served by a filter driver, so open it again normally. Keep the first
		// handle open until we have proved the second one is the very same file.
		h2, err := createFile(path, 0)
		if err != nil {
			windows.CloseHandle(h)
			return nil, err
		}
		same, err := sameFile(h, h2)
		windows.CloseHandle(h)
		if err != nil {
			return fail(h2, err)
		}
		if !same {
			return fail(h2, ErrLink)
		}
		if attrs, _, err = attributeTag(h2); err != nil {
			return fail(h2, err)
		}
		if IsCloudOnly(attrs) {
			return fail(h2, ErrCloudOnly)
		}
		h = h2
	}
	return os.NewFile(uintptr(h), filepath.Clean(path)), nil
}

// Canonical returns the real path of an existing folder or file: links in the path are
// resolved and the casing matches the disk. Use it once when the user picks a folder;
// everything else in safefs then refuses paths that go through links.
func Canonical(path string) (string, error) {
	p, err := longPath(path)
	if err != nil {
		return "", err
	}
	p16, err := windows.UTF16PtrFromString(p)
	if err != nil {
		return "", err
	}
	h, err := windows.CreateFile(p16, 0, // no access needed, metadata only
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return "", &os.PathError{Op: "canonical", Path: path, Err: err}
	}
	defer windows.CloseHandle(h)
	return finalPath(h)
}

// ReadFile reads a whole file, refusing files larger than limit bytes.
func ReadFile(path string, limit int64) ([]byte, error) {
	if limit < 0 || limit == math.MaxInt64 {
		return nil, fmt.Errorf("safefs: invalid limit %d", limit)
	}
	f, err := Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("safefs: %s is larger than %d bytes", path, limit)
	}
	return b, nil
}

// SkipDir and SkipAll can be returned from a WalkFunc, like in io/fs.
var (
	SkipDir = errors.New("skip this directory")
	SkipAll = errors.New("skip everything")
)

// WalkFunc is called for every entry under the root (not for the root itself).
// If reading a directory fails, it is called with that directory's Info and the error;
// returning nil or SkipDir then continues with the next sibling.
type WalkFunc func(info Info, err error) error

// WalkDir walks the tree under root in lexical order per directory. It never descends
// into links (I-5) or cloud-only folders (I-4); those entries are still passed to fn so
// callers can count them. root must be a real path (see Canonical).
//
// Like io/fs: returning SkipDir for a directory skips it; returning SkipDir for a file
// skips the rest of that file's directory; SkipAll stops the walk.
func WalkDir(root string, fn WalkFunc) error {
	info, err := Stat(root)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.IsLink() {
		return &os.PathError{Op: "walk", Path: root, Err: errors.New("root must be a real directory")}
	}
	err = walk(filepath.Clean(root), fn)
	if errors.Is(err, SkipAll) || errors.Is(err, SkipDir) {
		return nil
	}
	return err
}

func walk(dir string, fn WalkFunc) error {
	entries, err := ReadDir(dir)
	if err != nil {
		info, _ := Stat(dir)
		if err := fn(info, err); err != nil && !errors.Is(err, SkipDir) {
			return err
		}
		return nil
	}
	for _, e := range entries {
		err := fn(e, nil)
		if errors.Is(err, SkipDir) {
			if e.IsDir() {
				continue
			}
			return nil
		}
		if err != nil {
			return err
		}
		if e.IsDir() && !e.IsLink() && !e.IsCloudOnly() {
			if err := walk(e.Path, fn); err != nil {
				return err
			}
		}
	}
	return nil
}

// ReadDir lists one directory with FindFirstFile/FindNextFile (metadata only).
// It first proves the directory is reached without going through a link.
func ReadDir(dir string) ([]Info, error) {
	if err := checkDir(dir); err != nil {
		return nil, err
	}
	pattern, err := longPath(filepath.Join(dir, "*"))
	if err != nil {
		return nil, err
	}
	p16, err := windows.UTF16PtrFromString(pattern)
	if err != nil {
		return nil, err
	}
	var fd windows.Win32finddata
	h, err := windows.FindFirstFile(p16, &fd)
	if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
		return nil, nil // an empty drive root has no "." or ".." entries
	}
	if err != nil {
		return nil, &os.PathError{Op: "readdir", Path: dir, Err: err}
	}
	defer windows.FindClose(h)

	var out []Info
	for {
		name := windows.UTF16ToString(fd.FileName[:])
		if name != "." && name != ".." {
			out = append(out, infoFromFindData(filepath.Join(dir, name), &fd))
		}
		if err := windows.FindNextFile(h, &fd); err != nil {
			if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
				break
			}
			return nil, &os.PathError{Op: "readdir", Path: dir, Err: err}
		}
	}
	return out, nil
}

func infoFromFindData(path string, fd *windows.Win32finddata) Info {
	return Info{
		Path:       path,
		Name:       filepath.Base(path),
		Size:       int64(fd.FileSizeHigh)<<32 | int64(fd.FileSizeLow),
		ModTime:    time.Unix(0, fd.LastWriteTime.Nanoseconds()),
		Attrs:      fd.FileAttributes,
		ReparseTag: fd.Reserved0, // dwReserved0 holds the reparse tag (documented for FindFirstFile)
	}
}

func createFile(path string, extraFlags uint32) (windows.Handle, error) {
	p, err := longPath(path)
	if err != nil {
		return 0, err
	}
	p16, err := windows.UTF16PtrFromString(p)
	if err != nil {
		return 0, err
	}
	h, err := windows.CreateFile(p16,
		windows.GENERIC_READ, // read only, never write (I-1)
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, // never block others (I-2)
		nil,
		windows.OPEN_EXISTING, // never create
		windows.FILE_FLAG_SEQUENTIAL_SCAN|extraFlags,
		0)
	if err != nil {
		return 0, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return h, nil
}

// checkDir opens dir itself (not what it may point to) and refuses links, cloud-only
// folders and folders reached through a junction higher up in the path.
func checkDir(dir string) error {
	p, err := longPath(dir)
	if err != nil {
		return err
	}
	p16, err := windows.UTF16PtrFromString(p)
	if err != nil {
		return err
	}
	h, err := windows.CreateFile(p16, 0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return &os.PathError{Op: "readdir", Path: dir, Err: err}
	}
	defer windows.CloseHandle(h)
	attrs, tag, err := attributeTag(h)
	switch {
	case err != nil:
		return &os.PathError{Op: "readdir", Path: dir, Err: err}
	case isNameSurrogate(attrs, tag):
		return &os.PathError{Op: "readdir", Path: dir, Err: ErrLink}
	case IsCloudOnly(attrs):
		return &os.PathError{Op: "readdir", Path: dir, Err: ErrCloudOnly}
	}
	return checkFinalPath(h, dir)
}

// checkFinalPath refuses a handle whose real path differs from the path we asked for,
// which means some folder in the path is a junction or symlink. Comparison ignores case,
// like Windows. Paths written with 8.3 short names are refused too; safefs callers use
// the long names that WalkDir and Canonical return.
func checkFinalPath(h windows.Handle, want string) error {
	got, err := finalPath(h)
	if err != nil {
		return err
	}
	if !strings.EqualFold(got, filepath.Clean(stripLongPrefix(want))) {
		return fmt.Errorf("%w: real path is %s", ErrLink, got)
	}
	return nil
}

// finalPath returns the real path of an open handle in normal C:\... or \\server\... form.
func finalPath(h windows.Handle) (string, error) {
	buf := make([]uint16, 512)
	for {
		n, err := windows.GetFinalPathNameByHandle(h, &buf[0], uint32(len(buf)), 0)
		if err != nil {
			return "", err
		}
		if int(n) < len(buf) {
			return stripLongPrefix(windows.UTF16ToString(buf[:n])), nil
		}
		buf = make([]uint16, n+1) // too small: n is the size needed
	}
}

// stripLongPrefix turns \\?\C:\x into C:\x and \\?\UNC\server\x into \\server\x.
func stripLongPrefix(p string) string {
	if rest, ok := strings.CutPrefix(p, `\\?\UNC\`); ok {
		return `\\` + rest
	}
	return strings.TrimPrefix(p, `\\?\`)
}

// sameFile reports whether two handles refer to the same file on the same volume.
func sameFile(a, b windows.Handle) (bool, error) {
	var ia, ib windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(a, &ia); err != nil {
		return false, err
	}
	if err := windows.GetFileInformationByHandle(b, &ib); err != nil {
		return false, err
	}
	return ia.VolumeSerialNumber == ib.VolumeSerialNumber &&
		ia.FileIndexHigh == ib.FileIndexHigh &&
		ia.FileIndexLow == ib.FileIndexLow, nil
}

// attributeTag returns the attributes and reparse tag of an open handle.
func attributeTag(h windows.Handle) (attrs, tag uint32, err error) {
	var info struct {
		FileAttributes uint32
		ReparseTag     uint32
	}
	err = windows.GetFileInformationByHandleEx(h, windows.FileAttributeTagInfo,
		(*byte)(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)))
	return info.FileAttributes, info.ReparseTag, err
}

// longPath turns an absolute path into the \\?\ form so paths longer than 260
// characters work with the raw Windows API (os.* does this automatically; we call
// the API directly, so we do it ourselves).
func longPath(path string) (string, error) {
	if strings.HasPrefix(path, `\\?\`) {
		return path, nil
	}
	if !filepath.IsAbs(path) {
		return "", &os.PathError{Op: "safefs", Path: path, Err: ErrNotAbsolute}
	}
	p := filepath.Clean(path)
	if strings.HasPrefix(p, `\\`) { // UNC: \\server\share\... -> \\?\UNC\server\share\...
		return `\\?\UNC\` + p[2:], nil
	}
	return `\\?\` + p, nil
}
