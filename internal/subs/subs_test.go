package subs

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"shelf/internal/config"
	"shelf/internal/media"
)

type fakeOS struct {
	srv       *httptest.Server
	downloads atomic.Int32
	quota     int32
	lastQuery string
}

func newFake(t *testing.T, quota int32) *fakeOS {
	f := &fakeOS{quota: quota}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/file/") { // signed link: no API key needed
			w.Write([]byte("1\n00:00:01,000 --> 00:00:02,000\nHello " + strings.TrimPrefix(r.URL.Path, "/file/") + "\n"))
			return
		}
		if r.Header.Get("Api-Key") != "key" || r.Header.Get("User-Agent") == "" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		switch r.URL.Path {
		case "/login":
			var body map[string]string
			json.NewDecoder(r.Body).Decode(&body)
			if body["password"] != "pw" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.Write([]byte(`{"token":"tok","user":{"allowed_downloads":20,"remaining_downloads":20}}`))
		case "/subtitles":
			f.lastQuery = r.URL.RawQuery
			w.Write([]byte(`{"data":[
				{"attributes":{"language":"en","download_count":90000,"release":"Show.S01E02.1080p.BluRay.x264","files":[{"file_id":1}]}},
				{"attributes":{"language":"en","download_count":30000,"release":"Show.S01E02.1080p.WEB.h264-GRP","files":[{"file_id":2}]}},
				{"attributes":{"language":"en","download_count":500000,"machine_translated":true,"release":"x","files":[{"file_id":3}]}},
				{"attributes":{"language":"en","download_count":400000,"hearing_impaired":true,"release":"Show.S01E02.WEB","files":[{"file_id":4}]}},
				{"attributes":{"language":"es","download_count":10,"foreign_parts_only":true,"release":"x","files":[{"file_id":5}]}}
			]}`))
		case "/download":
			if r.Header.Get("Authorization") != "Bearer tok" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			n := f.downloads.Add(1)
			if n > f.quota {
				w.WriteHeader(http.StatusNotAcceptable)
				w.Write([]byte(`{"remaining":0,"message":"quota","reset_time_utc":"2030-01-01T00:00:00.000Z"}`))
				return
			}
			var body map[string]int
			json.NewDecoder(r.Body).Decode(&body)
			json.NewEncoder(w).Encode(map[string]any{"link": f.srv.URL + "/file/" + itoa(body["file_id"]), "remaining": f.quota - n, "reset_time_utc": "2030-01-01T00:00:00.000Z"})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

func fetcher(t *testing.T, f *fakeOS, sync *Syncer) *Fetcher {
	cfg := config.SubtitleDownload{Languages: []string{"en", "es"}, HearingImpaired: "avoid", Sync: "auto"}
	fe := &Fetcher{Client: NewClient("key", "me", "pw"), Cfg: cfg, Sync: sync, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	fe.Client.BaseURL = f.srv.URL
	return fe
}

func video(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, make([]byte, 200<<10), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestFetchEpisode(t *testing.T) {
	f := newFake(t, 20)
	d := t.TempDir()
	v := video(t, d, "Show S01E02.mkv")

	// A fake ffsubsync that copies input to output and reports an offset.
	bin := filepath.Join(d, "ffsubsync")
	os.WriteFile(bin, []byte("#!/bin/sh\n# video -i in -o out\ncp \"$3\" \"$5\"\necho 'offset seconds: 1.250'\n"), 0o755)

	fe := fetcher(t, f, &Syncer{Bin: bin})
	var created []string
	fe.OnCreate = func(_ Job, p string, _ Subtitle) { created = append(created, p) }
	m := media.Media{Kind: media.Episode, Show: "Show", Season: 1, Episode: 2, TMDBID: 77}

	// Dry run: chooses, doesn't download.
	res, err := fe.Fetch(context.Background(), Job{Video: v, Media: m}, Options{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 2 || res[0].Status != "would-download" || res[0].Sub.FileID != 2 || res[1].Status != "not-found" {
		t.Fatalf("dry run: %+v", res)
	}
	for _, want := range []string{"episode_number=2", "languages=en%2Ces", "moviehash=", "parent_tmdb_id=77", "season_number=1", "type=episode"} {
		if !strings.Contains(f.lastQuery, want) {
			t.Errorf("query %q lacks %q", f.lastQuery, want)
		}
	}
	if f.downloads.Load() != 0 {
		t.Error("dry run downloaded")
	}

	res, err = fe.Fetch(context.Background(), Job{Video: v, Media: m}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(d, "Show S01E02.en.srt")
	if res[0].Status != "downloaded" || res[0].Path != want || res[0].Sync != "shifted +1.250s" {
		t.Fatalf("result: %+v", res[0])
	}
	if b, _ := os.ReadFile(want); !strings.Contains(string(b), "Hello 2") {
		t.Errorf("subtitle content %q", b)
	}
	if fi, _ := os.Stat(want); fi.Mode().Perm()&0o044 != 0o044 {
		t.Errorf("subtitle not readable by others: %v", fi.Mode())
	}
	if len(created) != 1 || created[0] != want {
		t.Errorf("OnCreate: %v", created)
	}

	// Now present: nothing to do (es still has no usable candidate).
	res, _ = fe.Fetch(context.Background(), Job{Video: v, Media: m}, Options{Languages: []string{"en"}})
	if len(res) != 0 {
		t.Errorf("existing subtitle refetched: %+v", res)
	}
}

func TestQuota(t *testing.T) {
	f := newFake(t, 1)
	d := t.TempDir()
	fe := fetcher(t, f, nil)
	m := media.Media{Kind: media.Movie, Title: "Heat", Year: 1995, TMDBID: 949}
	a := video(t, d, "A/Heat (1995).mkv")
	if _, err := fe.Fetch(context.Background(), Job{Video: a, Media: m}, Options{Languages: []string{"en"}}); err != nil {
		t.Fatal(err)
	}
	b := video(t, d, "B/Heat (1995).mkv")
	_, err := fe.Fetch(context.Background(), Job{Video: b, Media: m}, Options{Languages: []string{"en"}})
	qe, ok := err.(*QuotaError)
	if !ok || qe.Reset.Year() != 2030 {
		t.Fatalf("want QuotaError with reset, got %v", err)
	}
	// Known exhausted: no further request is made.
	before := f.downloads.Load()
	fe.Fetch(context.Background(), Job{Video: b, Media: m}, Options{Languages: []string{"en"}})
	if f.downloads.Load() != before {
		t.Error("downloaded again while quota exhausted")
	}
	if !strings.Contains(f.lastQuery, "tmdb_id=949") || !strings.Contains(f.lastQuery, "type=movie") {
		t.Errorf("movie query %q", f.lastQuery)
	}
}

func TestBadLogin(t *testing.T) {
	f := newFake(t, 1)
	c := NewClient("key", "me", "wrong")
	c.BaseURL = f.srv.URL
	if _, err := c.Login(context.Background()); err != ErrAuth {
		t.Errorf("got %v", err)
	}
}

func TestRanking(t *testing.T) {
	cands := []Subtitle{
		{FileID: 1, Language: "en", Files: 1, Downloads: 90000, Release: "X.BluRay"},
		{FileID: 2, Language: "en", Files: 1, Downloads: 30000, Release: "X.WEB-DL"},
		{FileID: 3, Language: "en", Files: 1, Downloads: 10, HashMatch: true},
		{FileID: 4, Language: "en", Files: 2, Downloads: 900000},           // multi-CD
		{FileID: 5, Language: "en", Files: 1, Downloads: 900000, AI: true}, // AI-translated
	}
	r := Ranked(cands, "en", Prefs{HearingImpaired: "avoid"})
	var ids []int
	for _, c := range r {
		ids = append(ids, c.FileID)
	}
	if got := itoa(ids[0]) + itoa(ids[1]) + itoa(ids[2]); got != "321" || len(ids) != 3 {
		t.Errorf("order %v", ids)
	}
	// A BluRay video prefers BluRay-timed subtitles.
	if r := Ranked(cands[:2], "en", Prefs{Source: "BluRay"}); r[0].FileID != 1 {
		t.Errorf("bluray source picked %d", r[0].FileID)
	}
	// Ratings with enough votes lift a subtitle.
	rated := []Subtitle{
		{FileID: 1, Language: "en", Files: 1, Downloads: 1000},
		{FileID: 2, Language: "en", Files: 1, Downloads: 800, Rating: 9.5, Votes: 12},
	}
	if r := Ranked(rated, "en", Prefs{}); r[0].FileID != 2 {
		t.Errorf("well-rated subtitle not preferred")
	}
}

func TestMissing(t *testing.T) {
	d := t.TempDir()
	v := video(t, d, "Movie (2020).mkv")
	for _, n := range []string{"Movie (2020).eng.srt", "Movie (2020).es.forced.srt", "Other.fr.srt"} {
		os.WriteFile(filepath.Join(d, n), []byte("x"), 0o644)
	}
	got := Missing(context.Background(), v, []string{"en", "es", "fr"}, false)
	if strings.Join(got, ",") != "es,fr" {
		t.Errorf("missing = %v", got)
	}
}

func TestHash(t *testing.T) {
	d := t.TempDir()
	p := filepath.Join(d, "zeros")
	os.WriteFile(p, make([]byte, 131072), 0o644)
	if h, err := Hash(p); err != nil || h != "0000000000020000" {
		t.Errorf("hash = %s, %v", h, err)
	}
	b := make([]byte, 131072)
	b[0], b[131064] = 1, 2 // first word +1, last word +2
	os.WriteFile(p, b, 0o644)
	if h, _ := Hash(p); h != "0000000000020003" {
		t.Errorf("hash = %s", h)
	}
}
