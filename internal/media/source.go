package media

import (
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Provides shared access to media files for all participants
type MediaSource struct {
	mu sync.RWMutex

	config MediaConfig

	// Cached media data (loaded once, shared by all participants)
	videoNALs   []*NALUnit
	audioFrames []*OpusPacket

	// Timing info
	videoFPS      int
	audioDuration time.Duration // Duration per audio packet (20ms for Opus)

	totalVideoFrames int           // Total number of video frames
	totalAudioFrames int           // Total number of audio frames
	mediaDuration    time.Duration // Estimated media duration

	// State
	initialized bool
}

// Global media source instance
var (
	globalSource     *MediaSource
	globalSourceOnce sync.Once
)

func GetGlobalMediaSource() *MediaSource {
	globalSourceOnce.Do(func() {
		globalSource = &MediaSource{
			videoFPS:      60, // Default to 60fps for streaming
			audioDuration: 20 * time.Millisecond,
		}
	})
	return globalSource
}

func (ms *MediaSource) Initialize(config MediaConfig) error {
	ms.mu.Lock()
	defer ms.mu.Unlock()

	if ms.initialized {
		log.Printf("[MediaSource] Already initialized, skipping")
		return nil
	}

	ms.config = config
	log.Printf("[MediaSource] Initializing with config: video=%s, audio=%s, videoEnabled=%v, audioEnabled=%v",
		config.VideoPath, config.AudioPath, config.VideoEnabled, config.AudioEnabled)

	// Creating temp directory for extracted media
	tempDir := filepath.Join(os.TempDir(), "av-chaos-monkey-media")
	if err := os.MkdirAll(tempDir, 0755); err != nil {
		return fmt.Errorf("failed to create temp directory: %w", err)
	}

	// Handle video
	if config.VideoEnabled && config.VideoPath != "" {
		if err := ms.loadVideo(config.VideoPath, tempDir); err != nil {
			log.Printf("[MediaSource] Warning: Failed to load video: %v", err)
			// Don't fail completely, just disable video
			ms.config.VideoEnabled = false
		}
	}

	// Handle audio
	if config.AudioEnabled && config.AudioPath != "" {
		if err := ms.loadAudio(config.AudioPath, tempDir); err != nil {
			log.Printf("[MediaSource] Warning: Failed to load audio: %v", err)
			// Don't fail completely, just disable audio
			ms.config.AudioEnabled = false
		}
	}

	ms.initialized = true

	// Calculate media duration based on content
	ms.totalVideoFrames = len(ms.videoNALs)
	ms.totalAudioFrames = len(ms.audioFrames)

	// Duration is determined by the longer track
	videoDuration := time.Duration(ms.totalVideoFrames) * time.Second / time.Duration(ms.videoFPS)
	audioDuration := time.Duration(ms.totalAudioFrames) * ms.audioDuration

	ms.mediaDuration = max(videoDuration, audioDuration)
	log.Printf("[MediaSource] Initialized: %d video NALs, %d audio frames, duration=%.1fs",
		len(ms.videoNALs), len(ms.audioFrames), ms.mediaDuration.Seconds())

	return nil
}

// loads video NAL units from a file
func (ms *MediaSource) loadVideo(path string, tempDir string) error {
	ext := filepath.Ext(path)
	h264Path := path

	// If not already H.264, convert it
	if ext != ".h264" && ext != ".264" {
		h264Path = filepath.Join(tempDir, "video.h264")
		if err := ConvertVideoToH264(path, h264Path); err != nil {
			return fmt.Errorf("failed to convert video to H.264: %w", err)
		}
	}

	// Read all NAL units
	reader, err := NewH264Reader(h264Path)
	if err != nil {
		return fmt.Errorf("failed to create H.264 reader: %w", err)
	}
	defer reader.Close()

	ms.videoNALs = make([]*NALUnit, 0, 1000)
	for {
		nal, err := reader.NextNAL()
		if err != nil {
			if err == io.EOF {
				break
			}
			return fmt.Errorf("error reading NAL: %w", err)
		}
		if len(nal.Data) > 0 {
			// Make a copy to avoid buffer reuse issues
			nalCopy := &NALUnit{Data: make([]byte, len(nal.Data))}
			copy(nalCopy.Data, nal.Data)
			ms.videoNALs = append(ms.videoNALs, nalCopy)
		}
	}

	log.Printf("[MediaSource] Loaded %d video NAL units from %s", len(ms.videoNALs), path)
	return nil
}

// loads audio packets from a file
func (ms *MediaSource) loadAudio(path string, tempDir string) error {
	ext := filepath.Ext(path)
	opusPath := path

	// If not already Opus/OGG, convert it
	if ext != ".opus" && ext != ".ogg" {
		opusPath = filepath.Join(tempDir, "audio.opus")
		if err := ConvertAudioToOpus(path, opusPath); err != nil {
			return fmt.Errorf("failed to convert audio to Opus: %w", err)
		}
	}

	// Try OGG reader first
	reader, err := NewOggReader(opusPath)
	if err != nil {
		log.Printf("[MediaSource] OGG reader failed, trying simple reader: %v", err)
		// Fall back to simple reader
		return ms.loadAudioSimple(opusPath)
	}
	defer reader.Close()

	ms.audioFrames = make([]*OpusPacket, 0, 1000)
	for {
		packet, err := reader.NextPacket()
		if err != nil {
			if err == io.EOF {
				break
			}
			// Some OGG files have issues, try to continue
			log.Printf("[MediaSource] Warning reading audio packet: %v", err)
			break
		}
		if len(packet.Data) > 0 {
			// Make a copy
			packetCopy := &OpusPacket{Data: make([]byte, len(packet.Data))}
			copy(packetCopy.Data, packet.Data)
			ms.audioFrames = append(ms.audioFrames, packetCopy)
		}
	}

	log.Printf("[MediaSource] Loaded %d audio packets from %s", len(ms.audioFrames), path)
	return nil
}

// loads audio using simple reader
func (ms *MediaSource) loadAudioSimple(path string) error {
	reader, err := NewSimpleOpusReader(path)
	if err != nil {
		return err
	}
	defer reader.Close()

	ms.audioFrames = make([]*OpusPacket, 0, 1000)
	for {
		packet, err := reader.NextPacket()
		if err != nil {
			if err == io.EOF {
				break
			}
			return err
		}
		if len(packet.Data) > 0 {
			packetCopy := &OpusPacket{Data: make([]byte, len(packet.Data))}
			copy(packetCopy.Data, packet.Data)
			ms.audioFrames = append(ms.audioFrames, packetCopy)
		}
	}

	return nil
}

// Returns the NAL unit at the given index, or nil if past end
func (ms *MediaSource) GetVideoNAL(index int) *NALUnit {
	ms.mu.RLock()
	defer ms.mu.RUnlock()

	if len(ms.videoNALs) == 0 || index >= len(ms.videoNALs) {
		return nil // End of media, no looping
	}

	return ms.videoNALs[index]
}

// Returns the audio packet at the given index, or nil if past end
func (ms *MediaSource) GetAudioPacket(index int) *OpusPacket {
	ms.mu.RLock()
	defer ms.mu.RUnlock()

	if len(ms.audioFrames) == 0 || index >= len(ms.audioFrames) {
		return nil // End of media, no looping
	}

	return ms.audioFrames[index]
}

// is video streaming enabled
func (ms *MediaSource) IsVideoEnabled() bool {
	ms.mu.RLock()
	defer ms.mu.RUnlock()
	return ms.config.VideoEnabled && len(ms.videoNALs) > 0
}

// is audio streaming enabled
func (ms *MediaSource) IsAudioEnabled() bool {
	ms.mu.RLock()
	defer ms.mu.RUnlock()
	return ms.config.AudioEnabled && len(ms.audioFrames) > 0
}

// number of video NAL units
func (ms *MediaSource) GetVideoCount() int {
	ms.mu.RLock()
	defer ms.mu.RUnlock()
	return len(ms.videoNALs)
}

// number of audio packets
func (ms *MediaSource) GetAudioCount() int {
	ms.mu.RLock()
	defer ms.mu.RUnlock()
	return len(ms.audioFrames)
}

// current media configuration
func (ms *MediaSource) GetConfig() MediaConfig {
	ms.mu.RLock()
	defer ms.mu.RUnlock()
	return ms.config
}

func (ms *MediaSource) GetMediaDuration() time.Duration {
	ms.mu.RLock()
	defer ms.mu.RUnlock()
	return ms.mediaDuration
}

func (ms *MediaSource) GetTotalVideoFrames() int {
	ms.mu.RLock()
	defer ms.mu.RUnlock()
	return ms.totalVideoFrames
}

func (ms *MediaSource) GetTotalAudioFrames() int {
	ms.mu.RLock()
	defer ms.mu.RUnlock()
	return ms.totalAudioFrames
}

// sets the FPS used for streaming calculations
func (ms *MediaSource) SetStreamingFPS(fps int) {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	ms.videoFPS = fps
	// Recalculate duration
	if ms.totalVideoFrames > 0 {
		ms.mediaDuration = time.Duration(ms.totalVideoFrames) * time.Second / time.Duration(fps)
	}
	log.Printf("[MediaSource] Streaming FPS set to %d, duration=%.1fs", fps, ms.mediaDuration.Seconds())
}

// get current streaming FPS
func (ms *MediaSource) GetStreamingFPS() int {
	ms.mu.RLock()
	defer ms.mu.RUnlock()
	return ms.videoFPS
}

// Reset clears the media source for reinitialization
func (ms *MediaSource) Reset() {
	ms.mu.Lock()
	defer ms.mu.Unlock()

	ms.videoNALs = nil
	ms.audioFrames = nil
	ms.initialized = false
	ms.config = MediaConfig{}
}
