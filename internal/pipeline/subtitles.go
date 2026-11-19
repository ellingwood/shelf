package pipeline

import (
	"os"
	"path/filepath"
	"regexp"
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

var reTokens = regexp.MustCompile(`[._\-\s\[\]()]+`)

var langNames = map[string]string{
	"english": "en", "eng": "en", "spanish": "es", "spa": "es", "español": "es", "espanol": "es",
	"french": "fr", "fre": "fr", "fra": "fr", "german": "de", "ger": "de", "deu": "de",
	"italian": "it", "ita": "it", "portuguese": "pt", "por": "pt", "brazilian": "pt-BR", "ptbr": "pt-BR",
	"dutch": "nl", "nld": "nl", "dut": "nl", "japanese": "ja", "jpn": "ja", "korean": "ko", "kor": "ko",
	"chinese": "zh", "chi": "zh", "zho": "zh", "russian": "ru", "rus": "ru", "arabic": "ar", "ara": "ar",
	"hindi": "hi", "hin": "hi", "swedish": "sv", "swe": "sv", "norwegian": "no", "nor": "no",
	"danish": "da", "dan": "da", "finnish": "fi", "fin": "fi", "polish": "pl", "pol": "pl",
	"turkish": "tr", "tur": "tr", "greek": "el", "gre": "el", "ell": "el", "hebrew": "he", "heb": "he",
	"czech": "cs", "cze": "cs", "ces": "cs", "hungarian": "hu", "hun": "hu", "romanian": "ro", "rum": "ro", "ron": "ro",
	"thai": "th", "tha": "th", "vietnamese": "vi", "vie": "vi", "indonesian": "id", "ind": "id",
	"ukrainian": "uk", "ukr": "uk",
}

var langCodes = func() map[string]bool {
	m := map[string]bool{}
	for _, v := range langNames {
		m[strings.ToLower(v)] = true
	}
	return m
}()

// subtitleSuffix derives Jellyfin's ".lang[.flags]" suffix from a subtitle
// filename, e.g. "Movie.2019.English.SDH.srt" → ".en.sdh".
func subtitleSuffix(subPath, videoStem string) string {
	name := strings.TrimSuffix(filepath.Base(subPath), filepath.Ext(subPath))
	if strings.HasPrefix(strings.ToLower(name), strings.ToLower(videoStem)) {
		name = name[len(videoStem):]
	}
	var lang string
	var flags []string
	for _, tok := range reTokens.Split(strings.ToLower(name), -1) {
		switch tok {
		case "":
			continue
		case "forced", "foreign":
			flags = appendOnce(flags, "forced")
		case "sdh", "hi", "cc":
			flags = appendOnce(flags, "sdh")
		case "default":
			flags = appendOnce(flags, "default")
		}
		if lang != "" {
			continue
		}
		if l, ok := langNames[tok]; ok {
			lang = l
		} else if len(tok) == 2 && langCodes[tok] && tok != "hi" {
			lang = tok
		} else if tok == "pt-br" {
			lang = "pt-BR"
		}
	}
	var b strings.Builder
	if lang != "" {
		b.WriteString("." + lang)
	}
	for _, f := range flags {
		b.WriteString("." + f)
	}
	return b.String()
}

func appendOnce(s []string, v string) []string {
	for _, x := range s {
		if x == v {
			return s
		}
	}
	return append(s, v)
}
