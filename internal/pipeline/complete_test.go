package pipeline

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"shelf/internal/media"
	"shelf/internal/probe"
)

// makeVideo renders a short test clip with ffmpeg (next to ffprobe, e.g.
// Jellyfin's bundle), or skips the test.
func makeVideo(t *testing.T, path string, seconds string) {
	t.Helper()
	if probe.Find("") == "" {
		t.Skip("ffprobe not available")
	}
	ffmpeg := filepath.Join(filepath.Dir(probe.Bin), "ffmpeg")
	if _, err := os.Stat(ffmpeg); err != nil {
		if ffmpeg, err = exec.LookPath("ffmpeg"); err != nil {
			t.Skip("ffmpeg not available")
		}
	}
	out, err := exec.Command(ffmpeg, "-v", "error", "-y", "-f", "lavfi", "-i", "testsrc=size=320x240:rate=25",
		"-t", seconds, "-c:v", "libx264", "-g", "25", "-preset", "ultrafast", path).CombinedOutput()
	if err != nil {
		t.Skipf("ffmpeg failed: %v %s", err, out)
	}
}

func TestIncomplete(t *testing.T) {
	d := t.TempDir()
	full := filepath.Join(d, "full.mkv")
	makeVideo(t, full, "120")
	ctx := context.Background()

	if r := Incomplete(ctx, full, media.Media{}); r != "" {
		t.Errorf("complete file flagged: %s", r)
	}
	if r := Incomplete(ctx, full, media.Media{Runtime: 2}); r != "" {
		t.Errorf("complete file vs matching runtime flagged: %s", r)
	}
	if r := Incomplete(ctx, full, media.Media{Runtime: 30}); !strings.Contains(r, "much shorter") {
		t.Errorf("2 min file vs 30 min runtime: %q", r)
	}

	// Cut it off a third of the way: the header still says 2:00.
	data, _ := os.ReadFile(full)
	part := filepath.Join(d, "part.mkv")
	os.WriteFile(part, data[:len(data)/3], 0o644)
	if r := Incomplete(ctx, part, media.Media{}); !strings.Contains(r, "incomplete download: video stops at") || !strings.Contains(r, "of 2:00") {
		t.Errorf("truncated file: %q", r)
	}

	junk := filepath.Join(d, "junk.mp4")
	os.WriteFile(junk, data[:4096], 0o644)
	if r := Incomplete(ctx, junk, media.Media{}); r == "" {
		t.Error("unreadable file not flagged")
	}
}
