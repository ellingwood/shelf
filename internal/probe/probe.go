// Package probe reads real stream info (resolution, codecs, HDR) with
// ffprobe, so naming doesn't depend on what a filename claims.
package probe

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"shelf/internal/media"
)

// Bin is the ffprobe executable in use (set by Find).
var Bin = ""

// knownPaths are checked when ffprobe isn't on PATH. Jellyfin's bundled
// build (jellyfin-ffmpeg) installs outside PATH.
var knownPaths = []string{
	"/usr/lib/jellyfin-ffmpeg/ffprobe",
	"/usr/share/jellyfin-ffmpeg/ffprobe",
	"/opt/jellyfin-ffmpeg/ffprobe",
}

// Find locates ffprobe: an explicit path if given, else PATH, else known
// install locations. It sets Bin and returns it ("" if not found).
func Find(explicit string) string {
	Bin = ""
	candidates := []string{"ffprobe"}
	if explicit != "" {
		candidates = []string{explicit}
	} else {
		candidates = append(candidates, knownPaths...)
	}
	for _, c := range candidates {
		if p, err := exec.LookPath(c); err == nil {
			Bin = p
			break
		}
	}
	return Bin
}

// Available reports whether an ffprobe executable was found.
func Available() bool {
	if Bin == "" {
		Find("")
	}
	return Bin != ""
}

type stream struct {
	CodecType     string `json:"codec_type"`
	CodecName     string `json:"codec_name"`
	Profile       string `json:"profile"`
	Width         int    `json:"width"`
	Height        int    `json:"height"`
	ColorTransfer string `json:"color_transfer"`
	Channels      int    `json:"channels"`
	SideDataList  []struct {
		SideDataType string `json:"side_data_type"`
	} `json:"side_data_list"`
	Disposition struct {
		AttachedPic int `json:"attached_pic"`
	} `json:"disposition"`
}

// Enrich overwrites technical fields in m with values read from the file.
func Enrich(ctx context.Context, m *media.Media) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, Bin, "-v", "quiet", "-print_format", "json",
		"-show_streams", m.Path).Output()
	if err != nil {
		return fmt.Errorf("ffprobe %s: %w", m.Path, err)
	}
	var res struct{ Streams []stream }
	if err := json.Unmarshal(out, &res); err != nil {
		return fmt.Errorf("ffprobe output: %w", err)
	}
	apply(m, res.Streams)
	return nil
}

func apply(m *media.Media, streams []stream) {
	var videoDone, audioDone bool
	for _, s := range streams {
		switch {
		case s.CodecType == "video" && !videoDone && s.Disposition.AttachedPic == 0:
			videoDone = true
			if r := Resolution(s.Width, s.Height); r != "" {
				m.Resolution = r
			}
			if c := videoCodec(s.CodecName); c != "" {
				m.VideoCodec = c
			}
			if h := hdr(s); h != "" {
				m.HDR = h
			}
		case s.CodecType == "audio" && !audioDone:
			audioDone = true
			if c := audioCodec(s.CodecName, s.Profile); c != "" {
				m.AudioCodec = c
			}
		}
	}
}

// Resolution maps pixel dimensions to a label, tolerant of cropped
// (letterboxed) encodes where height is well under the nominal value.
func Resolution(w, h int) string {
	switch {
	case w >= 3200 || h >= 2000:
		return "2160p"
	case w >= 1700 || h >= 1000:
		return "1080p"
	case w >= 1100 || h >= 700:
		return "720p"
	case w > 0:
		if h > 480 {
			return "576p"
		}
		return "480p"
	}
	return ""
}

func videoCodec(c string) string {
	switch c {
	case "hevc", "h265":
		return "x265"
	case "h264":
		return "x264"
	case "av1":
		return "AV1"
	case "vp9":
		return "VP9"
	case "mpeg4":
		return "XviD"
	case "mpeg2video":
		return "MPEG2"
	}
	return strings.ToUpper(c)
}

func audioCodec(c, profile string) string {
	switch c {
	case "truehd":
		return "TrueHD"
	case "dts":
		return "DTS"
	case "eac3":
		return "EAC3"
	case "ac3":
		return "AC3"
	case "aac":
		return "AAC"
	case "flac":
		return "FLAC"
	case "opus":
		return "OPUS"
	case "mp3":
		return "MP3"
	}
	return strings.ToUpper(c)
}

func hdr(s stream) string {
	for _, sd := range s.SideDataList {
		if strings.Contains(strings.ToLower(sd.SideDataType), "dovi") {
			return "DV"
		}
	}
	switch s.ColorTransfer {
	case "smpte2084":
		return "HDR10"
	case "arib-std-b67":
		return "HLG"
	}
	return ""
}

// Check reports whether ffprobe can read a playable video stream and a
// duration from the file. It catches truncated or corrupt downloads.
func Check(ctx context.Context, path string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, Bin, "-v", "error", "-print_format", "json",
		"-show_entries", "format=duration:stream=codec_type,disposition", path).Output()
	if err != nil {
		return fmt.Errorf("ffprobe cannot read the file (%v)", err)
	}
	var res struct {
		Format  struct{ Duration string }
		Streams []stream
	}
	if err := json.Unmarshal(out, &res); err != nil {
		return fmt.Errorf("ffprobe output: %w", err)
	}
	hasVideo := false
	for _, s := range res.Streams {
		if s.CodecType == "video" && s.Disposition.AttachedPic == 0 {
			hasVideo = true
		}
	}
	if !hasVideo {
		return fmt.Errorf("no video stream")
	}
	if d, _ := strconv.ParseFloat(res.Format.Duration, 64); d <= 0 {
		return fmt.Errorf("no duration (truncated download?)")
	}
	return nil
}

// SubtitleLanguages lists the languages of a file's embedded subtitle
// tracks, as ffprobe reports them ("eng", "spa"...), skipping forced tracks.
func SubtitleLanguages(ctx context.Context, path string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, Bin, "-v", "quiet", "-print_format", "json",
		"-select_streams", "s", "-show_entries", "stream=index:stream_tags=language:stream_disposition=forced", path).Output()
	if err != nil {
		return nil, fmt.Errorf("ffprobe %s: %w", path, err)
	}
	var res struct {
		Streams []struct {
			Tags        struct{ Language string } `json:"tags"`
			Disposition struct {
				Forced int `json:"forced"`
			} `json:"disposition"`
		}
	}
	if err := json.Unmarshal(out, &res); err != nil {
		return nil, err
	}
	var langs []string
	for _, s := range res.Streams {
		if s.Disposition.Forced == 0 && s.Tags.Language != "" {
			langs = append(langs, s.Tags.Language)
		}
	}
	return langs, nil
}

// Durations returns how long the container says the file is and how far
// its video actually goes (the last video packet near the end). A
// truncated download keeps the full declared duration in its header but
// its packets stop early. actual is 0 if it can't be determined.
func Durations(ctx context.Context, path string) (declared, actual float64, err error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, Bin, "-v", "quiet", "-show_entries", "format=duration",
		"-of", "csv=p=0", path).Output()
	if err != nil {
		return 0, 0, fmt.Errorf("ffprobe cannot read the file (%v)", err)
	}
	declared, _ = strconv.ParseFloat(strings.TrimSpace(string(out)), 64)

	// Seeking far past the end lands on the last reachable keyframe.
	out, _ = exec.CommandContext(ctx, Bin, "-v", "quiet", "-select_streams", "v:0",
		"-read_intervals", "999999%+#50", "-show_entries", "packet=pts_time", "-of", "csv=p=0", path).Output()
	for _, line := range strings.Fields(string(out)) {
		if t, err := strconv.ParseFloat(strings.TrimSuffix(line, ","), 64); err == nil && t > actual {
			actual = t
		}
	}
	return declared, actual, nil
}
