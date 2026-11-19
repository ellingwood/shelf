// Package cleanup lists and deletes what has accumulated in review folders:
// files that couldn't be matched, older versions set aside by an import,
// and duplicates moved aside by retrofit.
package cleanup

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"shelf/internal/fileops"
)

// Categories, by the review folder's subfolder.
const (
	Unmatched  = "unmatched"  // top level: imports that need a better name
	Replaced   = "replaced"   // replaced/: older versions swapped out by an import
	Duplicates = "duplicates" // duplicates/: copies set aside by retrofit
)

var Categories = []string{Unmatched, Replaced, Duplicates}

// Item is one file, or one folder such as "X.trickplay", in a review folder.
type Item struct {
	Path     string
	Rel      string // relative to the review folder
	Category string
	Size     int64
	Added    time.Time // when it was moved in (inode change time)
	IsDir    bool
}

type Options struct {
	OlderThan time.Duration // only items added at least this long ago
	Only      []string      // only these categories (default: all)
	Now       time.Time
}

// Scan lists the items in a review folder. A missing folder is empty.
func Scan(dir string, opt Options) ([]Item, error) {
	if opt.Now.IsZero() {
		opt.Now = time.Now()
	}
	var items []Item
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if p == dir {
			return nil
		}
		rel, _ := filepath.Rel(dir, p)
		cat := category(rel)
		if d.IsDir() && !strings.HasSuffix(d.Name(), ".trickplay") {
			return nil
		}
		if fileops.IsTemp(d.Name()) {
			return nil // an import in progress
		}
		it := Item{Path: p, Rel: rel, Category: cat, IsDir: d.IsDir()}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		it.Added = changeTime(info)
		if d.IsDir() {
			it.Size = dirSize(p)
		} else {
			it.Size = info.Size()
		}
		if keep(it, opt) {
			items = append(items, it)
		}
		if d.IsDir() {
			return filepath.SkipDir
		}
		return nil
	})
	sort.SliceStable(items, func(i, j int) bool {
		if ci, cj := catOrder(items[i].Category), catOrder(items[j].Category); ci != cj {
			return ci < cj
		}
		return items[i].Rel < items[j].Rel
	})
	return items, err
}

func category(rel string) string {
	first := strings.SplitN(rel, string(filepath.Separator), 2)
	if len(first) == 2 {
		switch first[0] {
		case Replaced, Duplicates:
			return first[0]
		}
	}
	return Unmatched
}

func catOrder(c string) int {
	for i, x := range Categories {
		if x == c {
			return i
		}
	}
	return len(Categories)
}

func keep(it Item, opt Options) bool {
	if len(opt.Only) > 0 {
		found := false
		for _, c := range opt.Only {
			found = found || c == it.Category
		}
		if !found {
			return false
		}
	}
	return opt.OlderThan == 0 || opt.Now.Sub(it.Added) >= opt.OlderThan
}

// changeTime is when the inode last changed, which a rename into the review
// folder updates; the modification time still reflects the original file.
func changeTime(fi fs.FileInfo) time.Time {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return time.Unix(st.Ctim.Sec, st.Ctim.Nsec)
	}
	return fi.ModTime()
}

func dirSize(dir string) int64 {
	var n int64
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if fi, err := d.Info(); err == nil {
				n += fi.Size()
			}
		}
		return nil
	})
	return n
}

// Delete removes the items and any folders left empty inside dir (never
// dir itself). It keeps going after failures and returns them joined.
func Delete(dir string, items []Item) (deleted int, freed int64, err error) {
	var errs []error
	for _, it := range items {
		var e error
		if it.IsDir {
			e = os.RemoveAll(it.Path)
		} else {
			e = os.Remove(it.Path)
		}
		if e != nil && !errors.Is(e, fs.ErrNotExist) {
			errs = append(errs, e)
			continue
		}
		deleted++
		freed += it.Size
		fileops.RemoveEmptyDirs(filepath.Dir(it.Path), dir)
	}
	return deleted, freed, errors.Join(errs...)
}

// EmptyDirs lists folders inside dir (not dir itself) that contain no
// files, deepest first, so they can be removed in order.
func EmptyDirs(dir string) []string {
	var dirs []string
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() && p != dir && !hasFiles(p) {
			dirs = append(dirs, p)
		}
		return nil
	})
	sort.Slice(dirs, func(i, j int) bool { return len(dirs[i]) > len(dirs[j]) })
	return dirs
}

func hasFiles(dir string) bool {
	found := false
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			found = true
			return filepath.SkipAll
		}
		return nil
	})
	return found
}

// RemoveDirs removes the given empty folders; non-empty ones are left.
func RemoveDirs(dirs []string) int {
	n := 0
	for _, d := range dirs {
		if os.Remove(d) == nil {
			n++
		}
	}
	return n
}

var headings = map[string][2]string{
	Unmatched:  {"Unmatched imports", "rename clearly and put back in the watch folder, or delete"},
	Replaced:   {"Replaced versions", "older copies swapped out by a newer import"},
	Duplicates: {"Duplicates", "copies set aside by shelf retrofit"},
}

// Print lists items grouped by category.
func Print(w io.Writer, dir string, items []Item, now time.Time) {
	fmt.Fprintf(w, "Review folder %s\n", dir)
	if len(items) == 0 {
		fmt.Fprintln(w, "  (no files waiting)")
		return
	}
	var total int64
	for _, cat := range Categories {
		var group []Item
		for _, it := range items {
			if it.Category == cat {
				group = append(group, it)
			}
		}
		if len(group) == 0 {
			continue
		}
		var size int64
		for _, it := range group {
			size += it.Size
		}
		total += size
		h := headings[cat]
		fmt.Fprintf(w, "\n%s (%d, %s): %s\n", h[0], len(group), HumanSize(size), h[1])
		for _, it := range group {
			rel := it.Rel
			if cat != Unmatched {
				rel = strings.TrimPrefix(rel, cat+string(filepath.Separator))
			}
			if it.IsDir {
				rel += "/"
			}
			fmt.Fprintf(w, "  %-9s %-8s  %s\n", HumanSize(it.Size), Ago(now.Sub(it.Added)), rel)
		}
	}
	fmt.Fprintf(w, "\nTotal: %d item(s), %s\n", len(items), HumanSize(total))
}

func HumanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// Ago renders a duration as "5m ago", "3h ago", "2d ago".
func Ago(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// ParseAge accepts Go durations plus a "d" (days) suffix: "7d", "36h".
func ParseAge(s string) (time.Duration, error) {
	if s == "" {
		return 0, nil
	}
	if strings.HasSuffix(s, "d") {
		var n float64
		if _, err := fmt.Sscanf(strings.TrimSuffix(s, "d"), "%g", &n); err != nil || n < 0 {
			return 0, fmt.Errorf("invalid age %q", s)
		}
		return time.Duration(n * float64(24*time.Hour)), nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("invalid age %q (use e.g. 7d or 12h)", s)
	}
	return d, nil
}
