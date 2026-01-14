package media

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
)

var (
	ErrNoMediaFile    = errors.New("no media file specified")
	ErrFileNotFound   = errors.New("media file not found")
	ErrFFmpegNotFound = errors.New("ffmpeg not found in PATH")
	ErrInvalidFormat  = errors.New("invalid media format")
)

type MediaConfig struct {
	VideoPath    string
	AudioPath    string
	VideoEnabled bool
	AudioEnabled bool
}

// Reads H.264 NAL units from an Annex B stream
type H264Reader struct {
	reader    *bufio.Reader
	file      *os.File
	cmd       *exec.Cmd
	mu        sync.Mutex
	closed    bool
	nalBuffer []byte
}

// Reads Opus packets from an OGG file
type OpusReader struct {
	reader *bufio.Reader
	file   *os.File
	cmd    *exec.Cmd
	mu     sync.Mutex
	closed bool
}

// H.264 NAL unit
type NALUnit struct {
	Data []byte
}

// Opus audio packet
type OpusPacket struct {
	Data []byte
}

// Extracts video and audio from a container file (mkv/mp4)
// Returns paths to the extracted h264 and opus files
func ExtractMedia(inputPath, outputDir string) (videoPath, audioPath string, err error) {
	if _, err := os.Stat(inputPath); os.IsNotExist(err) {
		return "", "", fmt.Errorf("%w: %s", ErrFileNotFound, inputPath)
	}

	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		return "", "", ErrFFmpegNotFound
	}

	baseName := filepath.Base(inputPath)
	ext := filepath.Ext(baseName)
	name := baseName[:len(baseName)-len(ext)]

	videoPath = filepath.Join(outputDir, name+".h264")
	audioPath = filepath.Join(outputDir, name+".opus")

	// Extract H.264 video stream (Annex B format)
	log.Printf("[Media] Extracting H.264 video from %s to %s", inputPath, videoPath)
	videoCmd := exec.Command(ffmpegPath,
		"-y",            // Overwrite output
		"-i", inputPath, // Input file
		"-an",             // No audio
		"-c:v", "libx264", // H.264 codec
		"-bsf:v", "h264_mp4toannexb", // Convert to Annex B format
		"-b:v", "2M", // Bitrate
		"-max_delay", "0",
		"-bf", "0", // No B-frames for lower latency
		"-f", "h264", // Output format
		videoPath,
	)
	videoCmd.Stderr = os.Stderr
	if err := videoCmd.Run(); err != nil {
		log.Printf("[Media] Warning: Failed to extract video: %v", err)
		videoPath = ""
	}

	// Extract Opus audio stream
	log.Printf("[Media] Extracting Opus audio from %s to %s", inputPath, audioPath)
	audioCmd := exec.Command(ffmpegPath,
		"-y",            // Overwrite output
		"-i", inputPath, // Input file
		"-vn",             // No video
		"-c:a", "libopus", // Opus codec
		"-b:a", "128k", // Audio bitrate
		"-ar", "48000", // Sample rate
		"-ac", "2", // Stereo
		"-page_duration", "20000", // 20ms pages for RTP
		audioPath,
	)
	audioCmd.Stderr = os.Stderr
	if err := audioCmd.Run(); err != nil {
		log.Printf("[Media] Warning: Failed to extract audio: %v", err)
		audioPath = ""
	}

	if videoPath == "" && audioPath == "" {
		return "", "", fmt.Errorf("failed to extract any media from %s", inputPath)
	}

	return videoPath, audioPath, nil
}

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

func NewH264Reader(path string) (*H264Reader, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("failed to open H.264 file: %w", err)
	}

	return &H264Reader{
		reader:    bufio.NewReaderSize(file, 1024*1024), // 1MB buffer
		file:      file,
		nalBuffer: make([]byte, 0, 1024*1024),
	}, nil
}

// NextNAL reads the next NAL unit from the H.264 stream
// H.264 Annex B format uses start codes: 0x00 0x00 0x01 or 0x00 0x00 0x00 0x01
func (r *H264Reader) NextNAL() (*NALUnit, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed {
		return nil, io.EOF
	}

	// Reset buffer
	r.nalBuffer = r.nalBuffer[:0]

	// Find start code
	if err := r.skipToStartCode(); err != nil {
		return nil, err
	}

	// Read until next start code or EOF
	for {
		b, err := r.reader.ReadByte()
		if err != nil {
			if err == io.EOF && len(r.nalBuffer) > 0 {
				// Return last NAL
				nal := &NALUnit{Data: make([]byte, len(r.nalBuffer))}
				copy(nal.Data, r.nalBuffer)
				return nal, nil
			}
			return nil, err
		}

		r.nalBuffer = append(r.nalBuffer, b)

		// Check for start code at end of buffer
		bufLen := len(r.nalBuffer)
		if bufLen >= 4 {
			// Check for 4-byte start code
			if r.nalBuffer[bufLen-4] == 0 && r.nalBuffer[bufLen-3] == 0 &&
				r.nalBuffer[bufLen-2] == 0 && r.nalBuffer[bufLen-1] == 1 {
				// Found start code, return NAL without it
				nal := &NALUnit{Data: make([]byte, bufLen-4)}
				copy(nal.Data, r.nalBuffer[:bufLen-4])
				r.nalBuffer = r.nalBuffer[:0]
				return nal, nil
			}
		}
		if bufLen >= 3 {
			// Check for 3-byte start code
			if r.nalBuffer[bufLen-3] == 0 && r.nalBuffer[bufLen-2] == 0 &&
				r.nalBuffer[bufLen-1] == 1 {
				// Found start code, return NAL without it
				nal := &NALUnit{Data: make([]byte, bufLen-3)}
				copy(nal.Data, r.nalBuffer[:bufLen-3])
				r.nalBuffer = r.nalBuffer[:0]
				return nal, nil
			}
		}
	}
}

func (r *H264Reader) skipToStartCode() error {
	zeroCount := 0
	for {
		b, err := r.reader.ReadByte()
		if err != nil {
			return err
		}

		if b == 0 {
			zeroCount++
		} else if b == 1 && zeroCount >= 2 {
			// Found start code
			return nil
		} else {
			zeroCount = 0
		}
	}
}

// Resets the reader to the beginning of the file
func (r *H264Reader) Reset() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, err := r.file.Seek(0, 0); err != nil {
		return err
	}
	r.reader.Reset(r.file)
	r.nalBuffer = r.nalBuffer[:0]
	return nil
}

// Closes the H.264 reader
func (r *H264Reader) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.closed = true
	if r.cmd != nil && r.cmd.Process != nil {
		r.cmd.Process.Kill()
	}
	if r.file != nil {
		return r.file.Close()
	}
	return nil
}
