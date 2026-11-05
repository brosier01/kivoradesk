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
// is inserted before the extension. At most limit bytes are written.
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
		n, err := io.Copy(f, io.LimitReader(r, limit+1))
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
