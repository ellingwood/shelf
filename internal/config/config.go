// Package config loads and validates shelf's YAML configuration.
package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"shelf/internal/naming"
)

type Config struct {
	TMDB      TMDB             `yaml:"tmdb"`
	Preset    string           `yaml:"preset"`
	Templates naming.Templates `yaml:"templates"`

	Action         string   `yaml:"action"`   // move | copy | hardlink | symlink
	Conflict       string   `yaml:"conflict"` // skip | overwrite | suffix | keep-larger
	StableFor      Duration `yaml:"stable_for"`
	RescanInterval Duration `yaml:"rescan_interval"`
	Ignore         []string `yaml:"ignore"`
	MinSize        Size     `yaml:"min_size"`
	Probe          string   `yaml:"probe"`   // auto | always | never
	FFprobe        string   `yaml:"ffprobe"` // path to ffprobe (default: PATH, then Jellyfin's bundled copy)
	HistoryFile    string   `yaml:"history_file"`
	CacheDir       string   `yaml:"cache_dir"`

	Jellyfin Jellyfin `yaml:"jellyfin"`
	Rules    []Rule   `yaml:"rules"`
}

type TMDB struct {
	APIKey        string  `yaml:"api_key"`
	Language      string  `yaml:"language"`
	MinConfidence float64 `yaml:"min_confidence"`
	// OnNoMatch: "review" moves unmatched files to the review dir;
	// "parsed" names them from the filename alone; "skip" leaves them.
	OnNoMatch string `yaml:"on_no_match"`
}

type Jellyfin struct {
	URL      string    `yaml:"url"`
	APIKey   string    `yaml:"api_key"`
	Refresh  bool      `yaml:"refresh"`
	Mode     string    `yaml:"mode"` // targeted | full
	Debounce Duration  `yaml:"debounce"`
	PathMap  []PathMap `yaml:"path_map"`
}

// PathMap rewrites local paths into the paths Jellyfin sees (e.g. Docker).
type PathMap struct {
	From string `yaml:"from"`
	To   string `yaml:"to"`
}

type Rule struct {
	Name             string           `yaml:"name"`
	Watch            string           `yaml:"watch"`
	Output           string           `yaml:"output"`
	Recursive        *bool            `yaml:"recursive"`
	Extensions       []string         `yaml:"extensions"`
	Type             string           `yaml:"type"` // auto | movie | tv
	Subtitles        *bool            `yaml:"subtitles"`
	Action           string           `yaml:"action"`
	Conflict         string           `yaml:"conflict"`
	Preset           string           `yaml:"preset"`
	Templates        naming.Templates `yaml:"templates"`
	ReviewDir        string           `yaml:"review_dir"`
	CleanupEmptyDirs *bool            `yaml:"cleanup_empty_dirs"`
	// CheckDuplicates sends a file to review when the library already holds
	// a video with identical content (e.g. a download helper that saved the
	// same stream under several episode names).
	CheckDuplicates *bool `yaml:"check_duplicates"`

	renderer *naming.Renderer
}

// Renderer returns the rule's compiled templates (set by Validate).
func (r *Rule) Renderer() *naming.Renderer { return r.renderer }

func (r *Rule) IsRecursive() bool     { return r.Recursive == nil || *r.Recursive }
func (r *Rule) WantSubtitles() bool   { return r.Subtitles == nil || *r.Subtitles }
func (r *Rule) WantCleanupDirs() bool { return r.CleanupEmptyDirs == nil || *r.CleanupEmptyDirs }
func (r *Rule) WantDupeCheck() bool   { return r.CheckDuplicates == nil || *r.CheckDuplicates }

// MatchesExt reports whether a file extension (with dot) is handled by the rule.
func (r *Rule) MatchesExt(ext string) bool {
	ext = strings.ToLower(strings.TrimPrefix(ext, "."))
	for _, e := range r.Extensions {
		if strings.ToLower(strings.TrimPrefix(e, ".")) == ext {
			return true
		}
	}
	return false
}

// DefaultPath is $XDG_CONFIG_HOME/shelf/config.yaml.
func DefaultPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = "."
	}
	return filepath.Join(dir, "shelf", "config.yaml")
}

// Load reads, expands, defaults and validates a config file.
//
// Before parsing, it loads an "env" file from the same directory (if present)
// so secrets like TMDB_API_KEY don't have to be exported in every shell.
func Load(path string) (*Config, error) {
	if err := LoadEnvFile(filepath.Join(filepath.Dir(path), "env")); err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(raw)
}

// Parse is Load without the file read.
func Parse(raw []byte) (*Config, error) {
	var c Config
	dec := yaml.NewDecoder(strings.NewReader(ExpandEnv(string(raw))))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	c.applyDefaults()
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// Default returns a config with defaults applied and no rules, used when no
// config file exists (e.g. for `shelf preview`).
func Default() *Config {
	c := &Config{}
	c.applyDefaults()
	return c
}

var reEnv = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(?::-([^}]*))?\}`)

// ExpandEnv replaces ${VAR} and ${VAR:-default}. Bare $VAR is left alone so
// Go template variables like {{$x}} survive.
func ExpandEnv(s string) string {
	return reEnv.ReplaceAllStringFunc(s, func(m string) string {
		sm := reEnv.FindStringSubmatch(m)
		if v, ok := os.LookupEnv(sm[1]); ok && v != "" {
			return v
		}
		return sm[2]
	})
}

func (c *Config) applyDefaults() {
	if c.TMDB.APIKey == "" {
		c.TMDB.APIKey = os.Getenv("TMDB_API_KEY")
	}
	def(&c.TMDB.Language, "en-US")
	if c.TMDB.MinConfidence == 0 {
		c.TMDB.MinConfidence = 0.75
	}
	def(&c.TMDB.OnNoMatch, "review")
	def(&c.Preset, "jellyfin")
	def(&c.Action, "move")
	def(&c.Conflict, "skip")
	def(&c.Probe, "auto")
	if c.StableFor == 0 {
		c.StableFor = Duration(10 * time.Second)
	}
	if c.RescanInterval == 0 {
		c.RescanInterval = Duration(10 * time.Minute)
	}
	if c.Ignore == nil {
		c.Ignore = []string{"*.part", "*.crdownload", "*.tmp", "*.!qb", "*.download", "*sample*", ".*"}
	}
	state := stateDir()
	def(&c.HistoryFile, filepath.Join(state, "history.jsonl"))
	if c.CacheDir == "" {
		if d, err := os.UserCacheDir(); err == nil {
			c.CacheDir = filepath.Join(d, "shelf")
		} else {
			c.CacheDir = filepath.Join(state, "cache")
		}
	}
	c.HistoryFile = ExpandHome(c.HistoryFile)
	c.FFprobe = ExpandHome(c.FFprobe)
	c.CacheDir = ExpandHome(c.CacheDir)

	if c.Jellyfin.APIKey == "" {
		c.Jellyfin.APIKey = os.Getenv("JELLYFIN_API_KEY")
	}
	def(&c.Jellyfin.Mode, "targeted")
	if c.Jellyfin.Debounce == 0 {
		c.Jellyfin.Debounce = Duration(5 * time.Second)
	}
	c.Jellyfin.URL = strings.TrimRight(c.Jellyfin.URL, "/")

	for i := range c.Rules {
		r := &c.Rules[i]
		r.Watch = ExpandHome(r.Watch)
		r.Output = ExpandHome(r.Output)
		def(&r.Name, fmt.Sprintf("rule%d", i+1))
		def(&r.Type, "auto")
		def(&r.Action, c.Action)
		def(&r.Conflict, c.Conflict)
		def(&r.Preset, c.Preset)
		if r.Extensions == nil {
			r.Extensions = []string{"mkv", "mp4", "m4v", "avi", "mov", "wmv", "ts", "m2ts", "webm", "mpg", "mpeg"}
		}
		if r.ReviewDir == "" && r.Output != "" {
			r.ReviewDir = filepath.Join(r.Output, ".shelf-review")
		}
		r.ReviewDir = ExpandHome(r.ReviewDir)
	}
}

// Validate checks values and compiles each rule's templates.
func (c *Config) Validate() error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	oneOf := func(field, v string, opts ...string) {
		for _, o := range opts {
			if v == o {
				return
			}
		}
		add("%s: %q is not one of %s", field, v, strings.Join(opts, ", "))
	}
	actions := []string{"move", "copy", "hardlink", "symlink"}
	conflicts := []string{"skip", "overwrite", "suffix", "keep-larger"}
	oneOf("action", c.Action, actions...)
	oneOf("conflict", c.Conflict, conflicts...)
	oneOf("probe", c.Probe, "auto", "always", "never")
	oneOf("tmdb.on_no_match", c.TMDB.OnNoMatch, "review", "parsed", "skip")
	oneOf("jellyfin.mode", c.Jellyfin.Mode, "targeted", "full")
	if c.Jellyfin.Refresh && (c.Jellyfin.URL == "" || c.Jellyfin.APIKey == "") {
		add("jellyfin.refresh requires jellyfin.url and jellyfin.api_key")
	}
	if _, ok := naming.Presets[c.Preset]; !ok {
		add("preset: unknown preset %q (have %s)", c.Preset, strings.Join(naming.PresetNames(), ", "))
	}

	names := map[string]bool{}
	for i := range c.Rules {
		r := &c.Rules[i]
		p := fmt.Sprintf("rules[%s]", r.Name)
		if names[r.Name] {
			add("%s: duplicate rule name", p)
		}
		names[r.Name] = true
		if r.Watch == "" {
			add("%s: watch is required", p)
		} else if !filepath.IsAbs(r.Watch) {
			add("%s: watch must be an absolute path", p)
		}
		if r.Output == "" {
			add("%s: output is required", p)
		} else if !filepath.IsAbs(r.Output) {
			add("%s: output must be an absolute path", p)
		}
		if r.Watch != "" && r.Output != "" && filepath.Clean(r.Watch) == filepath.Clean(r.Output) {
			add("%s: watch and output must differ", p)
		}
		oneOf(p+".action", r.Action, actions...)
		oneOf(p+".conflict", r.Conflict, conflicts...)
		oneOf(p+".type", r.Type, "auto", "movie", "tv")
		rend, err := c.RendererFor(r)
		if err != nil {
			add("%s: %v", p, err)
		}
		r.renderer = rend
	}
	return errors.Join(errs...)
}

// RendererFor builds the template set for a rule: rule templates override
// global templates, which override the preset.
func (c *Config) RendererFor(r *Rule) (*naming.Renderer, error) {
	presetName := c.Preset
	if r != nil && r.Preset != "" {
		presetName = r.Preset
	}
	t, ok := naming.Presets[presetName]
	if !ok {
		return nil, fmt.Errorf("unknown preset %q", presetName)
	}
	override := func(o naming.Templates) {
		if o.Movie != "" {
			t.Movie = o.Movie
		}
		if o.Episode != "" {
			t.Episode = o.Episode
		}
	}
	override(c.Templates)
	if r != nil {
		override(r.Templates)
	}
	return naming.New(t)
}

func def(dst *string, v string) {
	if *dst == "" {
		*dst = v
	}
}

func stateDir() string {
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "shelf")
	}
	if h, err := os.UserHomeDir(); err == nil {
		return filepath.Join(h, ".local", "state", "shelf")
	}
	return ".shelf"
}

// ExpandHome expands a leading "~/".
func ExpandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}

// Duration decodes YAML strings like "10s" or "5m".
type Duration time.Duration

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	v, err := time.ParseDuration(n.Value)
	if err != nil {
		return fmt.Errorf("line %d: invalid duration %q", n.Line, n.Value)
	}
	*d = Duration(v)
	return nil
}

func (d Duration) D() time.Duration { return time.Duration(d) }

// Size decodes YAML values like "50MB", "1.5GiB" or 1048576.
type Size int64

var reSize = regexp.MustCompile(`(?i)^\s*([\d.]+)\s*([kmgt]?i?b?)\s*$`)

func (s *Size) UnmarshalYAML(n *yaml.Node) error {
	v, err := ParseSize(n.Value)
	if err != nil {
		return fmt.Errorf("line %d: %w", n.Line, err)
	}
	*s = Size(v)
	return nil
}

func ParseSize(str string) (int64, error) {
	sm := reSize.FindStringSubmatch(str)
	if sm == nil {
		return 0, fmt.Errorf("invalid size %q", str)
	}
	f, err := strconv.ParseFloat(sm[1], 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q", str)
	}
	unit := strings.ToUpper(strings.TrimSuffix(strings.TrimSuffix(sm[2], "b"), "B"))
	base := 1000.0
	if strings.HasSuffix(unit, "I") {
		base, unit = 1024, strings.TrimSuffix(unit, "I")
	}
	mult := map[string]float64{"": 1, "K": base, "M": base * base, "G": base * base * base, "T": base * base * base * base}[unit]
	return int64(f * mult), nil
}

// PrepareRule readies an ad-hoc rule (CLI one-shot use, no watch dir): it
// checks the action/conflict and compiles templates.
func (c *Config) PrepareRule(r *Rule) error {
	tmp := Config{Action: r.Action, Conflict: r.Conflict, Probe: "auto", Preset: c.Preset,
		TMDB: TMDB{OnNoMatch: "review"}, Jellyfin: Jellyfin{Mode: "targeted"}}
	if err := tmp.Validate(); err != nil {
		return err
	}
	rend, err := c.RendererFor(r)
	if err != nil {
		return err
	}
	r.renderer = rend
	return nil
}

// LoadEnvFile reads KEY=VALUE lines (systemd EnvironmentFile / dotenv style)
// into the process environment. Variables already set in the environment win,
// so `TMDB_API_KEY=x shelf ...` still overrides the file. A missing file is
// not an error.
func LoadEnvFile(path string) error {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("env file: %w", err)
	}
	for i, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, val, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" {
			return fmt.Errorf("%s:%d: expected KEY=VALUE", path, i+1)
		}
		val = strings.TrimSpace(val)
		if len(val) >= 2 && (val[0] == '"' || val[0] == '\'') && val[len(val)-1] == val[0] {
			val = val[1 : len(val)-1]
		}
		if _, set := os.LookupEnv(key); !set {
			os.Setenv(key, val)
		}
	}
	return nil
}
