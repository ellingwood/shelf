package pipeline

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"shelf/internal/config"
)

func TestDuplicateContentGoesToReview(t *testing.T) {
	d := t.TempDir()
	lib := filepath.Join(d, "lib")
	in := filepath.Join(d, "in")
	put := func(path string, seed byte) string {
		os.MkdirAll(filepath.Dir(path), 0o755)
		os.WriteFile(path, bytes.Repeat([]byte{seed}, 4096), 0o644)
		return path
	}
	put(filepath.Join(lib, "Shows/Show/Season 01/Show S01E02.mkv"), 1)

	newPipeline := func(extra string) (*Pipeline, *config.Rule) {
		cfg, err := config.Parse([]byte(fmt.Sprintf(`
probe: never
history_file: %s/history.jsonl
rules: [{ watch: %s, output: %s %s }]
`, d, in, lib, extra)))
		if err != nil {
			t.Fatal(err)
		}
		p, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, true, true)
		if err != nil {
			t.Fatal(err)
		}
		return p, &cfg.Rules[0]
	}
	p, rule := newPipeline("")

	same := put(filepath.Join(in, "S01E03 - Show.mkv"), 1)
	pl, err := p.MakePlan(context.Background(), rule, same, in)
	if err != nil {
		t.Fatal(err)
	}
	if !pl.Review || !strings.Contains(pl.Reason, "Show S01E02.mkv") {
		t.Errorf("identical file: review=%v reason=%q", pl.Review, pl.Reason)
	}

	// Same content as the file it would replace: not a duplicate of another episode.
	again := put(filepath.Join(in, "S01E02 - Show.mkv"), 1)
	if pl, _ := p.MakePlan(context.Background(), rule, again, in); pl.Review {
		t.Errorf("re-import of the same episode flagged: %s", pl.Reason)
	}

	different := put(filepath.Join(in, "S01E04 - Show.mkv"), 2)
	if pl, _ := p.MakePlan(context.Background(), rule, different, in); pl.Review {
		t.Errorf("different file flagged: %s", pl.Reason)
	}

	p, rule = newPipeline(", check_duplicates: false")
	if pl, _ := p.MakePlan(context.Background(), rule, same, in); pl.Review {
		t.Errorf("check disabled but flagged: %s", pl.Reason)
	}
}

func TestOtherContainerVersions(t *testing.T) {
	type setup struct {
		conflict string
		newSize  int
	}
	run := func(t *testing.T, s setup) (lib, in string, p *Pipeline) {
		d := t.TempDir()
		lib, in = filepath.Join(d, "lib"), filepath.Join(d, "in")
		put := func(path string, size int, seed byte) {
			os.MkdirAll(filepath.Dir(path), 0o755)
			os.WriteFile(path, bytes.Repeat([]byte{seed}, size), 0o644)
		}
		movie := filepath.Join(lib, "Movies/Heat (1995)")
		put(filepath.Join(movie, "Heat (1995).mp4"), 1000, 1)
		put(filepath.Join(movie, "Heat (1995).en.srt"), 10, 1)
		put(filepath.Join(movie, "Heat (1995).trickplay/320 - 10x10/0.jpg"), 10, 1)
		put(filepath.Join(movie, "Heat (1995)-trailer.mp4"), 10, 3) // extra: stays
		put(filepath.Join(in, "Heat.1995.1080p.mkv"), s.newSize, 2)

		cfg, err := config.Parse([]byte(fmt.Sprintf(`
probe: never
conflict: %s
history_file: %s/history.jsonl
rules: [{ watch: %s, output: %s }]
`, s.conflict, d, in, lib)))
		if err != nil {
			t.Fatal(err)
		}
		p, err = New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, false, true)
		if err != nil {
			t.Fatal(err)
		}
		if err := p.Process(context.Background(), &cfg.Rules[0], filepath.Join(in, "Heat.1995.1080p.mkv"), in); err != nil {
			t.Fatal(err)
		}
		return lib, in, p
	}
	exists := func(p string) bool { _, err := os.Stat(p); return err == nil }

	t.Run("overwrite sets the old version aside and undo restores it", func(t *testing.T) {
		lib, in, p := run(t, setup{"overwrite", 500})
		for _, want := range []string{
			"Movies/Heat (1995)/Heat (1995).mkv",
			"Movies/Heat (1995)/Heat (1995)-trailer.mp4",
			".shelf-review/replaced/Movies/Heat (1995)/Heat (1995).mp4",
			".shelf-review/replaced/Movies/Heat (1995)/Heat (1995).en.srt",
			".shelf-review/replaced/Movies/Heat (1995)/Heat (1995).trickplay/320 - 10x10/0.jpg",
		} {
			if !exists(filepath.Join(lib, want)) {
				t.Errorf("missing %s", want)
			}
		}
		if exists(filepath.Join(lib, "Movies/Heat (1995)/Heat (1995).mp4")) {
			t.Error("old version still in the library")
		}
		entries, _ := p.Hist.Active()
		for i := len(entries) - 1; i >= 0; i-- {
			if err := p.Hist.Undo(entries[i]); err != nil {
				t.Fatalf("undo %s: %v", entries[i].Src, err)
			}
		}
		for _, want := range []string{"Movies/Heat (1995)/Heat (1995).mp4", "Movies/Heat (1995)/Heat (1995).en.srt", "Movies/Heat (1995)/Heat (1995).trickplay/320 - 10x10/0.jpg"} {
			if !exists(filepath.Join(lib, want)) {
				t.Errorf("undo did not restore %s", want)
			}
		}
		if !exists(filepath.Join(in, "Heat.1995.1080p.mkv")) {
			t.Error("undo did not return the new file")
		}
	})

	for _, tc := range []setup{{"skip", 5000}, {"keep-larger", 500}} {
		t.Run(tc.conflict+" keeps the existing version", func(t *testing.T) {
			lib, in, _ := run(t, tc)
			if !exists(filepath.Join(in, "Heat.1995.1080p.mkv")) || exists(filepath.Join(lib, "Movies/Heat (1995)/Heat (1995).mkv")) {
				t.Error("new file was placed")
			}
			if !exists(filepath.Join(lib, "Movies/Heat (1995)/Heat (1995).mp4")) {
				t.Error("existing file moved")
			}
		})
	}

	t.Run("keep-larger replaces a smaller version", func(t *testing.T) {
		lib, _, _ := run(t, setup{"keep-larger", 5000})
		if !exists(filepath.Join(lib, "Movies/Heat (1995)/Heat (1995).mkv")) || exists(filepath.Join(lib, "Movies/Heat (1995)/Heat (1995).mp4")) {
			t.Error("larger file did not replace the smaller one")
		}
	})

	t.Run("suffix keeps both", func(t *testing.T) {
		lib, _, _ := run(t, setup{"suffix", 500})
		if !exists(filepath.Join(lib, "Movies/Heat (1995)/Heat (1995).mkv")) || !exists(filepath.Join(lib, "Movies/Heat (1995)/Heat (1995).mp4")) {
			t.Error("expected both versions")
		}
	})
}
