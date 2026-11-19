package media

import (
	"path/filepath"
	"regexp"
	"strings"
)

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

// SubtitleSuffix derives Jellyfin's ".lang[.flags]" suffix from a subtitle
// filename, e.g. "Movie.2019.English.SDH.srt" → ".en.sdh".
func SubtitleSuffix(subPath, videoStem string) string {
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

// LangCode normalizes a language name or ISO 639-1/639-2 code ("English",
// "eng", "en") to the code shelf uses in filenames ("en"). It returns ""
// for unknown values.
func LangCode(s string) string {
	t := strings.ToLower(strings.TrimSpace(s))
	if l, ok := langNames[t]; ok {
		return l
	}
	if t == "pt-br" {
		return "pt-BR"
	}
	if langCodes[t] {
		return t
	}
	return ""
}

// SubtitleLang returns the language code of a subtitle file named after a
// video ("Movie.en.srt", "Movie.eng.forced.srt"), "" if none, and whether it
// is a forced (foreign-parts-only) track.
func SubtitleLang(subPath, videoStem string) (lang string, forced bool) {
	suf := SubtitleSuffix(subPath, videoStem)
	for i, part := range strings.Split(strings.TrimPrefix(suf, "."), ".") {
		switch {
		case part == "forced":
			forced = true
		case i == 0 && part != "sdh" && part != "default":
			lang = part
		}
	}
	return lang, forced
}
