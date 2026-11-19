// Command shelf watches download folders and files media into a library
// using Go templates, with a built-in Jellyfin layout.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"shelf/internal/cleanup"
	"shelf/internal/config"
	"shelf/internal/health"
	"shelf/internal/history"
	"shelf/internal/jellyfin"
	"shelf/internal/match"
	"shelf/internal/media"
	"shelf/internal/parse"
	"shelf/internal/pipeline"
	"shelf/internal/probe"
	"shelf/internal/retrofit"
	"shelf/internal/subs"
	"shelf/internal/watch"
)

const usage = `shelf — organize media files for Jellyfin

Usage:
  shelf watch   [flags]              watch configured folders (daemon)
  shelf rename  [flags] <paths...>   organize files/folders once
  shelf preview [flags] <names...>   show how names parse and where they'd go
  shelf retrofit [-apply] [paths...] re-organize an existing library in place
  shelf subs    [-apply] [paths...]  download missing subtitles (OpenSubtitles)
  shelf cleanup [-delete]           list (and delete) review-folder items
  shelf health  [-probe] [paths...]  report library problems (duplicates, gaps...)
  shelf history [-n N]               list recent operations
  shelf undo    [-last N | -id ID]   reverse operations
  shelf check                        validate config and test TMDB/Jellyfin
  shelf init    [-force]             write a starter config

Common flags:
  -config PATH   config file (default %s)
  -v             debug logging

Run "shelf <command> -h" for command flags.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, usage, config.DefaultPath())
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch cmd {
	case "watch":
		err = cmdWatch(ctx, args)
	case "rename":
		err = cmdRename(ctx, args)
	case "preview":
		err = cmdPreview(ctx, args)
	case "retrofit":
		err = cmdRetrofit(ctx, args)
	case "subs":
		err = cmdSubs(ctx, args)
	case "cleanup":
		err = cmdCleanup(args)
	case "health":
		err = cmdHealth(ctx, args)
	case "history":
		err = cmdHistory(args)
	case "undo":
		err = cmdUndo(args)
	case "check":
		err = cmdCheck(ctx, args)
	case "init":
		err = cmdInit(args)
	case "-h", "--help", "help":
		fmt.Printf(usage, config.DefaultPath())
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n"+usage, cmd, config.DefaultPath())
		os.Exit(2)
	}
	if errors.Is(err, errProblems) {
		os.Exit(1)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// errProblems makes a command exit with status 1 after it has already
// reported why (e.g. health found problems).
var errProblems = errors.New("problems found")

type common struct {
	fs      *flag.FlagSet
	cfgPath *string
	verbose *bool
}

func newFlags(name string) common {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	return common{
		fs:      fs,
		cfgPath: fs.String("config", config.DefaultPath(), "config file"),
		verbose: fs.Bool("v", false, "debug logging"),
	}
}

func (c common) logger() *slog.Logger {
	lvl := slog.LevelInfo
	if *c.verbose {
		lvl = slog.LevelDebug
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
}

// load reads the config; if optional and the file is missing, defaults are used.
func (c common) load(optional bool) (*config.Config, error) {
	cfg, err := config.Load(*c.cfgPath)
	if errors.Is(err, fs.ErrNotExist) {
		if optional {
			return config.Default(), nil
		}
		return nil, fmt.Errorf("no config at %s (run \"shelf init\" or pass -config)", *c.cfgPath)
	}
	return cfg, err
}

func cmdWatch(ctx context.Context, args []string) error {
	f := newFlags("watch")
	dry := f.fs.Bool("dry-run", false, "log what would happen without touching files")
	offline := f.fs.Bool("offline", false, "don't query TMDB")
	f.fs.Parse(args)
	log := f.logger()

	cfg, err := f.load(false)
	if err != nil {
		return err
	}
	if len(cfg.Rules) == 0 {
		return errors.New("config has no rules")
	}
	p, err := pipeline.New(cfg, log, nil, *dry, *offline)
	if err != nil {
		return err
	}
	if *dry {
		p.Out = os.Stdout
	}
	subsState := "off"
	if p.Subs != nil {
		subsState = "on, no sync"
		if p.Subs.Sync != nil {
			subsState = "on, ffsubsync"
		}
		if !*dry {
			p.Subs.Start(ctx)
		}
	}
	log.Info("shelf starting", "tmdb", p.TMDB != nil, "ffprobe", probe.Bin, "jellyfin", p.Jelly != nil, "subtitles", subsState, "dry_run", *dry)

	w, err := watch.New(cfg, log, func(ctx context.Context, rule *config.Rule, path string) {
		if err := p.Process(ctx, rule, path, rule.Watch); err != nil {
			log.Error("failed", "file", path, "err", err)
		}
	})
	if err != nil {
		return err
	}
	err = w.Run(ctx)
	flushCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	p.Flush(flushCtx)
	log.Info("shelf stopped")
	return err
}

func cmdRename(ctx context.Context, args []string) error {
	f := newFlags("rename")
	dry := f.fs.Bool("dry-run", false, "show the plan without touching files")
	offline := f.fs.Bool("offline", false, "don't query TMDB")
	ruleName := f.fs.String("rule", "", "rule to use (default: rule watching the path, else the first)")
	output := f.fs.String("output", "", "override output directory")
	action := f.fs.String("action", "", "override action: move|copy|hardlink|symlink")
	f.fs.Parse(args)
	if f.fs.NArg() == 0 {
		return errors.New("rename needs at least one path")
	}
	log := f.logger()

	cfg, err := f.load(*output != "")
	if err != nil {
		return err
	}
	p, err := pipeline.New(cfg, log, os.Stdout, *dry, *offline)
	if err != nil {
		return err
	}
	defer p.Flush(context.Background())

	var failures int
	for _, arg := range f.fs.Args() {
		abs, err := filepath.Abs(arg)
		if err != nil {
			return err
		}
		rule, err := pickRule(cfg, *ruleName, abs, *output, *action)
		if err != nil {
			return err
		}
		root := rule.Watch
		fi, err := os.Stat(abs)
		if err != nil {
			return err
		}
		if root == "" || !within(abs, root) {
			root = filepath.Dir(abs) // a folder argument's own name is used for parsing
			if !fi.IsDir() {
				root = ""
			}
		}
		for _, file := range collect(cfg, rule, abs, fi) {
			if err := p.Process(ctx, rule, file, root); err != nil {
				log.Error("failed", "file", file, "err", err)
				failures++
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
		}
	}
	if failures > 0 {
		return fmt.Errorf("%d file(s) failed", failures)
	}
	return nil
}

// pickRule chooses a rule for a one-shot path, applying CLI overrides.
func pickRule(cfg *config.Config, name, path, output, action string) (*config.Rule, error) {
	var rule *config.Rule
	for i := range cfg.Rules {
		r := &cfg.Rules[i]
		if (name != "" && r.Name == name) || (name == "" && within(path, r.Watch)) {
			rule = r
			break
		}
	}
	if rule == nil && name != "" {
		return nil, fmt.Errorf("no rule named %q", name)
	}
	if rule == nil && len(cfg.Rules) > 0 {
		rule = &cfg.Rules[0]
	}
	cp := config.Rule{}
	if rule != nil {
		cp = *rule
	} else {
		cp = config.Rule{Name: "cli", Action: cfg.Action, Conflict: cfg.Conflict, Type: "auto"}
	}
	if output != "" {
		abs, err := filepath.Abs(config.ExpandHome(output))
		if err != nil {
			return nil, err
		}
		cp.Output = abs
		cp.ReviewDir = filepath.Join(abs, ".shelf-review")
	}
	if action != "" {
		cp.Action = action
	}
	if cp.Output == "" {
		return nil, errors.New("no output directory: add a rule to the config or pass -output")
	}
	if len(cp.Extensions) == 0 {
		for _, e := range media.VideoExts {
			cp.Extensions = append(cp.Extensions, e)
		}
	}
	if err := cfg.PrepareRule(&cp); err != nil {
		return nil, err
	}
	return &cp, nil
}

// collect lists the video files to process for a path argument.
func collect(cfg *config.Config, rule *config.Rule, path string, fi fs.FileInfo) []string {
	w, _ := watch.New(cfg, slog.Default(), nil)
	if !fi.IsDir() {
		return []string{path}
	}
	var out []string
	filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p != path && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if w.Accept(rule, p) {
			if info, err := d.Info(); err == nil && info.Size() >= int64(cfg.MinSize) {
				out = append(out, p)
			}
		}
		return nil
	})
	sort.Strings(out)
	return out
}

func within(path, dir string) bool {
	if dir == "" {
		return false
	}
	dir = filepath.Clean(dir)
	return path == dir || strings.HasPrefix(path, dir+string(filepath.Separator))
}

func cmdPreview(ctx context.Context, args []string) error {
	f := newFlags("preview")
	offline := f.fs.Bool("offline", false, "don't query TMDB")
	ruleName := f.fs.String("rule", "", "rule whose templates to use")
	f.fs.Parse(args)
	if f.fs.NArg() == 0 {
		return errors.New("preview needs at least one filename")
	}
	cfg, err := f.load(true)
	if err != nil {
		return err
	}
	out := "/library"
	if len(cfg.Rules) > 0 {
		out = ""
	}
	rule, err := pickRule(cfg, *ruleName, "", out, "")
	if err != nil {
		return err
	}
	no := false
	rule.Subtitles = &no

	var tmdb *match.Client
	if cfg.TMDB.APIKey != "" && !*offline {
		tmdb = match.New(cfg.TMDB.APIKey, cfg.TMDB.Language, cfg.TMDB.MinConfidence, cfg.CacheDir)
	}
	probe.Find(cfg.FFprobe)
	p := &pipeline.Pipeline{Cfg: cfg, TMDB: tmdb, Log: f.logger(), Probe: cfg.Probe != "never" && probe.Available()}

	for _, name := range f.fs.Args() {
		path := name
		if abs, err := filepath.Abs(name); err == nil {
			path = abs
		}
		root := ""
		if _, err := os.Stat(path); err != nil {
			root = filepath.Dir(path) // nonexistent: parse the name only
		}
		parsed := parse.Path(path, root)
		plan, err := p.MakePlan(ctx, rule, path, root)
		fmt.Printf("%s\n", name)
		printMedia("  parsed ", parsed)
		if err != nil {
			fmt.Printf("  error   %v\n\n", err)
			continue
		}
		if tmdb != nil && plan.Candidate != nil {
			fmt.Printf("  tmdb    %q (%d) id=%d score=%.2f\n", plan.Candidate.Title, plan.Candidate.Year, plan.Candidate.ID, plan.Candidate.Score)
		}
		if plan.Review {
			fmt.Printf("  REVIEW  %s\n", plan.Reason)
		}
		fmt.Printf("  dest    %s\n\n", plan.Dst)
	}
	return nil
}

func printMedia(prefix string, m media.Media) {
	var parts []string
	add := func(k string, v any) {
		s := fmt.Sprint(v)
		if s != "" && s != "0" && s != "[]" {
			parts = append(parts, k+"="+s)
		}
	}
	add("kind", m.Kind)
	if m.Kind == media.Episode {
		add("show", fmt.Sprintf("%q", m.Show))
		add("showyear", m.ShowYear)
		parts = append(parts, fmt.Sprintf("season=%d", m.Season))
		add("episodes", m.Episodes)
		if m.EpisodeTitle != "" {
			add("eptitle", fmt.Sprintf("%q", m.EpisodeTitle))
		}
	} else {
		add("title", fmt.Sprintf("%q", m.Title))
		add("year", m.Year)
		add("edition", m.Edition)
	}
	add("res", m.Resolution)
	add("source", m.Source)
	add("vcodec", m.VideoCodec)
	add("acodec", m.AudioCodec)
	add("hdr", m.HDR)
	fmt.Println(prefix, strings.Join(parts, " "))
}

func cmdHistory(args []string) error {
	f := newFlags("history")
	n := f.fs.Int("n", 20, "number of entries")
	f.fs.Parse(args)
	cfg, err := f.load(true)
	if err != nil {
		return err
	}
	h, err := history.Open(cfg.HistoryFile)
	if err != nil {
		return err
	}
	entries, err := h.Active()
	if err != nil {
		return err
	}
	if len(entries) > *n {
		entries = entries[len(entries)-*n:]
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tTIME\tACTION\tSOURCE\tDESTINATION")
	for _, e := range entries {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", e.ID, e.Time.Format("2006-01-02 15:04"), e.Action, e.Src, e.Dst)
	}
	return tw.Flush()
}

func cmdUndo(args []string) error {
	f := newFlags("undo")
	last := f.fs.Int("last", 1, "undo the last N operations")
	id := f.fs.String("id", "", "undo a specific operation by ID (prefix ok)")
	dry := f.fs.Bool("dry-run", false, "show what would be undone")
	f.fs.Parse(args)
	cfg, err := f.load(true)
	if err != nil {
		return err
	}
	h, err := history.Open(cfg.HistoryFile)
	if err != nil {
		return err
	}
	var targets []history.Entry
	if *id != "" {
		e, err := h.Find(*id)
		if err != nil {
			return err
		}
		targets = []history.Entry{e}
	} else {
		entries, err := h.Active()
		if err != nil {
			return err
		}
		for i := len(entries) - 1; i >= 0 && len(targets) < *last; i-- {
			targets = append(targets, entries[i])
		}
	}
	if len(targets) == 0 {
		fmt.Println("nothing to undo")
		return nil
	}
	var errs []error
	for _, e := range targets {
		verb := map[string]string{"move": "move back", "copy": "delete copy", "hardlink": "remove link", "symlink": "remove symlink"}[e.Action]
		fmt.Printf("%s %s: %s ← %s\n", map[bool]string{true: "would", false: "undo"}[*dry], verb, e.Src, e.Dst)
		if *dry {
			continue
		}
		if err := h.Undo(e); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", e.ID, err))
		}
	}
	return errors.Join(errs...)
}

func cmdCheck(ctx context.Context, args []string) error {
	f := newFlags("check")
	f.fs.Parse(args)
	cfg, err := f.load(false)
	if err != nil {
		return err
	}
	ok := func(format string, a ...any) { fmt.Printf("  ✓ "+format+"\n", a...) }
	bad := func(format string, a ...any) { fmt.Printf("  ✗ "+format+"\n", a...) }
	warn := func(format string, a ...any) { fmt.Printf("  ! "+format+"\n", a...) }
	failed := false

	fmt.Println("config:", *f.cfgPath)
	ok("valid (%d rule(s), preset %q)", len(cfg.Rules), cfg.Preset)

	for _, r := range cfg.Rules {
		fmt.Printf("rule %q:\n", r.Name)
		if fi, err := os.Stat(r.Watch); err != nil || !fi.IsDir() {
			warn("watch dir %s does not exist yet (will be created)", r.Watch)
		} else {
			ok("watch dir %s", r.Watch)
		}
		if fi, err := os.Stat(r.Output); err != nil || !fi.IsDir() {
			bad("output dir %s does not exist — is the drive mounted?", r.Output)
			failed = true
		} else if err := writable(r.Output); err != nil {
			bad("output dir %s is not writable: %v", r.Output, err)
			failed = true
		} else {
			ok("output dir %s (writable)", r.Output)
		}
		if n, total, err := jellyfinCannotWrite(r.Output); err == nil && n > 0 {
			warn("Jellyfin (user jellyfin) cannot write %d of %d library folders: it can't save subtitles or delete items there. See \"Permissions\" in docs/SETUP.md", n, total)
		} else if err == nil {
			ok("Jellyfin can write to all %d library folders", total)
		}
		if sameDevice(r.Watch, r.Output) == false && r.Action == "hardlink" {
			bad("action hardlink but watch and output are on different filesystems; use move")
			failed = true
		} else {
			ok("action %s, conflict %s", r.Action, r.Conflict)
		}
	}

	fmt.Println("services:")
	if cfg.TMDB.APIKey == "" {
		warn("TMDB: no api key — naming from filenames only")
	} else {
		c := match.New(cfg.TMDB.APIKey, cfg.TMDB.Language, cfg.TMDB.MinConfidence, "")
		m := media.Media{Kind: media.Movie, Title: "The Matrix", Year: 1999}
		if _, err := c.Enrich(ctx, &m); err != nil {
			bad("TMDB: %v", err)
			failed = true
		} else {
			ok("TMDB: reachable, key valid (The Matrix → tmdb %d)", m.TMDBID)
		}
	}
	if probe.Find(cfg.FFprobe) != "" {
		ok("ffprobe: %s (real resolution/codec detection on)", probe.Bin)
	} else if cfg.Probe == "always" {
		bad("ffprobe: not found but probe: always")
		failed = true
	} else {
		warn("ffprobe: not found — install ffmpeg for accurate resolution/HDR detection")
	}
	sd := cfg.SubtitleDownload
	if o := sd.OpenSubtitles; o.APIKey != "" && o.Username != "" && o.Password != "" {
		c := subs.NewClient(o.APIKey, o.Username, o.Password)
		if allowed, err := c.Login(ctx); err != nil {
			bad("OpenSubtitles: %v", err)
			failed = true
		} else {
			state := "download on import"
			if !sd.WantOnImport() {
				state = "only via shelf subs (subtitle_download.enabled/on_import off)"
			}
			ok("OpenSubtitles: logged in, %d downloads/day; languages %s; %s", allowed, strings.Join(sd.Languages, ","), state)
		}
		if sd.Sync == "never" {
			warn("ffsubsync: disabled (sync: never)")
		} else if bin := subs.FindFFsubsync(sd.FFsubsync); bin != "" {
			ok("ffsubsync: %s", bin)
		} else {
			warn("ffsubsync: not found — subtitles keep their original timing (install: uv tool install ffsubsync)")
		}
	} else if sd.Enabled {
		bad("OpenSubtitles: subtitle_download enabled but credentials missing")
		failed = true
	} else {
		warn("subtitle download disabled")
	}
	if cfg.Jellyfin.Refresh {
		if err := jellyfin.New(cfg.Jellyfin, f.logger()).Ping(ctx); err != nil {
			bad("Jellyfin: %v", err)
			failed = true
		} else {
			ok("Jellyfin: reachable at %s", cfg.Jellyfin.URL)
		}
	} else {
		warn("Jellyfin refresh disabled")
	}
	if failed {
		return errors.New("check failed")
	}
	return nil
}

// jellyfinCannotWrite counts library folders (outside Jellyfin's own
// metadata and hidden folders) that the local "jellyfin" user can't write.
// It returns an error if there is no such user.
func jellyfinCannotWrite(root string) (bad, total int, err error) {
	u, err := user.Lookup("jellyfin")
	if err != nil {
		return 0, 0, err
	}
	uid, _ := strconv.Atoi(u.Uid)
	groups := map[int]bool{}
	if gids, err := u.GroupIds(); err == nil {
		for _, g := range gids {
			n, _ := strconv.Atoi(g)
			groups[n] = true
		}
	}
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		if p != root && (strings.HasPrefix(d.Name(), ".") || (filepath.Dir(p) == root && d.Name() == "metadata")) {
			return filepath.SkipDir
		}
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok {
			return nil
		}
		total++
		m := fi.Mode().Perm()
		canWrite := (int(st.Uid) == uid && m&0o300 == 0o300) ||
			(groups[int(st.Gid)] && m&0o030 == 0o030) || m&0o003 == 0o003
		if !canWrite {
			bad++
		}
		return nil
	})
	return bad, total, nil
}

func writable(dir string) error {
	f, err := os.CreateTemp(dir, ".shelf-check-*")
	if err != nil {
		return err
	}
	f.Close()
	return os.Remove(f.Name())
}

func sameDevice(a, b string) bool {
	var sa, sb syscall.Stat_t
	if syscall.Stat(a, &sa) != nil || syscall.Stat(b, &sb) != nil {
		return true // unknown; don't complain
	}
	return sa.Dev == sb.Dev
}

func cmdInit(args []string) error {
	f := newFlags("init")
	force := f.fs.Bool("force", false, "overwrite an existing config")
	f.fs.Parse(args)
	path := *f.cfgPath
	if _, err := os.Stat(path); err == nil && !*force {
		return fmt.Errorf("%s exists (use -force to overwrite)", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(starterConfig), 0o600); err != nil {
		return err
	}
	fmt.Println("wrote", path)
	fmt.Println("edit the watch/output paths, put secrets in", filepath.Join(filepath.Dir(path), "env"), ", then run: shelf check")
	return nil
}

const starterConfig = `# shelf configuration. ${VAR} and ${VAR:-default} expand from the environment.

tmdb:
  api_key: ${TMDB_API_KEY}      # v3 key or v4 read token
  language: en-US
  min_confidence: 0.75          # below this, files go to the review folder
  on_no_match: review           # review | parsed | skip

preset: jellyfin                # jellyfin | jellyfin-versions
action: move                    # move | copy | hardlink | symlink
conflict: skip                  # skip | overwrite | suffix | keep-larger
stable_for: 10s                 # file must stop growing this long before import
rescan_interval: 10m
min_size: 50MB                  # ignore tiny files (samples, extras)
probe: auto                     # use ffprobe when installed: auto | always | never
# ffprobe: /usr/lib/jellyfin-ffmpeg/ffprobe   # auto-detected from PATH or Jellyfin's bundle
ignore: ["*.part", "*.crdownload", "*.tmp", "*.!qb", "*.download", "*sample*", ".*"]

jellyfin:
  url: http://localhost:8096
  api_key: ${JELLYFIN_API_KEY}
  refresh: false                # set true once api_key is set
  mode: targeted                # targeted (just new files) | full (whole library)
  debounce: 5s
  # If Jellyfin runs in Docker and sees the library at a different path:
  # path_map:
  #   - { from: /mnt/media, to: /media }

# Download missing subtitles from OpenSubtitles after each import (and with
# "shelf subs"). Credentials: OPENSUBTITLES_API_KEY, OPENSUBTITLES_USERNAME,
# OPENSUBTITLES_PASSWORD in the env file.
subtitle_download:
  enabled: false
  languages: [en]               # OpenSubtitles codes: en, es, pt-BR, ...
  hearing_impaired: avoid       # avoid | prefer | exclude | include
  sync: auto                    # re-time with ffsubsync: auto | always | never
  # ffsubsync: ~/.local/bin/ffsubsync

rules:
  - name: incoming
    watch: ~/Downloads/shelf     # on the root filesystem
    output: /mnt/media           # library drive; point Jellyfin libraries at
                                 #   /mnt/media/Movies and /mnt/media/Shows
    recursive: true
    type: auto                   # auto | movie | tv
    subtitles: true
    cleanup_empty_dirs: true
    check_duplicates: true       # review files whose content matches a video
                                 #   already in the library
    # extensions: [mkv, mp4]     # default: common video formats
    # templates:                 # override the preset with Go templates
    #   movie: '{{.Title | clean}} ({{.Year}})/{{.Title | clean}} ({{.Year}}) - {{.Resolution}}{{.Ext}}'
`

func cmdRetrofit(ctx context.Context, args []string) error {
	f := newFlags("retrofit")
	apply := f.fs.Bool("apply", false, "actually move files (default is a dry run)")
	offline := f.fs.Bool("offline", false, "don't query TMDB (names from files/folders only)")
	ruleName := f.fs.String("rule", "", "rule whose output library to retrofit (default: first rule)")
	dups := f.fs.String("duplicates", "skip", "when files collide: skip (report) | keep-larger (move smaller aside)")
	f.fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `Usage: shelf retrofit [flags] [paths inside the library...]

Re-identifies every video in a rule's output library and renames it with the
current templates: merges duplicate show/movie folders, adds [tmdbid-N] tags
and episode titles, and moves subtitles, NFOs, thumbnails and folder artwork
along. Without -apply it only prints the plan. Every move is recorded, so
"shelf undo -last N" can reverse it. Nothing is deleted.

Flags:
`)
		f.fs.PrintDefaults()
	}
	f.fs.Parse(args)
	if *dups != "skip" && *dups != "keep-larger" {
		return fmt.Errorf("-duplicates must be skip or keep-larger")
	}
	log := f.logger()
	cfg, err := f.load(false)
	if err != nil {
		return err
	}
	rule, err := pickRule(cfg, *ruleName, "", "", "")
	if err != nil {
		return err
	}
	var paths []string
	for _, a := range f.fs.Args() {
		abs, err := filepath.Abs(a)
		if err != nil {
			return err
		}
		paths = append(paths, abs)
	}
	p, err := pipeline.New(cfg, log, nil, !*apply, *offline)
	if err != nil {
		return err
	}
	if p.TMDB == nil {
		fmt.Fprintln(os.Stderr, "note: TMDB disabled; names come from existing files and folders only")
	}
	fmt.Fprintf(os.Stderr, "scanning %s ...\n", rule.Output)
	plan, err := retrofit.Build(ctx, p, rule, retrofit.Options{Duplicates: *dups, Paths: paths})
	if err != nil {
		return err
	}
	plan.Print(os.Stdout)
	if len(plan.Ops) == 0 {
		return nil
	}
	if !*apply {
		fmt.Println("\nDry run: nothing changed. Re-run with -apply to perform these moves.")
		return nil
	}
	err = plan.Apply(p, rule, os.Stdout)
	fmt.Printf("\nApplied %d move(s). Undo with: shelf undo -last %d\n", len(plan.Ops), len(plan.Ops))
	if p.Jelly != nil {
		if jerr := p.Jelly.RefreshAll(ctx); jerr != nil {
			log.Error("jellyfin refresh failed", "err", jerr)
		} else {
			fmt.Println("Jellyfin library refresh started.")
		}
	}
	return err
}

func cmdHealth(ctx context.Context, args []string) error {
	f := newFlags("health")
	offline := f.fs.Bool("offline", false, "don't query TMDB (no aired-episode check; names judged from files/folders)")
	withProbe := f.fs.Bool("probe", false, "run ffprobe on every video to find unreadable/truncated files (slower)")
	ruleName := f.fs.String("rule", "", "rule whose output library to check (default: first rule)")
	f.fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `Usage: shelf health [flags] [paths inside the library...]

Inspects the library and reports problems; changes nothing:
  - videos with identical content (one stream saved under several names)
  - several files for the same episode or movie
  - missing episodes (aired per TMDB, or gaps in the numbering)
  - names that don't follow the templates (fix with shelf retrofit)
  - unidentifiable, empty or (with -probe) unreadable files
  - temp files from interrupted copies and empty folders
Exits with status 1 if problems are found, so it can run from cron.

Flags:
`)
		f.fs.PrintDefaults()
	}
	f.fs.Parse(args)
	cfg, err := f.load(false)
	if err != nil {
		return err
	}
	rule, err := pickRule(cfg, *ruleName, "", "", "")
	if err != nil {
		return err
	}
	var paths []string
	for _, a := range f.fs.Args() {
		abs, err := filepath.Abs(a)
		if err != nil {
			return err
		}
		paths = append(paths, abs)
	}
	p, err := pipeline.New(cfg, f.logger(), nil, true, *offline)
	if err != nil {
		return err
	}
	if *withProbe && !probe.Available() {
		return fmt.Errorf("-probe: ffprobe not found")
	}
	fmt.Fprintf(os.Stderr, "checking %s ...\n", rule.Output)
	rep, err := health.Check(ctx, p, rule, health.Options{Paths: paths, Probe: *withProbe})
	if err != nil {
		return err
	}
	rep.Print(os.Stdout)
	if rep.Problems() > 0 {
		return errProblems
	}
	return nil
}

func cmdSubs(ctx context.Context, args []string) error {
	f := newFlags("subs")
	apply := f.fs.Bool("apply", false, "download (default: only show what would be downloaded)")
	force := f.fs.Bool("force", false, "fetch even where a subtitle already exists, replacing it")
	langs := f.fs.String("lang", "", "comma-separated languages (default: subtitle_download.languages)")
	limit := f.fs.Int("limit", 0, "stop after this many downloads (0 = until the daily quota runs out)")
	ruleName := f.fs.String("rule", "", "rule whose output library to scan (default: first rule)")
	f.fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `Usage: shelf subs [flags] [paths inside the library...]

Finds library videos without subtitles in the configured languages (no
"<name>.<lang>.srt" next to them and, if ffprobe is available, no embedded
track), picks the best OpenSubtitles match and, with -apply, downloads it as
"<name>.<lang>.srt", re-timed to the audio with ffsubsync when available.
Without -apply it only searches (searches don't use the download quota).
Downloads are recorded, so "shelf undo" removes them.

Flags:
`)
		f.fs.PrintDefaults()
	}
	f.fs.Parse(args)
	log := f.logger()
	cfg, err := f.load(false)
	if err != nil {
		return err
	}
	rule, err := pickRule(cfg, *ruleName, "", "", "")
	if err != nil {
		return err
	}
	p, err := pipeline.New(cfg, log, nil, !*apply, true)
	if err != nil {
		return err
	}
	if p.Subs == nil {
		return errors.New("OpenSubtitles credentials are not configured: set OPENSUBTITLES_API_KEY, OPENSUBTITLES_USERNAME and OPENSUBTITLES_PASSWORD in the env file (see docs/SETUP.md)")
	}
	var paths []string
	for _, a := range f.fs.Args() {
		abs, err := filepath.Abs(a)
		if err != nil {
			return err
		}
		paths = append(paths, abs)
	}
	videos, err := retrofit.Collect(rule.Output, paths, rule.ReviewDir)
	if err != nil {
		return err
	}
	opt := subs.Options{Force: *force, DryRun: !*apply}
	if *langs != "" {
		opt.Languages = strings.Split(*langs, ",")
	}
	if *apply && p.Subs.Sync == nil && cfg.SubtitleDownload.Sync != "never" {
		fmt.Fprintln(os.Stderr, "note: ffsubsync not found; subtitles keep their original timing")
	}

	var downloaded, notFound, failed, checked int
	for i, v := range videos {
		if err := ctx.Err(); err != nil {
			return err
		}
		if *limit > 0 && downloaded >= *limit {
			fmt.Printf("\nStopped after %d download(s) (-limit); %d video(s) not checked.\n", downloaded, len(videos)-i)
			break
		}
		m := parse.Path(v, rule.Output)
		results, err := p.Subs.Fetch(ctx, subs.Job{Video: v, Media: m, Rule: rule}, opt)
		var qe *subs.QuotaError
		if len(results) > 0 || err != nil {
			checked++
			fmt.Println(relTo(rule.Output, v))
		}
		for _, r := range results {
			switch r.Status {
			case "would-download", "downloaded":
				verb := "would get"
				if r.Status == "downloaded" {
					verb, downloaded = "got", downloaded+1
				}
				fmt.Printf("  %-5s %s %s (score %.1f)\n", r.Lang, verb, r.Sub.Describe(), r.Score)
				if r.Sync != "" {
					fmt.Printf("        sync: %s\n", r.Sync)
				}
				if !*apply {
					downloaded++
				}
			case "not-found":
				notFound++
				fmt.Printf("  %-5s no usable subtitle on OpenSubtitles\n", r.Lang)
			case "failed":
				failed++
				fmt.Printf("  %-5s failed: %v\n", r.Lang, r.Err)
			}
		}
		if errors.As(err, &qe) {
			fmt.Printf("\n%v. %d video(s) left; run again after the reset (or let the daemon catch up).\n", qe, len(videos)-i)
			break
		}
		if err != nil {
			failed++
			fmt.Printf("  error: %v\n", err)
		}
	}
	verb := "downloaded"
	if !*apply {
		verb = "to download"
	}
	fmt.Printf("\n%d video(s) scanned, %d need subtitles: %d %s, %d not found, %d failed.\n", len(videos), checked, downloaded, verb, notFound, failed)
	if left, reset := p.Subs.Client.Remaining(); left >= 0 && *apply {
		fmt.Printf("OpenSubtitles downloads left today: %d (resets %s)\n", left, reset.Local().Format("Jan 2 15:04"))
	}
	if !*apply && downloaded > 0 {
		fmt.Println("Dry run: nothing downloaded. Re-run with -apply.")
	}
	if *apply && downloaded > 0 {
		p.Flush(ctx)
	}
	return nil
}

func relTo(root, p string) string {
	if r, err := filepath.Rel(root, p); err == nil && !strings.HasPrefix(r, "..") {
		return r
	}
	return p
}

func cmdCleanup(args []string) error {
	f := newFlags("cleanup")
	del := f.fs.Bool("delete", false, "permanently delete the listed items (default: only list them)")
	older := f.fs.String("older-than", "", "only items moved to review at least this long ago, e.g. 7d or 12h")
	only := f.fs.String("only", "", "only these groups, comma-separated: "+strings.Join(cleanup.Categories, ", "))
	f.fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `Usage: shelf cleanup [flags]

Lists what is waiting in each rule's review folder, grouped by why it is
there: unmatched imports, older versions replaced by a new import, and
duplicates set aside by retrofit. Nothing is deleted unless you pass
-delete, which removes exactly the items listed with the same filters.

Flags:
`)
		f.fs.PrintDefaults()
	}
	f.fs.Parse(args)
	cfg, err := f.load(false)
	if err != nil {
		return err
	}
	opt := cleanup.Options{Now: time.Now()}
	if opt.OlderThan, err = cleanup.ParseAge(*older); err != nil {
		return err
	}
	if *only != "" {
		for _, c := range strings.Split(*only, ",") {
			c = strings.TrimSpace(c)
			if !slices.Contains(cleanup.Categories, c) {
				return fmt.Errorf("-only: unknown group %q (have %s)", c, strings.Join(cleanup.Categories, ", "))
			}
			opt.Only = append(opt.Only, c)
		}
	}

	seen := map[string]bool{}
	var count, deleted int
	var freed int64
	var errs []error
	for _, r := range cfg.Rules {
		if r.ReviewDir == "" || seen[r.ReviewDir] {
			continue
		}
		seen[r.ReviewDir] = true
		items, err := cleanup.Scan(r.ReviewDir, opt)
		if err != nil {
			return err
		}
		if len(seen) > 1 {
			fmt.Println()
		}
		cleanup.Print(os.Stdout, r.ReviewDir, items, opt.Now)
		count += len(items)
		if *del && len(items) > 0 {
			n, size, err := cleanup.Delete(r.ReviewDir, items)
			deleted += n
			freed += size
			if err != nil {
				errs = append(errs, err)
			}
		}
		if empty := cleanup.EmptyDirs(r.ReviewDir); len(empty) > 0 {
			if *del {
				fmt.Printf("Removed %d empty folder(s).\n", cleanup.RemoveDirs(empty))
			} else {
				fmt.Printf("  plus %d empty folder(s), removed by -delete\n", len(empty))
				count++
			}
		}
	}
	switch {
	case count == 0:
	case *del:
		fmt.Printf("\nDeleted %d item(s), freed %s.\n", deleted, cleanup.HumanSize(freed))
	default:
		cmd := "shelf cleanup -delete"
		if *older != "" {
			cmd += " -older-than " + *older
		}
		if *only != "" {
			cmd += " -only " + *only
		}
		fmt.Printf("\nNothing deleted. To permanently delete these items: %s\n", cmd)
	}
	return errors.Join(errs...)
}
