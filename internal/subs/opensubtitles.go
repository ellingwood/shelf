// Package subs downloads subtitles from OpenSubtitles (api.opensubtitles.com)
// and optionally re-times them to the video with ffsubsync.
package subs

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const defaultBase = "https://api.opensubtitles.com/api/v1"

// QuotaError means the daily download allowance is used up.
type QuotaError struct{ Reset time.Time }

func (e *QuotaError) Error() string {
	if e.Reset.IsZero() {
		return "opensubtitles: daily download quota used up"
	}
	return "opensubtitles: daily download quota used up until " + e.Reset.Local().Format("Jan 2 15:04")
}

// ErrAuth means the API key or account credentials were rejected.
var ErrAuth = errors.New("opensubtitles: authentication failed (check api_key, username, password)")

// Client talks to the OpenSubtitles REST API.
type Client struct {
	APIKey, Username, Password string
	BaseURL                    string // default api.opensubtitles.com; login may switch it (VIP)
	UserAgent                  string
	HTTP                       *http.Client

	throttleMu sync.Mutex
	mu         sync.Mutex
	token      string
	remaining  int // downloads left today (-1 = unknown)
	reset      time.Time
	last       time.Time
}

func NewClient(apiKey, user, pass string) *Client {
	return &Client{
		APIKey: apiKey, Username: user, Password: pass,
		BaseURL: defaultBase, UserAgent: "shelf v1.0",
		HTTP:      &http.Client{Timeout: 30 * time.Second},
		remaining: -1,
	}
}

// Subtitle is one search result, flattened to what selection needs.
type Subtitle struct {
	FileID      int
	Language    string
	Release     string
	Downloads   int
	Rating      float64
	Votes       int
	HI          bool // hearing impaired / SDH
	ForeignOnly bool // forced: only foreign-language parts
	AI, Machine bool // AI- or machine-translated
	Trusted     bool
	HashMatch   bool // made for a file with this exact hash
	Files       int
	URL         string
}

// Query describes what to search for. Set TMDBID (movies) or ParentTMDBID +
// Season + Episode (episodes); Text is the fallback when IDs are unknown.
type Query struct {
	Languages    []string
	Hash         string
	TMDBID       int
	ParentTMDBID int
	Season       int
	Episode      int
	Text         string
	Year         int
	Episodic     bool
}

// Search returns all results for q (first page, up to 60, sorted by
// OpenSubtitles' relevance; selection re-ranks them).
func (c *Client) Search(ctx context.Context, q Query) ([]Subtitle, error) {
	v := url.Values{}
	langs := make([]string, len(q.Languages))
	for i, l := range q.Languages {
		langs[i] = strings.ToLower(l)
	}
	v.Set("languages", strings.Join(langs, ","))
	if q.Hash != "" {
		v.Set("moviehash", q.Hash)
	}
	switch {
	case q.Episodic && q.ParentTMDBID != 0:
		v.Set("parent_tmdb_id", strconv.Itoa(q.ParentTMDBID))
		v.Set("season_number", strconv.Itoa(q.Season))
		v.Set("episode_number", strconv.Itoa(q.Episode))
	case !q.Episodic && q.TMDBID != 0:
		v.Set("tmdb_id", strconv.Itoa(q.TMDBID))
	case q.Text != "":
		v.Set("query", strings.ToLower(q.Text))
		if q.Episodic {
			v.Set("season_number", strconv.Itoa(q.Season))
			v.Set("episode_number", strconv.Itoa(q.Episode))
		} else if q.Year != 0 {
			v.Set("year", strconv.Itoa(q.Year))
		}
	default:
		return nil, fmt.Errorf("opensubtitles: nothing to search for")
	}
	if q.Episodic {
		v.Set("type", "episode")
	} else {
		v.Set("type", "movie")
	}
	// The API wants keys sorted and values lowercase, or it redirects;
	// url.Values.Encode sorts keys.
	var res struct {
		Data []struct {
			Attributes struct {
				Language          string  `json:"language"`
				DownloadCount     int     `json:"download_count"`
				HearingImpaired   bool    `json:"hearing_impaired"`
				ForeignPartsOnly  bool    `json:"foreign_parts_only"`
				AITranslated      bool    `json:"ai_translated"`
				MachineTranslated bool    `json:"machine_translated"`
				FromTrusted       bool    `json:"from_trusted"`
				MoviehashMatch    bool    `json:"moviehash_match"`
				Ratings           float64 `json:"ratings"`
				Votes             int     `json:"votes"`
				Release           string  `json:"release"`
				URL               string  `json:"url"`
				Files             []struct {
					FileID int `json:"file_id"`
				} `json:"files"`
			} `json:"attributes"`
		} `json:"data"`
	}
	if err := c.do(ctx, http.MethodGet, "/subtitles?"+v.Encode(), nil, false, &res); err != nil {
		return nil, err
	}
	out := make([]Subtitle, 0, len(res.Data))
	for _, d := range res.Data {
		a := d.Attributes
		if len(a.Files) == 0 {
			continue
		}
		out = append(out, Subtitle{
			FileID: a.Files[0].FileID, Files: len(a.Files), Language: a.Language, Release: a.Release,
			Downloads: a.DownloadCount, Rating: a.Ratings, Votes: a.Votes, HI: a.HearingImpaired,
			ForeignOnly: a.ForeignPartsOnly, AI: a.AITranslated, Machine: a.MachineTranslated,
			Trusted: a.FromTrusted, HashMatch: a.MoviehashMatch, URL: a.URL,
		})
	}
	return out, nil
}

// Download fetches a subtitle file (SRT). It counts against the daily quota.
func (c *Client) Download(ctx context.Context, fileID int) ([]byte, error) {
	c.mu.Lock()
	if c.remaining == 0 && time.Now().Before(c.reset) {
		c.mu.Unlock()
		return nil, &QuotaError{Reset: c.reset}
	}
	c.mu.Unlock()
	if err := c.login(ctx); err != nil {
		return nil, err
	}
	var res struct {
		Link         string `json:"link"`
		Remaining    int    `json:"remaining"`
		Message      string `json:"message"`
		ResetTimeUTC string `json:"reset_time_utc"`
	}
	body := map[string]any{"file_id": fileID, "sub_format": "srt"}
	err := c.do(ctx, http.MethodPost, "/download", body, true, &res)
	reset, _ := time.Parse(time.RFC3339, res.ResetTimeUTC)
	var se *statusError
	if errors.As(err, &se) && se.code == http.StatusNotAcceptable {
		c.setQuota(0, reset)
		return nil, &QuotaError{Reset: reset}
	}
	if err != nil {
		return nil, err
	}
	c.setQuota(res.Remaining, reset)
	if res.Link == "" {
		return nil, fmt.Errorf("opensubtitles: no download link (%s)", res.Message)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, res.Link, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", c.UserAgent)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("opensubtitles: download link: HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 20<<20))
}

// Remaining reports downloads left today, or -1 if not yet known.
func (c *Client) Remaining() (int, time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.remaining, c.reset
}

func (c *Client) setQuota(n int, reset time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.remaining = n
	if !reset.IsZero() {
		c.reset = reset
	}
}

// Login checks the credentials and returns the daily download allowance.
func (c *Client) Login(ctx context.Context) (allowed int, err error) {
	c.mu.Lock()
	c.token = ""
	c.mu.Unlock()
	return c.loginAllowed(ctx)
}

func (c *Client) login(ctx context.Context) error {
	c.mu.Lock()
	ok := c.token != ""
	c.mu.Unlock()
	if ok {
		return nil
	}
	_, err := c.loginAllowed(ctx)
	return err
}

func (c *Client) loginAllowed(ctx context.Context) (int, error) {
	var res struct {
		Token   string `json:"token"`
		BaseURL string `json:"base_url"`
		User    struct {
			AllowedDownloads   int `json:"allowed_downloads"`
			RemainingDownloads int `json:"remaining_downloads"`
		} `json:"user"`
	}
	body := map[string]string{"username": c.Username, "password": c.Password}
	if err := c.do(ctx, http.MethodPost, "/login", body, false, &res); err != nil {
		return 0, err
	}
	if res.Token == "" {
		return 0, ErrAuth
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.token = res.Token
	if res.BaseURL != "" && c.BaseURL == defaultBase {
		host := strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(res.BaseURL, "https://"), "http://"), "/")
		c.BaseURL = "https://" + host + "/api/v1"
	}
	if res.User.RemainingDownloads > 0 { // absent in some responses: learned from the first download
		c.remaining = res.User.RemainingDownloads
	}
	return res.User.AllowedDownloads, nil
}

type statusError struct {
	code int
	msg  string
}

func (e *statusError) Error() string { return fmt.Sprintf("opensubtitles: HTTP %d %s", e.code, e.msg) }

// do sends a request, throttled to stay under the API's rate limit
// (5 requests/second) and retrying 429 responses.
func (c *Client) do(ctx context.Context, method, path string, body any, auth bool, out any) error {
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return err
		}
	}
	for attempt := 0; ; attempt++ {
		c.throttle()
		c.mu.Lock()
		base, token := c.BaseURL, c.token
		c.mu.Unlock()
		req, err := http.NewRequestWithContext(ctx, method, base+path, bytes.NewReader(payload))
		if err != nil {
			return err
		}
		req.Header.Set("Api-Key", c.APIKey)
		req.Header.Set("User-Agent", c.UserAgent)
		req.Header.Set("Accept", "application/json")
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if auth && token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := c.HTTP.Do(req)
		if err != nil {
			return err
		}
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()
		switch {
		case resp.StatusCode == http.StatusTooManyRequests && attempt < 4:
			wait := time.Duration(attempt+1) * time.Second
			if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil {
				wait = time.Duration(s) * time.Second
			}
			select {
			case <-time.After(wait):
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
			if auth && attempt == 0 { // token expired: log in again once
				c.mu.Lock()
				c.token = ""
				c.mu.Unlock()
				if c.login(ctx) == nil {
					continue
				}
			}
			return ErrAuth
		case resp.StatusCode >= 300:
			if out != nil {
				_ = json.Unmarshal(data, out) // 406 bodies carry quota details
			}
			return &statusError{resp.StatusCode, strings.TrimSpace(string(data[:min(len(data), 300)]))}
		}
		if out == nil {
			return nil
		}
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("opensubtitles: bad response: %w", err)
		}
		return nil
	}
}

func (c *Client) throttle() {
	c.throttleMu.Lock()
	defer c.throttleMu.Unlock()
	if wait := 250*time.Millisecond - time.Since(c.last); wait > 0 {
		time.Sleep(wait)
	}
	c.last = time.Now()
}

// Hash computes the OpenSubtitles movie hash: the file size plus the sum of
// the first and last 64 KiB read as little-endian uint64s, in hex.
func Hash(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return "", err
	}
	const chunk = 64 << 10
	size := fi.Size()
	if size < chunk {
		return "", fmt.Errorf("file too small to hash")
	}
	h := uint64(size)
	buf := make([]byte, chunk)
	for _, off := range []int64{0, size - chunk} {
		if _, err := f.ReadAt(buf, off); err != nil {
			return "", err
		}
		for i := 0; i < chunk; i += 8 {
			h += binary.LittleEndian.Uint64(buf[i:])
		}
	}
	return fmt.Sprintf("%016x", h), nil
}
