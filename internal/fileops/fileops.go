// Package fileops places files at their destination with move, copy,
// hardlink or symlink semantics, safely across filesystems.
package fileops

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// ErrSkipped means the destination exists and the conflict policy said skip.
var ErrSkipped = errors.New("destination exists")

// Result describes what Place did.
type Result struct {
	Dst         string // final destination (may differ from requested with "suffix")
	Overwrote   bool
	CrossDevice bool // a move fell back to copy+delete
	Noop        bool // src and dst are already the same file
}

// Place puts src at dst. action is move|copy|hardlink|symlink; conflict is
// skip|overwrite|suffix|keep-larger.
func Place(src, dst, action, conflict string) (Result, error) {
	res := Result{Dst: dst}
	srcInfo, err := os.Stat(src)
	if err != nil {
		return res, err
	}
	if !srcInfo.Mode().IsRegular() {
		return res, fmt.Errorf("%s is not a regular file", src)
	}

	if dstInfo, err := os.Lstat(dst); err == nil {
		if os.SameFile(srcInfo, dstInfo) {
			res.Noop = true
			return res, nil
		}
		switch conflict {
		case "overwrite":
			res.Overwrote = true
		case "keep-larger":
			if srcInfo.Size() <= dstInfo.Size() {
				return res, fmt.Errorf("%w (existing file is not smaller): %s", ErrSkipped, dst)
			}
			res.Overwrote = true
		case "suffix":
			res.Dst = freeName(dst)
		default:
			return res, fmt.Errorf("%w: %s", ErrSkipped, dst)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return res, err
	}

	if err := os.MkdirAll(filepath.Dir(res.Dst), 0o775); err != nil {
		return res, err
	}

	switch action {
	case "move":
		err = os.Rename(src, res.Dst)
		if isCrossDevice(err) {
			res.CrossDevice = true
			if err = copyFile(src, res.Dst, srcInfo); err == nil {
				err = os.Remove(src)
			}
		}
	case "copy":
		err = copyFile(src, res.Dst, srcInfo)
	case "hardlink":
		err = replaceWith(res.Dst, res.Overwrote, func(p string) error { return os.Link(src, p) })
		if isCrossDevice(err) {
			err = fmt.Errorf("hardlink across filesystems is impossible (%s → %s); use action: move or copy", src, res.Dst)
		}
	case "symlink":
		abs, _ := filepath.Abs(src)
		err = replaceWith(res.Dst, res.Overwrote, func(p string) error { return os.Symlink(abs, p) })
	default:
		err = fmt.Errorf("unknown action %q", action)
	}
	return res, err
}

// replaceWith creates a link at a temp name then renames it over dst, so an
// overwrite is atomic.
func replaceWith(dst string, overwrite bool, mk func(string) error) error {
	if !overwrite {
		return mk(dst)
	}
	tmp := tempName(dst)
	if err := mk(tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// copyFile copies to a temp file next to dst, fsyncs, verifies the size,
// then renames into place. A crash never leaves a truncated file at dst.
func copyFile(src, dst string, info os.FileInfo) (err error) {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	tmp := tempName(dst)
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			out.Close()
			os.Remove(tmp)
		}
	}()

	n, err := io.Copy(out, in)
	if err != nil {
		return fmt.Errorf("copy %s: %w", src, err)
	}
	if n != info.Size() {
		return fmt.Errorf("copy %s: wrote %d of %d bytes", src, n, info.Size())
	}
	if err = out.Sync(); err != nil {
		return err
	}
	if err = out.Close(); err != nil {
		return err
	}
	_ = os.Chtimes(tmp, info.ModTime(), info.ModTime())
	if err = os.Rename(tmp, dst); err != nil {
		return err
	}
	syncDir(filepath.Dir(dst))
	return nil
}

func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
}

func tempName(dst string) string {
	return filepath.Join(filepath.Dir(dst), "."+filepath.Base(dst)+".shelf-tmp")
}

// IsTemp reports whether path is one of shelf's in-progress temp files.
func IsTemp(path string) bool { return strings.HasSuffix(path, ".shelf-tmp") }

func freeName(dst string) string {
	ext := filepath.Ext(dst)
	stem := strings.TrimSuffix(dst, ext)
	for i := 1; ; i++ {
		p := fmt.Sprintf("%s (%d)%s", stem, i, ext)
		if _, err := os.Lstat(p); errors.Is(err, os.ErrNotExist) {
			return p
		}
	}
}

func isCrossDevice(err error) bool {
	var le *os.LinkError
	return errors.As(err, &le) && errors.Is(le.Err, syscall.EXDEV)
}

// RemoveEmptyDirs removes dir and its empty parents, stopping at (and never
// removing) stop.
func RemoveEmptyDirs(dir, stop string) {
	dir, stop = filepath.Clean(dir), filepath.Clean(stop)
	for dir != stop && strings.HasPrefix(dir, stop+string(filepath.Separator)) {
		if err := os.Remove(dir); err != nil { // fails if not empty
			return
		}
		dir = filepath.Dir(dir)
	}
}
