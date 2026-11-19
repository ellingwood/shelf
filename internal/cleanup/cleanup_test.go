package cleanup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestScanPrintDelete(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".shelf-review")
	files := map[string]int{
		"Weird.Name.mkv": 100,
		"Weird.Name.srt": 1,
		"replaced/Shows/X/Season 01/X S01E01.mp4":         200,
		"replaced/Shows/X/Season 01/X S01E01.en.srt":      2,
		"replaced/Shows/X/Season 01/X S01E01.trickplay/a": 5,
		"replaced/Shows/X/Season 01/X S01E01.trickplay/b": 5,
		"duplicates/Movies/H (1995)/H (1995).mkv":         300,
		"replaced/Movies/.H (1995).mkv.shelf-tmp":         9, // in progress: ignored
	}
	for rel, n := range files {
		p := filepath.Join(dir, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, make([]byte, n), 0o644)
	}
	os.MkdirAll(filepath.Join(dir, "replaced/Movies/Gone (2000)"), 0o755) // empty leftover

	items, err := Scan(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, it := range items {
		got = append(got, it.Category+":"+it.Rel)
	}
	want := []string{
		"unmatched:Weird.Name.mkv",
		"unmatched:Weird.Name.srt",
		"replaced:replaced/Shows/X/Season 01/X S01E01.en.srt",
		"replaced:replaced/Shows/X/Season 01/X S01E01.mp4",
		"replaced:replaced/Shows/X/Season 01/X S01E01.trickplay",
		"duplicates:duplicates/Movies/H (1995)/H (1995).mkv",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("items:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if items[4].Size != 10 || !items[4].IsDir {
		t.Errorf("trickplay item = %+v", items[4])
	}

	var out strings.Builder
	Print(&out, dir, items, time.Now())
	for _, s := range []string{"Unmatched imports", "Replaced versions (3,", "Duplicates (1, 300 B): copies set aside", "Shows/X/Season 01/X S01E01.trickplay/", "Total: 6 item(s)"} {
		if !strings.Contains(out.String(), s) {
			t.Errorf("output lacks %q:\n%s", s, out.String())
		}
	}

	// Filters.
	if only, _ := Scan(dir, Options{Only: []string{Replaced}}); len(only) != 3 {
		t.Errorf("-only replaced: %d items", len(only))
	}
	if old, _ := Scan(dir, Options{OlderThan: time.Hour}); len(old) != 0 {
		t.Errorf("-older-than 1h matched fresh items: %d", len(old))
	}
	if old, _ := Scan(dir, Options{OlderThan: time.Hour, Now: time.Now().Add(2 * time.Hour)}); len(old) != 6 {
		t.Errorf("-older-than 1h two hours later: %d", len(old))
	}

	// Delete only replaced, then prune empty folders.
	repl, _ := Scan(dir, Options{Only: []string{Replaced}})
	n, freed, err := Delete(dir, repl)
	if err != nil || n != 3 || freed != 212 {
		t.Errorf("delete: n=%d freed=%d err=%v", n, freed, err)
	}
	RemoveDirs(EmptyDirs(dir))
	if _, err := os.Stat(filepath.Join(dir, "replaced/Movies/Gone (2000)")); !os.IsNotExist(err) {
		t.Error("empty folder not removed")
	}
	if _, err := os.Stat(filepath.Join(dir, "replaced/Movies/.H (1995).mkv.shelf-tmp")); err != nil {
		t.Error("in-progress temp file was touched")
	}
	if left, _ := Scan(dir, Options{}); len(left) != 3 {
		t.Errorf("after delete: %d items left, want 3", len(left))
	}
	if _, err := os.Stat(dir); err != nil {
		t.Error("review folder itself removed")
	}
}

func TestParseAge(t *testing.T) {
	for in, want := range map[string]time.Duration{"7d": 7 * 24 * time.Hour, "12h": 12 * time.Hour, "1.5d": 36 * time.Hour, "": 0} {
		if got, err := ParseAge(in); err != nil || got != want {
			t.Errorf("ParseAge(%q) = %v, %v", in, got, err)
		}
	}
	if _, err := ParseAge("soon"); err == nil {
		t.Error("expected error")
	}
}
