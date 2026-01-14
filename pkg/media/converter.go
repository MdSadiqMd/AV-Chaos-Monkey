package media

import (
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
)

var (
	ErrFileNotFound   = errors.New("media file not found")
	ErrFFmpegNotFound = errors.New("ffmpeg not found in PATH")
)

// Convert a video file to H.264 Annex B format
func ConvertVideoToH264(inputPath, outputPath string) error {
	if _, err := os.Stat(inputPath); os.IsNotExist(err) {
		return fmt.Errorf("%w: %s", ErrFileNotFound, inputPath)
	}

	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		return ErrFFmpegNotFound
	}

	log.Printf("[Media] Converting video %s to H.264 Annex B format", inputPath)
	cmd := exec.Command(ffmpegPath,
		"-y",
		"-i", inputPath,
		"-an",
		"-c:v", "libx264",
		"-bsf:v", "h264_mp4toannexb",
		"-b:v", "2M",
		"-max_delay", "0",
		"-bf", "0",
		"-f", "h264",
		outputPath,
	)
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// Convert audio file to Opus format
func ConvertAudioToOpus(inputPath, outputPath string) error {
	if _, err := os.Stat(inputPath); os.IsNotExist(err) {
		return fmt.Errorf("%w: %s", ErrFileNotFound, inputPath)
	}

	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		return ErrFFmpegNotFound
	}

	log.Printf("[Media] Converting audio %s to Opus format", inputPath)
	cmd := exec.Command(ffmpegPath,
		"-y",
		"-i", inputPath,
		"-vn",
		"-c:a", "libopus",
		"-b:a", "128k",
		"-ar", "48000",
		"-ac", "2",
		"-page_duration", "20000",
		outputPath,
	)
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
