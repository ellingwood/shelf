package subs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"shelf/internal/config"
	"shelf/internal/fileops"
	"shelf/internal/media"
	"shelf/internal/probe"
)

// Fetcher finds, downloads, re-times and places subtitles for videos.
type Fetcher struct {
	Client *Client
	Cfg    config.SubtitleDownload
	Sync   *Syncer // nil: no re-timing
	Log    *slog.Logger
	Probe  bool // check embedded subtitle tracks too
	// OnCreate is called for each subtitle file written.
	OnCreate func(job Job, path string, sub Subtitle)

	queue chan Job
}

// Job is one video to fetch subtitles for.
type Job struct {
	Video string
	Media media.Media
	Rule  *config.Rule
}

// Result describes the outcome for one language.
type Result struct {
	Lang   string
	Status string // downloaded | would-download | not-found | failed
	Sub    *Subtitle
	Score  float64
	Path   string
	Sync   string // ffsubsync summary, or why it was skipped
	Err    error
}

type Options struct {
	Languages []string // default: Cfg.Languages
	Force     bool     // fetch even if a subtitle exists (replacing it)
	DryRun    bool     // search and choose, don't download
}

func New(cfg config.SubtitleDownload, ffprobe string, log *slog.Logger) *Fetcher {
	f := &Fetcher{
		Client: NewClient(cfg.OpenSubtitles.APIKey, cfg.OpenSubtitles.Username, cfg.OpenSubtitles.Password),
		Cfg:    cfg,
		Log:    log,
		Probe:  probe.Available(),
	}
	if cfg.Sync != "never" {
		if bin := FindFFsubsync(cfg.FFsubsync); bin != "" {
			f.Sync = &Syncer{Bin: bin, FFmpegDir: FFmpegDirFor(ffprobe)}
		}
	}
	return f
}

// Missing returns the wanted languages that have neither a subtitle file
// named after the video nor (when probe is true) an embedded track.
func Missing(ctx context.Context, video string, langs []string, useProbe bool) []string {
	have := map[string]bool{}
	dir := filepath.Dir(video)
	stem := strings.TrimSuffix(filepath.Base(video), filepath.Ext(video))
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !media.IsSubtitle(filepath.Ext(name)) || !strings.HasPrefix(strings.ToLower(name), strings.ToLower(stem)+".") {
				continue
			}
			if l, forced := media.SubtitleLang(name, stem); l != "" && !forced {
				have[strings.ToLower(l)] = true
			}
		}
	}
	if useProbe {
		if tracks, err := probe.SubtitleLanguages(ctx, video); err == nil {
			for _, t := range tracks {
				if l := media.LangCode(t); l != "" {
					have[strings.ToLower(l)] = true
				}
			}
		}
	}
	var out []string
	for _, l := range langs {
		if !have[strings.ToLower(l)] {
			out = append(out, l)
		}
	}
	return out
}

// Fetch gets subtitles for one video. On a QuotaError it returns the
// results so far along with the error.
func (f *Fetcher) Fetch(ctx context.Context, job Job, opt Options) ([]Result, error) {
	langs := opt.Languages
	if len(langs) == 0 {
		langs = f.Cfg.Languages
	}
	if !opt.Force {
		langs = Missing(ctx, job.Video, langs, f.Probe)
	}
	if len(langs) == 0 {
		return nil, nil
	}
	q := queryFor(job.Media, langs)
	if h, err := Hash(job.Video); err == nil {
		q.Hash = h
	}
	cands, err := f.Client.Search(ctx, q)
	if err != nil {
		return nil, err
	}
	prefs := Prefs{HearingImpaired: f.Cfg.HearingImpaired, Source: job.Media.Source}

	var results []Result
	for _, lang := range langs {
		ranked := Ranked(cands, lang, prefs)
		if len(ranked) == 0 {
			results = append(results, Result{Lang: lang, Status: "not-found"})
			continue
		}
		best := ranked[0]
		r := Result{Lang: lang, Sub: &best, Score: Score(best, prefs), Path: f.destination(job.Video, lang, best)}
		if opt.DryRun {
			r.Status = "would-download"
			results = append(results, r)
			continue
		}
		if err := f.place(ctx, job, &r, opt.Force); err != nil {
			var qe *QuotaError
			if errors.As(err, &qe) {
				return results, err
			}
			r.Status, r.Err = "failed", err
		} else {
			r.Status = "downloaded"
			if f.OnCreate != nil {
				f.OnCreate(job, r.Path, best)
			}
		}
		results = append(results, r)
	}
	return results, nil
}

func queryFor(m media.Media, langs []string) Query {
	q := Query{Languages: langs, Episodic: m.Kind == media.Episode}
	if q.Episodic {
		q.ParentTMDBID, q.Season, q.Episode, q.Text = m.TMDBID, m.Season, m.Episode, m.Show
	} else {
		q.TMDBID, q.Text, q.Year = m.TMDBID, m.Title, m.Year
	}
	return q
}

// destination is "<video stem>.<lang>[.sdh].srt" next to the video.
func (f *Fetcher) destination(video, lang string, s Subtitle) string {
	suffix := "." + lang
	if s.HI {
		suffix += ".sdh"
	}
	return strings.TrimSuffix(video, filepath.Ext(video)) + suffix + ".srt"
}

// place downloads, optionally re-times, and moves the subtitle into place.
func (f *Fetcher) place(ctx context.Context, job Job, r *Result, force bool) error {
	data, err := f.Client.Download(ctx, r.Sub.FileID)
	if err != nil {
		return err
	}
	tmpDir, err := os.MkdirTemp("", "shelf-subs-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)
	raw := filepath.Join(tmpDir, "download.srt")
	if err := os.WriteFile(raw, data, 0o664); err != nil {
		return err
	}
	final := raw

	switch {
	case f.Cfg.Sync == "never":
		r.Sync = "off"
	case r.Sub.HashMatch && f.Cfg.Sync == "auto":
		r.Sync = "not needed (made for this file)"
	case f.Sync == nil:
		r.Sync = "skipped (ffsubsync not installed)"
	default:
		synced := filepath.Join(tmpDir, "synced.srt")
		summary, err := f.Sync.Sync(ctx, job.Video, raw, synced)
		if err != nil {
			r.Sync = "failed, kept original timing"
			f.Log.Warn("subtitle sync failed; keeping original timing", "video", job.Video, "err", err)
		} else {
			r.Sync, final = summary, synced
		}
	}
	os.Chmod(final, 0o664) // readable by Jellyfin; group-writable for a shared media group
	conflict := "skip"
	if force {
		conflict = "overwrite"
	}
	res, err := fileops.Place(final, r.Path, "move", conflict)
	if err != nil {
		return err
	}
	r.Path = res.Dst
	return nil
}

// Start runs a background worker for Submit. It waits out quota resets.
func (f *Fetcher) Start(ctx context.Context) {
	f.queue = make(chan Job, 1000)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case job := <-f.queue:
				f.runWithRetry(ctx, job)
			}
		}
	}()
}

// Submit fetches subtitles for a newly imported video: queued when the
// worker runs (daemon), otherwise right away.
func (f *Fetcher) Submit(ctx context.Context, job Job) {
	if f.queue == nil {
		f.run(ctx, job)
		return
	}
	select {
	case f.queue <- job:
	default:
		f.Log.Warn("subtitle queue full; run `shelf subs` later", "video", job.Video)
	}
}

func (f *Fetcher) runWithRetry(ctx context.Context, job Job) {
	for {
		var qe *QuotaError
		if err := f.run(ctx, job); !errors.As(err, &qe) {
			return
		}
		wait := time.Until(qe.Reset) + time.Minute
		if qe.Reset.IsZero() || wait <= 0 {
			wait = time.Hour
		}
		f.Log.Info("waiting for subtitle quota reset", "until", time.Now().Add(wait).Format(time.Kitchen))
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return
		}
	}
}

func (f *Fetcher) run(ctx context.Context, job Job) error {
	results, err := f.Fetch(ctx, job, Options{})
	for _, r := range results {
		switch r.Status {
		case "downloaded":
			f.Log.Info("subtitle downloaded", "file", r.Path, "downloads", r.Sub.Downloads, "release", r.Sub.Release, "sync", r.Sync)
		case "not-found":
			f.Log.Info("no subtitle found", "video", job.Video, "lang", r.Lang)
		case "failed":
			f.Log.Warn("subtitle download failed", "video", job.Video, "lang", r.Lang, "err", r.Err)
		}
	}
	if err != nil {
		f.Log.Warn("subtitle fetch failed", "video", job.Video, "err", err)
	}
	return err
}

// Describe is a one-line summary of a candidate for humans.
func (s Subtitle) Describe() string {
	var tags []string
	if s.HashMatch {
		tags = append(tags, "exact file match")
	}
	if s.HI {
		tags = append(tags, "SDH")
	}
	if s.Trusted {
		tags = append(tags, "trusted")
	}
	if s.Votes > 0 {
		tags = append(tags, fmt.Sprintf("rated %.1f (%d votes)", s.Rating, s.Votes))
	}
	desc := fmt.Sprintf("%q, %d downloads", s.Release, s.Downloads)
	if len(tags) > 0 {
		desc += ", " + strings.Join(tags, ", ")
	}
	return desc
}
