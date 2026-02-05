package webrtc

import (
	"encoding/binary"
	"fmt"
	"time"

	"github.com/MdSadiqMd/AV-Chaos-Monkey/pkg/constants"
	"github.com/pion/rtp"
)

func (p *VirtualParticipant) streamVideo() {
	fps := constants.StreamingFPS
	ticker := time.NewTicker(time.Duration(1000/fps) * time.Millisecond)
	defer ticker.Stop()

	seq := uint16(0)
	timestamp := uint32(0)
	hasRealMedia := p.mediaSource != nil && p.mediaSource.GetTotalVideoFrames() > 0

	for range ticker.C {
		if !p.active.Load() {
			return
		}
		if p.shouldDropFrame() {
			p.videoFrameIdx++
			continue
		}

		var frame []byte
		var err error
		if hasRealMedia {
			nal := p.mediaSource.GetVideoNAL(p.videoFrameIdx)
			if nal == nil || len(nal.Data) == 0 {
				p.videoFrameIdx = 0
				nal = p.mediaSource.GetVideoNAL(p.videoFrameIdx)
			}
			if nal != nil && len(nal.Data) > 0 {
				frame = nal.Data
			} else {
				frame, err = p.frameGen.NextFrame()
			}
			p.videoFrameIdx++
		} else {
			frame, err = p.frameGen.NextFrame()
		}
		if err != nil {
			continue
		}

		rtpPackets := packetizeH264(frame, seq, timestamp, p.id)
		for _, pkt := range rtpPackets {
			if p.shouldDropPacket() {
				continue
			}
			extData := make([]byte, 4)
			binary.LittleEndian.PutUint32(extData, p.id)
			if err := p.videoTrack.WriteRTP(pkt); err == nil {
				p.packetsSent.Add(1)
				p.bytesSent.Add(int64(len(pkt.Payload)))
			}
		}
		p.framesSent.Add(1)
		seq += uint16(len(rtpPackets))
		timestamp += uint32(constants.RTPClockRate / fps)
	}
}

func (p *VirtualParticipant) streamAudio() {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()

	seq := uint16(0)
	timestamp := uint32(0)
	const samplesPerFrame = 960
	hasRealMedia := p.mediaSource != nil && p.mediaSource.GetTotalAudioFrames() > 0

	for range ticker.C {
		if !p.active.Load() {
			return
		}
		if p.shouldMuteAudio() {
			silenceFrame := make([]byte, 3)
			silenceFrame[0] = 0xFC
			pkt := &rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 111, SequenceNumber: seq, Timestamp: timestamp, SSRC: p.id*1000 + 1}, Payload: silenceFrame}
			p.audioTrack.WriteRTP(pkt)
			seq++
			timestamp += samplesPerFrame
			continue
		}

		var audioFrame []byte
		if hasRealMedia {
			packet := p.mediaSource.GetAudioPacket(p.audioFrameIdx)
			if packet == nil || len(packet.Data) == 0 {
				p.audioFrameIdx = 0
				packet = p.mediaSource.GetAudioPacket(p.audioFrameIdx)
			}
			if packet != nil && len(packet.Data) > 0 {
				audioFrame = packet.Data
			} else {
				audioFrame = generateOpusFrame(p.id, seq)
			}
			p.audioFrameIdx++
		} else {
			audioFrame = generateOpusFrame(p.id, seq)
		}

		pkt := &rtp.Packet{Header: rtp.Header{Version: 2, PayloadType: 111, SequenceNumber: seq, Timestamp: timestamp, SSRC: p.id*1000 + 1}, Payload: audioFrame}
		p.audioTrack.WriteRTP(pkt)
		seq++
		timestamp += samplesPerFrame
	}
}

func (p *VirtualParticipant) shouldDropFrame() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, params := range p.activeSpikes {
		if params["type"] == "frame_drop" {
			if dropPct, ok := params["drop_percentage"]; ok {
				var pct int
				fmt.Sscanf(dropPct, "%d", &pct)
				return int(time.Now().UnixNano()%100) < pct
			}
		}
	}
	return false
}

func (p *VirtualParticipant) shouldDropPacket() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, params := range p.activeSpikes {
		if params["type"] == "rtp_packet_loss" {
			if lossPct, ok := params["loss_percentage"]; ok {
				var pct int
				fmt.Sscanf(lossPct, "%d", &pct)
				return int(time.Now().UnixNano()%100) < pct
			}
		}
	}
	return false
}

func (p *VirtualParticipant) shouldMuteAudio() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, params := range p.activeSpikes {
		if params["type"] == "audio_silence" {
			return true
		}
	}
	return false
}
