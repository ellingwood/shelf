package parse

import (
	"slices"
	"testing"

	"shelf/internal/media"
)

func TestName(t *testing.T) {
	tests := []struct {
		in   string
		want media.Media
	}{
		{"The.Matrix.1999.1080p.BluRay.x264-GROUP.mkv", media.Media{Kind: media.Movie, Title: "The Matrix", Year: 1999, Resolution: "1080p", Source: "BluRay", VideoCodec: "x264"}},
		{"Blade.Runner.2049.2017.2160p.UHD.BluRay.REMUX.HDR.HEVC.Atmos-EPSiLON.mkv", media.Media{Kind: media.Movie, Title: "Blade Runner 2049", Year: 2017, Resolution: "2160p", Source: "Remux", VideoCodec: "x265", HDR: "HDR10", AudioCodec: "Atmos"}},
		{"2001.A.Space.Odyssey.1968.1080p.mkv", media.Media{Kind: media.Movie, Title: "2001 A Space Odyssey", Year: 1968, Resolution: "1080p"}},
		{"1917 (2019) [1080p].mp4", media.Media{Kind: media.Movie, Title: "1917", Year: 2019, Resolution: "1080p"}},
		{"Charlotte's Web (2006).mkv", media.Media{Kind: media.Movie, Title: "Charlotte's Web", Year: 2006}},
		{"Back to the Future 1985 720p WEB-DL.mkv", media.Media{Kind: media.Movie, Title: "Back to the Future", Year: 1985, Resolution: "720p", Source: "WEB-DL"}},
		{"Heat.1995.Directors.Cut.1080p.BluRay.DTS.x264.mkv", media.Media{Kind: media.Movie, Title: "Heat", Year: 1995, Edition: "Director's Cut", Resolution: "1080p", Source: "BluRay", AudioCodec: "DTS", VideoCodec: "x264"}},
		{"[YTS.MX] Parasite (2019) [1080p].mp4", media.Media{Kind: media.Movie, Title: "Parasite", Year: 2019, Resolution: "1080p"}},
		{"the_shawshank_redemption_1994.avi", media.Media{Kind: media.Movie, Title: "The Shawshank Redemption", Year: 1994}},
		{"Inception.mkv", media.Media{Kind: media.Movie, Title: "Inception"}},

		{"Breaking.Bad.S01E02.Cats.in.the.Bag.720p.WEB-DL.x264.mkv", media.Media{Kind: media.Episode, Show: "Breaking Bad", Season: 1, Episode: 2, Episodes: []int{2}, EpisodeTitle: "Cats in the Bag", Resolution: "720p", Source: "WEB-DL", VideoCodec: "x264"}},
		{"Doctor.Who.2005.S03E10.Blink.1080p.mkv", media.Media{Kind: media.Episode, Show: "Doctor Who", ShowYear: 2005, Season: 3, Episode: 10, Episodes: []int{10}, EpisodeTitle: "Blink", Resolution: "1080p"}},
		{"The Office (US) - S02E01-E02 - The Dundies.mkv", media.Media{Kind: media.Episode, Show: "The Office US", Season: 2, Episode: 1, Episodes: []int{1, 2}, EpisodeTitle: "The Dundies"}},
		{"show.name.s01e01e02e03.mkv", media.Media{Kind: media.Episode, Show: "Show Name", Season: 1, Episode: 1, Episodes: []int{1, 2, 3}}},
		{"Seinfeld 4x11 The Contest.avi", media.Media{Kind: media.Episode, Show: "Seinfeld", Season: 4, Episode: 11, Episodes: []int{11}, EpisodeTitle: "The Contest"}},
		{"1883.S01E01.1080p.WEB.H264.mkv", media.Media{Kind: media.Episode, Show: "1883", Season: 1, Episode: 1, Episodes: []int{1}, Resolution: "1080p", Source: "WEBRip", VideoCodec: "x264"}},
		{"Show Season 2 Episode 5.mp4", media.Media{Kind: media.Episode, Show: "Show", Season: 2, Episode: 5, Episodes: []int{5}}},
		{"Planet.Earth.S00E03.Special.mkv", media.Media{Kind: media.Episode, Show: "Planet Earth", Season: 0, Episode: 3, Episodes: []int{3}, EpisodeTitle: "Special"}},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got := Name(tt.in)
			check(t, got, tt.want)
		})
	}
}

func TestPath(t *testing.T) {
	tests := []struct {
		path, root string
		want       media.Media
	}{
		{"/in/Breaking Bad/Season 1/S01E03.mkv", "/in", media.Media{Kind: media.Episode, Show: "Breaking Bad", Season: 1, Episode: 3, Episodes: []int{3}}},
		{"/in/The Wire (2002)/Season 02/Episode 4.mkv", "/in", media.Media{Kind: media.Episode, Show: "The Wire", ShowYear: 2002, Season: 2, Episode: 4, Episodes: []int{4}}},
		{"/in/Heat (1995)/heat.1080p.mkv", "/in", media.Media{Kind: media.Movie, Title: "Heat", Year: 1995, Resolution: "1080p"}},
		{"/in/Some.Movie.2010.1080p.BluRay/abc-somemovie-1080p.mkv", "/in", media.Media{Kind: media.Movie, Title: "Some Movie", Year: 2010, Resolution: "1080p", Source: "BluRay"}},
		// Never climb to or above the root.
		{"/in/S01E03.mkv", "/in", media.Media{Kind: media.Episode, Season: 1, Episode: 3, Episodes: []int{3}}},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			check(t, Path(tt.path, tt.root), tt.want)
		})
	}
}

func check(t *testing.T, got, want media.Media) {
	t.Helper()
	if got.Kind != want.Kind {
		t.Errorf("Kind = %v, want %v", got.Kind, want.Kind)
	}
	if want.Kind == media.Movie && got.Title != want.Title {
		t.Errorf("Title = %q, want %q", got.Title, want.Title)
	}
	str := func(name, g, w string) {
		if g != w {
			t.Errorf("%s = %q, want %q", name, g, w)
		}
	}
	num := func(name string, g, w int) {
		if g != w {
			t.Errorf("%s = %d, want %d", name, g, w)
		}
	}
	if want.Kind == media.Episode {
		str("Show", got.Show, want.Show)
		num("ShowYear", got.ShowYear, want.ShowYear)
		num("Season", got.Season, want.Season)
		num("Episode", got.Episode, want.Episode)
		if !slices.Equal(got.Episodes, want.Episodes) {
			t.Errorf("Episodes = %v, want %v", got.Episodes, want.Episodes)
		}
		str("EpisodeTitle", got.EpisodeTitle, want.EpisodeTitle)
	} else {
		num("Year", got.Year, want.Year)
		str("Edition", got.Edition, want.Edition)
	}
	str("Resolution", got.Resolution, want.Resolution)
	if want.Source != "" {
		str("Source", got.Source, want.Source)
	}
	if want.VideoCodec != "" {
		str("VideoCodec", got.VideoCodec, want.VideoCodec)
	}
	if want.AudioCodec != "" {
		str("AudioCodec", got.AudioCodec, want.AudioCodec)
	}
	if want.HDR != "" {
		str("HDR", got.HDR, want.HDR)
	}
}
