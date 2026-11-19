// Package dupe detects video files with identical content. A browser
// download helper that keeps grabbing the same stream produces files named
// for different episodes but holding the same bytes; metadata looks right
// while every episode plays the same video.
//
// Files are compared by size first (a stat), and only same-size files are
// fingerprinted: SHA-256 over 1 MiB samples from the start, middle and end.
// That reads 3 MiB per file regardless of its size.
package dupe

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"shelf/internal/media"
)

const sample = 1 << 20

// Fingerprint returns a content fingerprint of the file.
func Fingerprint(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return "", err
	}
	size := fi.Size()
	h := sha256.New()
	buf := make([]byte, sample)
	for _, off := range []int64{0, size/2 - sample/2, size - sample} {
		if off < 0 {
			off = 0
		}
		n, err := f.ReadAt(buf, off)
		if err != nil && !errors.Is(err, io.EOF) {
			return "", err
		}
		h.Write(buf[:n])
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Cache memoizes fingerprints by path, size and modification time.
type Cache struct {
	mu sync.Mutex
	m  map[string]entry
}

type entry struct {
	size  int64
	mtime time.Time
	sum   string
}

func (c *Cache) get(path string, fi fs.FileInfo) (string, error) {
	c.mu.Lock()
	if c.m == nil {
		c.m = map[string]entry{}
	}
	e, ok := c.m[path]
	c.mu.Unlock()
	if ok && e.size == fi.Size() && e.mtime.Equal(fi.ModTime()) {
		return e.sum, nil
	}
	sum, err := Fingerprint(path)
	if err != nil {
		return "", err
	}
	c.mu.Lock()
	c.m[path] = entry{fi.Size(), fi.ModTime(), sum}
	c.mu.Unlock()
	return sum, nil
}

// Same reports whether two files have identical size and fingerprint.
func (c *Cache) Same(a, b string) (bool, error) {
	fa, err := os.Stat(a)
	if err != nil {
		return false, err
	}
	fb, err := os.Stat(b)
	if err != nil {
		return false, err
	}
	if fa.Size() != fb.Size() || os.SameFile(fa, fb) {
		return false, nil
	}
	sa, err := c.get(a, fa)
	if err != nil {
		return false, err
	}
	sb, err := c.get(b, fb)
	if err != nil {
		return false, err
	}
	return sa == sb, nil
}

// FindIn returns a video under dir with the same content as path, or "" if
// there is none. Paths in skip (and path itself) are not considered;
// hidden directories are not entered.
func (c *Cache) FindIn(path, dir string, skip ...string) (string, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	excluded := map[string]bool{filepath.Clean(path): true}
	for _, s := range skip {
		excluded[filepath.Clean(s)] = true
	}
	var found string
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if found != "" {
			return filepath.SkipAll
		}
		if d.IsDir() {
			if p != dir && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if excluded[p] || !media.IsVideo(filepath.Ext(p)) {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Size() != fi.Size() {
			return nil
		}
		if same, err := c.Same(path, p); err == nil && same {
			found = p
		}
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) {
		err = nil
	}
	return found, err
}

// Groups returns the sets of paths (two or more each) with identical
// content, in input order.
func (c *Cache) Groups(paths []string) [][]string {
	bySize := map[int64][]string{}
	var sizes []int64
	for _, p := range paths {
		fi, err := os.Stat(p)
		if err != nil {
			continue
		}
		if _, ok := bySize[fi.Size()]; !ok {
			sizes = append(sizes, fi.Size())
		}
		bySize[fi.Size()] = append(bySize[fi.Size()], p)
	}
	var out [][]string
	for _, size := range sizes {
		group := bySize[size]
		if len(group) < 2 {
			continue
		}
		bySum := map[string][]string{}
		var sums []string
		for _, p := range group {
			fi, err := os.Stat(p)
			if err != nil {
				continue
			}
			sum, err := c.get(p, fi)
			if err != nil {
				continue
			}
			if _, ok := bySum[sum]; !ok {
				sums = append(sums, sum)
			}
			bySum[sum] = append(bySum[sum], p)
		}
		for _, s := range sums {
			if len(bySum[s]) > 1 {
				out = append(out, bySum[s])
			}
		}
	}
	return out
}
