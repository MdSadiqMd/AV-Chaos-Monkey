package webrtc

import (
	"encoding/binary"
	"fmt"
	"log"
	"maps"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/MdSadiqMd/AV-Chaos-Monkey/pkg/constants"
	"github.com/MdSadiqMd/AV-Chaos-Monkey/pkg/media"
	"github.com/pion/rtp"
	pionwebrtc "github.com/pion/webrtc/v3"
)

type VideoConfig struct {
	Width       int
	Height      int
	FPS         int
	BitrateKbps int
	Codec       string
}

type VirtualParticipant struct {
	mu         sync.RWMutex
	id         uint32
	peerConn   *pionwebrtc.PeerConnection
	videoTrack *pionwebrtc.TrackLocalStaticRTP
	audioTrack *pionwebrtc.TrackLocalStaticRTP
	frameGen   *H264FrameGenerator
	active     atomic.Bool

	// Media source
	mediaSource   *media.MediaSource
	videoFrameIdx int
	audioFrameIdx int

	// Metrics
	framesSent  atomic.Int64
	packetsSent atomic.Int64
	bytesSent   atomic.Int64

	// Spikes
	activeSpikes map[string]map[string]string // spike_id -> params
}

// Generating H.264 frames with proper NALU structure
type H264FrameGenerator struct {
	config     VideoConfig
	frameCount int64
	mu         sync.Mutex
	buffer     []byte
}

func NewH264FrameGenerator(config VideoConfig) *H264FrameGenerator {
	return &H264FrameGenerator{
		config: config,
	}
}

// Next H.264 frame with proper NALU structure
func (g *H264FrameGenerator) NextFrame() ([]byte, error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.frameCount++

	// Calculate frame size based on bitrate
	fps := g.config.FPS
	if fps <= 0 {
		fps = constants.DefaultFPS
	}
	bitrate := g.config.BitrateKbps
	if bitrate <= 0 {
		bitrate = constants.DefaultBitrateKbps
	}

	// Frame size: bitrate_kbps * 1000 / 8 / fps
	frameSize := (bitrate * 1000 / 8) / fps
	frameSize = max(constants.MinFrameSize, min(frameSize, constants.MaxFrameSize))

	// Reuse buffer
	if cap(g.buffer) < frameSize {
		g.buffer = make([]byte, frameSize)
	} else {
		g.buffer = g.buffer[:frameSize]
	}
	data := g.buffer

	// Create H.264 NALU structure
	// NAL unit header (1 byte): forbidden_zero_bit(1) | nal_ref_idc(2) | nal_unit_type(5)
	if g.frameCount%constants.KeyframeInterval == 0 {
		// IDR frame (keyframe)
		data[0] = 0x65 // nal_ref_idc=3, nal_unit_type=5 (IDR slice)
	} else {
		// P-frame
		data[0] = 0x41 // nal_ref_idc=2, nal_unit_type=1 (non-IDR slice)
	}

	// Fill with realistic-looking encoded data
	for i := 1; i < frameSize; i++ {
		data[i] = byte((i*3 + int(g.frameCount)*7) % 256)
	}

	return data, nil
}

// Creates new virtual participant with WebRTC connection
func NewParticipant(id uint32, config VideoConfig) (*VirtualParticipant, error) {
	// Create media engine with codecs
	m := &pionwebrtc.MediaEngine{}
	if err := m.RegisterDefaultCodecs(); err != nil {
		return nil, fmt.Errorf("failed to register codecs: %w", err)
	}

	// Create API with settings
	settingEngine := pionwebrtc.SettingEngine{}
	// Enable non-standard extensions for testing
	settingEngine.SetSRTPReplayProtectionWindow(512)

	// Configure ICE timeouts for faster connection establishment
	// Reduced timeouts to fail faster if TURN is not available
	settingEngine.SetICETimeouts(5*time.Second, 15*time.Second, 3*time.Second)

	// Note: For Kubernetes deployments, the server generates ICE candidates with pod IPs
	// (e.g., 10.244.0.x) which the client cannot reach directly. This requires:
	// 1. A TURN server for NAT traversal (recommended for production) - configured below
	// 2. TURN servers must be accessible from within the Kubernetes cluster
	// 3. Without TURN, WebRTC connections will fail in Kubernetes environments

	api := pionwebrtc.NewAPI(
		pionwebrtc.WithMediaEngine(m),
		pionwebrtc.WithSettingEngine(settingEngine),
	)

	// Create peer connection config with real ICE servers
	// Note: For Kubernetes deployments, TURN servers are essential for NAT traversal
	// The STUN servers help with discovery, but TURN is needed for relay when both peers are behind NAT
	// Get TURN server from environment or use default
	turnHost := os.Getenv("TURN_HOST")
	if turnHost == "" {
		turnHost = "coturn"
	}
	turnUsername := os.Getenv("TURN_USERNAME")
	if turnUsername == "" {
		turnUsername = "webrtc"
	}
	turnPassword := os.Getenv("TURN_PASSWORD")
	if turnPassword == "" {
		turnPassword = "webrtc123"
	}

	pcConfig := pionwebrtc.Configuration{
		ICEServers: []pionwebrtc.ICEServer{
			{URLs: []string{"stun:stun.l.google.com:19302"}},
			{URLs: []string{"stun:stun1.l.google.com:19302"}},
			// Local TURN server (coturn in docker-compose or Kubernetes)
			{URLs: []string{fmt.Sprintf("turn:%s:3478", turnHost)}, Username: turnUsername, Credential: turnPassword},
			{URLs: []string{fmt.Sprintf("turn:%s:3478?transport=tcp", turnHost)}, Username: turnUsername, Credential: turnPassword},
			// Fallback to public TURN servers if local server is not available
			{URLs: []string{"turn:openrelay.metered.ca:80"}, Username: "openrelayproject", Credential: "openrelayproject"},
			{URLs: []string{"turn:openrelay.metered.ca:443"}, Username: "openrelayproject", Credential: "openrelayproject"},
		},
		// Use ICETransportPolicyAll to try all candidates (host, srflx, relay)
		// If TURN is not accessible, connection will fail (expected in Kubernetes without proper TURN)
		ICETransportPolicy: pionwebrtc.ICETransportPolicyAll,
		BundlePolicy:       pionwebrtc.BundlePolicyMaxBundle,
		RTCPMuxPolicy:      pionwebrtc.RTCPMuxPolicyRequire,
	}

	// Create peer connection
	pc, err := api.NewPeerConnection(pcConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create peer connection: %w", err)
	}

	// Set up connection state callback
	pc.OnConnectionStateChange(func(state pionwebrtc.PeerConnectionState) {
		log.Printf("[WebRTC] Participant %d connection state: %s", id, state.String())
	})
	pc.OnICEConnectionStateChange(func(state pionwebrtc.ICEConnectionState) {
		log.Printf("[WebRTC] Participant %d ICE state: %s", id, state.String())
		if state == pionwebrtc.ICEConnectionStateFailed {
			log.Printf("[WebRTC] Participant %d: ICE connection failed. This may be due to NAT traversal issues.", id)
		}
	})

	pc.OnICECandidate(func(candidate *pionwebrtc.ICECandidate) {
		if candidate != nil {
			candidateType := candidate.Typ.String()
			log.Printf("[WebRTC] Participant %d: Generated ICE candidate [%s]: %s", id, candidateType, candidate.String())
			switch candidate.Typ {
			case pionwebrtc.ICECandidateTypeRelay:
				log.Printf("[WebRTC] Participant %d: ✅ TURN relay candidate generated! This is required for Kubernetes NAT traversal.", id)
			case pionwebrtc.ICECandidateTypeHost:
				log.Printf("[WebRTC] Participant %d: ⚠️  Host candidate (pod IP) - client may not be able to reach this", id)
			case pionwebrtc.ICECandidateTypeSrflx:
				log.Printf("[WebRTC] Participant %d: ℹ️  Server reflexive candidate (via STUN) - may work if NAT allows", id)
			}
		} else {
			log.Printf("[WebRTC] Participant %d: ICE candidate gathering complete", id)
		}
	})

	// Create video track with H.264
	videoTrack, err := pionwebrtc.NewTrackLocalStaticRTP(
		pionwebrtc.RTPCodecCapability{
			MimeType:    pionwebrtc.MimeTypeH264,
			ClockRate:   constants.RTPClockRate,
			SDPFmtpLine: "profile-level-id=42e01f;packetization-mode=1",
		},
		fmt.Sprintf("video-%d", id),
		fmt.Sprintf("stream-%d", id),
	)
	if err != nil {
		pc.Close()
		return nil, fmt.Errorf("failed to create video track: %w", err)
	}

	// Create audio track with Opus
	audioTrack, err := pionwebrtc.NewTrackLocalStaticRTP(
		pionwebrtc.RTPCodecCapability{
			MimeType:  pionwebrtc.MimeTypeOpus,
			ClockRate: 48000,
			Channels:  2,
		},
		fmt.Sprintf("audio-%d", id),
		fmt.Sprintf("stream-%d", id),
	)
	if err != nil {
		pc.Close()
		return nil, fmt.Errorf("failed to create audio track: %w", err)
	}

	// Add tracks to peer connection
	if _, err := pc.AddTrack(videoTrack); err != nil {
		pc.Close()
		return nil, fmt.Errorf("failed to add video track: %w", err)
	}

	if _, err := pc.AddTrack(audioTrack); err != nil {
		pc.Close()
		return nil, fmt.Errorf("failed to add audio track: %w", err)
	}

	participant := &VirtualParticipant{
		id:           id,
		peerConn:     pc,
		videoTrack:   videoTrack,
		audioTrack:   audioTrack,
		frameGen:     NewH264FrameGenerator(config),
		mediaSource:  media.GetGlobalMediaSource(),
		activeSpikes: make(map[string]map[string]string),
	}

	log.Printf("[WebRTC] Created participant %d with video and audio tracks", id)

	return participant, nil
}

func (p *VirtualParticipant) Start() {
	p.active.Store(true)
	go p.streamVideo()
	go p.streamAudio()
}

func (p *VirtualParticipant) Stop() {
	p.active.Store(false)
}

func (p *VirtualParticipant) Close() error {
	p.active.Store(false)
	if p.peerConn != nil {
		return p.peerConn.Close()
	}
	return nil
}

// Generates and sends real video RTP packets
func (p *VirtualParticipant) streamVideo() {
	fps := constants.StreamingFPS // Use consistent FPS with media source

	ticker := time.NewTicker(time.Duration(1000/fps) * time.Millisecond)
	defer ticker.Stop()

	seq := uint16(0)
	timestamp := uint32(0)

	hasRealMedia := p.mediaSource != nil && p.mediaSource.GetTotalVideoFrames() > 0
	if hasRealMedia {
		log.Printf("[WebRTC] Participant %d: Streaming real video from media source (%d frames)",
			p.id, p.mediaSource.GetTotalVideoFrames())
	} else {
		log.Printf("[WebRTC] Participant %d: Using synthetic video frames (no media source)", p.id)
	}

	for range ticker.C {
		if !p.active.Load() {
			return
		}

		// Check for frame drop
		if p.shouldDropFrame() {
			p.videoFrameIdx++
			continue
		}

		var frame []byte
		var err error

		if hasRealMedia {
			// Get NAL unit from media source
			nal := p.mediaSource.GetVideoNAL(p.videoFrameIdx)
			if nal == nil || len(nal.Data) == 0 {
				// Loop back to beginning when media ends
				p.videoFrameIdx = 0
				nal = p.mediaSource.GetVideoNAL(p.videoFrameIdx)
				if nal == nil || len(nal.Data) == 0 {
					// Fall back to synthetic if still no data
					frame, err = p.frameGen.NextFrame()
					if err != nil {
						log.Printf("[WebRTC] Frame generation error: %v", err)
						continue
					}
				} else {
					frame = nal.Data
				}
			} else {
				frame = nal.Data
			}
			p.videoFrameIdx++
		} else {
			// Use synthetic frames
			frame, err = p.frameGen.NextFrame()
			if err != nil {
				log.Printf("[WebRTC] Frame generation error: %v", err)
				continue
			}
		}

		// Packetize H.264 NALU → RTP and send
		rtpPackets := packetizeH264(frame, seq, timestamp, p.id)

		for _, pkt := range rtpPackets {
			// Check for packet loss
			if p.shouldDropPacket() {
				continue
			}

			// Add custom participant_id extension
			extData := make([]byte, 4)
			binary.LittleEndian.PutUint32(extData, p.id)

			// Write real RTP packet through WebRTC
			if err := p.videoTrack.WriteRTP(pkt); err != nil {
				log.Printf("[WebRTC] Write RTP error: %v", err)
			} else {
				p.packetsSent.Add(1)
				p.bytesSent.Add(int64(len(pkt.Payload)))
			}
		}

		p.framesSent.Add(1)
		seq += uint16(len(rtpPackets))
		timestamp += uint32(constants.RTPClockRate / fps)
	}
}

// Generates and sends real audio RTP packets
func (p *VirtualParticipant) streamAudio() {
	// Opus typically uses 20ms frames at 48kHz
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()

	seq := uint16(0)
	timestamp := uint32(0)
	const samplesPerFrame = 960 // 48000 Hz * 20ms = 960 samples

	hasRealMedia := p.mediaSource != nil && p.mediaSource.GetTotalAudioFrames() > 0
	if hasRealMedia {
		log.Printf("[WebRTC] Participant %d: Streaming real audio from media source (%d frames)",
			p.id, p.mediaSource.GetTotalAudioFrames())
	} else {
		log.Printf("[WebRTC] Participant %d: Using synthetic audio frames (no media source)", p.id)
	}

	for range ticker.C {
		if !p.active.Load() {
			return
		}

		// Check for audio silence spike
		if p.shouldMuteAudio() {
			// Send comfort noise instead of silence
			silenceFrame := make([]byte, 3) // Minimal Opus frame
			silenceFrame[0] = 0xFC          // Opus silence frame
			pkt := &rtp.Packet{
				Header: rtp.Header{
					Version:        2,
					PayloadType:    111, // Opus
					SequenceNumber: seq,
					Timestamp:      timestamp,
					SSRC:           p.id*1000 + 1,
					Marker:         false,
				},
				Payload: silenceFrame,
			}
			p.audioTrack.WriteRTP(pkt)
			seq++
			timestamp += samplesPerFrame
			continue
		}

		var audioFrame []byte

		if hasRealMedia {
			// Get audio packet from media source
			packet := p.mediaSource.GetAudioPacket(p.audioFrameIdx)
			if packet == nil || len(packet.Data) == 0 {
				// Loop back to beginning when media ends
				p.audioFrameIdx = 0
				packet = p.mediaSource.GetAudioPacket(p.audioFrameIdx)
				if packet == nil || len(packet.Data) == 0 {
					// Fall back to synthetic if still no data
					audioFrame = generateOpusFrame(p.id, seq)
				} else {
					audioFrame = packet.Data
				}
			} else {
				audioFrame = packet.Data
			}
			p.audioFrameIdx++
		} else {
			// Generate synthetic Opus-like audio frame
			audioFrame = generateOpusFrame(p.id, seq)
		}

		pkt := &rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				PayloadType:    111, // Opus
				SequenceNumber: seq,
				Timestamp:      timestamp,
				SSRC:           p.id*1000 + 1,
				Marker:         false,
			},
			Payload: audioFrame,
		}

		if err := p.audioTrack.WriteRTP(pkt); err != nil {
			log.Printf("[WebRTC] Write audio RTP error: %v", err)
		}

		seq++
		timestamp += samplesPerFrame
	}
}

// Generates a synthetic Opus-compatible audio frame
func generateOpusFrame(participantID uint32, seq uint16) []byte {
	// Generate a properly structured Opus frame
	// Opus TOC byte + encoded audio
	frameSize := 80 // ~20ms of audio at typical Opus compression
	frame := make([]byte, frameSize)

	// Opus TOC (Table of Contents) byte
	// Format: config(5) | s(1) | c(2)
	// config=10 (CELT-only, 20ms), s=0 (mono), c=0 (1 frame)
	frame[0] = 0x50 // CELT 20ms mono

	// Fill with audio-like data
	for i := 1; i < frameSize; i++ {
		frame[i] = byte((i*int(participantID) + int(seq)) % 256)
	}

	return frame
}

// Checks for frame drop spike
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

// Checks for packet loss spike
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

// Checks for audio silence spike
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

// Adds an active spike
func (p *VirtualParticipant) AddSpike(spikeID string, spikeType string, params map[string]string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	spikeParams := make(map[string]string)
	spikeParams["type"] = spikeType
	maps.Copy(spikeParams, params) // equivalent to looping and storing values in a map
	p.activeSpikes[spikeID] = spikeParams

	log.Printf("[WebRTC] Participant %d: Added spike %s type=%s", p.id, spikeID, spikeType)
}

// Removes an active spike
func (p *VirtualParticipant) RemoveSpike(spikeID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.activeSpikes, spikeID)
	log.Printf("[WebRTC] Participant %d: Removed spike %s", p.id, spikeID)
}

func (p *VirtualParticipant) GetID() uint32 {
	return p.id
}

// GetPeerConnection returns the underlying peer connection
func (p *VirtualParticipant) GetPeerConnection() any {
	return p.peerConn
}

// NeedsRecreation checks if the participant needs to be recreated for a new connection
// Returns true if:
// 1. Connection is in failed/closed/disconnected state
// 2. Connection already has a remote description (client reconnecting)
func (p *VirtualParticipant) NeedsRecreation() (bool, string) {
	if p.peerConn == nil {
		return true, "peer connection is nil"
	}

	state := p.peerConn.ConnectionState()
	if state == pionwebrtc.PeerConnectionStateFailed ||
		state == pionwebrtc.PeerConnectionStateClosed ||
		state == pionwebrtc.PeerConnectionStateDisconnected {
		return true, fmt.Sprintf("connection state is %s", state.String())
	}

	// If remote description is already set, the client is reconnecting
	// We need to recreate the participant to allow a fresh connection
	if p.peerConn.CurrentRemoteDescription() != nil {
		return true, "remote description already set (client reconnecting)"
	}

	return false, ""
}

// Creates an SDP offer (non-blocking, returns immediately with trickle ICE)
func (p *VirtualParticipant) CreateOffer() (string, error) {
	// Check if we already have a local description (offer was already created)
	if p.peerConn.LocalDescription() != nil {
		// Return existing offer
		return p.peerConn.LocalDescription().SDP, nil
	}

	offer, err := p.peerConn.CreateOffer(nil)
	if err != nil {
		return "", err
	}

	// Set local description to start ICE gathering (non-blocking)
	// ICE candidates will be gathered in the background and can be added via trickle ICE
	if err := p.peerConn.SetLocalDescription(offer); err != nil {
		return "", err
	}

	// Return SDP immediately without waiting for ICE gathering
	// This allows the HTTP request to complete quickly
	// ICE candidates will be gathered asynchronously and can be sent via /ice endpoint
	sdp := p.peerConn.LocalDescription().SDP

	go func() {
		gatherComplete := pionwebrtc.GatheringCompletePromise(p.peerConn)
		select {
		case <-gatherComplete:
			// Check if we got any relay candidates
			sdp := p.peerConn.LocalDescription().SDP
			hasRelay := strings.Contains(sdp, "typ relay")
			hasHost := strings.Contains(sdp, "typ host")
			hasSrflx := strings.Contains(sdp, "typ srflx")

			if hasRelay {
				log.Printf("[WebRTC] Participant %d: ✅ ICE gathering complete - SDP contains TURN relay candidates", p.id)
			} else {
				log.Printf("[WebRTC] Participant %d: ⚠️  ICE gathering complete - No TURN relay candidates", p.id)
				log.Printf("[WebRTC] Participant %d:   - Host candidates: %v", p.id, hasHost)
				log.Printf("[WebRTC] Participant %d:   - Srflx candidates: %v", p.id, hasSrflx)
				log.Printf("[WebRTC] Participant %d:   - Relay candidates: %v (REQUIRED for Kubernetes)", p.id, hasRelay)
			}
		case <-time.After(10 * time.Second):
			sdp := p.peerConn.LocalDescription().SDP
			hasRelay := strings.Contains(sdp, "typ relay")
			log.Printf("[WebRTC] Participant %d: ⚠️  ICE gathering timeout (10s)", p.id)
			if !hasRelay {
				log.Printf("[WebRTC] Participant %d: ⚠️  No relay candidates - TURN servers may be unreachable", p.id)
			}
		}
	}()

	log.Printf("[WebRTC] Participant %d: SDP offer created (ICE gathering in background)", p.id)
	return sdp, nil
}

// Sets the remote SDP answer
func (p *VirtualParticipant) SetRemoteAnswer(sdp string) error {
	answer := pionwebrtc.SessionDescription{
		Type: pionwebrtc.SDPTypeAnswer,
		SDP:  sdp,
	}
	return p.peerConn.SetRemoteDescription(answer)
}

// Adds an ICE candidate
func (p *VirtualParticipant) AddICECandidate(candidate string) error {
	return p.peerConn.AddICECandidate(pionwebrtc.ICECandidateInit{
		Candidate: candidate,
	})
}

// Return WebRTC statistics
func (p *VirtualParticipant) GetStats() map[string]any {
	stats := p.peerConn.GetStats()
	result := make(map[string]any)

	for _, s := range stats {
		switch v := s.(type) {
		case pionwebrtc.OutboundRTPStreamStats:
			result["outbound_rtp"] = map[string]any{
				"packets_sent": v.PacketsSent,
				"bytes_sent":   v.BytesSent,
			}
		case pionwebrtc.ICECandidatePairStats:
			result["ice_candidate_pair"] = map[string]any{
				"state":               string(v.State),
				"bytes_sent":          v.BytesSent,
				"bytes_received":      v.BytesReceived,
				"current_round_trip":  v.CurrentRoundTripTime,
				"available_bandwidth": v.AvailableOutgoingBitrate,
			}
		}
	}

	return result
}

// Converts H.264 NALU to RTP packets with proper fragmentation
func packetizeH264(nalu []byte, seq uint16, timestamp uint32, ssrc uint32) []*rtp.Packet {
	packets := []*rtp.Packet{}
	mtu := 1200 // Conservative MTU for NAT traversal

	if len(nalu) <= mtu {
		// Single NAL unit packet
		pkt := &rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				Padding:        false,
				Extension:      false,
				Marker:         true,
				PayloadType:    constants.RTPPayloadType,
				SequenceNumber: seq,
				Timestamp:      timestamp,
				SSRC:           ssrc * 1000,
			},
			Payload: nalu,
		}
		packets = append(packets, pkt)
	} else {
		// FU-A fragmentation for large NALUs
		naluType := nalu[0] & 0x1F
		nri := nalu[0] & 0x60
		data := nalu[1:] // Skip NALU header

		for i := 0; i < len(data); i += mtu - 2 {
			end := i + mtu - 2
			end = min(end, len(data))

			// FU-A indicator: F(1) | NRI(2) | Type(5) where Type=28 for FU-A
			fuIndicator := byte(28) | nri

			// FU-A header: S(1) | E(1) | R(1) | Type(5)
			fuHeader := naluType
			if i == 0 {
				fuHeader |= 0x80 // Start bit
			}
			if end >= len(data) {
				fuHeader |= 0x40 // End bit
			}

			payload := make([]byte, 2+end-i)
			payload[0] = fuIndicator
			payload[1] = fuHeader
			copy(payload[2:], data[i:end])

			pkt := &rtp.Packet{
				Header: rtp.Header{
					Version:        2,
					Padding:        false,
					Extension:      false,
					Marker:         end >= len(data),
					PayloadType:    constants.RTPPayloadType,
					SequenceNumber: seq,
					Timestamp:      timestamp,
					SSRC:           ssrc * 1000,
				},
				Payload: payload,
			}

			packets = append(packets, pkt)
			seq++
		}
	}

	return packets
}
