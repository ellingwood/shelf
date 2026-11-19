package pipeline

import (
	"os"
	"path/filepath"
	"strings"

	"shelf/internal/media"
)

// findSubtitles locates sidecar subtitles for a video:
//   - same directory, filename starting with the video's stem
//   - Subs/ or Subtitles/ subdirectories (including Subs/<stem>/)
//   - any subtitle in those places when the video is alone in its folder
func findSubtitles(video string) []string {
	dir := filepath.Dir(video)
	stem := strings.ToLower(strings.TrimSuffix(filepath.Base(video), filepath.Ext(video)))
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	videos := 0
	for _, e := range entries {
		if !e.IsDir() && media.IsVideo(filepath.Ext(e.Name())) && !strings.Contains(strings.ToLower(e.Name()), "sample") {
			videos++
		}
	}
	alone := videos == 1

	var out []string
	pick := func(d string, all bool) {
		es, err := os.ReadDir(d)
		if err != nil {
			return
		}
		for _, e := range es {
			if e.IsDir() || !media.IsSubtitle(filepath.Ext(e.Name())) {
				continue
			}
			if all || strings.HasPrefix(strings.ToLower(e.Name()), stem) {
				out = append(out, filepath.Join(d, e.Name()))
			}
		}
	}
	pick(dir, alone)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		switch strings.ToLower(e.Name()) {
		case "subs", "subtitles", "sub", "subtitle":
			sd := filepath.Join(dir, e.Name())
			if fi, err := os.Stat(filepath.Join(sd, filepath.Base(strings.TrimSuffix(video, filepath.Ext(video))))); err == nil && fi.IsDir() {
				pick(filepath.Join(sd, fi.Name()), true)
			} else {
				pick(sd, alone)
			}
		}
	}
	return dedupe(out)
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	out := in[:0]
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// subtitleSuffix derives Jellyfin's ".lang[.flags]" suffix from a subtitle
// filename, e.g. "Movie.2019.English.SDH.srt" → ".en.sdh".
var subtitleSuffix = media.SubtitleSuffix
