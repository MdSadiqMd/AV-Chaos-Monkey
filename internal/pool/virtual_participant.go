package pool

import (
	"encoding/hex"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/MdSadiqMd/AV-Chaos-Monkey/pkg/constants"
	"github.com/MdSadiqMd/AV-Chaos-Monkey/pkg/logging"
	"github.com/MdSadiqMd/AV-Chaos-Monkey/internal/media"
	"github.com/MdSadiqMd/AV-Chaos-Monkey/pkg/metrics"
	pb "github.com/MdSadiqMd/AV-Chaos-Monkey/internal/protobuf"
	"github.com/MdSadiqMd/AV-Chaos-Monkey/internal/rtcp"
	"github.com/MdSadiqMd/AV-Chaos-Monkey/internal/rtp"
	"github.com/MdSadiqMd/AV-Chaos-Monkey/pkg/utils"
)

type VirtualParticipant struct {
	ID             uint32
	VideoConfig    *pb.VideoConfig
	AudioConfig    *pb.AudioConfig
	IceUfrag       string
	IcePassword    string
	SrtpMasterKey  []byte
	SrtpMasterSalt []byte
	BackendRTPPort int

	active      atomic.Bool
	frameCount  atomic.Int64
	bytesSent   atomic.Int64
	packetsSent atomic.Int64

	metrics         *metrics.RTPMetrics
	packetizer      *rtp.H264Packetizer
	audioPacketizer *rtp.OpusPacketizer
	sequencer       uint16
	audioSequencer  uint16
	timestamp       uint32
	audioTimestamp  uint32

	udpConn    *net.UDPConn
	targetAddr *net.UDPAddr

	mediaSource   *media.MediaSource
	videoFrameIdx int
	audioFrameIdx int

	activeSpikesMu sync.RWMutex
	activeSpikes   map[string]*pb.SpikeEvent

	frameBufferMu sync.Mutex
	frameBuffer   []byte
}

func NewVirtualParticipant(id uint32, video *pb.VideoConfig, audio *pb.AudioConfig, backendPort int) *VirtualParticipant {
	iceUfrag, icePassword := utils.GenerateIceCredentials(constants.IceUfragLength, constants.IcePasswordLength)
	return &VirtualParticipant{
		ID:              id,
		VideoConfig:     video,
		AudioConfig:     audio,
		IceUfrag:        iceUfrag,
		IcePassword:     icePassword,
		SrtpMasterKey:   utils.GenerateRandomBytes(constants.SrtpMasterKeyLength),
		SrtpMasterSalt:  utils.GenerateRandomBytes(constants.SrtpMasterSaltLength),
		BackendRTPPort:  backendPort,
		metrics:         metrics.NewRTPMetrics(id),
		packetizer:      rtp.NewH264Packetizer(id, constants.RTPPayloadType, constants.RTPClockRate),
		audioPacketizer: rtp.NewOpusPacketizer(id, constants.RTPPayloadTypeOpus, constants.RTPClockRateOpus),
		mediaSource:     media.GetGlobalMediaSource(),
		activeSpikes:    make(map[string]*pb.SpikeEvent),
	}
}

func (p *VirtualParticipant) SetupUDP(host string, port int) error {
	localAddr, err := net.ResolveUDPAddr("udp", ":0")
	if err != nil {
		return fmt.Errorf("failed to resolve local address: %w", err)
	}
	conn, err := net.ListenUDP("udp", localAddr)
	if err != nil {
		return fmt.Errorf("failed to create UDP socket: %w", err)
	}
	targetAddr, err := net.ResolveUDPAddr("udp", fmt.Sprintf("%s:%d", host, port))
	if err != nil {
		conn.Close()
		return fmt.Errorf("failed to resolve target address: %w", err)
	}
	p.udpConn = conn
	p.targetAddr = targetAddr
	logging.LogInfo("Participant %d: UDP socket ready: local=%s target=%s", p.ID, conn.LocalAddr(), targetAddr)
	return nil
}

func (p *VirtualParticipant) Start() {
	p.active.Store(true)
	if p.mediaSource.IsVideoEnabled() {
		go p.runVideoLoop()
	} else {
		go p.runFrameLoop()
	}
	if p.mediaSource.IsAudioEnabled() {
		go p.runAudioLoop()
	}
}

func (p *VirtualParticipant) Stop() {
	p.active.Store(false)
	if p.udpConn != nil {
		p.udpConn.Close()
	}
}

func (p *VirtualParticipant) AddSpike(spike *pb.SpikeEvent) {
	p.activeSpikesMu.Lock()
	p.activeSpikes[spike.SpikeId] = spike
	p.activeSpikesMu.Unlock()

	if spike.Type == "bitrate_reduce" {
		if newBitrate, ok := spike.Params["new_bitrate_kbps"]; ok {
			var kbps int32
			fmt.Sscanf(newBitrate, "%d", &kbps)
			p.VideoConfig.BitrateKbps = kbps
			logging.LogChaos("Participant %d: Reduced bitrate to %d kbps", p.ID, kbps)
		}
	}
	if spike.DurationSeconds > 0 {
		go func() {
			time.Sleep(time.Duration(spike.DurationSeconds) * time.Second)
			p.RemoveSpike(spike.SpikeId)
		}()
	}
}

func (p *VirtualParticipant) RemoveSpike(spikeID string) {
	p.activeSpikesMu.Lock()
	delete(p.activeSpikes, spikeID)
	p.activeSpikesMu.Unlock()
}

func (p *VirtualParticipant) GetMetrics() *pb.ParticipantMetrics {
	return p.GetMetricsWithRTCP(nil)
}

func (p *VirtualParticipant) GetMetricsWithRTCP(feedback *rtcp.FeedbackStore) *pb.ParticipantMetrics {
	p.activeSpikesMu.RLock()
	activeSpikeCount := int32(len(p.activeSpikes))
	p.activeSpikesMu.RUnlock()

	var jitterMs, packetLoss, mosScore float64
	ssrc := p.ID * 1000
	if feedback != nil {
		loss, jitter, rtt := feedback.GetMetrics(ssrc)
		if loss > 0 || jitter > 0 || rtt > 0 {
			jitterMs = jitter
			packetLoss = loss
			mosScore = utils.CalculateMOS(loss, jitter, rtt)
		} else {
			jitterMs = p.metrics.GetJitter()
			packetLoss = p.metrics.GetPacketLoss()
			mosScore = p.metrics.GetMOS()
		}
	} else {
		jitterMs = p.metrics.GetJitter()
		packetLoss = p.metrics.GetPacketLoss()
		mosScore = p.metrics.GetMOS()
	}

	return &pb.ParticipantMetrics{
		ParticipantId:      p.ID,
		FramesSent:         p.frameCount.Load(),
		BytesSent:          p.bytesSent.Load(),
		PacketsSent:        p.packetsSent.Load(),
		JitterMs:           jitterMs,
		PacketLossPercent:  packetLoss,
		NackCount:          p.metrics.GetNackCount(),
		PliCount:           p.metrics.GetPliCount(),
		MosScore:           mosScore,
		CurrentBitrateKbps: p.VideoConfig.BitrateKbps,
		ActiveSpikeCount:   activeSpikeCount,
	}
}

func (p *VirtualParticipant) GetSetup() *pb.ParticipantSetup {
	return &pb.ParticipantSetup{
		ParticipantId:  p.ID,
		IceUfrag:       p.IceUfrag,
		IcePassword:    p.IcePassword,
		SrtpMasterKey:  hex.EncodeToString(p.SrtpMasterKey),
		SrtpMasterSalt: hex.EncodeToString(p.SrtpMasterSalt),
		BackendRtpPort: int32(p.BackendRTPPort),
	}
}

func (p *VirtualParticipant) GetUDPStats() (bytesSent int64, packetsSent int64) {
	return p.bytesSent.Load(), p.packetsSent.Load()
}
