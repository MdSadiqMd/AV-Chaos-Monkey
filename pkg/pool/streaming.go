package pool

import (
	"fmt"
	"math/rand"
	"time"

	"github.com/MdSadiqMd/AV-Chaos-Monkey/pkg/constants"
	"github.com/MdSadiqMd/AV-Chaos-Monkey/pkg/logging"
)

func (p *VirtualParticipant) runFrameLoop() {
	fps := p.VideoConfig.Fps
	if fps <= 0 {
		fps = constants.DefaultFPS
	}
	ticker := time.NewTicker(time.Duration(1000/fps) * time.Millisecond)
	defer ticker.Stop()

	for range ticker.C {
		if !p.active.Load() {
			return
		}
		if p.shouldDropFrame() {
			p.metrics.RecordFrameDropped()
			continue
		}
		frame := p.generateSyntheticFrame()
		packets := p.packetizer.Packetize(frame, p.sequencer, p.timestamp)
		p.frameCount.Add(1)
		p.sequencer += uint16(len(packets))
		p.timestamp += uint32(constants.RTPClockRate / fps)

		for _, pkt := range packets {
			data := pkt.Marshal()
			p.packetsSent.Add(1)
			p.bytesSent.Add(int64(len(data)))
			p.metrics.RecordPacketSent(len(data))
			if p.shouldDropPacket() {
				p.metrics.RecordPacketLost()
				continue
			}
			if _, err := p.udpConn.WriteToUDP(data, p.targetAddr); err != nil {
				p.metrics.RecordPacketLost()
				logging.LogError("Participant %d: UDP send error: %v", p.ID, err)
			}
		}
	}
}

func (p *VirtualParticipant) generateSyntheticFrame() []byte {
	fps := p.VideoConfig.Fps
	if fps <= 0 {
		fps = constants.DefaultFPS
	}
	bitrateKbps := p.VideoConfig.BitrateKbps
	if bitrateKbps <= 0 {
		bitrateKbps = constants.DefaultBitrateKbps
	}
	frameSize := (bitrateKbps * 1000 / 8) / int32(fps)
	frameSize = max(constants.MinFrameSize, min(frameSize, constants.MaxFrameSize))

	p.frameBufferMu.Lock()
	if cap(p.frameBuffer) < int(frameSize) {
		p.frameBuffer = make([]byte, frameSize)
	} else {
		p.frameBuffer = p.frameBuffer[:frameSize]
	}
	data := p.frameBuffer
	p.frameBufferMu.Unlock()

	frameNum := int(p.frameCount.Load())
	if frameNum%constants.KeyframeInterval == 0 {
		data[0] = 0x65
	} else {
		data[0] = 0x41
	}
	idOffset := int(p.ID)
	for i := 1; i < int(frameSize); i++ {
		data[i] = byte((i*3 + frameNum*7 + idOffset*11) % 256)
	}
	return data
}

func (p *VirtualParticipant) runVideoLoop() {
	fps := constants.StreamingFPS
	ticker := time.NewTicker(time.Duration(1000/fps) * time.Millisecond)
	defer ticker.Stop()

	totalFrames := p.mediaSource.GetTotalVideoFrames()
	logging.LogInfo("Participant %d: Starting video stream: %d frames at %dfps (looping)", p.ID, totalFrames, fps)

	for range ticker.C {
		if !p.active.Load() {
			return
		}
		if p.shouldDropFrame() {
			p.metrics.RecordFrameDropped()
			p.videoFrameIdx++
			continue
		}
		nal := p.mediaSource.GetVideoNAL(p.videoFrameIdx)
		if nal == nil || len(nal.Data) == 0 {
			p.videoFrameIdx = 0
			nal = p.mediaSource.GetVideoNAL(p.videoFrameIdx)
			if nal == nil || len(nal.Data) == 0 {
				go p.runFrameLoop()
				return
			}
		}
		packets := p.packetizer.Packetize(nal.Data, p.sequencer, p.timestamp)
		p.frameCount.Add(1)
		p.videoFrameIdx++
		p.sequencer += uint16(len(packets))
		p.timestamp += uint32(constants.RTPClockRate / int32(fps))

		for _, pkt := range packets {
			data := pkt.Marshal()
			p.packetsSent.Add(1)
			p.bytesSent.Add(int64(len(data)))
			p.metrics.RecordPacketSent(len(data))
			if p.shouldDropPacket() {
				p.metrics.RecordPacketLost()
				continue
			}
			if _, err := p.udpConn.WriteToUDP(data, p.targetAddr); err != nil {
				p.metrics.RecordPacketLost()
			}
		}
	}
}

func (p *VirtualParticipant) runAudioLoop() {
	ticker := time.NewTicker(constants.OpusFrameDuration * time.Millisecond)
	defer ticker.Stop()

	totalFrames := p.mediaSource.GetTotalAudioFrames()
	logging.LogInfo("Participant %d: Starting audio stream: %d frames (looping)", p.ID, totalFrames)

	for range ticker.C {
		if !p.active.Load() {
			return
		}
		packet := p.mediaSource.GetAudioPacket(p.audioFrameIdx)
		if packet == nil || len(packet.Data) == 0 {
			p.audioFrameIdx = 0
			packet = p.mediaSource.GetAudioPacket(p.audioFrameIdx)
			if packet == nil || len(packet.Data) == 0 {
				return
			}
		}
		rtpPacket := p.audioPacketizer.Packetize(packet.Data, p.audioSequencer, p.audioTimestamp)
		p.audioFrameIdx++
		p.audioSequencer++
		p.audioTimestamp += 960

		data := rtpPacket.Marshal()
		p.packetsSent.Add(1)
		p.bytesSent.Add(int64(len(data)))
		p.metrics.RecordPacketSent(len(data))
		if p.shouldDropPacket() {
			p.metrics.RecordPacketLost()
			continue
		}
		if _, err := p.udpConn.WriteToUDP(data, p.targetAddr); err != nil {
			p.metrics.RecordPacketLost()
		}
	}
}

func (p *VirtualParticipant) shouldDropFrame() bool {
	p.activeSpikesMu.RLock()
	defer p.activeSpikesMu.RUnlock()
	for _, spike := range p.activeSpikes {
		if spike.Type == "frame_drop" {
			if dropPct, ok := spike.Params["drop_percentage"]; ok {
				var pct int
				fmt.Sscanf(dropPct, "%d", &pct)
				return rand.Intn(100) < pct
			}
		}
	}
	return false
}

func (p *VirtualParticipant) shouldDropPacket() bool {
	p.activeSpikesMu.RLock()
	defer p.activeSpikesMu.RUnlock()
	for _, spike := range p.activeSpikes {
		if spike.Type == "rtp_packet_loss" {
			if lossPct, ok := spike.Params["loss_percentage"]; ok {
				var pct int
				fmt.Sscanf(lossPct, "%d", &pct)
				return rand.Intn(100) < pct
			}
		}
	}
	return false
}
