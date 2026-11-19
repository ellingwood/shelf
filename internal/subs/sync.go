package subs

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Syncer re-times subtitles to a video's audio with ffsubsync.
type Syncer struct {
	Bin       string // ffsubsync executable
	FFmpegDir string // directory holding ffmpeg/ffprobe, if not on PATH
}

// FindFFsubsync locates ffsubsync: the explicit path, PATH, then
// ~/.local/bin (where pipx and uv install tools). It returns "" if absent.
func FindFFsubsync(explicit string) string {
	if explicit != "" {
		if isExec(explicit) {
			return explicit
		}
		return ""
	}
	if p, err := exec.LookPath("ffsubsync"); err == nil {
		return p
	}
	if home, err := os.UserHomeDir(); err == nil {
		if p := filepath.Join(home, ".local", "bin", "ffsubsync"); isExec(p) {
			return p
		}
	}
	return ""
}

// FFmpegDirFor returns the directory to pass as --ffmpeg-path: empty when
// ffmpeg is on PATH, otherwise the directory of ffprobe if ffmpeg sits next
// to it (Jellyfin's bundle).
func FFmpegDirFor(ffprobe string) string {
	if _, err := exec.LookPath("ffmpeg"); err == nil {
		return ""
	}
	if ffprobe != "" {
		dir := filepath.Dir(ffprobe)
		if isExec(filepath.Join(dir, "ffmpeg")) {
			return dir
		}
	}
	return ""
}

func isExec(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0
}

var reOffset = regexp.MustCompile(`offset seconds: (-?[0-9.]+)`)

// Sync writes a re-timed copy of in to out and returns a short summary
// such as "shifted +2.35s".
func (s *Syncer) Sync(ctx context.Context, video, in, out string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Minute)
	defer cancel()
	args := []string{video, "-i", in, "-o", out}
	if s.FFmpegDir != "" {
		args = append(args, "--ffmpeg-path", s.FFmpegDir)
	}
	cmd := exec.CommandContext(ctx, s.Bin, args...)
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("ffsubsync: %v: %s", err, lastLines(buf.String(), 3))
	}
	if fi, err := os.Stat(out); err != nil || fi.Size() == 0 {
		return "", fmt.Errorf("ffsubsync produced no output: %s", lastLines(buf.String(), 3))
	}
	if m := reOffset.FindStringSubmatch(buf.String()); m != nil {
		if strings.HasPrefix(m[1], "-") {
			return "shifted " + m[1] + "s", nil
		}
		return "shifted +" + m[1] + "s", nil
	}
	return "synced", nil
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " | ")
}
