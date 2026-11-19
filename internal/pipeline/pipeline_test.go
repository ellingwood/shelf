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
