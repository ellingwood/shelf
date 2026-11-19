package health

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
	"shelf/internal/pipeline"
)

func TestCheck(t *testing.T) {
	d := t.TempDir()
	lib := filepath.Join(d, "lib")
	files := map[string]byte{
		"Shows/X (2020)/Season 01/X S01E01.mkv":   1,
		"Shows/X (2020)/Season 01/X S01E02.mkv":   1, // same content as E01
		"Shows/X (2020)/Season 01/X S01E05.mkv":   2, // E03, E04 missing
		"Shows/X (2020)/Season 02/X S02E01.mkv":   3,
		"Movies/Loose 2001 1080p.mkv":             4, // misnamed
		"Movies/A (2000)/.A (2000).mkv.shelf-tmp": 5,
		"metadata/People/Someone/":                0, // Jellyfin's, not ours
		"Movies/Empty (1999)/":                    0,
	}
	for rel, seed := range files {
		p := filepath.Join(lib, rel)
		if strings.HasSuffix(rel, "/") {
			os.MkdirAll(p, 0o755)
			continue
		}
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, bytes.Repeat([]byte{seed}, 2048), 0o644)
	}
	cfg, err := config.Parse([]byte(fmt.Sprintf("probe: never\nhistory_file: %s/h.jsonl\nrules: [{ watch: %s/in, output: %s }]\n", d, d, lib)))
	if err != nil {
		t.Fatal(err)
	}
	p, err := pipeline.New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, true, true)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := Check(context.Background(), p, &cfg.Rules[0], Options{})
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	rep.Print(&out)
	report := out.String()

	if rep.Videos != 5 {
		t.Errorf("videos = %d, want 5", rep.Videos)
	}
	if len(rep.Identical) != 1 || len(rep.Identical[0]) != 2 {
		t.Errorf("identical = %v", rep.Identical)
	}
	if len(rep.Gaps) != 1 || rep.Gaps[0].Season != 1 || fmt.Sprint(rep.Gaps[0].Missing) != "[3 4]" {
		t.Errorf("gaps = %+v", rep.Gaps)
	}
	if len(rep.Misnamed) != 1 || !strings.Contains(rep.Misnamed[0].Dst, "Loose (2001)") {
		t.Errorf("misnamed = %+v", rep.Misnamed)
	}
	if len(rep.Leftovers) != 2 {
		t.Errorf("leftovers = %+v", rep.Leftovers)
	}
	for _, want := range []string{"E03-E04", "shelf-tmp", "Movies/Empty (1999)"} {
		if !strings.Contains(report, want) {
			t.Errorf("report lacks %q:\n%s", want, report)
		}
	}
	if strings.Contains(report, "metadata") {
		t.Errorf("report includes Jellyfin metadata:\n%s", report)
	}
}

func TestEpisodeList(t *testing.T) {
	if got := episodeList([]int{1, 2, 3, 7, 9, 10}); got != "E01-E03, E07, E09-E10" {
		t.Errorf("got %q", got)
	}
}
