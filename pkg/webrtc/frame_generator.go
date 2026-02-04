package webrtc

import (
	"sync"

	"github.com/MdSadiqMd/AV-Chaos-Monkey/pkg/constants"
)

// Generates H.264 frames with proper NALU structure
type H264FrameGenerator struct {
	config     VideoConfig
	frameCount int64
	mu         sync.Mutex
	buffer     []byte
}

func NewH264FrameGenerator(config VideoConfig) *H264FrameGenerator {
	return &H264FrameGenerator{config: config}
}

func (g *H264FrameGenerator) NextFrame() ([]byte, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.frameCount++

	fps := g.config.FPS
	if fps <= 0 {
		fps = constants.DefaultFPS
	}
	bitrate := g.config.BitrateKbps
	if bitrate <= 0 {
		bitrate = constants.DefaultBitrateKbps
	}

	frameSize := (bitrate * 1000 / 8) / fps
	frameSize = max(constants.MinFrameSize, min(frameSize, constants.MaxFrameSize))

	if cap(g.buffer) < frameSize {
		g.buffer = make([]byte, frameSize)
	} else {
		g.buffer = g.buffer[:frameSize]
	}
	data := g.buffer

	if g.frameCount%constants.KeyframeInterval == 0 {
		data[0] = 0x65 // IDR frame (keyframe)
	} else {
		data[0] = 0x41 // P-frame
	}

	for i := 1; i < frameSize; i++ {
		data[i] = byte((i*3 + int(g.frameCount)*7) % 256)
	}

	return data, nil
}

func generateOpusFrame(participantID uint32, seq uint16) []byte {
	frameSize := 80
	frame := make([]byte, frameSize)
	frame[0] = 0x50 // CELT 20ms mono
	for i := 1; i < frameSize; i++ {
		frame[i] = byte((i*int(participantID) + int(seq)) % 256)
	}
	return frame
}
