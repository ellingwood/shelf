// Package jellyfin tells a Jellyfin server about newly imported files so
// they show up without waiting for a scheduled library scan.
package jellyfin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"shelf/internal/config"
)

// Notifier batches paths and sends them after a quiet period.
type Notifier struct {
	cfg  config.Jellyfin
	http *http.Client
	log  *slog.Logger

	mu      sync.Mutex
	pending []string
	timer   *time.Timer
}

func New(cfg config.Jellyfin, log *slog.Logger) *Notifier {
	return &Notifier{cfg: cfg, http: &http.Client{Timeout: 30 * time.Second}, log: log}
}

// Notify queues a created file. The batch is sent after cfg.Debounce of
// inactivity, so a season pack triggers one request, not twenty.
func (n *Notifier) Notify(path string) {
	if n == nil {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	n.pending = append(n.pending, n.mapPath(path))
	if n.timer != nil {
		n.timer.Stop()
	}
	n.timer = time.AfterFunc(n.cfg.Debounce.D(), func() { n.Flush(context.Background()) })
}

// Flush sends any queued paths now.
func (n *Notifier) Flush(ctx context.Context) {
	if n == nil {
		return
	}
	n.mu.Lock()
	paths := n.pending
	n.pending = nil
	if n.timer != nil {
		n.timer.Stop()
	}
	n.mu.Unlock()
	if len(paths) == 0 {
		return
	}
	var err error
	if n.cfg.Mode == "full" {
		err = n.post(ctx, "/Library/Refresh", nil)
	} else {
		type update struct{ Path, UpdateType string }
		body := struct{ Updates []update }{}
		for _, p := range paths {
			body.Updates = append(body.Updates, update{Path: p, UpdateType: "Created"})
		}
		err = n.post(ctx, "/Library/Media/Updated", body)
	}
	if err != nil {
		n.log.Error("jellyfin notify failed", "err", err, "files", len(paths))
		return
	}
	n.log.Info("jellyfin notified", "files", len(paths), "mode", n.cfg.Mode)
}

// RefreshAll asks Jellyfin to rescan every library (used after bulk moves,
// where targeted "created" notices can't describe removals).
func (n *Notifier) RefreshAll(ctx context.Context) error {
	if n == nil {
		return nil
	}
	n.mu.Lock()
	n.pending = nil
	if n.timer != nil {
		n.timer.Stop()
	}
	n.mu.Unlock()
	return n.post(ctx, "/Library/Refresh", nil)
}

// Ping checks connectivity and the API key.
func (n *Notifier) Ping(ctx context.Context) error {
	req, err := n.request(ctx, http.MethodGet, "/System/Info", nil)
	if err != nil {
		return err
	}
	return n.do(req)
}

func (n *Notifier) post(ctx context.Context, path string, body any) error {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		r = bytes.NewReader(b)
	}
	req, err := n.request(ctx, http.MethodPost, path, r)
	if err != nil {
		return err
	}
	return n.do(req)
}

func (n *Notifier) request(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, n.cfg.URL+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", fmt.Sprintf(`MediaBrowser Client="shelf", Device="shelf", DeviceId="shelf", Version="1.0", Token="%s"`, n.cfg.APIKey))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

func (n *Notifier) do(req *http.Request) error {
	resp, err := n.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s %s: HTTP %d %s", req.Method, req.URL.Path, resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return nil
}

// mapPath rewrites a local path into the path Jellyfin sees, using the
// first matching path_map prefix.
func (n *Notifier) mapPath(p string) string {
	for _, m := range n.cfg.PathMap {
		from := filepath.Clean(m.From)
		if p == from || strings.HasPrefix(p, from+string(filepath.Separator)) {
			return filepath.Join(m.To, strings.TrimPrefix(p, from))
		}
	}
	return p
}
