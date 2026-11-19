package subs

import (
	"math"
	"regexp"
	"sort"
	"strings"
)

// Prefs steer the choice between candidates.
type Prefs struct {
	HearingImpaired string // avoid | prefer | exclude | include
	Source          string // the video's source (WEB-DL, BluRay...), "" if unknown
}

var (
	reWebRelease    = regexp.MustCompile(`(?i)(^|[^a-z])(web[ ._-]?(dl|rip)?|amzn|nf|dsnp|hmax|max|atvp|hulu|pcok|pmtp|itunes|stan)([^a-z]|$)`)
	reBluRayRelease = regexp.MustCompile(`(?i)blu-?ray|bdrip|brrip|bdremux|(^|[^a-z])bd([^a-z]|$)`)
	reTVRelease     = regexp.MustCompile(`(?i)hdtv|pdtv|dsr`)
	reDVDRelease    = regexp.MustCompile(`(?i)dvd`)
)

func sourceOf(s string) string {
	switch {
	case reWebRelease.MatchString(s):
		return "web"
	case reBluRayRelease.MatchString(s):
		return "bluray"
	case reTVRelease.MatchString(s):
		return "tv"
	case reDVDRelease.MatchString(s):
		return "dvd"
	}
	return ""
}

// Score ranks a candidate; higher is better. Downloads dominate (ratings
// are sparse: most subtitles have no votes), with bonuses for a hash match
// (timed for this exact file), a release from the same kind of source
// (timing and cuts match more often), trusted uploaders and well-rated
// subtitles, and the hearing-impaired preference.
func Score(s Subtitle, p Prefs) float64 {
	sc := math.Log10(float64(s.Downloads) + 1) // 0 … ~6
	if s.HashMatch {
		sc += 100
	}
	want := sourceOf(p.Source)
	if want == "" {
		want = "web" // streaming captures are the common case
	}
	if src := sourceOf(s.Release); src == want {
		sc += 1
	} else if src != "" {
		sc -= 0.5
	}
	if s.Votes >= 3 {
		sc += (s.Rating - 5) / 5 // ratings are 0–10: -1 … +1
	}
	if s.Trusted {
		sc += 0.5
	}
	switch p.HearingImpaired {
	case "avoid":
		if s.HI {
			sc -= 1.5
		}
	case "prefer":
		if s.HI {
			sc += 1.5
		}
	}
	return sc
}

// Usable reports whether a candidate may be chosen at all.
func Usable(s Subtitle, lang string, p Prefs) bool {
	return strings.EqualFold(s.Language, lang) && !s.AI && !s.Machine && !s.ForeignOnly &&
		s.Files == 1 && !(p.HearingImpaired == "exclude" && s.HI)
}

// Ranked returns the usable candidates for lang, best first.
func Ranked(cands []Subtitle, lang string, p Prefs) []Subtitle {
	var out []Subtitle
	for _, c := range cands {
		if Usable(c, lang, p) {
			out = append(out, c)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return Score(out[i], p) > Score(out[j], p) })
	return out
}
