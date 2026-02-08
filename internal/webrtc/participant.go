package webrtc

import (
	"fmt"
	"log"
	"maps"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/MdSadiqMd/AV-Chaos-Monkey/pkg/constants"
	"github.com/MdSadiqMd/AV-Chaos-Monkey/internal/media"
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

	mediaSource   *media.MediaSource
	videoFrameIdx int
	audioFrameIdx int

	framesSent  atomic.Int64
	packetsSent atomic.Int64
	bytesSent   atomic.Int64

	activeSpikes map[string]map[string]string
}

func NewParticipant(id uint32, config VideoConfig) (*VirtualParticipant, error) {
	m := &pionwebrtc.MediaEngine{}
	if err := m.RegisterDefaultCodecs(); err != nil {
		return nil, fmt.Errorf("failed to register codecs: %w", err)
	}

	settingEngine := pionwebrtc.SettingEngine{}
	settingEngine.SetSRTPReplayProtectionWindow(512)
	settingEngine.SetICETimeouts(5*time.Second, 15*time.Second, 3*time.Second)

	api := pionwebrtc.NewAPI(pionwebrtc.WithMediaEngine(m), pionwebrtc.WithSettingEngine(settingEngine))

	iceServers := buildICEServers(id)
	pcConfig := pionwebrtc.Configuration{
		ICEServers:         iceServers,
		ICETransportPolicy: pionwebrtc.ICETransportPolicyAll,
		BundlePolicy:       pionwebrtc.BundlePolicyMaxBundle,
		RTCPMuxPolicy:      pionwebrtc.RTCPMuxPolicyRequire,
	}

	pc, err := api.NewPeerConnection(pcConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create peer connection: %w", err)
	}

	setupConnectionCallbacks(pc, id)

	videoTrack, err := pionwebrtc.NewTrackLocalStaticRTP(
		pionwebrtc.RTPCodecCapability{MimeType: pionwebrtc.MimeTypeH264, ClockRate: constants.RTPClockRate, SDPFmtpLine: "profile-level-id=42e01f;packetization-mode=1"},
		fmt.Sprintf("video-%d", id), fmt.Sprintf("stream-%d", id),
	)
	if err != nil {
		pc.Close()
		return nil, fmt.Errorf("failed to create video track: %w", err)
	}

	audioTrack, err := pionwebrtc.NewTrackLocalStaticRTP(
		pionwebrtc.RTPCodecCapability{MimeType: pionwebrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2},
		fmt.Sprintf("audio-%d", id), fmt.Sprintf("stream-%d", id),
	)
	if err != nil {
		pc.Close()
		return nil, fmt.Errorf("failed to create audio track: %w", err)
	}

	if _, err := pc.AddTrack(videoTrack); err != nil {
		pc.Close()
		return nil, fmt.Errorf("failed to add video track: %w", err)
	}
	if _, err := pc.AddTrack(audioTrack); err != nil {
		pc.Close()
		return nil, fmt.Errorf("failed to add audio track: %w", err)
	}

	participant := &VirtualParticipant{
		id: id, peerConn: pc, videoTrack: videoTrack, audioTrack: audioTrack,
		frameGen: NewH264FrameGenerator(config), mediaSource: media.GetGlobalMediaSource(),
		activeSpikes: make(map[string]map[string]string),
	}

	log.Printf("[WebRTC] Created participant %d with video and audio tracks", id)
	return participant, nil
}

func (p *VirtualParticipant) Start() { p.active.Store(true); go p.streamVideo(); go p.streamAudio() }
func (p *VirtualParticipant) Stop()  { p.active.Store(false) }
func (p *VirtualParticipant) Close() error {
	p.active.Store(false)
	if p.peerConn != nil {
		return p.peerConn.Close()
	}
	return nil
}
func (p *VirtualParticipant) GetID() uint32          { return p.id }
func (p *VirtualParticipant) GetPeerConnection() any { return p.peerConn }

func (p *VirtualParticipant) AddSpike(spikeID string, spikeType string, params map[string]string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	spikeParams := make(map[string]string)
	spikeParams["type"] = spikeType
	maps.Copy(spikeParams, params)
	p.activeSpikes[spikeID] = spikeParams
	log.Printf("[WebRTC] Participant %d: Added spike %s type=%s", p.id, spikeID, spikeType)
}

func (p *VirtualParticipant) RemoveSpike(spikeID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.activeSpikes, spikeID)
	log.Printf("[WebRTC] Participant %d: Removed spike %s", p.id, spikeID)
}

func (p *VirtualParticipant) NeedsRecreation() (bool, string) {
	if p.peerConn == nil {
		return true, "peer connection is nil"
	}
	state := p.peerConn.ConnectionState()
	if state == pionwebrtc.PeerConnectionStateFailed || state == pionwebrtc.PeerConnectionStateClosed || state == pionwebrtc.PeerConnectionStateDisconnected {
		return true, fmt.Sprintf("connection state is %s", state.String())
	}
	if p.peerConn.CurrentRemoteDescription() != nil {
		return true, "remote description already set (client reconnecting)"
	}
	return false, ""
}

func (p *VirtualParticipant) CreateOffer() (string, error) {
	if p.peerConn.LocalDescription() != nil {
		return p.peerConn.LocalDescription().SDP, nil
	}
	offer, err := p.peerConn.CreateOffer(nil)
	if err != nil {
		return "", err
	}
	if err := p.peerConn.SetLocalDescription(offer); err != nil {
		return "", err
	}
	sdp := p.peerConn.LocalDescription().SDP

	go func() {
		gatherComplete := pionwebrtc.GatheringCompletePromise(p.peerConn)
		select {
		case <-gatherComplete:
			sdp := p.peerConn.LocalDescription().SDP
			hasRelay := strings.Contains(sdp, "typ relay")
			if hasRelay {
				log.Printf("[WebRTC] Participant %d: ✅ ICE gathering complete - SDP contains TURN relay candidates", p.id)
			} else {
				log.Printf("[WebRTC] Participant %d: ⚠️  ICE gathering complete - No TURN relay candidates", p.id)
			}
		case <-time.After(10 * time.Second):
			log.Printf("[WebRTC] Participant %d: ⚠️  ICE gathering timeout (10s)", p.id)
		}
	}()

	log.Printf("[WebRTC] Participant %d: SDP offer created (ICE gathering in background)", p.id)
	return sdp, nil
}

func (p *VirtualParticipant) SetRemoteAnswer(sdp string) error {
	return p.peerConn.SetRemoteDescription(pionwebrtc.SessionDescription{Type: pionwebrtc.SDPTypeAnswer, SDP: sdp})
}

func (p *VirtualParticipant) AddICECandidate(candidate string) error {
	return p.peerConn.AddICECandidate(pionwebrtc.ICECandidateInit{Candidate: candidate})
}

func (p *VirtualParticipant) GetStats() map[string]any {
	stats := p.peerConn.GetStats()
	result := make(map[string]any)
	for _, s := range stats {
		switch v := s.(type) {
		case pionwebrtc.OutboundRTPStreamStats:
			result["outbound_rtp"] = map[string]any{"packets_sent": v.PacketsSent, "bytes_sent": v.BytesSent}
		case pionwebrtc.ICECandidatePairStats:
			result["ice_candidate_pair"] = map[string]any{"state": string(v.State), "bytes_sent": v.BytesSent, "bytes_received": v.BytesReceived, "current_round_trip": v.CurrentRoundTripTime, "available_bandwidth": v.AvailableOutgoingBitrate}
		case pionwebrtc.RemoteInboundRTPStreamStats:
			result["remote_inbound_rtp"] = map[string]any{"packets_lost": v.PacketsLost, "jitter": v.Jitter, "round_trip_time": v.RoundTripTime, "fraction_lost": v.FractionLost}
		}
	}
	return result
}

func (p *VirtualParticipant) GetRealMetrics() (packetLoss float64, jitterMs float64, rttMs float64) {
	if p.peerConn == nil {
		return 0, 0, 0
	}
	stats := p.peerConn.GetStats()
	for _, s := range stats {
		switch v := s.(type) {
		case pionwebrtc.RemoteInboundRTPStreamStats:
			return v.FractionLost * 100, v.Jitter * 1000, v.RoundTripTime * 1000
		case pionwebrtc.ICECandidatePairStats:
			if rttMs == 0 && v.CurrentRoundTripTime > 0 {
				rttMs = v.CurrentRoundTripTime * 1000
			}
		}
	}
	return
}
