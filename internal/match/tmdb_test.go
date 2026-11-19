package match

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"shelf/internal/media"
)

func fakeTMDB(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("api_key") != "k" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/search/movie":
			switch strings.ToLower(r.URL.Query().Get("query")) {
			case "the matrix":
				w.Write([]byte(`{"results":[
					{"id":603,"title":"The Matrix","release_date":"1999-03-30"},
					{"id":604,"title":"The Matrix Reloaded","release_date":"2003-05-15"}]}`))
			case "heat":
				// A remake with the same title: the year must decide.
				w.Write([]byte(`{"results":[
					{"id":1,"title":"Heat","release_date":"1986-01-01"},
					{"id":949,"title":"Heat","release_date":"1995-12-15"}]}`))
			default:
				w.Write([]byte(`{"results":[{"id":9,"title":"Something Else Entirely","release_date":"2001-01-01"}]}`))
			}
		case "/search/tv":
			w.Write([]byte(`{"results":[{"id":1396,"name":"Breaking Bad","first_air_date":"2008-01-20"}]}`))
		case "/movie/603":
			w.Write([]byte(`{"id":603,"title":"The Matrix","release_date":"1999-03-30"}`))
		case "/tv/1396":
			w.Write([]byte(`{"id":1396,"name":"Breaking Bad","first_air_date":"2008-01-20"}`))
		case "/tv/1396/season/1":
			w.Write([]byte(`{"episodes":[{"episode_number":1,"name":"Pilot"},{"episode_number":2,"name":"Cat's in the Bag..."}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
}

func client(srv *httptest.Server) *Client {
	c := New("k", "en-US", 0.75, "")
	c.BaseURL = srv.URL
	return c
}

func TestMovieMatch(t *testing.T) {
	srv := fakeTMDB(t)
	defer srv.Close()
	c := client(srv)

	m := media.Media{Kind: media.Movie, Title: "the matrix", Year: 1999}
	if _, err := c.Enrich(context.Background(), &m); err != nil {
		t.Fatal(err)
	}
	if m.TMDBID != 603 || m.Title != "The Matrix" {
		t.Errorf("got %+v", m)
	}

	m = media.Media{Kind: media.Movie, Title: "Heat", Year: 1995}
	c.Enrich(context.Background(), &m)
	if m.TMDBID != 949 {
		t.Errorf("year should disambiguate Heat: got id %d", m.TMDBID)
	}

	m = media.Media{Kind: media.Movie, Title: "Totally Unknown Film", Year: 2010}
	if _, err := c.Enrich(context.Background(), &m); !errors.Is(err, ErrNoMatch) {
		t.Errorf("expected ErrNoMatch, got %v", err)
	}
	if m.TMDBID != 0 {
		t.Error("media should be untouched on no match")
	}
}

func TestEpisodeMatch(t *testing.T) {
	srv := fakeTMDB(t)
	defer srv.Close()
	m := media.Media{Kind: media.Episode, Show: "Breaking Bad", Season: 1, Episode: 2, Episodes: []int{2}}
	if _, err := client(srv).Enrich(context.Background(), &m); err != nil {
		t.Fatal(err)
	}
	if m.TMDBID != 1396 || m.ShowYear != 2008 || m.EpisodeTitle != "Cat's in the Bag..." {
		t.Errorf("got %+v", m)
	}
}

func TestUnauthorized(t *testing.T) {
	srv := fakeTMDB(t)
	defer srv.Close()
	c := client(srv)
	c.key = "wrong"
	m := media.Media{Kind: media.Movie, Title: "The Matrix"}
	if _, err := c.Enrich(context.Background(), &m); err == nil || errors.Is(err, ErrNoMatch) {
		t.Errorf("expected auth error, got %v", err)
	}
}

func TestSimilarity(t *testing.T) {
	if s := similarity(normTitle("The Office (US)"), normTitle("The Office")); s < 0.6 {
		t.Errorf("similarity too low: %f", s)
	}
	if s := similarity(normTitle("Fast & Furious"), normTitle("Fast and Furious")); s != 1 {
		t.Errorf("& should equal and: %f", s)
	}
}

func TestLookupByID(t *testing.T) {
	srv := fakeTMDB(t)
	defer srv.Close()
	c := client(srv)

	// Garbage title, but the ID from a folder tag is authoritative.
	m := media.Media{Kind: media.Movie, Title: "matrx", TMDBID: 603}
	if _, err := c.Enrich(context.Background(), &m); err != nil {
		t.Fatal(err)
	}
	if m.Title != "The Matrix" || m.Year != 1999 {
		t.Errorf("movie by id: %+v", m)
	}

	e := media.Media{Kind: media.Episode, Show: "bb", TMDBID: 1396, Season: 1, Episode: 2, Episodes: []int{2}}
	if _, err := c.Enrich(context.Background(), &e); err != nil {
		t.Fatal(err)
	}
	if e.Show != "Breaking Bad" || e.ShowYear != 2008 || e.EpisodeTitle != "Cat's in the Bag..." {
		t.Errorf("episode by id: %+v", e)
	}

	// A stale ID (404) falls back to searching by title.
	s := media.Media{Kind: media.Movie, Title: "The Matrix", Year: 1999, TMDBID: 999999}
	if _, err := c.Enrich(context.Background(), &s); err != nil || s.TMDBID != 603 {
		t.Errorf("stale id fallback: %v %+v", err, s)
	}
}
