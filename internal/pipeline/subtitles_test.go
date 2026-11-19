package pipeline

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

func TestSubtitleSuffix(t *testing.T) {
	stem := "Movie.2019.1080p"
	tests := map[string]string{
		"Movie.2019.1080p.srt":            "",
		"Movie.2019.1080p.en.srt":         ".en",
		"Movie.2019.1080p.English.srt":    ".en",
		"Movie.2019.1080p.eng.forced.srt": ".en.forced",
		"2_English.srt":                   ".en",
		"3_English.SDH.srt":               ".en.sdh",
		"Movie.2019.1080p.spa.srt":        ".es",
		"Movie.2019.1080p.hi.srt":         ".sdh", // "hi" means hearing impaired, not Hindi
		"Movie.2019.1080p.Hindi.srt":      ".hi",
		"French (Forced).ass":             ".fr.forced",
	}
	for in, want := range tests {
		if got := subtitleSuffix(in, stem); got != want {
			t.Errorf("subtitleSuffix(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFindSubtitles(t *testing.T) {
	d := t.TempDir()
	touch := func(rel string) {
		p := filepath.Join(d, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte("x"), 0o644)
	}
	// A season folder: subs must match their own episode only.
	touch("S01E01.mkv")
	touch("S01E02.mkv")
	touch("S01E01.en.srt")
	touch("S01E02.en.srt")
	touch("Subs/S01E01/2_English.srt")
	touch("Subs/S01E02/2_English.srt")

	got := findSubtitles(filepath.Join(d, "S01E01.mkv"))
	for i := range got {
		got[i], _ = filepath.Rel(d, got[i])
	}
	sort.Strings(got)
	want := []string{"S01E01.en.srt", "Subs/S01E01/2_English.srt"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("got %v, want %v", got, want)
	}
}
