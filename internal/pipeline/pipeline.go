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
	"strings"
	"sync"
	"time"

	"shelf/internal/config"
	"shelf/internal/fileops"
	"shelf/internal/history"
	"shelf/internal/jellyfin"
	"shelf/internal/match"
	"shelf/internal/media"
	"shelf/internal/parse"
	"shelf/internal/probe"
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
	}
	if rule.Action == "move" && rule.WantCleanupDirs() && rule.Watch != "" {
		fileops.RemoveEmptyDirs(filepath.Dir(plan.Src), rule.Watch)
		for _, s := range plan.Subs {
			fileops.RemoveEmptyDirs(filepath.Dir(s.Src), rule.Watch)
		}
	}
	return nil
}

// Record logs a completed operation to history (no-op in dry-run mode).
func (p *Pipeline) Record(rule *config.Rule, src string, res fileops.Result) {
	p.record(rule, src, res)
}

func (p *Pipeline) record(rule *config.Rule, src string, res fileops.Result) {
	if p.Hist == nil {
		return
	}
	if _, err := p.Hist.Record(history.Entry{
		Action: rule.Action, Src: src, Dst: res.Dst, Overwrote: res.Overwrote,
		Rule: rule.Name, OutputDir: rule.Output,
	}); err != nil {
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
	fmt.Fprintf(w, "%s  [%s]\n  %s → %s\n", filepath.Base(plan.Src), desc, verb, plan.Dst)
	for _, s := range plan.Subs {
		fmt.Fprintf(w, "    + %s → %s\n", filepath.Base(s.Src), filepath.Base(s.Dst))
	}
}
