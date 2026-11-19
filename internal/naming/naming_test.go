package naming

import (
	"testing"

	"shelf/internal/media"
)

func TestJellyfinPreset(t *testing.T) {
	r, err := New(Presets["jellyfin"])
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		m    media.Media
		want string
	}{
		{media.Media{Kind: media.Movie, Title: "The Matrix", Year: 1999, TMDBID: 603, Ext: ".mkv"},
			"Movies/The Matrix (1999) [tmdbid-603]/The Matrix (1999).mkv"},
		{media.Media{Kind: media.Movie, Title: "Mission: Impossible", Year: 1996, Ext: ".mp4"},
			"Movies/Mission - Impossible (1996)/Mission - Impossible (1996).mp4"},
		{media.Media{Kind: media.Movie, Title: "Heat", Year: 1995, Edition: "Director's Cut", Ext: ".mkv"},
			"Movies/Heat (1995)/Heat (1995) - Director's Cut.mkv"},
		{media.Media{Kind: media.Movie, Title: "Inception", Ext: ".mkv"},
			"Movies/Inception/Inception.mkv"},
		{media.Media{Kind: media.Episode, Show: "Breaking Bad", ShowYear: 2008, TMDBID: 1396, Season: 1, Episode: 2, EpisodeTitle: "Cat's in the Bag...", Ext: ".mkv"},
			"Shows/Breaking Bad (2008) [tmdbid-1396]/Season 01/Breaking Bad S01E02 - Cat's in the Bag.mkv"},
		{media.Media{Kind: media.Episode, Show: "The Office", Season: 2, Episode: 1, Episodes: []int{1, 2}, Ext: ".mkv"},
			"Shows/The Office/Season 02/The Office S02E01-E02.mkv"},
		{media.Media{Kind: media.Episode, Show: "Planet Earth", Season: 0, Episode: 3, Ext: ".mkv"},
			"Shows/Planet Earth/Season 00/Planet Earth S00E03.mkv"},
		{media.Media{Kind: media.Episode, Show: "AC/DC: Live", Season: 1, Episode: 1, EpisodeTitle: "What? Why?", Ext: ".mkv"},
			"Shows/AC-DC - Live/Season 01/AC-DC - Live S01E01 - What Why.mkv"},
	}
	for _, tt := range tests {
		got, err := r.Render(tt.m)
		if err != nil {
			t.Errorf("%+v: %v", tt.m, err)
			continue
		}
		if got != tt.want {
			t.Errorf("got  %q\nwant %q", got, tt.want)
		}
	}
}

func TestSafePathRejectsEscape(t *testing.T) {
	for _, p := range []string{"../etc/passwd", "a/../../b", ""} {
		if _, err := SafePath(p); err == nil {
			t.Errorf("SafePath(%q) should fail", p)
		}
	}
	if got, _ := SafePath("/abs/path.mkv"); got != "abs/path.mkv" {
		t.Errorf("absolute path not made relative: %q", got)
	}
}

func TestCustomTemplateFuncs(t *testing.T) {
	r, err := New(Templates{
		Movie:   `{{.Title | sortname | first 1 | upper}}/{{join " " (.Title | sortname) (printf "%d" .Year) .Resolution}}{{.Ext}}`,
		Episode: `{{.Show | lower}}/{{default "unknown" .EpisodeTitle}}{{.Ext}}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := r.Render(media.Media{Kind: media.Movie, Title: "The Matrix", Year: 1999, Ext: ".mkv"})
	if want := "M/Matrix, The 1999.mkv"; got != want {
		t.Errorf("got %q want %q", got, want)
	}
	got, _ = r.Render(media.Media{Kind: media.Episode, Show: "Lost", Ext: ".mkv"})
	if want := "lost/unknown.mkv"; got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestBadTemplate(t *testing.T) {
	if _, err := New(Templates{Movie: "{{.Nope", Episode: "x"}); err == nil {
		t.Error("expected parse error")
	}
	r, _ := New(Templates{Movie: "{{.Nope}}", Episode: "x"})
	if _, err := r.Render(media.Media{Kind: media.Movie, Title: "x"}); err == nil {
		t.Error("expected execution error for unknown field")
	}
}
