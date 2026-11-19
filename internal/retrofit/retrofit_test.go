package retrofit

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"shelf/internal/config"
	"shelf/internal/history"
	"shelf/internal/pipeline"
)

func setup(t *testing.T, files map[string]int) (string, *config.Config, *pipeline.Pipeline) {
	t.Helper()
	d := t.TempDir()
	lib := filepath.Join(d, "lib")
	for rel, size := range files {
		p := filepath.Join(lib, rel)
		if strings.HasSuffix(rel, "/") {
			os.MkdirAll(p, 0o755)
			continue
		}
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, make([]byte, size), 0o644)
	}
	cfg, err := config.Parse([]byte(fmt.Sprintf(`
probe: never
history_file: %s/history.jsonl
rules: [{ watch: %s/in, output: %s }]
`, d, d, lib)))
	if err != nil {
		t.Fatal(err)
	}
	p, err := pipeline.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, false, true)
	if err != nil {
		t.Fatal(err)
	}
	return lib, cfg, p
}

func tree(t *testing.T, root string) []string {
	var out []string
	filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			r, _ := filepath.Rel(root, p)
			out = append(out, r)
		}
		return nil
	})
	sort.Strings(out)
	return out
}

func TestMergeShowFoldersAndUndo(t *testing.T) {
	lib, cfg, p := setup(t, map[string]int{
		"Shows/Show (2020)/Season 01/Show S01E01.mkv":            10,
		"Shows/Show (2020)/Season 01/Show S01E01.en.srt":         1,
		"Shows/Show (2020)/Season 01/Show S01E01-E02.mkv":        10, // must keep its own sidecar
		"Shows/Show (2020)/Season 01/Show S01E01-E02.en.srt":     1,
		"Shows/Show (2020)/Season 01/Show S01E03.trickplay/x":    1,
		"Shows/Show (2020)/Season 01/Show S01E03.mkv":            10,
		"Shows/Show (2020)/poster.jpg":                           1,
		"Shows/Show (2020) [tmdbid-7]/Season 01/Show S01E04.mkv": 10,
		"Shows/Show (2020) [tmdbid-7]/poster.jpg":                1, // collides: old one stays
		"Movies/Loose Movie 2001 1080p.mkv":                      10,
		"Movies/Kept (1999)/Kept (1999).mkv":                     10,
		"Movies/Kept (1999)/extras/Making Of.mkv":                10, // extras untouched
	})
	before := tree(t, lib)
	rule := &cfg.Rules[0]
	plan, err := Build(context.Background(), p, rule, Options{Duplicates: "skip"})
	if err != nil {
		t.Fatal(err)
	}
	if err := plan.Apply(p, rule, io.Discard); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(tree(t, lib), "\n")
	for _, want := range []string{
		"Shows/Show (2020) [tmdbid-7]/Season 01/Show S01E01.mkv",
		"Shows/Show (2020) [tmdbid-7]/Season 01/Show S01E01.en.srt",
		"Shows/Show (2020) [tmdbid-7]/Season 01/Show S01E01-E02.mkv",
		"Shows/Show (2020) [tmdbid-7]/Season 01/Show S01E01-E02.en.srt",
		"Shows/Show (2020) [tmdbid-7]/Season 01/Show S01E03.trickplay/x",
		"Shows/Show (2020) [tmdbid-7]/poster.jpg",
		"Shows/Show (2020)/poster.jpg", // name taken in the new folder: left, not overwritten
		"Movies/Loose Movie (2001)/Loose Movie (2001).mkv",
		"Movies/Kept (1999)/extras/Making Of.mkv",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s\n--- tree:\n%s", want, got)
		}
	}

	// Undo everything and compare.
	h, _ := history.Open(cfg.HistoryFile)
	entries, _ := h.Active()
	for i := len(entries) - 1; i >= 0; i-- {
		if err := h.Undo(entries[i]); err != nil {
			t.Fatalf("undo %s: %v", entries[i].Src, err)
		}
	}
	if after := tree(t, lib); strings.Join(after, "\n") != strings.Join(before, "\n") {
		t.Errorf("undo did not restore the library:\nbefore %v\nafter  %v", before, after)
	}
}

func TestDuplicates(t *testing.T) {
	files := map[string]int{
		"Movies/Heat (1995)/Heat (1995).mkv":              5,
		"Movies/Heat (1995)/Heat (1995)-trailer.mp4":      1,
		"Movies/Heat (1995) [tmdbid-949]/Heat (1995).mkv": 9,
	}
	// skip: nothing moves, both reported.
	lib, cfg, p := setup(t, files)
	plan, err := Build(context.Background(), p, &cfg.Rules[0], Options{Duplicates: "skip"})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Ops) != 0 || len(plan.Conflicts) != 2 {
		t.Errorf("skip: ops=%d conflicts=%d", len(plan.Ops), len(plan.Conflicts))
	}

	// keep-larger: smaller copy set aside, trailer follows the kept copy.
	lib, cfg, p = setup(t, files)
	rule := &cfg.Rules[0]
	plan, _ = Build(context.Background(), p, rule, Options{Duplicates: "keep-larger"})
	if err := plan.Apply(p, rule, io.Discard); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(tree(t, lib), "\n")
	for _, want := range []string{
		"Movies/Heat (1995) [tmdbid-949]/Heat (1995).mkv",
		"Movies/Heat (1995) [tmdbid-949]/Heat (1995)-trailer.mp4",
		".shelf-review/duplicates/Movies/Heat (1995)/Heat (1995).mkv",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s\n--- tree:\n%s", want, got)
		}
	}
	if _, err := os.Stat(filepath.Join(lib, "Movies/Heat (1995)")); !os.IsNotExist(err) {
		t.Error("emptied folder not removed")
	}
}

func TestOtherContainerIsDuplicate(t *testing.T) {
	files := map[string]int{
		"Movies/Heat (1995)/Heat (1995).mp4":    5,
		"Movies/Heat (1995)/Heat (1995).mkv":    9,
		"Movies/Heat (1995)/Heat (1995).en.srt": 1,
	}
	_, cfg, p := setup(t, files)
	plan, _ := Build(context.Background(), p, &cfg.Rules[0], Options{Duplicates: "skip"})
	if len(plan.Ops) != 0 || len(plan.Conflicts) != 2 {
		t.Errorf("skip: ops=%v conflicts=%v", plan.Ops, plan.Conflicts)
	}

	lib, cfg, p := setup(t, files)
	rule := &cfg.Rules[0]
	plan, _ = Build(context.Background(), p, rule, Options{Duplicates: "keep-larger"})
	if err := plan.Apply(p, rule, io.Discard); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(tree(t, lib), "\n")
	want := strings.Join([]string{
		".shelf-review/duplicates/Movies/Heat (1995)/Heat (1995).mp4",
		"Movies/Heat (1995)/Heat (1995).en.srt", // stays with the kept copy
		"Movies/Heat (1995)/Heat (1995).mkv",
	}, "\n")
	if got != want {
		t.Errorf("tree:\n%s\nwant:\n%s", got, want)
	}
}

func TestPathsMustBeInsideLibrary(t *testing.T) {
	_, cfg, p := setup(t, map[string]int{"Movies/A (2000)/A (2000).mkv": 1})
	if _, err := Build(context.Background(), p, &cfg.Rules[0], Options{Paths: []string{"/etc"}}); err == nil {
		t.Error("expected error for path outside library")
	}
}
