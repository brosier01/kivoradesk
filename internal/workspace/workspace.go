// Package workspace lets the desktop account write files into the shared
// workspace safely.
//
// The workspace is also writable by the worker account, which runs the
// agent's shell commands. A worker could plant a symlink where the desktop
// account is about to write and redirect the write into the desktop
// account's private files. Every path is therefore resolved with openat2,
// refusing symlinks and anything outside the workspace, and existing files
// are never overwritten. The directory stays open while a file is written, so
// that cleaning up after an error cannot follow what the worker put in its place.
//
// The data disk keeps a reserve for the audit log, the vault and the browser
// profile, and the desktop account may use it. A write therefore stops when
// the space left to ordinary accounts runs out, so that a download cannot eat
// the reserve once the worker has filled the workspace.
package workspace

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

// maxName is the longest name SanitizeName returns, in bytes, and maxExt the
// longest extension it keeps when it has to shorten one.
const (
	maxName = 200
	maxExt  = 16
)

// A write checks the free space before it starts and again every checkEvery
// bytes, and stops while minFree bytes are still free outside the reserve.
const (
	checkEvery = 4 << 20
	minFree    = 1 << 20
)

// ErrDiskFull is returned when a write would use the reserved space.
var ErrDiskFull = errors.New("the disk is full: the space left is kept for the audit log and the credentials; free some room in /workspace")

// statfs is unix.Fstatfs, replaced in tests.
var statfs = unix.Fstatfs

// freeSpace returns the bytes free outside the reserve on the disk of fd.
func freeSpace(fd int) (uint64, error) {
	var st unix.Statfs_t
	if err := statfs(fd, &st); err != nil {
		return 0, fmt.Errorf("check free space: %w", err)
	}
	return st.Bavail * uint64(st.Bsize), nil
}

// spaceWriter writes to f, checking the free space on the disk of fd.
type spaceWriter struct {
	f      io.Writer
	fd     int
	budget uint64 // bytes it may write before the next check
}

// refill checks the free space and sets the budget until the next check.
func (w *spaceWriter) refill() error {
	free, err := freeSpace(w.fd)
	if err != nil {
		return err
	}
	if free <= minFree {
		return ErrDiskFull
	}
	w.budget = min(free-minFree, checkEvery)
	return nil
}

func (w *spaceWriter) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		if w.budget == 0 {
			if err := w.refill(); err != nil {
				return written, err
			}
		}
		chunk := p[:min(uint64(len(p)), w.budget)]
		n, err := w.f.Write(chunk)
		written += n
		w.budget -= uint64(n)
		if errors.Is(err, unix.ENOSPC) {
			return written, ErrDiskFull
		}
		if err != nil {
			return written, err
		}
		p = p[n:]
	}
	return written, nil
}

// SanitizeName turns a suggested file name into a safe single path element, which
// a site chose. It stays valid UTF-8 and readable: control characters and those
// that reorder text are gone, and it cannot pass for an option of a command.
func SanitizeName(name string) string {
	name = path.Base(strings.ReplaceAll(name, "\\", "/"))
	name = strings.Map(func(r rune) rune {
		switch {
		case r == utf8.RuneError || r == '/' || unicode.IsControl(r):
			return '_'
		case unicode.Is(unicode.Cf, r):
			return -1
		}
		return r
	}, name)
	name = strings.TrimLeft(strings.TrimSpace(name), ". -")
	if name == "" {
		return "file"
	}
	if len(name) <= maxName {
		return name
	}
	ext := path.Ext(name)
	if len(ext) > maxExt {
		ext = ""
	}
	return cutRunes(strings.TrimSuffix(name, ext), maxName-len(ext)) + ext
}

// cutRunes shortens s to at most n bytes, on the boundary of a character.
func cutRunes(s string, n int) string {
	for len(s) > n {
		_, size := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-size]
	}
	return s
}

// Create writes r into root/dir/name and returns the path relative to root.
// dir is created if needed (one level). If name exists, " (1)", " (2)"...
// is inserted before the extension. At most limit bytes are written, and
// none of the reserved space (see ErrDiskFull).
func Create(root, dir, name string, r io.Reader, limit int64) (string, error) {
	rootFD, err := unix.Open(root, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return "", fmt.Errorf("open workspace: %w", err)
	}
	defer unix.Close(rootFD)

	const resolve = unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS
	dirFD := rootFD
	if dir != "" {
		dir = SanitizeName(dir)
		if err := unix.Mkdirat(rootFD, dir, 0o770); err != nil && !errors.Is(err, unix.EEXIST) {
			return "", fmt.Errorf("create %s: %w", dir, err)
		}
		dirFD, err = unix.Openat2(rootFD, dir, &unix.OpenHow{Flags: unix.O_PATH | unix.O_DIRECTORY | unix.O_CLOEXEC, Resolve: resolve})
		if err != nil {
			return "", fmt.Errorf("open %s: %w", dir, err)
		}
		defer unix.Close(dirFD)
	}

	// Refuse before creating anything when the disk is already full.
	w := &spaceWriter{fd: dirFD}
	if err := w.refill(); err != nil {
		return "", err
	}

	name = SanitizeName(name)
	ext := path.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 0; i < 1000; i++ {
		candidate := name
		if i > 0 {
			candidate = fmt.Sprintf("%s (%d)%s", stem, i, ext)
		}
		rel := path.Join(dir, candidate)
		fd, err := unix.Openat2(dirFD, candidate, &unix.OpenHow{
			Flags: unix.O_WRONLY | unix.O_CREAT | unix.O_EXCL | unix.O_CLOEXEC, Mode: 0o660, Resolve: resolve,
		})
		if errors.Is(err, unix.EEXIST) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("create %s: %w", rel, err)
		}
		f := os.NewFile(uintptr(fd), rel)
		w.f = f
		n, err := io.Copy(w, io.LimitReader(r, limit+1))
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err == nil && n > limit {
			err = fmt.Errorf("file larger than %d bytes", limit)
		}
		if err != nil {
			unix.Unlinkat(dirFD, candidate, 0)
			return "", err
		}
		return rel, nil
	}
	return "", fmt.Errorf("too many files named %s", name)
}
