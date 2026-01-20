package media

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
)

type MediaInfo struct {
	Duration float64
	Width    int
	Height   int
	FPS      float64
	Codec    string
}

func ProbeMedia(path string) (*MediaInfo, error) {
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		return nil, fmt.Errorf("ffprobe not found: %w", err)
	}

	cmd := exec.Command(ffprobe,
		"-v", "quiet",
		"-print_format", "json",
		"-show_format",
		"-show_streams",
		path,
	)

	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("ffprobe failed: %w", err)
	}

	var probe struct {
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
		Streams []struct {
			CodecType  string `json:"codec_type"`
			CodecName  string `json:"codec_name"`
			Width      int    `json:"width"`
			Height     int    `json:"height"`
			RFrameRate string `json:"r_frame_rate"`
		} `json:"streams"`
	}

	if err := json.Unmarshal(output, &probe); err != nil {
		return nil, fmt.Errorf("failed to parse ffprobe output: %w", err)
	}

	info := &MediaInfo{}
	if probe.Format.Duration != "" {
		info.Duration, _ = strconv.ParseFloat(probe.Format.Duration, 64)
	}

	for _, stream := range probe.Streams {
		if stream.CodecType == "video" {
			info.Width = stream.Width
			info.Height = stream.Height
			info.Codec = stream.CodecName
			info.FPS = parseFPS(stream.RFrameRate)
			break
		}
	}

	return info, nil
}

// parse a frame rate string like "30/1" or "29.97".
func parseFPS(fpsStr string) float64 {
	if fpsStr == "" {
		return 0
	}

	// Try parsing as fraction (e.g., "30/1")
	var num, den int
	if n, _ := fmt.Sscanf(fpsStr, "%d/%d", &num, &den); n == 2 && den > 0 {
		return float64(num) / float64(den)
	}

	// Try parsing as decimal
	fps, _ := strconv.ParseFloat(fpsStr, 64)
	return fps
}
