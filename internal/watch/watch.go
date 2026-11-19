// Package watch monitors directories and hands off files once they have
// finished arriving (size and mtime stable for a configured period).
package watch

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"

	"shelf/internal/config"
	"shelf/internal/fileops"
)

// Handler processes one stable file. It runs on a single worker goroutine,
// so files are handled one at a time.
type Handler func(ctx context.Context, rule *config.Rule, path string)

type Watcher struct {
	cfg     *config.Config
	log     *slog.Logger
	handler Handler
	fsw     *fsnotify.Watcher

	// Tick is how often pending files are checked (exposed for tests).
	Tick time.Duration

	mu      sync.Mutex
	pending map[string]*pend
	queued  map[string]bool
	queue   chan job
}

type pend struct {
	rule  *config.Rule
	size  int64
	mtime time.Time
	since time.Time
}

type job struct {
	rule *config.Rule
	path string
}

func New(cfg *config.Config, log *slog.Logger, h Handler) (*Watcher, error) {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	return &Watcher{
		cfg: cfg, log: log, handler: h, fsw: fsw, Tick: time.Second,
		pending: map[string]*pend{}, queued: map[string]bool{},
		queue: make(chan job, 4096),
	}, nil
}

// Run watches until ctx is cancelled.
func (w *Watcher) Run(ctx context.Context) error {
	defer w.fsw.Close()

	for i := range w.cfg.Rules {
		r := &w.cfg.Rules[i]
		if err := os.MkdirAll(r.Watch, 0o755); err != nil {
			return err
		}
		if err := w.addTree(r, r.Watch); err != nil {
			return err
		}
		w.log.Info("watching", "rule", r.Name, "dir", r.Watch, "output", r.Output, "recursive", r.IsRecursive())
	}
	w.scanAll()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() { // single worker: sequential, predictable disk I/O
		defer wg.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case j := <-w.queue:
				w.handler(ctx, j.rule, j.path)
				w.mu.Lock()
				delete(w.queued, j.path)
				w.mu.Unlock()
			}
		}
	}()
	defer wg.Wait()

	tick := time.NewTicker(w.Tick)
	defer tick.Stop()
	rescan := time.NewTicker(w.cfg.RescanInterval.D())
	defer rescan.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case ev, ok := <-w.fsw.Events:
			if !ok {
				return nil
			}
			w.handleEvent(ev)
		case err, ok := <-w.fsw.Errors:
			if !ok {
				return nil
			}
			w.log.Error("watch error (will rescan)", "err", err)
			w.scanAll() // e.g. event queue overflow: recover by rescanning
		case <-tick.C:
			w.checkPending()
		case <-rescan.C:
			w.scanAll()
		}
	}
}

func (w *Watcher) handleEvent(ev fsnotify.Event) {
	rule := w.ruleFor(ev.Name)
	if rule == nil {
		return
	}
	switch {
	case ev.Has(fsnotify.Create), ev.Has(fsnotify.Write), ev.Has(fsnotify.Chmod):
		fi, err := os.Stat(ev.Name)
		if err != nil {
			return
		}
		if fi.IsDir() {
			if ev.Has(fsnotify.Create) && rule.IsRecursive() && !w.skipDir(rule, ev.Name) {
				// Files may have landed before the watch was added: scan too.
				if err := w.addTree(rule, ev.Name); err != nil {
					w.log.Warn("cannot watch directory", "dir", ev.Name, "err", err)
				}
				w.scanTree(rule, ev.Name)
			}
			return
		}
		w.consider(rule, ev.Name, fi)
	case ev.Has(fsnotify.Remove), ev.Has(fsnotify.Rename):
		w.mu.Lock()
		delete(w.pending, ev.Name)
		w.mu.Unlock()
	}
}

// consider adds a file to the pending set if the rule wants it.
func (w *Watcher) consider(rule *config.Rule, path string, fi fs.FileInfo) {
	if !fi.Mode().IsRegular() || !w.Accept(rule, path) {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.queued[path] {
		return
	}
	p, ok := w.pending[path]
	if !ok {
		w.pending[path] = &pend{rule: rule, size: fi.Size(), mtime: fi.ModTime(), since: time.Now()}
		w.log.Debug("pending", "file", path)
		return
	}
	if p.size != fi.Size() || !p.mtime.Equal(fi.ModTime()) {
		p.size, p.mtime, p.since = fi.Size(), fi.ModTime(), time.Now()
	}
}

// checkPending queues files whose size/mtime haven't changed for stable_for.
func (w *Watcher) checkPending() {
	stable := w.cfg.StableFor.D()
	now := time.Now()
	w.mu.Lock()
	defer w.mu.Unlock()
	for path, p := range w.pending {
		fi, err := os.Stat(path)
		if err != nil {
			delete(w.pending, path)
			continue
		}
		if fi.Size() != p.size || !fi.ModTime().Equal(p.mtime) {
			p.size, p.mtime, p.since = fi.Size(), fi.ModTime(), now
			continue
		}
		if now.Sub(p.since) < stable {
			continue
		}
		delete(w.pending, path)
		if fi.Size() < int64(w.cfg.MinSize) {
			w.log.Debug("ignored: below min_size", "file", path, "size", fi.Size())
			continue
		}
		select {
		case w.queue <- job{rule: p.rule, path: path}:
			w.queued[path] = true
		default:
			w.log.Warn("queue full; will retry on next scan", "file", path)
		}
	}
}

// Accept reports whether a file should be handled by rule.
func (w *Watcher) Accept(rule *config.Rule, path string) bool {
	base := filepath.Base(path)
	if fileops.IsTemp(path) || !rule.MatchesExt(filepath.Ext(base)) {
		return false
	}
	lower := strings.ToLower(base)
	for _, g := range w.cfg.Ignore {
		if ok, _ := filepath.Match(strings.ToLower(g), lower); ok {
			return false
		}
	}
	return !w.insideOutput(rule, path)
}

func (w *Watcher) insideOutput(rule *config.Rule, path string) bool {
	for _, d := range []string{rule.Output, rule.ReviewDir} {
		if d != "" && (path == d || strings.HasPrefix(path, filepath.Clean(d)+string(filepath.Separator))) {
			return true
		}
	}
	return false
}

func (w *Watcher) skipDir(rule *config.Rule, dir string) bool {
	if dir == rule.Watch {
		return false
	}
	return strings.HasPrefix(filepath.Base(dir), ".") || w.insideOutput(rule, dir)
}

// ruleFor finds the rule whose watch dir contains path (most specific wins).
func (w *Watcher) ruleFor(path string) *config.Rule {
	var best *config.Rule
	for i := range w.cfg.Rules {
		r := &w.cfg.Rules[i]
		root := filepath.Clean(r.Watch)
		if path != root && !strings.HasPrefix(path, root+string(filepath.Separator)) {
			continue
		}
		if !r.IsRecursive() && filepath.Dir(path) != root && path != root {
			continue
		}
		if best == nil || len(r.Watch) > len(best.Watch) {
			best = r
		}
	}
	return best
}

func (w *Watcher) addTree(rule *config.Rule, root string) error {
	if !rule.IsRecursive() {
		return w.fsw.Add(root)
	}
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if !d.IsDir() {
			return nil
		}
		if w.skipDir(rule, p) {
			return filepath.SkipDir
		}
		return w.fsw.Add(p)
	})
}

func (w *Watcher) scanAll() {
	for i := range w.cfg.Rules {
		r := &w.cfg.Rules[i]
		w.scanTree(r, r.Watch)
	}
}

// scanTree adds every acceptable file under root to the pending set.
func (w *Watcher) scanTree(rule *config.Rule, root string) {
	var files []string
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if w.skipDir(rule, p) || (!rule.IsRecursive() && p != root) {
				return filepath.SkipDir
			}
			return nil
		}
		files = append(files, p)
		return nil
	})
	sort.Strings(files) // episodes in order
	for _, p := range files {
		if fi, err := os.Stat(p); err == nil {
			w.consider(rule, p, fi)
		}
	}
}
