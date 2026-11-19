// Package health inspects a library and reports problems without changing
// anything: files with identical content, several files claiming one
// episode or movie, names that don't follow the templates, missing
// episodes, unreadable files and leftovers from interrupted operations.
package health

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"shelf/internal/config"
	"shelf/internal/fileops"
	"shelf/internal/media"
	"shelf/internal/parse"
	"shelf/internal/pipeline"
	"shelf/internal/probe"
	"shelf/internal/retrofit"
	"shelf/internal/subs"
)

type Options struct {
	Paths []string // limit to these paths inside the library
	Probe bool     // run ffprobe on every video
	Now   time.Time
}

type Issue = retrofit.Issue

// Gap is a season missing episodes.
type Gap struct {
	Show     string // show folder, relative to the library
	Season   int
	Have     int
	Missing  []int
	FromTMDB bool // true: compared with aired episodes; false: holes in the numbering
}

type Report struct {
	Root       string
	Videos     int
	Bytes      int64
	Identical  [][]string
	Conflicts  []Issue
	Misnamed   []retrofit.Op
	Unmatched  []Issue
	Gaps       []Gap
	Unreadable []Issue
	Leftovers  []Issue
	InReview   int
	ReviewDir  string
	// MissingSubs lists videos without subtitles in the configured
	// languages (informational; only when subtitle_download is enabled).
	MissingSubs []Issue
}

// Problems counts the findings that need attention.
func (r *Report) Problems() int {
	return len(r.Identical) + len(r.Conflicts) + len(r.Misnamed) + len(r.Unmatched) +
		len(r.Gaps) + len(r.Unreadable) + len(r.Leftovers)
}

// Check builds a report. p must be a dry-run pipeline.
func Check(ctx context.Context, p *pipeline.Pipeline, rule *config.Rule, opt Options) (*Report, error) {
	if !p.DryRun {
		return nil, fmt.Errorf("health check needs a dry-run pipeline")
	}
	if opt.Now.IsZero() {
		opt.Now = time.Now()
	}
	root := filepath.Clean(rule.Output)
	rep := &Report{Root: root, ReviewDir: rule.ReviewDir}

	// Naming, collisions and identical content come from a retrofit plan.
	r := *rule
	on := true
	r.CheckDuplicates = &on
	plan, err := retrofit.Build(ctx, p, &r, retrofit.Options{Duplicates: "skip", Paths: opt.Paths})
	if err != nil {
		return nil, err
	}
	rep.Identical, rep.Conflicts, rep.Unmatched = plan.Identical, plan.Conflicts, plan.Unmatched
	for _, op := range plan.Ops {
		if op.Kind == "video" {
			rep.Misnamed = append(rep.Misnamed, op)
		}
	}

	files, err := retrofit.Collect(root, opt.Paths, rule.ReviewDir)
	if err != nil {
		return nil, err
	}
	for _, f := range files {
		fi, err := os.Stat(f)
		if err != nil {
			continue
		}
		rep.Videos++
		rep.Bytes += fi.Size()
		if fi.Size() == 0 {
			rep.Unreadable = append(rep.Unreadable, Issue{Path: f, Reason: "empty file"})
			continue
		}
		if opt.Probe {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			if err := probe.Check(ctx, f); err != nil {
				rep.Unreadable = append(rep.Unreadable, Issue{Path: f, Reason: err.Error()})
			} else if reason := pipeline.Incomplete(ctx, f, parse.Path(f, root)); reason != "" {
				rep.Unreadable = append(rep.Unreadable, Issue{Path: f, Reason: reason})
			}
		}
	}

	if sd := p.Cfg.SubtitleDownload; sd.Enabled {
		for _, f := range files {
			if miss := subs.Missing(ctx, f, sd.Languages, opt.Probe); len(miss) > 0 {
				rep.MissingSubs = append(rep.MissingSubs, Issue{Path: f, Reason: "no subtitles: " + strings.Join(miss, ", ")})
			}
		}
	}
	rep.Gaps = gaps(ctx, p, root, files, opt.Now)
	rep.Leftovers = leftovers(root, opt.Paths, rule.ReviewDir, files)
	rep.InReview = countFiles(rule.ReviewDir)
	return rep, nil
}

type seasonKey struct {
	show   string
	season int
}

// gaps finds missing episodes per season: against TMDB's aired episodes
// when the show folder carries a TMDB ID and TMDB is enabled, otherwise
// holes between episode 1 and the highest episode present.
func gaps(ctx context.Context, p *pipeline.Pipeline, root string, files []string, now time.Time) []Gap {
	have := map[seasonKey]map[int]bool{}
	ids := map[seasonKey]int{}
	var keys []seasonKey
	for _, f := range files {
		m := parse.Path(f, root)
		if m.Kind != media.Episode || m.Season == 0 {
			continue // specials are rarely complete; don't nag
		}
		showDir := filepath.Dir(f)
		if strings.HasPrefix(strings.ToLower(filepath.Base(showDir)), "season") {
			showDir = filepath.Dir(showDir)
		}
		k := seasonKey{rel(root, showDir), m.Season}
		if have[k] == nil {
			have[k] = map[int]bool{}
			keys = append(keys, k)
		}
		for _, e := range m.Episodes {
			have[k][e] = true
		}
		if m.TMDBID != 0 {
			ids[k] = m.TMDBID
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].show != keys[j].show {
			return keys[i].show < keys[j].show
		}
		return keys[i].season < keys[j].season
	})

	var out []Gap
	for _, k := range keys {
		g := Gap{Show: k.show, Season: k.season, Have: len(have[k])}
		if id := ids[k]; id != 0 && p.TMDB != nil {
			if eps, err := p.TMDB.SeasonEpisodes(ctx, id, k.season); err == nil && len(eps) > 0 {
				g.FromTMDB = true
				for _, e := range eps {
					if !e.Aired.IsZero() && !e.Aired.After(now) && !have[k][e.Number] {
						g.Missing = append(g.Missing, e.Number)
					}
				}
			}
		}
		if !g.FromTMDB {
			highest := 0
			for e := range have[k] {
				highest = max(highest, e)
			}
			for e := 1; e < highest; e++ {
				if !have[k][e] {
					g.Missing = append(g.Missing, e)
				}
			}
		}
		if len(g.Missing) > 0 {
			out = append(out, g)
		}
	}
	return out
}

// leftovers finds shelf temp files from interrupted copies and empty
// folders below the category level. Only categories that hold videos
// (Movies/, Shows/) are searched, so Jellyfin's own metadata/ folder or
// other tools' data next to them is left out.
func leftovers(root string, paths []string, reviewDir string, videos []string) []Issue {
	categories := map[string]bool{}
	for _, v := range videos {
		if r := rel(root, v); r != v {
			categories[strings.SplitN(r, string(filepath.Separator), 2)[0]] = true
		}
	}
	if len(paths) == 0 {
		for c := range categories {
			paths = append(paths, filepath.Join(root, c))
		}
		sort.Strings(paths)
	}
	var out []Issue
	for _, start := range paths {
		filepath.WalkDir(start, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				r := rel(root, p)
				if p == reviewDir || (r != "." && !categories[strings.SplitN(r, string(filepath.Separator), 2)[0]]) {
					return filepath.SkipDir
				}
				if depth(root, p) >= 2 {
					if entries, err := os.ReadDir(p); err == nil && len(entries) == 0 {
						out = append(out, Issue{Path: p, Reason: "empty folder"})
					}
				}
				return nil
			}
			if fileops.IsTemp(d.Name()) {
				out = append(out, Issue{Path: p, Reason: "temp file from an interrupted copy"})
			}
			return nil
		})
	}
	return out
}

func countFiles(dir string) int {
	n := 0
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			n++
		}
		return nil
	})
	return n
}

// Print writes the report for humans.
func (r *Report) Print(w io.Writer) {
	fmt.Fprintf(w, "Library %s: %d video(s), %s\n", r.Root, r.Videos, humanSize(r.Bytes))

	if len(r.Identical) > 0 {
		fmt.Fprintf(w, "\n✗ Identical content (%d set(s)): these files hold the same video, so all but one are wrong. Re-download them.\n", len(r.Identical))
		for _, g := range r.Identical {
			for i, f := range g {
				mark := "  │ "
				switch i {
				case 0:
					mark = "  ┌ "
				case len(g) - 1:
					mark = "  └ "
				}
				fmt.Fprintf(w, "%s%s\n", mark, rel(r.Root, f))
			}
		}
	}
	issues := func(icon, title string, list []Issue) {
		if len(list) == 0 {
			return
		}
		fmt.Fprintf(w, "\n%s %s (%d):\n", icon, title, len(list))
		for _, i := range list {
			fmt.Fprintf(w, "  %s\n      %s\n", rel(r.Root, i.Path), i.Reason)
		}
	}
	issues("✗", "Unreadable or incomplete", r.Unreadable)
	issues("✗", "Several files for one title: same episode or movie twice", r.Conflicts)

	if len(r.Gaps) > 0 {
		fmt.Fprintf(w, "\n! Missing episodes (%d season(s)):\n", len(r.Gaps))
		for _, g := range r.Gaps {
			how := "gaps in numbering"
			if g.FromTMDB {
				how = "aired on TMDB"
			}
			fmt.Fprintf(w, "  %s, season %d: missing %s  (have %d; %s)\n", g.Show, g.Season, episodeList(g.Missing), g.Have, how)
		}
	}
	if len(r.Misnamed) > 0 {
		fmt.Fprintf(w, "\n! Not named per template (%d), fix with: shelf retrofit\n", len(r.Misnamed))
		for _, op := range r.Misnamed {
			fmt.Fprintf(w, "  %s\n      → %s\n", rel(r.Root, op.Src), rel(r.Root, op.Dst))
		}
	}
	issues("!", "Not identified", r.Unmatched)
	issues("!", "Leftovers", r.Leftovers)
	if len(r.MissingSubs) > 0 {
		fmt.Fprintf(w, "\n· Missing subtitles (%d), fetch with: shelf subs -apply\n", len(r.MissingSubs))
		for _, i := range r.MissingSubs {
			fmt.Fprintf(w, "  %s  (%s)\n", rel(r.Root, i.Path), strings.TrimPrefix(i.Reason, "no subtitles: "))
		}
	}
	if r.InReview > 0 {
		fmt.Fprintf(w, "\n· %d file(s) waiting in %s (see: shelf cleanup)\n", r.InReview, r.ReviewDir)
	}

	if n := r.Problems(); n == 0 {
		fmt.Fprintln(w, "\n✓ No problems found.")
	} else {
		fmt.Fprintf(w, "\n%d problem(s) found.\n", n)
	}
}

// episodeList renders [1 2 3 7] as "E01-E03, E07".
func episodeList(eps []int) string {
	var parts []string
	for i := 0; i < len(eps); {
		j := i
		for j+1 < len(eps) && eps[j+1] == eps[j]+1 {
			j++
		}
		if j > i {
			parts = append(parts, fmt.Sprintf("E%02d-E%02d", eps[i], eps[j]))
		} else {
			parts = append(parts, fmt.Sprintf("E%02d", eps[i]))
		}
		i = j + 1
	}
	return strings.Join(parts, ", ")
}

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func rel(root, p string) string {
	if r, err := filepath.Rel(root, p); err == nil && !strings.HasPrefix(r, "..") {
		return r
	}
	return p
}

func depth(root, p string) int {
	r := rel(root, p)
	if r == "." || r == p {
		return 0
	}
	return strings.Count(r, string(filepath.Separator)) + 1
}
