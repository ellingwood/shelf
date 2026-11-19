// Package pipeline turns one incoming file into a placed, named, recorded
// and announced library item.
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"shelf/internal/config"
	"shelf/internal/dupe"
	"shelf/internal/fileops"
	"shelf/internal/history"
	"shelf/internal/jellyfin"
	"shelf/internal/match"
	"shelf/internal/media"
	"shelf/internal/parse"
	"shelf/internal/probe"
	"shelf/internal/subs"
)

// ErrSkip means the file was deliberately left alone.
var ErrSkip = errors.New("skipped")

type Pipeline struct {
	Cfg    *config.Config
	TMDB   *match.Client      // nil = offline
	Jelly  *jellyfin.Notifier // nil = disabled
	Hist   *history.Store
	Log    *slog.Logger
	DryRun bool
	Out    io.Writer // where plans are printed (dry run / rename)
	Probe  bool
	Dupes  dupe.Cache
	Subs   *subs.Fetcher // nil: subtitle download disabled

	mu     sync.Mutex
	failed map[string]time.Time // path → mtime of a deterministic failure
}

// Plan is everything decided about a file before anything is touched.
type Plan struct {
	Rule      *config.Rule
	Media     media.Media
	Src, Dst  string
	Subs      []SubPlan
	Review    bool
	Reason    string // why it goes to review
	Candidate *match.Candidate
	// Replaces lists library videos with the same name but another
	// extension ("Heat (1995).mp4" for a new "Heat (1995).mkv") that the
	// conflict policy says to set aside under <review_dir>/replaced/.
	Replaces []string
	// Blocked is set when such a video exists and the conflict policy
	// keeps it; the new file is then left where it is.
	Blocked string
}

type SubPlan struct{ Src, Dst string }

// New builds a pipeline from config, wiring optional services.
func New(cfg *config.Config, log *slog.Logger, out io.Writer, dryRun, offline bool) (*Pipeline, error) {
	p := &Pipeline{Cfg: cfg, Log: log, Out: out, DryRun: dryRun, failed: map[string]time.Time{}}
	if cfg.TMDB.APIKey != "" && !offline {
		p.TMDB = match.New(cfg.TMDB.APIKey, cfg.TMDB.Language, cfg.TMDB.MinConfidence, cfg.CacheDir)
	}
	if cfg.Jellyfin.Refresh && !dryRun {
		p.Jelly = jellyfin.New(cfg.Jellyfin, log)
	}
	if cfg.Probe != "never" && probe.Find(cfg.FFprobe) == "" && cfg.FFprobe != "" {
		return nil, fmt.Errorf("ffprobe: %s not found or not executable", cfg.FFprobe)
	}
	switch cfg.Probe {
	case "always":
		if !probe.Available() {
			return nil, fmt.Errorf("probe: always, but ffprobe is not installed")
		}
		p.Probe = true
	case "auto":
		p.Probe = probe.Available()
	}
	// Created whenever credentials exist, so `shelf subs` works even with
	// download-on-import off; Execute checks WantOnImport.
	if o := cfg.SubtitleDownload.OpenSubtitles; o.APIKey != "" && o.Username != "" && o.Password != "" {
		p.Subs = subs.New(cfg.SubtitleDownload, probe.Bin, log)
		p.Subs.Probe = p.Probe
		p.Subs.OnCreate = p.subtitleCreated
	}
	if !dryRun {
		h, err := history.Open(cfg.HistoryFile)
		if err != nil {
			return nil, err
		}
		p.Hist = h
	}
	return p, nil
}

// MakePlan decides where a file goes. root bounds parent-directory lookups.
func (p *Pipeline) MakePlan(ctx context.Context, rule *config.Rule, path, root string) (*Plan, error) {
	m := parse.Path(path, root)
	plan := &Plan{Rule: rule, Src: path}

	switch rule.Type {
	case "movie":
		if m.Kind == media.Episode && m.Title == "" {
			m.Title = m.Show
		}
		m.Kind = media.Movie
	case "tv":
		if m.Kind != media.Episode {
			plan.Review, plan.Reason = true, "rule expects TV but no season/episode found"
		}
	}
	if m.Kind == media.Unknown && !plan.Review {
		plan.Review, plan.Reason = true, "could not parse a title"
	}

	if p.TMDB != nil && !plan.Review {
		cand, err := p.TMDB.Enrich(ctx, &m)
		plan.Candidate = cand
		switch {
		case errors.Is(err, match.ErrNoMatch):
			reason := "no TMDB match"
			if cand != nil {
				reason = fmt.Sprintf("best TMDB match %q (%d) scored %.2f < %.2f", cand.Title, cand.Year, cand.Score, p.Cfg.TMDB.MinConfidence)
			}
			switch p.Cfg.TMDB.OnNoMatch {
			case "review":
				plan.Review, plan.Reason = true, reason
			case "skip":
				return plan, fmt.Errorf("%w: %s", ErrSkip, reason)
			case "parsed":
				p.Log.Warn("using parsed name", "file", path, "reason", reason)
			}
		case err != nil:
			return plan, err // transient (network/auth): retried later
		}
	}

	if p.Probe && !plan.Review {
		if _, err := os.Stat(path); err == nil {
			if err := probe.Enrich(ctx, &m); err != nil {
				p.Log.Warn("probe failed; using filename info", "file", path, "err", err)
			}
		}
	}
	plan.Media = m

	if plan.Review {
		plan.Dst = filepath.Join(rule.ReviewDir, filepath.Base(path))
	} else {
		rel, err := rule.Renderer().Render(m)
		if err != nil {
			return plan, err
		}
		plan.Dst = filepath.Join(rule.Output, rel)
		if rule.WantDupeCheck() && rule.Action != "hardlink" && rule.Action != "symlink" {
			if other := p.findDuplicate(rule, path, plan.Dst); other != "" {
				r, _ := filepath.Rel(rule.Output, other)
				plan.Review, plan.Reason = true, "same video content as "+r
				plan.Dst = filepath.Join(rule.ReviewDir, filepath.Base(path))
			}
		}
	}

	if !plan.Review {
		p.planVersions(plan)
	}

	if rule.WantSubtitles() {
		videoStem := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
		dstStem := strings.TrimSuffix(plan.Dst, filepath.Ext(plan.Dst))
		used := map[string]int{}
		for _, s := range findSubtitles(path) {
			var dst string
			if plan.Review {
				dst = filepath.Join(rule.ReviewDir, filepath.Base(s))
			} else {
				suffix := subtitleSuffix(s, videoStem)
				ext := strings.ToLower(filepath.Ext(s))
				key := suffix + ext
				used[key]++
				if n := used[key]; n > 1 {
					suffix = fmt.Sprintf("%s.%d", suffix, n)
				}
				dst = dstStem + suffix + ext
			}
			plan.Subs = append(plan.Subs, SubPlan{Src: s, Dst: dst})
		}
	}
	return plan, nil
}

// findDuplicate looks for a video with the same content as path in the
// library category (e.g. Shows/) that dst goes into. The destination itself
// is ignored: replacing a file with an identical copy is the conflict
// policy's business.
func (p *Pipeline) findDuplicate(rule *config.Rule, path, dst string) string {
	rel, err := filepath.Rel(rule.Output, dst)
	if err != nil || strings.HasPrefix(rel, "..") {
		return ""
	}
	scope := rule.Output
	if i := strings.IndexRune(rel, filepath.Separator); i > 0 {
		scope = filepath.Join(rule.Output, rel[:i])
	}
	found, err := p.Dupes.FindIn(path, scope, dst)
	if errors.Is(err, os.ErrNotExist) {
		return "" // previewing a name, not a file
	}
	if err != nil {
		p.Log.Warn("duplicate check failed", "file", path, "err", err)
		return ""
	}
	return found
}

// OtherVersions returns videos next to dst with the same name but a
// different extension, e.g. "Heat (1995).mp4" for "Heat (1995).mkv". The
// exact name and skip paths are not included.
func OtherVersions(dst string, skip ...string) []string {
	dir := filepath.Dir(dst)
	stem := strings.TrimSuffix(filepath.Base(dst), filepath.Ext(dst))
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		ext := filepath.Ext(name)
		if e.IsDir() || !media.IsVideo(ext) || strings.TrimSuffix(name, ext) != stem || name == filepath.Base(dst) {
			continue
		}
		p := filepath.Join(dir, name)
		if !slices.Contains(skip, p) {
			out = append(out, p)
		}
	}
	return out
}

// planVersions applies the conflict policy to same-name videos with another
// extension, which the filesystem doesn't see as a collision.
func (p *Pipeline) planVersions(plan *Plan) {
	others := OtherVersions(plan.Dst, plan.Src)
	if len(others) == 0 {
		return
	}
	names := make([]string, len(others))
	for i, o := range others {
		names[i] = filepath.Base(o)
	}
	switch plan.Rule.Conflict {
	case "overwrite":
		plan.Replaces = others
	case "keep-larger":
		src, err := os.Stat(plan.Src)
		if err != nil {
			return // dry-run preview of a name that doesn't exist
		}
		for _, o := range others {
			if fi, err := os.Stat(o); err == nil && fi.Size() >= src.Size() {
				plan.Blocked = fmt.Sprintf("library already has %s, which is not smaller (conflict: keep-larger)", filepath.Base(o))
				return
			}
		}
		plan.Replaces = others
	case "suffix":
		// keep both
	default:
		plan.Blocked = fmt.Sprintf("library already has %s (conflict: skip)", strings.Join(names, ", "))
	}
}

// setAside moves a replaced video and its "<stem>." sidecars (subtitles,
// NFO, trickplay) to <review_dir>/replaced/, mirroring their library path.
func (p *Pipeline) setAside(rule *config.Rule, video string) error {
	dir := filepath.Dir(video)
	stem := strings.TrimSuffix(filepath.Base(video), filepath.Ext(video))
	items := []string{video}
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			name := e.Name()
			// Other videos with this stem are other versions, not sidecars.
			if strings.HasPrefix(name, stem+".") && name != filepath.Base(video) && (e.IsDir() || !media.IsVideo(filepath.Ext(name))) {
				items = append(items, filepath.Join(dir, name))
			}
		}
	}
	for _, src := range items {
		rel, err := filepath.Rel(rule.Output, src)
		if err != nil || strings.HasPrefix(rel, "..") {
			rel = filepath.Base(src)
		}
		dst := filepath.Join(rule.ReviewDir, "replaced", rel)
		var res fileops.Result
		if fi, err := os.Stat(src); err == nil && fi.IsDir() {
			dst = freeDir(dst)
			if err = os.MkdirAll(filepath.Dir(dst), 0o775); err == nil {
				err = os.Rename(src, dst)
			}
			if err != nil {
				return err
			}
			res.Dst = dst
		} else if res, err = fileops.Place(src, dst, "move", "suffix"); err != nil {
			return err
		}
		p.recordAction("move", rule, src, res)
		p.Log.Info("set aside", "file", src, "to", res.Dst)
		if src == video {
			p.Jelly.NotifyDeleted(src)
		}
	}
	return nil
}

func freeDir(p string) string {
	if _, err := os.Lstat(p); errors.Is(err, os.ErrNotExist) {
		return p
	}
	for i := 1; ; i++ {
		c := fmt.Sprintf("%s (%d)", p, i)
		if _, err := os.Lstat(c); errors.Is(err, os.ErrNotExist) {
			return c
		}
	}
}

// Process plans and executes one file.
func (p *Pipeline) Process(ctx context.Context, rule *config.Rule, path, root string) error {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil // already handled (e.g. duplicate event)
		}
		return err
	}
	if p.knownFailure(path, info.ModTime()) {
		return nil
	}

	plan, err := p.MakePlan(ctx, rule, path, root)
	if err != nil {
		if errors.Is(err, ErrSkip) {
			p.Log.Info("skipped", "file", path, "reason", err)
			p.markFailed(path, info.ModTime())
			return nil
		}
		if !isTransient(err) {
			p.markFailed(path, info.ModTime())
		}
		return err
	}
	return p.Execute(ctx, plan)
}

// Execute carries out a plan (or prints it in dry-run mode).
func (p *Pipeline) Execute(ctx context.Context, plan *Plan) error {
	if p.Out != nil {
		PrintPlan(p.Out, plan, p.DryRun)
	}
	if p.DryRun {
		return nil
	}
	rule := plan.Rule
	if plan.Blocked != "" {
		if info, serr := os.Stat(plan.Src); serr == nil {
			p.markFailed(plan.Src, info.ModTime())
		}
		p.Log.Warn("not placed", "file", plan.Src, "reason", plan.Blocked)
		return nil
	}
	for _, old := range plan.Replaces {
		if err := p.setAside(rule, old); err != nil {
			return fmt.Errorf("setting aside %s: %w", old, err)
		}
	}
	res, err := fileops.Place(plan.Src, plan.Dst, rule.Action, rule.Conflict)
	if err != nil {
		if info, serr := os.Stat(plan.Src); serr == nil {
			p.markFailed(plan.Src, info.ModTime())
		}
		if errors.Is(err, fileops.ErrSkipped) {
			p.Log.Warn("not placed", "file", plan.Src, "reason", err)
			return nil
		}
		return err
	}
	if res.Noop {
		p.Log.Info("already in place", "file", plan.Dst)
		return nil
	}
	p.record(rule, plan.Src, res)
	level := slog.LevelInfo
	if p.Out != nil {
		level = slog.LevelDebug // the plan was already printed
	}
	msg := "imported"
	if plan.Review {
		level, msg = slog.LevelWarn, "needs review"
	}
	p.Log.Log(ctx, level, msg, "src", plan.Src, "dst", res.Dst, "action", rule.Action, "reason", plan.Reason)

	for _, s := range plan.Subs {
		sres, err := fileops.Place(s.Src, s.Dst, rule.Action, rule.Conflict)
		if err != nil {
			p.Log.Warn("subtitle not placed", "file", s.Src, "err", err)
			continue
		}
		if !sres.Noop {
			p.record(rule, s.Src, sres)
		}
	}

	if !plan.Review {
		p.Jelly.Notify(res.Dst)
		if p.Subs != nil && p.Cfg.SubtitleDownload.WantOnImport() {
			p.Subs.Submit(ctx, subs.Job{Video: res.Dst, Media: plan.Media, Rule: rule})
		}
	}
	if rule.Action == "move" && rule.WantCleanupDirs() && rule.Watch != "" {
		fileops.RemoveEmptyDirs(filepath.Dir(plan.Src), rule.Watch)
		for _, s := range plan.Subs {
			fileops.RemoveEmptyDirs(filepath.Dir(s.Src), rule.Watch)
		}
	}
	return nil
}

// RecordMove logs a completed move to history (no-op in dry-run mode).
func (p *Pipeline) RecordMove(rule *config.Rule, src string, res fileops.Result) {
	p.recordAction("move", rule, src, res)
}

func (p *Pipeline) record(rule *config.Rule, src string, res fileops.Result) {
	p.recordAction(rule.Action, rule, src, res)
}

// subtitleCreated records a downloaded subtitle (so undo can remove it)
// and tells Jellyfin.
func (p *Pipeline) subtitleCreated(job subs.Job, path string, sub subs.Subtitle) {
	p.recordAction("create", job.Rule, fmt.Sprintf("opensubtitles:%d", sub.FileID), fileops.Result{Dst: path})
	p.Jelly.Notify(path)
}

func (p *Pipeline) recordAction(action string, rule *config.Rule, src string, res fileops.Result) {
	if p.Hist == nil {
		return
	}
	e := history.Entry{Action: action, Src: src, Dst: res.Dst, Overwrote: res.Overwrote}
	if rule != nil {
		e.Rule, e.OutputDir = rule.Name, rule.Output
	}
	if _, err := p.Hist.Record(e); err != nil {
		p.Log.Error("history write failed", "err", err)
	}
}

// Flush sends pending Jellyfin notifications (call before exiting).
func (p *Pipeline) Flush(ctx context.Context) { p.Jelly.Flush(ctx) }

func (p *Pipeline) knownFailure(path string, mtime time.Time) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	t, ok := p.failed[path]
	return ok && t.Equal(mtime)
}

func (p *Pipeline) markFailed(path string, mtime time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.failed[path] = mtime
}

// isTransient reports errors worth retrying on the next scan.
func isTransient(err error) bool {
	s := err.Error()
	return strings.Contains(s, "tmdb") || errors.Is(err, context.DeadlineExceeded)
}

// PrintPlan writes a human-readable summary of a plan.
func PrintPlan(w io.Writer, plan *Plan, dryRun bool) {
	verb := map[string]string{"move": "move", "copy": "copy", "hardlink": "link", "symlink": "symlink"}[plan.Rule.Action]
	if dryRun {
		verb = "would " + verb
	}
	m := plan.Media
	desc := m.Kind.String()
	if m.TMDBID != 0 {
		desc += fmt.Sprintf(", tmdb %d", m.TMDBID)
	}
	if plan.Review {
		desc = "REVIEW: " + plan.Reason
	}
	if plan.Blocked != "" {
		fmt.Fprintf(w, "%s  [%s]\n  not placed: %s\n", filepath.Base(plan.Src), desc, plan.Blocked)
		return
	}
	fmt.Fprintf(w, "%s  [%s]\n  %s → %s\n", filepath.Base(plan.Src), desc, verb, plan.Dst)
	for _, r := range plan.Replaces {
		fmt.Fprintf(w, "    replaces %s (set aside in review/replaced)\n", filepath.Base(r))
	}
	for _, s := range plan.Subs {
		fmt.Fprintf(w, "    + %s → %s\n", filepath.Base(s.Src), filepath.Base(s.Dst))
	}
}
