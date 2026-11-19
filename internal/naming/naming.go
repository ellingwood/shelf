// Package naming renders destination paths from media metadata using Go
// text/template, with presets tailored to media servers.
package naming

import (
	"bytes"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"

	"shelf/internal/media"
	"shelf/internal/parse"
)

// Templates holds one template per media kind.
type Templates struct {
	Movie   string `yaml:"movie"`
	Episode string `yaml:"episode"`
}

// Presets are built-in template sets.
var Presets = map[string]Templates{
	// Jellyfin: https://jellyfin.org/docs/general/server/media/movies and /shows
	// - [tmdbid-N] in folder names pins exact metadata matches.
	// - "Season 00" holds specials; multi-episode files use S01E01-E02.
	// - Multiple versions of a movie share a folder: "Title (Year) - 2160p.mkv".
	"jellyfin": {
		Movie: `Movies/{{.Title | clean}}{{with .Year}} ({{.}}){{end}}{{with .TMDBID}} [tmdbid-{{.}}]{{end}}/` +
			`{{.Title | clean}}{{with .Year}} ({{.}}){{end}}{{with .Edition}} - {{. | clean}}{{end}}{{.Ext}}`,
		Episode: `Shows/{{.Show | clean}}{{with .ShowYear}} ({{.}}){{end}}{{with .TMDBID}} [tmdbid-{{.}}]{{end}}/` +
			`Season {{pad .Season 2}}/` +
			`{{.Show | clean}} {{epcode .}}{{with .EpisodeTitle}} - {{. | clean}}{{end}}{{.Ext}}`,
	},
	// Same layout, but each movie version is tagged by resolution, so a 1080p
	// and a 2160p copy can live side by side.
	"jellyfin-versions": {
		Movie: `Movies/{{.Title | clean}}{{with .Year}} ({{.}}){{end}}{{with .TMDBID}} [tmdbid-{{.}}]{{end}}/` +
			`{{.Title | clean}}{{with .Year}} ({{.}}){{end}}{{with .Resolution}} - {{.}}{{end}}{{.Ext}}`,
		Episode: `Shows/{{.Show | clean}}{{with .ShowYear}} ({{.}}){{end}}{{with .TMDBID}} [tmdbid-{{.}}]{{end}}/` +
			`Season {{pad .Season 2}}/` +
			`{{.Show | clean}} {{epcode .}}{{with .EpisodeTitle}} - {{. | clean}}{{end}}{{.Ext}}`,
	},
}

// PresetNames lists the built-in presets.
func PresetNames() []string {
	names := make([]string, 0, len(Presets))
	for n := range Presets {
		names = append(names, n)
	}
	return names
}

// Renderer is a compiled template set.
type Renderer struct {
	movie, episode *template.Template
}

// New compiles a template set.
func New(t Templates) (*Renderer, error) {
	r := &Renderer{}
	var err error
	if r.movie, err = compile("movie", t.Movie); err != nil {
		return nil, err
	}
	if r.episode, err = compile("episode", t.Episode); err != nil {
		return nil, err
	}
	return r, nil
}

func compile(name, text string) (*template.Template, error) {
	if strings.TrimSpace(text) == "" {
		return nil, fmt.Errorf("%s template is empty", name)
	}
	t, err := template.New(name).Option("missingkey=error").Funcs(Funcs).Parse(text)
	if err != nil {
		return nil, fmt.Errorf("%s template: %w", name, err)
	}
	return t, nil
}

// Render returns the destination path (relative, using OS separators).
func (r *Renderer) Render(m media.Media) (string, error) {
	t := r.movie
	if m.Kind == media.Episode {
		t = r.episode
		if m.Show == "" {
			return "", fmt.Errorf("episode has no show name")
		}
	} else if m.Title == "" {
		return "", fmt.Errorf("movie has no title")
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, m); err != nil {
		return "", fmt.Errorf("render %s template: %w", t.Name(), err)
	}
	return SafePath(buf.String())
}

// SafePath sanitizes each segment of a rendered path and ensures it stays
// relative (no absolute paths, no ".." escapes).
func SafePath(p string) (string, error) {
	p = strings.ReplaceAll(p, "\\", "/")
	var segs []string
	for _, seg := range strings.Split(p, "/") {
		if strings.TrimSpace(seg) == ".." {
			return "", fmt.Errorf("rendered path %q escapes the output directory", p)
		}
		seg = sanitizeSegment(seg)
		if seg == "" || seg == "." {
			continue
		}
		segs = append(segs, seg)
	}
	if len(segs) == 0 {
		return "", fmt.Errorf("rendered path is empty")
	}
	return filepath.Join(segs...), nil
}

var (
	reIllegal   = regexp.MustCompile(`[<>"|?*\x00-\x1f]`)
	reMultiSpc  = regexp.MustCompile(`\s{2,}`)
	reColonSpcd = regexp.MustCompile(`\s*:\s+`)
)

// sanitizeSegment makes one path segment safe on Linux, Windows and SMB.
func sanitizeSegment(s string) string {
	s = reColonSpcd.ReplaceAllString(s, " - ") // "Mission: Impossible" → "Mission - Impossible"
	s = strings.ReplaceAll(s, ":", "-")
	s = reIllegal.ReplaceAllString(s, "")
	s = reMultiSpc.ReplaceAllString(s, " ")
	s = strings.TrimSpace(s)
	s = strings.TrimRight(s, ". ") // Windows forbids trailing dots/spaces
	if len(s) > 240 {              // keep under common 255-byte limits
		s = s[:240]
	}
	return s
}

// Funcs is the template function map.
var Funcs = template.FuncMap{
	"clean": func(s string) string {
		s = strings.ReplaceAll(s, "/", "-")
		return sanitizeSegment(s)
	},
	"pad": func(n any, width int) string {
		return fmt.Sprintf("%0*d", width, toInt(n))
	},
	"epcode": func(m media.Media) string {
		code := fmt.Sprintf("S%02dE%02d", m.Season, m.Episode)
		if last := m.LastEpisode(); last != m.Episode {
			code += fmt.Sprintf("-E%02d", last)
		}
		return code
	},
	"lower":   strings.ToLower,
	"upper":   strings.ToUpper,
	"title":   parse.TitleCase,
	"trim":    strings.TrimSpace,
	"replace": func(old, new, s string) string { return strings.ReplaceAll(s, old, new) },
	"join":    func(sep string, parts ...string) string { return joinNonEmpty(sep, parts) },
	"default": func(def string, v any) string {
		if s := fmt.Sprint(v); v != nil && s != "" && s != "0" {
			return s
		}
		return def
	},
	"first": func(n int, s string) string {
		r := []rune(s)
		if n < len(r) {
			return string(r[:n])
		}
		return s
	},
	// sortname moves a leading article: "The Matrix" → "Matrix, The".
	"sortname": func(s string) string {
		for _, a := range []string{"The ", "A ", "An "} {
			if strings.HasPrefix(s, a) {
				return s[len(a):] + ", " + strings.TrimSpace(a)
			}
		}
		return s
	},
}

func joinNonEmpty(sep string, parts []string) string {
	var out []string
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, sep)
}

func toInt(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	}
	return 0
}
