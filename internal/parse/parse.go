// Package parse extracts media metadata from release-style filenames such as
// "The.Matrix.1999.1080p.BluRay.x264-GROUP.mkv" or
// "Breaking.Bad.S01E02.Cats.in.the.Bag.720p.WEB-DL.mkv".
package parse

import (
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"shelf/internal/media"
)

type marker struct {
	re    *regexp.Regexp
	apply func(m *media.Media, match []string)
	// ambiguous markers are words that also appear in real titles ("Web",
	// "Complete", "Extended"). They only end a movie title when no year exists.
	ambiguous bool
}

var (
	reSeparators = regexp.MustCompile(`[._]+`)
	reSpaces     = regexp.MustCompile(`\s+`)
	reLeadGroup  = regexp.MustCompile(`^\s*[\[{【][^\]}】]*[\]}】]\s*`)
	reBrackets   = regexp.MustCompile(`[\[\](){}]`)
	reWebsite    = regexp.MustCompile(`(?i)^\s*www\s[a-z0-9-]+\s(com|org|net|to|mx|ag|io|me|tv)\s+-?\s*`)
	reSeasonDir  = regexp.MustCompile(`^(season|series|s)\s?\d{1,2}$`)

	// Episode patterns, tried in order.
	reSxxEyy  = regexp.MustCompile(`(?i)\bs(\d{1,2})\s?e(\d{1,3})((?:\s?-?\s?e\d{1,3}|-\d{1,3}\b)*)`)
	reNxNN    = regexp.MustCompile(`(?i)\b(\d{1,2})x(\d{2,3})((?:-?x?\d{2,3}\b)*)`)
	reVerbose = regexp.MustCompile(`(?i)\bseason\s?(\d{1,2})\s?episode\s?(\d{1,3})\b`)
	reEpOnly  = regexp.MustCompile(`(?i)\b(?:e|ep|episode)\s?(\d{1,3})\b`)
	reSeason  = regexp.MustCompile(`(?i)\b(?:season|series|s)\s?(\d{1,2})\b`)
	reEpNums  = regexp.MustCompile(`\d{1,3}`)

	reYear = regexp.MustCompile(`\b(19[2-9]\d|20[0-4]\d)\b`)

	// Jellyfin/Plex-style database tags: "[tmdbid-603]", "{tmdb-603}".
	reTMDBTag = regexp.MustCompile(`(?i)[\[{]\s*tmdb(?:id)?[-=](\d+)\s*[\]}]`)
)

// Technical markers. Every match marks where the title ends.
var markers = []marker{
	{regexp.MustCompile(`(?i)\b(2160p|4k|uhd)\b`), func(m *media.Media, _ []string) { m.Resolution = "2160p" }, false},
	{regexp.MustCompile(`(?i)\b(1080[pi])\b`), func(m *media.Media, _ []string) { m.Resolution = "1080p" }, false},
	{regexp.MustCompile(`(?i)\b(720p)\b`), func(m *media.Media, _ []string) { m.Resolution = "720p" }, false},
	{regexp.MustCompile(`(?i)\b(576p|480p|360p)\b`), func(m *media.Media, s []string) { m.Resolution = strings.ToLower(s[1]) }, false},

	{regexp.MustCompile(`(?i)\b(remux)\b`), func(m *media.Media, _ []string) { setOnce(&m.Source, "Remux") }, false},
	{regexp.MustCompile(`(?i)\b(blu-?ray|bdrip|brrip|bdremux|bd)\b`), func(m *media.Media, _ []string) { setOnce(&m.Source, "BluRay") }, true},
	{regexp.MustCompile(`(?i)\b(web-?dl|webdl)\b`), func(m *media.Media, _ []string) { setOnce(&m.Source, "WEB-DL") }, false},
	{regexp.MustCompile(`(?i)\b(web-?rip|web)\b`), func(m *media.Media, _ []string) { setOnce(&m.Source, "WEBRip") }, true},
	{regexp.MustCompile(`(?i)\b(hdtv|pdtv|hdtvrip)\b`), func(m *media.Media, _ []string) { setOnce(&m.Source, "HDTV") }, false},
	{regexp.MustCompile(`(?i)\b(dvdrip|dvd-?r|dvd)\b`), func(m *media.Media, _ []string) { setOnce(&m.Source, "DVD") }, true},
	{regexp.MustCompile(`(?i)\b(hdrip)\b`), func(m *media.Media, _ []string) { setOnce(&m.Source, "HDRip") }, false},
	{regexp.MustCompile(`(?i)\b(amzn|nf|dsnp|hmax|atvp|hulu|pcok|pmtp)\b`), func(*media.Media, []string) {}, true},

	{regexp.MustCompile(`(?i)\b(x\s?265|h\s?265|hevc)\b`), func(m *media.Media, _ []string) { setOnce(&m.VideoCodec, "x265") }, false},
	{regexp.MustCompile(`(?i)\b(x\s?264|h\s?264|avc)\b`), func(m *media.Media, _ []string) { setOnce(&m.VideoCodec, "x264") }, false},
	{regexp.MustCompile(`(?i)\b(av1)\b`), func(m *media.Media, _ []string) { setOnce(&m.VideoCodec, "AV1") }, false},
	{regexp.MustCompile(`(?i)\b(xvid|divx)\b`), func(m *media.Media, _ []string) { setOnce(&m.VideoCodec, "XviD") }, false},
	{regexp.MustCompile(`(?i)\b(10-?bit|8-?bit)\b`), func(*media.Media, []string) {}, true},

	{regexp.MustCompile(`(?i)\b(hdr10\+|hdr10plus)`), func(m *media.Media, _ []string) { setOnce(&m.HDR, "HDR10+") }, false},
	{regexp.MustCompile(`(?i)\b(hdr10|hdr)\b`), func(m *media.Media, _ []string) { setOnce(&m.HDR, "HDR10") }, true},
	{regexp.MustCompile(`(?i)\b(dv|dovi|dolby\s?vision)\b`), func(m *media.Media, _ []string) { setOnce(&m.HDR, "DV") }, true},

	{regexp.MustCompile(`(?i)\b(truehd|true-hd)\b`), func(m *media.Media, _ []string) { setOnce(&m.AudioCodec, "TrueHD") }, false},
	{regexp.MustCompile(`(?i)\b(atmos)\b`), func(m *media.Media, _ []string) { setOnce(&m.AudioCodec, "Atmos") }, true},
	{regexp.MustCompile(`(?i)\b(dts-?hd(\s?ma)?|dts-?x|dts)\b`), func(m *media.Media, _ []string) { setOnce(&m.AudioCodec, "DTS") }, false},
	{regexp.MustCompile(`(?i)\b(ddp|dd\+|e-?ac-?3)\s?(\d\s\d)?`), func(m *media.Media, _ []string) { setOnce(&m.AudioCodec, "EAC3") }, false},
	{regexp.MustCompile(`(?i)\b(dd|ac3)\s?(\d\s\d)?\b`), func(m *media.Media, _ []string) { setOnce(&m.AudioCodec, "AC3") }, true},
	{regexp.MustCompile(`(?i)\b(aac)(\s?\d\s\d)?\b`), func(m *media.Media, _ []string) { setOnce(&m.AudioCodec, "AAC") }, false},
	{regexp.MustCompile(`(?i)\b(flac|opus|mp3)\b`), func(m *media.Media, s []string) { setOnce(&m.AudioCodec, strings.ToUpper(s[1])) }, true},
	{regexp.MustCompile(`(?i)\b([257]\s1)\b`), func(*media.Media, []string) {}, true},

	{regexp.MustCompile(`(?i)\b(directors?\s?cut|director'?s\s?cut)\b`), func(m *media.Media, _ []string) { setOnce(&m.Edition, "Director's Cut") }, true},
	{regexp.MustCompile(`(?i)\b(extended(\s?(cut|edition))?)\b`), func(m *media.Media, _ []string) { setOnce(&m.Edition, "Extended") }, true},
	{regexp.MustCompile(`(?i)\b(unrated)\b`), func(m *media.Media, _ []string) { setOnce(&m.Edition, "Unrated") }, true},
	{regexp.MustCompile(`(?i)\b(theatrical(\s?cut)?)\b`), func(m *media.Media, _ []string) { setOnce(&m.Edition, "Theatrical") }, true},
	{regexp.MustCompile(`(?i)\b(remastered)\b`), func(m *media.Media, _ []string) { setOnce(&m.Edition, "Remastered") }, true},
	{regexp.MustCompile(`(?i)\b(imax)\b`), func(m *media.Media, _ []string) { setOnce(&m.Edition, "IMAX") }, true},
	{regexp.MustCompile(`(?i)\b(criterion)\b`), func(m *media.Media, _ []string) { setOnce(&m.Edition, "Criterion") }, true},

	{regexp.MustCompile(`(?i)\b(proper|repack|rerip|internal|limited|complete|multi|dubbed|subbed)\b`), func(*media.Media, []string) {}, true},
}

func setOnce(dst *string, v string) {
	if *dst == "" {
		*dst = v
	}
}

// Name parses a single base filename (no directories).
func Name(base string) media.Media {
	m := media.Media{Original: base}

	stem := base
	if ext := filepath.Ext(base); media.IsVideo(ext) || media.IsSubtitle(ext) {
		m.Ext = strings.ToLower(ext)
		stem = strings.TrimSuffix(base, ext)
	}

	if sm := reTMDBTag.FindStringSubmatch(stem); sm != nil {
		m.TMDBID, _ = strconv.Atoi(sm[1])
		stem = reTMDBTag.ReplaceAllString(stem, " ")
	}

	s := normalize(stem)
	cut := len(s) // where the title ends

	// Episode detection.
	var codeFirst bool
	var codeFirstShow string
	var codeFirstYear int
	if loc, ok := parseEpisode(&m, s); ok {
		m.Kind = media.Episode
		cut = loc[0]
		rest := s[loc[1]:]
		if strings.TrimSpace(s[:loc[0]]) == "" {
			// Code-first naming: "S01E02 - Show Name (2005) - Episode Title - 1080p".
			// The first " - " segment is the show, an optional second one
			// (if it isn't technical info) is the episode title.
			codeFirst = true
			segs := strings.Split(strings.TrimLeft(rest, " -"), " - ")
			show := segs[0][:untilMarker(segs[0], true)]
			if l := reYear.FindStringIndex(show); l != nil && l[0] > 0 {
				codeFirstYear, _ = strconv.Atoi(show[l[0]:l[1]])
				show = show[:l[0]]
			}
			codeFirstShow = cleanTitle(show)
			if len(segs) > 1 && untilMarker(segs[0], true) == len(segs[0]) {
				m.EpisodeTitle = cleanTitle(segs[1][:untilMarker(segs[1], false)])
			}
		} else {
			// Episode title: text after the episode code up to the next marker.
			end := untilMarker(rest, false)
			if l := reYear.FindStringIndex(rest); l != nil && l[0] < end {
				end = l[0]
			}
			m.EpisodeTitle = cleanTitle(rest[:end])
		}
	}

	// Technical markers: apply all, and remember where the first ones are.
	firstStrong, firstAny := len(s), len(s)
	for _, mk := range markers {
		for _, loc := range mk.re.FindAllStringSubmatchIndex(s, -1) {
			mk.apply(&m, submatches(s, loc))
			if loc[0] == 0 {
				continue
			}
			firstAny = min(firstAny, loc[0])
			if !mk.ambiguous {
				firstStrong = min(firstStrong, loc[0])
			}
		}
	}

	// Year: the last year that isn't at the very start of the name (handles
	// "2001 A Space Odyssey 1968" and "1917 2019"), preferring years before
	// the technical tail. For episodes, only years before SxxEyy count.
	yearPos := -1
	limit := len(s)
	if m.Kind == media.Episode {
		limit = cut
	}
	years := reYear.FindAllStringIndex(s, -1)
	for pass := 0; pass < 2 && yearPos < 0; pass++ {
		for i := len(years) - 1; i >= 0; i-- {
			loc := years[i]
			if loc[0] == 0 || loc[0] >= limit || (pass == 0 && loc[0] > firstStrong) {
				continue
			}
			yearPos = loc[0]
			m.Year, _ = strconv.Atoi(s[loc[0]:loc[1]])
			break
		}
	}

	if m.Kind == media.Episode {
		if yearPos > 0 && yearPos < cut {
			cut = yearPos
		}
	} else if yearPos > 0 {
		cut = min(yearPos, firstStrong)
	} else {
		cut = firstAny
	}

	title := cleanTitle(s[:cut])
	if codeFirst {
		title, m.Year = codeFirstShow, codeFirstYear
	}
	if m.Kind == media.Episode {
		m.Show, m.ShowYear = title, m.Year
		// For episodes, an embedded year belongs to the show, never the title.
		m.Title = title
		if m.EpisodeTitle != "" && strings.EqualFold(m.EpisodeTitle, title) {
			m.EpisodeTitle = ""
		}
	} else {
		m.Title = title
		if title != "" {
			m.Kind = media.Movie
		}
	}
	return m
}

// Path parses a full path, falling back to parent directories when the
// filename alone lacks a title (e.g. "Show/Season 1/S01E02.mkv" or
// "Movie (1999)/movie.mkv"). root bounds how far up the tree it looks.
func Path(path, root string) media.Media {
	base := filepath.Base(path)
	m := Name(base)
	m.Path = path

	dirs := parentDirs(path, root, 3)

	switch m.Kind {
	case media.Episode:
		for _, d := range dirs {
			if isSeasonDir(d) {
				continue
			}
			pm := Name(d)
			if pm.Title == "" {
				continue
			}
			if m.Show == "" {
				m.Show, m.Title = pm.Title, pm.Title
			} else if !SameTitle(m.Show, pm.Title) {
				break // folder is about something else; ignore it
			}
			if m.ShowYear == 0 {
				m.ShowYear, m.Year = pm.Year, pm.Year
			}
			if m.TMDBID == 0 {
				m.TMDBID = pm.TMDBID
			}
			break
		}
	default:
		// Season dir + bare "E02"/"Episode 2" filename.
		if len(dirs) > 0 {
			if sm := reSeason.FindStringSubmatch(dirs[0]); sm != nil && isSeasonDir(dirs[0]) {
				if em := reEpOnly.FindStringSubmatch(normalize(strings.TrimSuffix(base, filepath.Ext(base)))); em != nil {
					m.Kind = media.Episode
					m.Season, _ = strconv.Atoi(sm[1])
					m.Episode, _ = strconv.Atoi(em[1])
					m.Episodes = []int{m.Episode}
					m.EpisodeTitle = ""
					m.Show, m.Title, m.ShowYear, m.Year = "", "", 0, 0
					for _, d := range dirs[1:] {
						if pm := Name(d); pm.Title != "" {
							m.Show, m.Title, m.ShowYear, m.Year = pm.Title, pm.Title, pm.Year, pm.Year
							break
						}
					}
					return m
				}
			}
		}
		// Movie: an ID tag on the movie's own folder applies to the file.
		if m.TMDBID == 0 && len(dirs) > 0 {
			if pm := Name(dirs[0]); pm.TMDBID != 0 && SameTitle(pm.Title, m.Title) {
				m.TMDBID = pm.TMDBID
			}
		}
		// Movie: if the filename gave no year but the folder does, trust the folder.
		if m.Year == 0 && len(dirs) > 0 {
			pm := Name(dirs[0])
			if pm.Kind == media.Movie && pm.Year != 0 && pm.Title != "" {
				tech := m
				m = pm
				m.Ext, m.Original, m.Path = tech.Ext, tech.Original, tech.Path
				for _, f := range []struct {
					dst *string
					v   string
				}{
					{&m.Resolution, tech.Resolution}, {&m.Source, tech.Source},
					{&m.VideoCodec, tech.VideoCodec}, {&m.AudioCodec, tech.AudioCodec},
					{&m.HDR, tech.HDR}, {&m.Edition, tech.Edition},
				} {
					if f.v != "" {
						*f.dst = f.v
					}
				}
			}
		}
	}
	return m
}

// untilMarker returns the index of the first technical marker in s, or len(s).
// With strongOnly, ambiguous markers (words that occur in titles) are ignored.
func untilMarker(s string, strongOnly bool) int {
	end := len(s)
	for _, mk := range markers {
		if strongOnly && mk.ambiguous {
			continue
		}
		if l := mk.re.FindStringIndex(s); l != nil && l[0] < end {
			end = l[0]
		}
	}
	return end
}

func parseEpisode(m *media.Media, s string) ([]int, bool) {
	if loc := reSxxEyy.FindStringSubmatchIndex(s); loc != nil {
		sm := submatches(s, loc)
		m.Season, _ = strconv.Atoi(sm[1])
		first, _ := strconv.Atoi(sm[2])
		m.Episodes = expandEpisodes(first, sm[3])
		m.Episode = first
		return loc[:2], true
	}
	if loc := reVerbose.FindStringSubmatchIndex(s); loc != nil {
		sm := submatches(s, loc)
		m.Season, _ = strconv.Atoi(sm[1])
		m.Episode, _ = strconv.Atoi(sm[2])
		m.Episodes = []int{m.Episode}
		return loc[:2], true
	}
	if loc := reNxNN.FindStringSubmatchIndex(s); loc != nil {
		sm := submatches(s, loc)
		// Avoid resolutions like 1920x1080 (regex limits digits, but be safe).
		m.Season, _ = strconv.Atoi(sm[1])
		first, _ := strconv.Atoi(sm[2])
		m.Episodes = expandEpisodes(first, sm[3])
		m.Episode = first
		return loc[:2], true
	}
	return nil, false
}

// expandEpisodes turns "E01" + "-E03" into [1 2 3], and "E01"+"E02" into [1 2].
func expandEpisodes(first int, rest string) []int {
	eps := []int{first}
	nums := reEpNums.FindAllString(rest, -1)
	if len(nums) == 0 {
		return eps
	}
	isRange := strings.Contains(rest, "-") && len(nums) == 1
	if isRange {
		last, _ := strconv.Atoi(nums[0])
		if last > first && last-first < 50 {
			for e := first + 1; e <= last; e++ {
				eps = append(eps, e)
			}
		}
		return eps
	}
	for _, n := range nums {
		v, _ := strconv.Atoi(n)
		if v > eps[len(eps)-1] {
			eps = append(eps, v)
		}
	}
	return eps
}

func submatches(s string, loc []int) []string {
	out := make([]string, len(loc)/2)
	for i := range out {
		if loc[2*i] >= 0 {
			out[i] = s[loc[2*i]:loc[2*i+1]]
		}
	}
	return out
}

// reDomain matches a site tag like "Site.com" or "www.site.net" that stands
// apart from the name (bounded by spaces, brackets, dashes or the string
// edges, never by dots), so dotted titles like "Back.to.the.Future" are safe.
var reDomain = regexp.MustCompile(`(?i)(^|[\s\[\](){}_-])(?:www\.)?[a-z0-9][a-z0-9-]*\.(?:com|org|net|to|mx|ag|io|me|tv|cc|ws|se|is|nu|lol|xyz|site|club|info|biz|co|li|ru|eu|top|vip|pro|watch|movie|movies|stream|online|live|fun|link|click|cx|sx|pw|am|ph|lt|vc|unblocked)($|[\s\[\](){}_-])`)

func normalize(s string) string {
	s = reLeadGroup.ReplaceAllString(s, "")
	if stripped := reDomain.ReplaceAllString(s, "${1} ${2}"); strings.Trim(stripped, " -_[](){}") != "" {
		s = stripped
	}
	s = reSeparators.ReplaceAllString(s, " ")
	s = reBrackets.ReplaceAllString(s, " ")
	s = reWebsite.ReplaceAllString(s, "")
	return strings.TrimSpace(reSpaces.ReplaceAllString(s, " "))
}

func cleanTitle(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, " -–—:,")
	s = reSpaces.ReplaceAllString(s, " ")
	// All-lowercase names get title-cased, as do multi-word all-caps names
	// ("THE MATRIX"). Single all-caps words ("FROM", "NCIS") are often the
	// real title, and mixed case is kept as-is.
	allCaps := s == strings.ToUpper(s) && strings.Contains(s, " ")
	if s != "" && (s == strings.ToLower(s) || allCaps) {
		s = TitleCase(s)
	}
	return s
}

var smallWords = map[string]bool{"a": true, "an": true, "and": true, "as": true, "at": true, "but": true, "by": true, "for": true, "in": true, "of": true, "on": true, "or": true, "the": true, "to": true, "vs": true}

// TitleCase capitalizes words, keeping short joining words lowercase.
func TitleCase(s string) string {
	words := strings.Fields(strings.ToLower(s))
	for i, w := range words {
		if i > 0 && smallWords[w] {
			continue
		}
		words[i] = strings.ToUpper(w[:1]) + w[1:]
	}
	return strings.Join(words, " ")
}

func isSeasonDir(d string) bool {
	n := strings.ToLower(normalize(d))
	if n == "specials" || n == "extras" {
		return true
	}
	return reSeasonDir.MatchString(n)
}

// parentDirs returns up to n parent directory names of path, nearest first,
// never climbing to or above root.
func parentDirs(path, root string, n int) []string {
	var out []string
	root = filepath.Clean(root)
	dir := filepath.Dir(path)
	for len(out) < n {
		if root != "" && root != "." && (dir == root || !strings.HasPrefix(dir, root+string(filepath.Separator))) {
			break
		}
		base := filepath.Base(dir)
		if base == "/" || base == "." || base == dir {
			break
		}
		out = append(out, base)
		dir = filepath.Dir(dir)
	}
	return out
}

// SameTitle compares titles ignoring case, punctuation and a leading "The".
func SameTitle(a, b string) bool {
	norm := func(s string) string {
		var out []rune
		for _, r := range strings.ToLower(s) {
			if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r > 127 {
				out = append(out, r)
			}
		}
		return strings.TrimPrefix(string(out), "the")
	}
	return a != "" && norm(a) == norm(b)
}
