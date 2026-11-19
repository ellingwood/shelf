// Package probe reads real stream info (resolution, codecs, HDR) with
// ffprobe, so naming doesn't depend on what a filename claims.
package probe

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
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
