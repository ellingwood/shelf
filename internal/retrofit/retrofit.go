// Package retrofit re-organizes an existing library in place: every video is
// re-identified and re-named with the current templates, duplicate show and
// movie folders are merged, and sidecar files (subtitles, NFOs, thumbnails,
// trickplay folders, artwork) travel with what they belong to.
//
// Planning is separate from execution so a full dry run is always possible,
// and nothing is ever deleted: duplicates are moved aside, not removed.
package retrofit

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"shelf/internal/config"
	"shelf/internal/fileops"
	"shelf/internal/media"
	"shelf/internal/pipeline"
)

type Options struct {
	// Duplicates decides what happens when several files want the same
	// destination: "skip" leaves them all in place and reports them;
	// "keep-larger" keeps the largest and moves the rest to
	// <review_dir>/duplicates/.
	Duplicates string
	// Paths limits the scan to these directories/files inside the library.
	Paths []string
}

// Op is one planned rename.
type Op struct {
	Src, Dst string
	Kind     string // video | sidecar | folder-item | duplicate
	IsDir    bool
}

// Issue is something the plan deliberately leaves alone.
type Issue struct{ Path, Reason string }

type Plan struct {
	Root      string
	Ops       []Op
	Unchanged int
	Unmatched []Issue
	Conflicts []Issue
	Kept      []Issue
	Identical [][]string // sets of videos with the same content
	prune     map[string]bool
}

type video struct {
	src, dst string
	size     int64
	kind     media.Kind
	atDst    bool // already at its destination
}

// Build scans the library and plans every change without touching files.
func Build(ctx context.Context, p *pipeline.Pipeline, rule *config.Rule, opt Options) (*Plan, error) {
	root := filepath.Clean(rule.Output)
	r := *rule
	off := false
	r.Subtitles = &off       // retrofit moves sidecars generically, keeping their suffixes
	r.CheckDuplicates = &off // identical files are reported below instead

	files, err := collect(root, opt.Paths, rule.ReviewDir)
	if err != nil {
		return nil, err
	}
	plan := &Plan{Root: root, prune: map[string]bool{}}

	// 1. Decide a destination for every video.
	type planned struct {
		pl   *pipeline.Plan
		size int64
	}
	var plans []planned
	for _, f := range files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		fi, err := os.Stat(f)
		if err != nil {
			continue
		}
		pl, err := p.MakePlan(ctx, &r, f, root)
		switch {
		case err != nil:
			plan.Unmatched = append(plan.Unmatched, Issue{f, err.Error()})
			continue
		case pl.Review:
			plan.Unmatched = append(plan.Unmatched, Issue{f, pl.Reason})
			continue
		}
		plans = append(plans, planned{pl, fi.Size()})
	}

	// 1a. Videos with identical content: renamed as usual (the names are
	//     right, the content is wrong), but reported so they can be replaced.
	if rule.WantDupeCheck() {
		plan.Identical = p.Dupes.Groups(files)
	}

	// 1b. Copies of the same show/movie must end up together: if any file of
	//     a title+year carries a TMDB ID (e.g. from a "[tmdbid-N]" folder),
	//     give it to the copies that don't, and re-render their paths.
	ids := map[string]int{}
	for _, x := range plans {
		if k := titleKey(x.pl.Media); k != "" && x.pl.Media.TMDBID != 0 {
			ids[k] = x.pl.Media.TMDBID
		}
	}
	groups := map[string][]*video{}
	var order []string
	for _, x := range plans {
		m := x.pl.Media
		if id := ids[titleKey(m)]; m.TMDBID == 0 && id != 0 {
			m.TMDBID = id
			relPath, err := r.Renderer().Render(m)
			if err != nil {
				plan.Unmatched = append(plan.Unmatched, Issue{x.pl.Src, err.Error()})
				continue
			}
			x.pl.Dst = filepath.Join(root, relPath)
		}
		v := &video{src: x.pl.Src, dst: x.pl.Dst, size: x.size, kind: m.Kind, atDst: x.pl.Dst == x.pl.Src}
		// "Heat (1995).mkv" and "Heat (1995).mp4" are the same title.
		k := stemOf(v.dst)
		if _, ok := groups[k]; !ok {
			order = append(order, k)
		}
		groups[k] = append(groups[k], v)
	}

	// 2. Resolve collisions (several files → one name, or a name already taken).
	type loss struct{ v, winner *video }
	var winners []*video
	var losers []loss
	for _, key := range order {
		g := groups[key]
		// Videos not in this scan that already hold the name (any extension).
		for _, occ := range occupants(g) {
			if fi, err := os.Stat(occ); err == nil {
				g = append(g, &video{src: occ, dst: occ, size: fi.Size(), atDst: true})
			}
		}
		if len(g) == 1 {
			if g[0].atDst {
				plan.Unchanged++
			} else {
				winners = append(winners, g[0])
			}
			continue
		}
		win := pickWinner(g, opt.Duplicates)
		if win == nil {
			for _, v := range g {
				plan.Conflicts = append(plan.Conflicts, Issue{v.src, fmt.Sprintf("%d files for %s", len(g), rel(root, key))})
			}
			continue
		}
		if win.atDst {
			plan.Unchanged++
		} else {
			winners = append(winners, win)
		}
		for _, v := range g {
			if v != win {
				losers = append(losers, loss{v, win})
			}
		}
	}

	claimed := map[string]bool{}
	moving := map[string]bool{}
	for _, k := range order {
		for _, v := range groups[k] {
			claimed[v.dst] = true
		}
	}

	dirMap := map[string]map[string]bool{}
	mapDir := func(from, to string) {
		if from == to || depth(root, from) < 2 || strings.HasPrefix(to+"/", from+"/") {
			return
		}
		if dirMap[from] == nil {
			dirMap[from] = map[string]bool{}
		}
		dirMap[from][to] = true
	}

	// 3. Duplicates move aside first, so winners can take their names. The
	//    loser's folder extras (trailers, artwork) follow the kept copy.
	for _, l := range losers {
		dupDst := filepath.Join(rule.ReviewDir, "duplicates", rel(root, l.v.src))
		plan.add(Op{Src: l.v.src, Dst: dupDst, Kind: "duplicate"}, moving, claimed)
		// Sidecars shared by name with the kept copy (same folder, same
		// stem, other extension) stay for the kept copy.
		if stemOf(l.v.src) != stemOf(l.winner.src) {
			plan.addSidecars(l.v.src, dupDst, moving, claimed)
		}
		mapDir(filepath.Dir(l.v.src), filepath.Dir(l.winner.dst))
	}
	// 4. Videos and their sidecars.
	for _, v := range winners {
		plan.add(Op{Src: v.src, Dst: v.dst, Kind: "video"}, moving, claimed)
		plan.addSidecars(v.src, v.dst, moving, claimed)
		mapDir(filepath.Dir(v.src), filepath.Dir(v.dst))
		if v.kind == media.Episode { // season folder → show folder too
			mapDir(filepath.Dir(filepath.Dir(v.src)), filepath.Dir(filepath.Dir(v.dst)))
		}
	}

	// 5. Folder-level leftovers (posters, tvshow.nfo, season.nfo, extras...)
	//    follow when their folder is being emptied into exactly one new folder.
	olds := make([]string, 0, len(dirMap))
	for d := range dirMap {
		olds = append(olds, d)
	}
	sort.Slice(olds, func(i, j int) bool { return depth(root, olds[i]) > depth(root, olds[j]) })
	for _, old := range olds {
		if len(dirMap[old]) != 1 {
			plan.Kept = append(plan.Kept, Issue{old, "files from this folder go to several places; folder extras left in place"})
			continue
		}
		var newDir string
		for d := range dirMap[old] {
			newDir = d
		}
		if stay := videoStaying(old, moving); stay != "" {
			plan.Kept = append(plan.Kept, Issue{old, "still contains " + rel(root, stay) + "; folder extras left in place"})
			continue
		}
		entries, err := os.ReadDir(old)
		if err != nil {
			continue
		}
		for _, e := range entries {
			src := filepath.Join(old, e.Name())
			if moving[src] || (e.IsDir() && dirMap[src] != nil) {
				continue
			}
			dst := filepath.Join(newDir, e.Name())
			if claimed[dst] || exists(dst) {
				plan.Kept = append(plan.Kept, Issue{src, "already exists at " + rel(root, dst)})
				continue
			}
			plan.add(Op{Src: src, Dst: dst, Kind: "folder-item", IsDir: e.IsDir()}, moving, claimed)
		}
		plan.prune[old] = true
	}
	return plan, nil
}

// stemOf drops the extension: the identity of a title's file.
func stemOf(p string) string { return strings.TrimSuffix(p, filepath.Ext(p)) }

// occupants lists existing videos with the group's destination name (any
// extension) that are not part of the group.
func occupants(g []*video) []string {
	seen := map[string]bool{}
	for _, v := range g {
		seen[v.src] = true
	}
	var out []string
	for _, v := range g {
		for _, p := range append(pipeline.OtherVersions(v.dst), v.dst) {
			if !seen[p] && exists(p) {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	return out
}

// titleKey identifies a show or movie by kind, normalized title and year.
func titleKey(m media.Media) string {
	title, year := m.Title, m.Year
	if m.Kind == media.Episode {
		title, year = m.Show, m.ShowYear
	}
	if title == "" {
		return ""
	}
	var b strings.Builder
	for _, r := range strings.ToLower(title) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r > 127 {
			b.WriteRune(r)
		}
	}
	return fmt.Sprintf("%s|%s|%d", m.Kind, b.String(), year)
}

func (pl *Plan) add(op Op, moving, claimed map[string]bool) {
	pl.Ops = append(pl.Ops, op)
	moving[op.Src] = true
	claimed[op.Dst] = true
	pl.prune[filepath.Dir(op.Src)] = true
}

// addSidecars plans moves for files named after the video ("Ep.en.srt",
// "Ep.nfo", "Ep-thumb.jpg", "Ep.trickplay/"), keeping their suffixes.
func (pl *Plan) addSidecars(src, dst string, moving, claimed map[string]bool) {
	dir := filepath.Dir(src)
	stem := strings.TrimSuffix(filepath.Base(src), filepath.Ext(src))
	newStem := strings.TrimSuffix(filepath.Base(dst), filepath.Ext(dst))
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	// Other videos' stems, so "Ep S01E01-E02.en.srt" isn't claimed by "Ep S01E01".
	var stems []string
	for _, e := range entries {
		if !e.IsDir() && media.IsVideo(filepath.Ext(e.Name())) {
			stems = append(stems, strings.TrimSuffix(e.Name(), filepath.Ext(e.Name())))
		}
	}
	for _, e := range entries {
		name := e.Name()
		if name == filepath.Base(src) || (!e.IsDir() && media.IsVideo(filepath.Ext(name))) {
			continue
		}
		if !sidecarOf(name, stem) || longestStem(name, stems) != stem {
			continue
		}
		s := filepath.Join(dir, name)
		d := filepath.Join(filepath.Dir(dst), newStem+name[len(stem):])
		if s == d || moving[s] {
			continue
		}
		if claimed[d] || exists(d) {
			pl.Kept = append(pl.Kept, Issue{s, "sidecar destination already exists"})
			continue
		}
		pl.add(Op{Src: s, Dst: d, Kind: "sidecar", IsDir: e.IsDir()}, moving, claimed)
	}
}

func sidecarOf(name, stem string) bool {
	return len(name) > len(stem) && strings.HasPrefix(name, stem) &&
		(name[len(stem)] == '.' || name[len(stem)] == '-')
}

func longestStem(name string, stems []string) string {
	best := ""
	for _, s := range stems {
		if sidecarOf(name, s) && len(s) > len(best) {
			best = s
		}
	}
	return best
}

// pickWinner chooses which file keeps a contested name, or nil to leave the
// whole group alone.
func pickWinner(g []*video, policy string) *video {
	if policy == "keep-larger" {
		win := g[0]
		for _, v := range g[1:] {
			if v.size > win.size || (v.size == win.size && v.atDst) {
				win = v
			}
		}
		return win
	}
	return nil
}

// videoStaying returns a video under dir that is not planned to move.
func videoStaying(dir string, moving map[string]bool) string {
	var found string
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || found != "" {
			return filepath.SkipDir
		}
		if !d.IsDir() && media.IsVideo(filepath.Ext(p)) && !moving[p] && !isExtra(p) {
			found = p
		}
		return nil
	})
	return found
}

// Apply executes the plan. It keeps going after individual failures and
// returns them joined.
func (pl *Plan) Apply(p *pipeline.Pipeline, rule *config.Rule, out io.Writer) error {
	var errs []error
	for _, op := range pl.Ops {
		var res fileops.Result
		var err error
		if op.IsDir {
			err = moveDir(op.Src, op.Dst)
			res.Dst = op.Dst
		} else {
			res, err = fileops.Place(op.Src, op.Dst, "move", "skip")
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", rel(pl.Root, op.Src), err))
			continue
		}
		p.RecordMove(rule, op.Src, res)
	}
	// Remove folders emptied by the moves, never the category roots.
	dirs := make([]string, 0, len(pl.prune))
	for d := range pl.prune {
		dirs = append(dirs, d)
	}
	sort.Slice(dirs, func(i, j int) bool { return depth(pl.Root, dirs[i]) > depth(pl.Root, dirs[j]) })
	for _, d := range dirs {
		if depth(pl.Root, d) >= 2 {
			fileops.RemoveEmptyDirs(d, filepath.Join(pl.Root, strings.Split(rel(pl.Root, d), string(filepath.Separator))[0]))
		}
	}
	return errors.Join(errs...)
}

func moveDir(src, dst string) error {
	if exists(dst) {
		return fmt.Errorf("%w: %s", fileops.ErrSkipped, dst)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.Rename(src, dst)
}

// Print writes the plan for humans.
func (pl *Plan) Print(w io.Writer) {
	labels := map[string]string{"video": "rename", "sidecar": "  with", "folder-item": " merge", "duplicate": " aside"}
	for _, op := range pl.Ops {
		suffix := ""
		if op.IsDir {
			suffix = "/"
		}
		fmt.Fprintf(w, "%s  %s%s\n        → %s%s\n", labels[op.Kind], rel(pl.Root, op.Src), suffix, rel(pl.Root, op.Dst), suffix)
	}
	section := func(title string, issues []Issue) {
		if len(issues) == 0 {
			return
		}
		fmt.Fprintf(w, "\n%s (%d):\n", title, len(issues))
		for _, i := range issues {
			fmt.Fprintf(w, "  %s\n      %s\n", rel(pl.Root, i.Path), i.Reason)
		}
	}
	section("Not matched — left in place", pl.Unmatched)
	section("Conflicts — left in place (use -duplicates keep-larger to resolve)", pl.Conflicts)
	section("Kept in place", pl.Kept)
	if len(pl.Identical) > 0 {
		fmt.Fprintf(w, "\nIdentical content — these files hold the same video; re-download all but the right one (%d set(s)):\n", len(pl.Identical))
		for _, g := range pl.Identical {
			for i, f := range g {
				mark := "  ┌ "
				switch {
				case len(g) == 1:
				case i == len(g)-1:
					mark = "  └ "
				case i > 0:
					mark = "  │ "
				}
				fmt.Fprintf(w, "%s%s\n", mark, rel(pl.Root, f))
			}
		}
	}

	counts := map[string]int{}
	for _, op := range pl.Ops {
		counts[op.Kind]++
	}
	fmt.Fprintf(w, "\nSummary: %d video(s) to rename, %d sidecar(s), %d folder item(s), %d duplicate(s) set aside; %d already correct, %d unmatched, %d conflict(s), %d identical-content set(s).\n",
		counts["video"], counts["sidecar"], counts["folder-item"], counts["duplicate"], pl.Unchanged, len(pl.Unmatched), len(pl.Conflicts), len(pl.Identical))
}

// Collect lists the library videos Build would consider.
func Collect(root string, paths []string, reviewDir string) ([]string, error) {
	return collect(filepath.Clean(root), paths, reviewDir)
}

// collect finds library videos, skipping extras, trickplay and the review dir.
func collect(root string, paths []string, reviewDir string) ([]string, error) {
	if len(paths) == 0 {
		paths = []string{root}
	}
	var out []string
	for _, start := range paths {
		start = filepath.Clean(start)
		if start != root && !strings.HasPrefix(start, root+string(filepath.Separator)) {
			return nil, fmt.Errorf("%s is not inside the library %s", start, root)
		}
		err := filepath.WalkDir(start, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if p != start && (strings.HasPrefix(d.Name(), ".") || p == reviewDir || extraDirs[strings.ToLower(d.Name())] || strings.HasSuffix(d.Name(), ".trickplay")) {
					return filepath.SkipDir
				}
				return nil
			}
			if media.IsVideo(filepath.Ext(p)) && !isExtra(p) {
				out = append(out, p)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Strings(out)
	return out, nil
}

// Jellyfin extras folders and filename suffixes: never renamed as episodes/movies.
var extraDirs = map[string]bool{
	"extras": true, "featurettes": true, "behind the scenes": true, "deleted scenes": true,
	"interviews": true, "scenes": true, "shorts": true, "trailers": true, "other": true,
	"clips": true, "samples": true, "sample": true,
}

var extraSuffixes = []string{"-trailer", "-sample", "-featurette", "-behindthescenes", "-deleted", "-deletedscene", "-interview", "-scene", "-short", "-other", "-extra", "-clip"}

func isExtra(p string) bool {
	stem := strings.ToLower(strings.TrimSuffix(filepath.Base(p), filepath.Ext(p)))
	for _, s := range extraSuffixes {
		if strings.HasSuffix(stem, s) {
			return true
		}
	}
	return stem == "sample" || stem == "trailer"
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

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}
