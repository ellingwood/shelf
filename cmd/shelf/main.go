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
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"shelf/internal/config"
	"shelf/internal/history"
	"shelf/internal/jellyfin"
	"shelf/internal/match"
	"shelf/internal/media"
	"shelf/internal/parse"
	"shelf/internal/pipeline"
	"shelf/internal/probe"
	"shelf/internal/watch"
)

const usage = `shelf — organize media files for Jellyfin

Usage:
  shelf watch   [flags]              watch configured folders (daemon)
  shelf rename  [flags] <paths...>   organize files/folders once
  shelf preview [flags] <names...>   show how names parse and where they'd go
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
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

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
	log.Info("shelf starting", "tmdb", p.TMDB != nil, "ffprobe", probe.Bin, "jellyfin", p.Jelly != nil, "dry_run", *dry)

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

rules:
  - name: incoming
    watch: ~/Downloads/shelf     # on the root filesystem
    output: /mnt/media           # library drive; point Jellyfin libraries at
                                 #   /mnt/media/Movies and /mnt/media/Shows
    recursive: true
    type: auto                   # auto | movie | tv
    subtitles: true
    cleanup_empty_dirs: true
    # extensions: [mkv, mp4]     # default: common video formats
    # templates:                 # override the preset with Go templates
    #   movie: '{{.Title | clean}} ({{.Year}})/{{.Title | clean}} ({{.Year}}) - {{.Resolution}}{{.Ext}}'
`
