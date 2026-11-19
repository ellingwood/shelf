package fileops

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, p, data string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestMoveCreatesDirs(t *testing.T) {
	d := t.TempDir()
	src, dst := filepath.Join(d, "in/a.mkv"), filepath.Join(d, "out/x/y/a.mkv")
	write(t, src, "video")
	if _, err := Place(src, dst, "move", "skip"); err != nil {
		t.Fatal(err)
	}
	if read(t, dst) != "video" {
		t.Error("content mismatch")
	}
	if _, err := os.Stat(src); !errors.Is(err, os.ErrNotExist) {
		t.Error("source still exists after move")
	}
}

func TestCopyFileVerifies(t *testing.T) {
	d := t.TempDir()
	src, dst := filepath.Join(d, "a.mkv"), filepath.Join(d, "b/a.mkv")
	write(t, src, "0123456789")
	info, _ := os.Stat(src)
	os.MkdirAll(filepath.Dir(dst), 0o755)
	if err := copyFile(src, dst, info); err != nil {
		t.Fatal(err)
	}
	if read(t, dst) != "0123456789" {
		t.Error("content mismatch")
	}
	if _, err := os.Stat(tempName(dst)); !errors.Is(err, os.ErrNotExist) {
		t.Error("temp file left behind")
	}
}

func TestConflicts(t *testing.T) {
	d := t.TempDir()
	src, dst := filepath.Join(d, "src.mkv"), filepath.Join(d, "dst.mkv")

	write(t, src, "new-bigger")
	write(t, dst, "old")
	if _, err := Place(src, dst, "copy", "skip"); !errors.Is(err, ErrSkipped) {
		t.Errorf("skip: got %v", err)
	}

	res, err := Place(src, dst, "copy", "suffix")
	if err != nil || res.Dst != filepath.Join(d, "dst (1).mkv") {
		t.Errorf("suffix: %v %q", err, res.Dst)
	}

	res, err = Place(src, dst, "copy", "keep-larger")
	if err != nil || !res.Overwrote || read(t, dst) != "new-bigger" {
		t.Errorf("keep-larger should replace smaller file: %v", err)
	}

	write(t, src, "x")
	if _, err := Place(src, dst, "copy", "keep-larger"); !errors.Is(err, ErrSkipped) {
		t.Errorf("keep-larger should skip when src smaller: %v", err)
	}

	if _, err := Place(src, dst, "hardlink", "overwrite"); err != nil {
		t.Fatal(err)
	}
	if read(t, dst) != "x" {
		t.Error("overwrite via hardlink failed")
	}
	// Same file now: noop.
	if res, err := Place(src, dst, "move", "skip"); err != nil || !res.Noop {
		t.Errorf("same file should be noop: %v %+v", err, res)
	}
}

func TestRemoveEmptyDirs(t *testing.T) {
	d := t.TempDir()
	deep := filepath.Join(d, "a/b/c")
	os.MkdirAll(deep, 0o755)
	write(t, filepath.Join(d, "a/keep.txt"), "x")
	RemoveEmptyDirs(deep, d)
	if _, err := os.Stat(filepath.Join(d, "a/b")); !errors.Is(err, os.ErrNotExist) {
		t.Error("empty dirs not removed")
	}
	if _, err := os.Stat(filepath.Join(d, "a")); err != nil {
		t.Error("non-empty dir removed")
	}
}
