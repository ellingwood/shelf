// Package match identifies parsed media against The Movie Database (TMDB)
// and fills canonical titles, years, IDs and episode titles.
package match

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"shelf/internal/media"
)

// ErrNoMatch means TMDB had no result above the confidence threshold.
var ErrNoMatch = errors.New("no confident TMDB match")

var errNotFound = errors.New("not found")

type Client struct {
	key, lang     string
	MinConfidence float64
	BaseURL       string
	HTTP          *http.Client
	cache         *cache

	mu   sync.Mutex
	last time.Time
}

// New creates a TMDB client. cacheDir may be empty to disable caching.
func New(apiKey, language string, minConfidence float64, cacheDir string) *Client {
	c := &Client{
		key: apiKey, lang: language, MinConfidence: minConfidence,
		BaseURL: "https://api.themoviedb.org/3",
		HTTP:    &http.Client{Timeout: 15 * time.Second},
	}
	if cacheDir != "" {
		c.cache = openCache(filepath.Join(cacheDir, "tmdb-cache.json"), 7*24*time.Hour)
	}
	return c
}

type movieResult struct {
	ID            int     `json:"id"`
	Title         string  `json:"title"`
	OriginalTitle string  `json:"original_title"`
	ReleaseDate   string  `json:"release_date"`
	Popularity    float64 `json:"popularity"`
}

type tvResult struct {
	ID           int     `json:"id"`
	Name         string  `json:"name"`
	OriginalName string  `json:"original_name"`
	FirstAirDate string  `json:"first_air_date"`
	Popularity   float64 `json:"popularity"`
}

type season struct {
	Episodes []struct {
		EpisodeNumber int    `json:"episode_number"`
		Name          string `json:"name"`
		AirDate       string `json:"air_date"`
	} `json:"episodes"`
}

// Episode is one entry of a TMDB season listing.
type Episode struct {
	Number int
	Name   string
	Aired  time.Time // zero if TMDB has no air date
}

// SeasonEpisodes lists a season's episodes.
func (c *Client) SeasonEpisodes(ctx context.Context, showID, seasonNum int) ([]Episode, error) {
	var s season
	if err := c.get(ctx, fmt.Sprintf("/tv/%d/season/%d", showID, seasonNum), nil, &s); err != nil {
		return nil, err
	}
	out := make([]Episode, 0, len(s.Episodes))
	for _, e := range s.Episodes {
		aired, _ := time.Parse("2006-01-02", e.AirDate)
		out = append(out, Episode{Number: e.EpisodeNumber, Name: strings.TrimSpace(e.Name), Aired: aired})
	}
	return out, nil
}

// Candidate is a scored search result, exposed for diagnostics.
type Candidate struct {
	ID    int
	Title string
	Year  int
	Score float64
}

// Enrich fills m from TMDB. On ErrNoMatch, m is unchanged and the best
// candidate (if any) is returned for logging.
func (c *Client) Enrich(ctx context.Context, m *media.Media) (*Candidate, error) {
	if m.Kind == media.Episode {
		return c.enrichEpisode(ctx, m)
	}
	return c.enrichMovie(ctx, m)
}

func (c *Client) enrichMovie(ctx context.Context, m *media.Media) (*Candidate, error) {
	if m.TMDBID != 0 {
		var r movieResult
		err := c.get(ctx, fmt.Sprintf("/movie/%d", m.TMDBID), nil, &r)
		if err == nil && r.ID != 0 {
			m.Title = r.Title
			if y := yearOf(r.ReleaseDate); y != 0 {
				m.Year = y
			}
			return &Candidate{ID: r.ID, Title: r.Title, Year: yearOf(r.ReleaseDate), Score: 1}, nil
		}
		if !errors.Is(err, errNotFound) && err != nil {
			return nil, err
		}
		m.TMDBID = 0 // stale/wrong ID: fall back to searching
	}
	if m.Title == "" {
		return nil, ErrNoMatch
	}
	var best *Candidate
	attempts := []int{m.Year}
	if m.Year != 0 {
		attempts = append(attempts, 0) // retry without year if nothing fits
	}
	for _, year := range attempts {
		q := url.Values{"query": {m.Title}, "include_adult": {"false"}}
		if year != 0 {
			q.Set("year", strconv.Itoa(year))
		}
		var resp struct{ Results []movieResult }
		if err := c.get(ctx, "/search/movie", q, &resp); err != nil {
			return nil, err
		}
		for i, r := range resp.Results {
			if i >= 10 {
				break
			}
			ry := yearOf(r.ReleaseDate)
			s := score(m.Title, m.Year, ry, r.Title, r.OriginalTitle) - float64(i)*0.001
			if best == nil || s > best.Score {
				best = &Candidate{ID: r.ID, Title: r.Title, Year: ry, Score: s}
			}
		}
		if best != nil && best.Score >= c.MinConfidence {
			break
		}
	}
	if best == nil || best.Score < c.MinConfidence {
		return best, ErrNoMatch
	}
	m.Title, m.TMDBID = best.Title, best.ID
	if best.Year != 0 {
		m.Year = best.Year
	}
	return best, nil
}

func (c *Client) enrichEpisode(ctx context.Context, m *media.Media) (*Candidate, error) {
	var best *Candidate
	if m.TMDBID != 0 {
		var r tvResult
		err := c.get(ctx, fmt.Sprintf("/tv/%d", m.TMDBID), nil, &r)
		if err == nil && r.ID != 0 {
			best = &Candidate{ID: r.ID, Title: r.Name, Year: yearOf(r.FirstAirDate), Score: 1}
			return best, c.applyShow(ctx, m, best)
		}
		if !errors.Is(err, errNotFound) && err != nil {
			return nil, err
		}
		m.TMDBID = 0
	}
	if m.Show == "" {
		return nil, ErrNoMatch
	}
	attempts := []int{m.ShowYear}
	if m.ShowYear != 0 {
		attempts = append(attempts, 0)
	}
	for _, year := range attempts {
		q := url.Values{"query": {m.Show}, "include_adult": {"false"}}
		if year != 0 {
			q.Set("first_air_date_year", strconv.Itoa(year))
		}
		var resp struct{ Results []tvResult }
		if err := c.get(ctx, "/search/tv", q, &resp); err != nil {
			return nil, err
		}
		for i, r := range resp.Results {
			if i >= 10 {
				break
			}
			ry := yearOf(r.FirstAirDate)
			s := score(m.Show, m.ShowYear, ry, r.Name, r.OriginalName) - float64(i)*0.001
			if best == nil || s > best.Score {
				best = &Candidate{ID: r.ID, Title: r.Name, Year: ry, Score: s}
			}
		}
		if best != nil && best.Score >= c.MinConfidence {
			break
		}
	}
	if best == nil || best.Score < c.MinConfidence {
		return best, ErrNoMatch
	}
	return best, c.applyShow(ctx, m, best)
}

// applyShow copies a matched show onto m and fetches the episode title.
func (c *Client) applyShow(ctx context.Context, m *media.Media, best *Candidate) error {
	m.Show, m.Title, m.TMDBID = best.Title, best.Title, best.ID
	if best.Year != 0 {
		m.ShowYear, m.Year = best.Year, best.Year
	}

	// Episode title (best effort: a missing season shouldn't fail the match).
	var s season
	if err := c.get(ctx, fmt.Sprintf("/tv/%d/season/%d", best.ID, m.Season), nil, &s); err == nil {
		for _, e := range s.Episodes {
			if e.EpisodeNumber == m.Episode && e.Name != "" {
				m.EpisodeTitle = strings.TrimSpace(e.Name)
				if m.LastEpisode() != m.Episode {
					m.EpisodeTitle = stripPartSuffix(m.EpisodeTitle)
				}
				break
			}
		}
	}
	return nil
}

// stripPartSuffix turns "Pilot (1)" into "Pilot" for multi-episode files.
func stripPartSuffix(s string) string {
	if i := strings.LastIndex(s, " ("); i > 0 && strings.HasSuffix(s, ")") {
		if _, err := strconv.Atoi(s[i+2 : len(s)-1]); err == nil {
			return s[:i]
		}
	}
	return s
}

// score rates a candidate: title similarity, adjusted for year agreement.
func score(query string, qYear, rYear int, titles ...string) float64 {
	q := normTitle(query)
	best := 0.0
	for _, t := range titles {
		if t == "" {
			continue
		}
		best = max(best, similarity(q, normTitle(t)))
	}
	if qYear != 0 && rYear != 0 {
		switch d := abs(qYear - rYear); {
		case d == 0:
			best += 0.15
		case d == 1:
			best += 0.05
		default:
			best -= 0.3
		}
	}
	return min(best, 1.15)
}

func normTitle(s string) string {
	s = strings.ToLower(s)
	s = strings.ReplaceAll(s, "&", " and ")
	var b strings.Builder
	for _, r := range s {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
		case unicode.IsSpace(r) || r == '-' || r == ':' || r == '.':
			b.WriteRune(' ')
		}
	}
	out := strings.Join(strings.Fields(b.String()), " ")
	return strings.TrimPrefix(out, "the ")
}

// similarity is 1 - normalized Levenshtein distance.
func similarity(a, b string) float64 {
	if a == b {
		return 1
	}
	ra, rb := []rune(a), []rune(b)
	if len(ra) == 0 || len(rb) == 0 {
		return 0
	}
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return 1 - float64(prev[len(rb)])/float64(max(len(ra), len(rb)))
}

func yearOf(date string) int {
	if len(date) >= 4 {
		y, _ := strconv.Atoi(date[:4])
		return y
	}
	return 0
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// get performs a cached, rate-limited GET and decodes JSON into out.
func (c *Client) get(ctx context.Context, path string, q url.Values, out any) error {
	if q == nil {
		q = url.Values{}
	}
	q.Set("language", c.lang)
	cacheKey := path + "?" + q.Encode()
	if c.cache != nil {
		if b, ok := c.cache.get(cacheKey); ok {
			return json.Unmarshal(b, out)
		}
	}

	bearer := strings.HasPrefix(c.key, "eyJ") // v4 read access token
	if !bearer {
		q.Set("api_key", c.key)
	}
	u := c.BaseURL + path + "?" + q.Encode()

	for attempt := 0; ; attempt++ {
		c.throttle()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Accept", "application/json")
		if bearer {
			req.Header.Set("Authorization", "Bearer "+c.key)
		}
		resp, err := c.HTTP.Do(req)
		if err != nil {
			return fmt.Errorf("tmdb %s: %w", path, err)
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		if err != nil {
			return fmt.Errorf("tmdb %s: %w", path, err)
		}
		switch {
		case resp.StatusCode == http.StatusTooManyRequests && attempt < 3:
			wait, _ := strconv.Atoi(resp.Header.Get("Retry-After"))
			select {
			case <-time.After(time.Duration(max(wait, 1)) * time.Second):
			case <-ctx.Done():
				return ctx.Err()
			}
			continue
		case resp.StatusCode == http.StatusUnauthorized:
			return fmt.Errorf("tmdb: unauthorized (check tmdb.api_key)")
		case resp.StatusCode == http.StatusNotFound:
			return fmt.Errorf("tmdb %s: %w", path, errNotFound)
		case resp.StatusCode != http.StatusOK:
			return fmt.Errorf("tmdb %s: HTTP %d", path, resp.StatusCode)
		}
		if err := json.Unmarshal(body, out); err != nil {
			return fmt.Errorf("tmdb %s: decode: %w", path, err)
		}
		if c.cache != nil {
			c.cache.put(cacheKey, body)
		}
		return nil
	}
}

// throttle keeps requests ≥ 50ms apart (TMDB allows ~40-50 req/s).
func (c *Client) throttle() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if d := 50*time.Millisecond - time.Since(c.last); d > 0 {
		time.Sleep(d)
	}
	c.last = time.Now()
}

// cache is a small persistent JSON response cache.
type cache struct {
	path string
	ttl  time.Duration
	mu   sync.Mutex
	data map[string]cacheEntry
}

type cacheEntry struct {
	Time time.Time       `json:"t"`
	Body json.RawMessage `json:"b"`
}

func openCache(path string, ttl time.Duration) *cache {
	c := &cache{path: path, ttl: ttl, data: map[string]cacheEntry{}}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &c.data)
	}
	return c
}

func (c *cache) get(k string) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.data[k]
	if !ok || time.Since(e.Time) > c.ttl {
		return nil, false
	}
	return e.Body, true
}

func (c *cache) put(k string, body []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.data[k] = cacheEntry{Time: time.Now(), Body: body}
	for key, e := range c.data { // prune expired entries
		if time.Since(e.Time) > c.ttl {
			delete(c.data, key)
		}
	}
	b, err := json.Marshal(c.data)
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(c.path), 0o755)
	tmp := c.path + ".tmp"
	if os.WriteFile(tmp, b, 0o644) == nil {
		_ = os.Rename(tmp, c.path)
	}
}
