// Package media defines the metadata model shared by the parser, matcher,
// prober and naming engine. Its exported fields are what templates see.
package media

import "strings"

// Kind classifies a media file.
type Kind int

const (
	Unknown Kind = iota
	Movie
	Episode
)

func (k Kind) String() string {
	switch k {
	case Movie:
		return "movie"
	case Episode:
		return "episode"
	default:
		return "unknown"
	}
}

// Media is everything known about a file. Zero values mean "unknown", so
// templates can use {{with .Field}} to make segments optional.
type Media struct {
	Kind Kind

	// Movie fields (Title/Year are also set for episodes: the show name/year).
	Title   string
	Year    int
	Edition string // e.g. "Director's Cut", "Extended"

	// Episode fields.
	Show         string
	ShowYear     int
	Season       int
	Episode      int   // first episode number
	Episodes     []int // all episode numbers for multi-episode files
	EpisodeTitle string

	// Technical fields.
	Resolution string // 2160p, 1080p, 720p, 480p
	VideoCodec string // x264, x265, AV1, ...
	AudioCodec string // AAC, DTS, TrueHD, ...
	Source     string // BluRay, WEB-DL, HDTV, ...
	HDR        string // HDR10, HDR10+, DV, HLG

	// Database identifiers (filled by TMDB matching).
	TMDBID int

	// File fields.
	Ext      string // including leading dot, lowercased
	Original string // original base filename
	Path     string // original full path
}

// LastEpisode returns the final episode number of a (multi-)episode file.
func (m *Media) LastEpisode() int {
	if len(m.Episodes) == 0 {
		return m.Episode
	}
	return m.Episodes[len(m.Episodes)-1]
}

// IsTV reports whether the media is an episode.
func (m *Media) IsTV() bool { return m.Kind == Episode }

var (
	VideoExts    = []string{".mkv", ".mp4", ".m4v", ".avi", ".mov", ".wmv", ".ts", ".m2ts", ".webm", ".mpg", ".mpeg", ".flv", ".ogv"}
	SubtitleExts = []string{".srt", ".ass", ".ssa", ".sub", ".idx", ".vtt", ".sup", ".smi"}
)

func hasExt(list []string, ext string) bool {
	ext = strings.ToLower(ext)
	for _, e := range list {
		if e == ext {
			return true
		}
	}
	return false
}

// IsVideo reports whether ext (with dot) is a known video extension.
func IsVideo(ext string) bool { return hasExt(VideoExts, ext) }

// IsSubtitle reports whether ext (with dot) is a known subtitle extension.
func IsSubtitle(ext string) bool { return hasExt(SubtitleExts, ext) }
