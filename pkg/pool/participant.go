package pool

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/MdSadiqMd/AV-Chaos-Monkey/pkg/constants"
	"github.com/MdSadiqMd/AV-Chaos-Monkey/pkg/logging"
	pb "github.com/MdSadiqMd/AV-Chaos-Monkey/pkg/protobuf"
	"github.com/MdSadiqMd/AV-Chaos-Monkey/pkg/rtcp"
)

type ParticipantPool struct {
	mu           sync.RWMutex
	participants map[uint32]*VirtualParticipant
	testID       string
	startTime    time.Time
	running      atomic.Bool

	targetHost string
	targetPort int

	webrtcParticipantsMu sync.RWMutex
	webrtcParticipants   map[uint32]WebRTCParticipant

	cachedMetricsMu   sync.RWMutex
	cachedMetrics     *pb.MetricsResponse
	cachedMetricsTime time.Time
	metricsCacheTTL   time.Duration

	rtcpServer   *rtcp.Server
	rtcpFeedback *rtcp.FeedbackStore
}

type WebRTCParticipant interface {
	GetID() uint32
	CreateOffer() (string, error)
	SetRemoteAnswer(sdpAnswer string) error
	AddICECandidate(candidate string) error
	Start()
	Stop()
	Close() error
	GetPeerConnection() any
	NeedsRecreation() (bool, string)
	GetRealMetrics() (packetLoss float64, jitterMs float64, rttMs float64)
}

func NewParticipantPool(testID string) *ParticipantPool {
	pp := &ParticipantPool{
		participants:       make(map[uint32]*VirtualParticipant),
		webrtcParticipants: make(map[uint32]WebRTCParticipant),
		testID:             testID,
		targetHost:         constants.DefaultTargetHost,
		metricsCacheTTL:    constants.MetricsCacheTTL * time.Millisecond,
		rtcpFeedback:       rtcp.GetGlobalFeedback(),
	}
	pp.rtcpServer = rtcp.NewServer(constants.RTCPFeedbackPort)
	if err := pp.rtcpServer.Start(); err != nil {
		logging.LogWarning("Failed to start RTCP server: %v (metrics will be simulated)", err)
	}
	go pp.runMetricsAggregator()
	return pp
}

func (pp *ParticipantPool) SetTarget(host string, port int) {
	pp.mu.Lock()
	defer pp.mu.Unlock()
	pp.targetHost = host
	pp.targetPort = port
	logging.LogConfig("Set target to %s:%d", host, port)
}

func (pp *ParticipantPool) AddParticipant(id uint32, video *pb.VideoConfig, audio *pb.AudioConfig, backendPort int) (*VirtualParticipant, error) {
	pp.mu.Lock()
	defer pp.mu.Unlock()

	if _, exists := pp.participants[id]; exists {
		return nil, fmt.Errorf("participant %d already exists", id)
	}

	participant := NewVirtualParticipant(id, video, audio, backendPort)

	targetHost := constants.DefaultTargetHost
	targetPort := constants.DefaultTargetPort
	if pp.targetPort > 0 {
		targetHost = pp.targetHost
		targetPort = pp.targetPort
	}

	if err := participant.SetupUDP(targetHost, targetPort); err != nil {
		return nil, fmt.Errorf("failed to setup UDP for participant %d: %w", id, err)
	}
	logging.LogInfo("Participant %d: UDP ready, sending RTP packets to %s:%d", id, targetHost, targetPort)

	pp.participants[id] = participant
	logging.LogInfo("Added participant %d on port %d", id, backendPort)
	return participant, nil
}

func (pp *ParticipantPool) GetParticipant(id uint32) *VirtualParticipant {
	pp.mu.RLock()
	defer pp.mu.RUnlock()
	return pp.participants[id]
}

func (pp *ParticipantPool) AddWebRTCParticipant(id uint32, participant WebRTCParticipant) {
	pp.webrtcParticipantsMu.Lock()
	pp.webrtcParticipants[id] = participant
	isRunning := pp.running.Load()
	pp.webrtcParticipantsMu.Unlock()

	logging.LogInfo("Added WebRTC participant %d (test running: %v)", id, isRunning)
	if isRunning {
		participant.Start()
		logging.LogInfo("Started WebRTC participant %d (test was already running)", id)
	}
}

func (pp *ParticipantPool) GetWebRTCParticipant(id uint32) WebRTCParticipant {
	pp.webrtcParticipantsMu.RLock()
	defer pp.webrtcParticipantsMu.RUnlock()
	return pp.webrtcParticipants[id]
}

func (pp *ParticipantPool) RemoveWebRTCParticipant(id uint32) {
	pp.webrtcParticipantsMu.Lock()
	defer pp.webrtcParticipantsMu.Unlock()
	delete(pp.webrtcParticipants, id)
	logging.LogInfo("Removed WebRTC participant %d", id)
}

func (pp *ParticipantPool) GetAllParticipants() []*VirtualParticipant {
	pp.mu.RLock()
	defer pp.mu.RUnlock()
	result := make([]*VirtualParticipant, 0, len(pp.participants))
	for _, p := range pp.participants {
		result = append(result, p)
	}
	return result
}

func (pp *ParticipantPool) Start() {
	pp.startTime = time.Now()
	pp.running.Store(true)

	pp.mu.RLock()
	participantCount := len(pp.participants)
	participants := make([]*VirtualParticipant, 0, participantCount)
	for _, p := range pp.participants {
		participants = append(participants, p)
	}
	pp.mu.RUnlock()
	logging.LogInfo("Starting %d UDP participants", participantCount)

	for _, p := range participants {
		p.Start()
	}

	pp.webrtcParticipantsMu.Lock()
	webrtcParticipants := make([]WebRTCParticipant, 0, len(pp.webrtcParticipants))
	for _, wp := range pp.webrtcParticipants {
		webrtcParticipants = append(webrtcParticipants, wp)
	}
	webrtcCount := len(webrtcParticipants)
	pp.webrtcParticipantsMu.Unlock()
	for _, wp := range webrtcParticipants {
		wp.Start()
	}

	logging.LogSuccess("Started %d UDP participants and %d WebRTC participants", participantCount, webrtcCount)
}

func (pp *ParticipantPool) Stop() {
	pp.running.Store(false)

	pp.mu.RLock()
	for _, p := range pp.participants {
		p.Stop()
	}
	udpCount := len(pp.participants)
	pp.mu.RUnlock()

	pp.webrtcParticipantsMu.Lock()
	for _, wp := range pp.webrtcParticipants {
		wp.Stop()
	}
	webrtcCount := len(pp.webrtcParticipants)
	pp.webrtcParticipantsMu.Unlock()

	logging.LogInfo("Stopped %d UDP participants and %d WebRTC participants", udpCount, webrtcCount)
}

func (pp *ParticipantPool) InjectSpike(spike *pb.SpikeEvent) error {
	pp.mu.RLock()
	targetIDs := spike.ParticipantIds
	if len(targetIDs) == 0 {
		targetIDs = make([]uint32, 0, len(pp.participants))
		for id := range pp.participants {
			targetIDs = append(targetIDs, id)
		}
	}
	participants := make([]*VirtualParticipant, 0, len(targetIDs))
	for _, id := range targetIDs {
		if p, ok := pp.participants[id]; ok {
			participants = append(participants, p)
		}
	}
	pp.mu.RUnlock()

	for _, p := range participants {
		p.AddSpike(spike)
	}
	logging.LogChaos("Injected spike %s type=%s to %d participants", spike.SpikeId, spike.Type, len(participants))
	return nil
}

func (pp *ParticipantPool) Size() int {
	pp.mu.RLock()
	defer pp.mu.RUnlock()
	return len(pp.participants)
}
