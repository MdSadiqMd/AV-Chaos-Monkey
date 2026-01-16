package pool

import (
	cryptorand "crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"math/rand"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/MdSadiqMd/AV-Chaos-Monkey/pkg/constants"
	"github.com/MdSadiqMd/AV-Chaos-Monkey/pkg/media"
	"github.com/MdSadiqMd/AV-Chaos-Monkey/pkg/metrics"
	pb "github.com/MdSadiqMd/AV-Chaos-Monkey/pkg/protobuf"
	"github.com/MdSadiqMd/AV-Chaos-Monkey/pkg/rtp"
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

	// State
	active      atomic.Bool
	frameCount  atomic.Int64
	bytesSent   atomic.Int64
	packetsSent atomic.Int64

	// Metrics
	metrics *metrics.RTPMetrics

	// RTP
	packetizer      *rtp.H264Packetizer
	audioPacketizer *rtp.OpusPacketizer
	sequencer       uint16
	audioSequencer  uint16
	timestamp       uint32
	audioTimestamp  uint32

	// UDP connection
	udpConn    *net.UDPConn
	targetAddr *net.UDPAddr

	// Media source
	mediaSource   *media.MediaSource
	videoFrameIdx int
	audioFrameIdx int

	// Spikes
	activeSpikesMu sync.RWMutex
	activeSpikes   map[string]*pb.SpikeEvent

	// Memory optimization: reusable frame buffer (for synthetic fallback)
	frameBufferMu sync.Mutex
	frameBuffer   []byte
}

type ParticipantPool struct {
	mu           sync.RWMutex
	participants map[uint32]*VirtualParticipant
	testID       string
	startTime    time.Time
	running      atomic.Bool

	// Target configuration for UDP
	targetHost string
	targetPort int

	// WebRTC participants (separate from UDP participants)
	webrtcParticipantsMu sync.RWMutex
	webrtcParticipants   map[uint32]WebRTCParticipant

	// Cached aggregate metrics for fast retrieval
	cachedMetricsMu   sync.RWMutex
	cachedMetrics     *pb.MetricsResponse
	cachedMetricsTime time.Time
	metricsCacheTTL   time.Duration
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
}

func NewParticipantPool(testID string) *ParticipantPool {
	pp := &ParticipantPool{
		participants:       make(map[uint32]*VirtualParticipant),
		webrtcParticipants: make(map[uint32]WebRTCParticipant),
		testID:             testID,
		targetHost:         constants.DefaultTargetHost,
		metricsCacheTTL:    constants.MetricsCacheTTL * time.Millisecond,
	}
	// background metrics aggregator
	go pp.runMetricsAggregator()
	return pp
}

// Setting target host/port for UDP transmission
func (pp *ParticipantPool) SetTarget(host string, port int) {
	pp.mu.Lock()
	defer pp.mu.Unlock()
	pp.targetHost = host
	pp.targetPort = port
	log.Printf("[Pool] Set target to %s:%d", host, port)
}

// Generate random bytes of specified length
func generateRandomBytes(length int) []byte {
	bytes := make([]byte, length)
	cryptorand.Read(bytes)
	return bytes
}

// Generate ICE ufrag and password
func generateIceCredentials() (string, string) {
	ufrag := make([]byte, constants.IceUfragLength)
	password := make([]byte, constants.IcePasswordLength)
	cryptorand.Read(ufrag)
	cryptorand.Read(password)
	return hex.EncodeToString(ufrag), hex.EncodeToString(password)
}

func (pp *ParticipantPool) AddParticipant(id uint32, video *pb.VideoConfig, audio *pb.AudioConfig, backendPort int) (*VirtualParticipant, error) {
	pp.mu.Lock()
	defer pp.mu.Unlock()

	if _, exists := pp.participants[id]; exists {
		return nil, fmt.Errorf("participant %d already exists", id)
	}

	iceUfrag, icePassword := generateIceCredentials()

	participant := &VirtualParticipant{
		ID:              id,
		VideoConfig:     video,
		AudioConfig:     audio,
		IceUfrag:        iceUfrag,
		IcePassword:     icePassword,
		SrtpMasterKey:   generateRandomBytes(constants.SrtpMasterKeyLength),
		SrtpMasterSalt:  generateRandomBytes(constants.SrtpMasterSaltLength),
		BackendRTPPort:  backendPort,
		metrics:         metrics.NewRTPMetrics(id),
		packetizer:      rtp.NewH264Packetizer(id, constants.RTPPayloadType, constants.RTPClockRate),
		audioPacketizer: rtp.NewOpusPacketizer(id, constants.RTPPayloadTypeOpus, constants.RTPClockRateOpus),
		mediaSource:     media.GetGlobalMediaSource(),
		activeSpikes:    make(map[string]*pb.SpikeEvent),
	}

	targetHost := constants.DefaultTargetHost
	targetPort := constants.DefaultTargetPort
	if pp.targetPort > 0 {
		// Use configured target for forwarding to external application
		targetHost = pp.targetHost
		targetPort = pp.targetPort
	}

	if err := participant.SetupUDP(targetHost, targetPort); err != nil {
		return nil, fmt.Errorf("failed to setup UDP for participant %d: %w", id, err)
	}
	log.Printf("[Pool] Participant %d: UDP ready, sending RTP packets to %s:%d", id, targetHost, targetPort)

	pp.participants[id] = participant
	log.Printf("[Pool] Added participant %d on port %d", id, backendPort)

	return participant, nil
}

// SetupUDP initializes the UDP connection for packet transmission
func (p *VirtualParticipant) SetupUDP(host string, port int) error {
	// Create local UDP socket
	localAddr, err := net.ResolveUDPAddr("udp", ":0") // Any available port
	if err != nil {
		return fmt.Errorf("failed to resolve local address: %w", err)
	}

	conn, err := net.ListenUDP("udp", localAddr)
	if err != nil {
		return fmt.Errorf("failed to create UDP socket: %w", err)
	}

	// Set up target address
	targetAddr, err := net.ResolveUDPAddr("udp", fmt.Sprintf("%s:%d", host, port))
	if err != nil {
		conn.Close()
		return fmt.Errorf("failed to resolve target address: %w", err)
	}

	p.udpConn = conn
	p.targetAddr = targetAddr

	log.Printf("[Participant %d] UDP socket ready: local=%s target=%s", p.ID, conn.LocalAddr(), targetAddr)
	return nil
}

func (pp *ParticipantPool) GetParticipant(id uint32) *VirtualParticipant {
	pp.mu.RLock()
	defer pp.mu.RUnlock()
	return pp.participants[id]
}

// Adding WebRTC participant to the pool, If the test is already running, it will start the participant immediately
func (pp *ParticipantPool) AddWebRTCParticipant(id uint32, participant WebRTCParticipant) {
	pp.webrtcParticipantsMu.Lock()
	pp.webrtcParticipants[id] = participant
	isRunning := pp.running.Load()
	pp.webrtcParticipantsMu.Unlock()

	log.Printf("[Pool] Added WebRTC participant %d (test running: %v)", id, isRunning)

	// If test is already running, start the participant immediately
	if isRunning {
		participant.Start()
		log.Printf("[Pool] Started WebRTC participant %d (test was already running)", id)
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
	log.Printf("[Pool] Removed WebRTC participant %d", id)
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

	// Start participants concurrently to avoid blocking
	pp.mu.RLock()
	participantCount := len(pp.participants)
	participants := make([]*VirtualParticipant, 0, participantCount)
	for _, p := range pp.participants {
		participants = append(participants, p)
	}
	pp.mu.RUnlock()

	// Start UDP participants in batches to avoid overwhelming the system
	// Use smaller batch size and longer delays to prevent OOM kills
	batchSize := constants.DefaultBatchSize
	batchDelay := constants.DefaultBatchDelay * time.Millisecond

	// For large participant counts, use even smaller batches
	if participantCount > constants.LargeParticipantCount {
		batchSize = constants.LargeBatchSize
		batchDelay = constants.LargeBatchDelay * time.Millisecond
	}
	if participantCount > constants.VeryLargeParticipantCount {
		batchSize = constants.VeryLargeBatchSize
		batchDelay = constants.VeryLargeBatchDelay * time.Millisecond
	}

	log.Printf("[Pool] Starting %d UDP participants in batches of %d", participantCount, batchSize)

	for i := 0; i < len(participants); i += batchSize {
		end := min(i+batchSize, len(participants))
		batch := participants[i:end]
		for _, p := range batch {
			p.Start()
		}
		// Delay between batches to prevent resource spikes and OOM kills
		if end < len(participants) {
			time.Sleep(batchDelay)
		}
	}

	// Start WebRTC participants concurrently
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

	log.Printf("[Pool] Started %d UDP participants and %d WebRTC participants", participantCount, webrtcCount)
}

func (pp *ParticipantPool) Stop() {
	pp.running.Store(false)

	pp.mu.RLock()
	for _, p := range pp.participants {
		p.Stop()
	}
	udpCount := len(pp.participants)
	pp.mu.RUnlock()

	// Stop WebRTC participants
	pp.webrtcParticipantsMu.Lock()
	for _, wp := range pp.webrtcParticipants {
		wp.Stop()
	}
	webrtcCount := len(pp.webrtcParticipants)
	pp.webrtcParticipantsMu.Unlock()

	log.Printf("[Pool] Stopped %d UDP participants and %d WebRTC participants", udpCount, webrtcCount)
}

// Background worker to update cached metrics
func (pp *ParticipantPool) runMetricsAggregator() {
	ticker := time.NewTicker(constants.MetricsAggregatorTick * time.Millisecond)
	defer ticker.Stop()

	for range ticker.C {
		if !pp.running.Load() {
			continue
		}
		pp.updateCachedMetrics()
	}
}

// Updates the cached aggregate metrics
func (pp *ParticipantPool) updateCachedMetrics() {
	pp.mu.RLock()
	participantCount := len(pp.participants)

	// Quick aggregate without individual participant metrics
	var totalFrames, totalPackets, totalBitrate int64
	var totalJitter, totalLoss, totalMos float64

	// Collect participant list for metrics generation
	participantsList := make([]*VirtualParticipant, 0, participantCount)
	for _, p := range pp.participants {
		participantsList = append(participantsList, p)
		// Use atomic loads directly to avoid lock contention
		totalFrames += p.frameCount.Load()
		totalPackets += p.packetsSent.Load()
		totalBitrate += int64(p.VideoConfig.BitrateKbps)

		pm := p.GetMetrics()
		totalMos += pm.MosScore
		totalLoss += pm.PacketLossPercent
		totalJitter += pm.JitterMs
	}
	pp.mu.RUnlock()

	count := float64(participantCount)
	if count == 0 {
		count = 1
	}

	// Cap packet loss at 100% (is already be capped in GetPacketLoss, but I'm insecure)
	avgLoss := totalLoss / count
	if avgLoss > 100.0 {
		avgLoss = 100.0
	}

	elapsed := int64(0)
	if !pp.startTime.IsZero() {
		elapsed = int64(time.Since(pp.startTime).Seconds())
	}

	// Include individual participant metrics for Prometheus export
	// For 50 participants, include all; for more, sample to keep reasonable
	participantMetrics := make([]*pb.ParticipantMetrics, 0, participantCount)
	if participantCount > 0 && len(participantsList) > 0 {
		sampleRate := 1
		if participantCount > constants.MaxSampledParticipants {
			sampleRate = participantCount / constants.MaxSampledParticipants
		}

		for i, p := range participantsList {
			if p == nil {
				continue
			}
			if i%sampleRate == 0 {
				// Get individual metrics (this is fast - uses atomic loads)
				pm := p.GetMetrics()
				if pm != nil {
					participantMetrics = append(participantMetrics, pm)
				}
			}
		}
	}

	metricsResp := &pb.MetricsResponse{
		TestId:         pp.testID,
		ElapsedSeconds: elapsed,
		Participants:   participantMetrics, // Include individual metrics for Prometheus
		Aggregate: &pb.AggregateMetrics{
			TotalFramesSent:  totalFrames,
			TotalPacketsSent: totalPackets,
			AvgJitterMs:      totalJitter / count,
			AvgPacketLoss:    avgLoss,
			AvgMosScore:      totalMos / count,
			TotalBitrateKbps: totalBitrate,
		},
	}

	pp.cachedMetricsMu.Lock()
	pp.cachedMetrics = metricsResp
	pp.cachedMetricsTime = time.Now()
	pp.cachedMetricsMu.Unlock()
}

// Returns cached metrics for fast access (used by HTTP handlers)
func (pp *ParticipantPool) GetMetrics() *pb.MetricsResponse {
	// Return cached metrics if available and fresh
	pp.cachedMetricsMu.RLock()
	cached := pp.cachedMetrics
	cacheTime := pp.cachedMetricsTime
	pp.cachedMetricsMu.RUnlock()

	if cached != nil && time.Since(cacheTime) < pp.metricsCacheTTL {
		// Update elapsed time in cached response
		elapsed := int64(0)
		if !pp.startTime.IsZero() {
			elapsed = int64(time.Since(pp.startTime).Seconds())
		}
		// Return copy with updated elapsed time
		return &pb.MetricsResponse{
			TestId:         cached.TestId,
			ElapsedSeconds: elapsed,
			Participants:   cached.Participants,
			Aggregate:      cached.Aggregate,
		}
	}

	// If no cache, compute fresh (this should only happen during startup)
	pp.updateCachedMetrics()

	pp.cachedMetricsMu.RLock()
	defer pp.cachedMetricsMu.RUnlock()
	return pp.cachedMetrics
}

// Injects a spike for specific participants(concurrently)
func (pp *ParticipantPool) InjectSpike(spike *pb.SpikeEvent) error {
	// Get target IDs first (quick read lock)
	pp.mu.RLock()
	targetIDs := spike.ParticipantIds
	if len(targetIDs) == 0 {
		// Apply to all participants - collect IDs
		targetIDs = make([]uint32, 0, len(pp.participants))
		for id := range pp.participants {
			targetIDs = append(targetIDs, id)
		}
	}

	// Collect participant references while holding lock
	participants := make([]*VirtualParticipant, 0, len(targetIDs))
	for _, id := range targetIDs {
		if p, ok := pp.participants[id]; ok {
			participants = append(participants, p)
		}
	}
	pp.mu.RUnlock()

	// Apply spike to participants without holding pool lock
	// This allows concurrent WebRTC operations
	for _, p := range participants {
		p.AddSpike(spike)
	}

	log.Printf("[Pool] Injected spike %s type=%s to %d participants",
		spike.SpikeId, spike.Type, len(participants))

	return nil
}

func (pp *ParticipantPool) Size() int {
	pp.mu.RLock()
	defer pp.mu.RUnlock()
	return len(pp.participants)
}

func (p *VirtualParticipant) Start() {
	p.active.Store(true)

	// Start video streaming if enabled
	if p.mediaSource.IsVideoEnabled() {
		go p.runVideoLoop()
	} else {
		// Fall back to synthetic frames if no media source
		go p.runFrameLoop()
	}

	// Start audio streaming if enabled
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

		// Check for frame drop spike
		if p.shouldDropFrame() {
			p.metrics.RecordFrameDropped()
			continue
		}

		// Generate synthetic frame
		frame := p.generateSyntheticFrame()

		// Packetize frame
		packets := p.packetizer.Packetize(frame, p.sequencer, p.timestamp)

		// Update counters
		p.frameCount.Add(1)
		p.sequencer += uint16(len(packets))
		p.timestamp += uint32(constants.RTPClockRate / fps)

		for _, pkt := range packets {
			// Serialize RTP packet
			data := pkt.Marshal()

			// Count packet as sent
			p.packetsSent.Add(1)
			p.bytesSent.Add(int64(len(data)))
			p.metrics.RecordPacketSent(len(data))

			// Apply packet loss spike if active (simulates network loss)
			if p.shouldDropPacket() {
				p.metrics.RecordPacketLost()
				continue // Don't send the packet
			}

			// Send UDP packet
			_, err := p.udpConn.WriteToUDP(data, p.targetAddr)
			if err != nil {
				// Network error - count as lost
				p.metrics.RecordPacketLost()
				log.Printf("[Participant %d] UDP send error: %v", p.ID, err)
				continue
			}
		}
	}
}

// Creating a synthetic frame with H.264 NALU structure
func (p *VirtualParticipant) generateSyntheticFrame() []byte {
	// Generate realistic H.264 NALU structure for testing
	// Frame size based on bitrate and FPS
	fps := p.VideoConfig.Fps
	if fps <= 0 {
		fps = constants.DefaultFPS
	}
	bitrateKbps := p.VideoConfig.BitrateKbps
	if bitrateKbps <= 0 {
		bitrateKbps = constants.DefaultBitrateKbps
	}

	// Calculate frame size: bitrate_kbps * 1000 / 8 / fps
	frameSize := (bitrateKbps * 1000 / 8) / int32(fps)
	frameSize = max(constants.MinFrameSize, min(frameSize, constants.MaxFrameSize))

	// Reuse or allocate frame buffer
	p.frameBufferMu.Lock()
	if cap(p.frameBuffer) < int(frameSize) {
		p.frameBuffer = make([]byte, frameSize)
	} else {
		p.frameBuffer = p.frameBuffer[:frameSize]
	}
	data := p.frameBuffer
	p.frameBufferMu.Unlock()

	// Create H.264 NALU structure
	frameNum := int(p.frameCount.Load())

	// H.264 NAL unit header (1 byte)
	// Format: forbidden_zero_bit (1) | nal_ref_idc (2) | nal_unit_type (5)
	if frameNum%constants.KeyframeInterval == 0 {
		// IDR frame (keyframe)
		data[0] = 0x65 // nal_ref_idc=3, nal_unit_type=5 (IDR slice)
	} else {
		// P-frame (non-IDR)
		data[0] = 0x41 // nal_ref_idc=2, nal_unit_type=1 (non-IDR slice)
	}

	// Fill with pseudo-random data that looks like encoded video
	idOffset := int(p.ID)
	for i := 1; i < int(frameSize); i++ {
		// Create pattern that varies by frame and participant
		data[i] = byte((i*3 + frameNum*7 + idOffset*11) % 256)
	}

	return data
}

// Streaming video from media source
func (p *VirtualParticipant) runVideoLoop() {
	// Use streaming FPS from constants (all participants use same rate)
	fps := constants.StreamingFPS

	ticker := time.NewTicker(time.Duration(1000/fps) * time.Millisecond)
	defer ticker.Stop()

	totalFrames := p.mediaSource.GetTotalVideoFrames()
	log.Printf("[Participant %d] Starting video stream: %d frames at %dfps (looping)",
		p.ID, totalFrames, fps)

	for range ticker.C {
		if !p.active.Load() {
			return
		}

		// Check for frame drop spike
		if p.shouldDropFrame() {
			p.metrics.RecordFrameDropped()
			p.videoFrameIdx++
			continue
		}

		// Get next NAL unit from media source (returns nil when media ends)
		nal := p.mediaSource.GetVideoNAL(p.videoFrameIdx)
		if nal == nil || len(nal.Data) == 0 {
			// Loop back to beginning when media ends
			p.videoFrameIdx = 0
			nal = p.mediaSource.GetVideoNAL(p.videoFrameIdx)
			if nal == nil || len(nal.Data) == 0 {
				// Fall back to synthetic frames if still no data
				go p.runFrameLoop()
				return
			}
		}

		// Packetize NAL unit
		packets := p.packetizer.Packetize(nal.Data, p.sequencer, p.timestamp)

		// Update counters
		p.frameCount.Add(1)
		p.videoFrameIdx++
		p.sequencer += uint16(len(packets))
		p.timestamp += uint32(constants.RTPClockRate / int32(fps))

		for _, pkt := range packets {
			// Serialize RTP packet
			data := pkt.Marshal()

			// Count packet as sent
			p.packetsSent.Add(1)
			p.bytesSent.Add(int64(len(data)))
			p.metrics.RecordPacketSent(len(data))

			// Apply packet loss spike if active
			if p.shouldDropPacket() {
				p.metrics.RecordPacketLost()
				continue
			}

			// Send UDP packet
			_, err := p.udpConn.WriteToUDP(data, p.targetAddr)
			if err != nil {
				p.metrics.RecordPacketLost()
				continue
			}
		}
	}
}

// Streaming real audio from media source
func (p *VirtualParticipant) runAudioLoop() {
	// Opus uses 20ms frames
	ticker := time.NewTicker(constants.OpusFrameDuration * time.Millisecond)
	defer ticker.Stop()

	totalFrames := p.mediaSource.GetTotalAudioFrames()
	log.Printf("[Participant %d] Starting audio stream: %d frames (looping)",
		p.ID, totalFrames)

	for range ticker.C {
		if !p.active.Load() {
			return
		}

		// Get next audio packet from media source (returns nil when media ends)
		packet := p.mediaSource.GetAudioPacket(p.audioFrameIdx)
		if packet == nil || len(packet.Data) == 0 {
			// Loop back to beginning when media ends
			p.audioFrameIdx = 0
			packet = p.mediaSource.GetAudioPacket(p.audioFrameIdx)
			if packet == nil || len(packet.Data) == 0 {
				// No audio available, stop audio streaming
				return
			}
		}

		// Create RTP packet for audio
		rtpPacket := p.audioPacketizer.Packetize(packet.Data, p.audioSequencer, p.audioTimestamp)

		// Update counters
		p.audioFrameIdx++
		p.audioSequencer++
		// Opus at 48kHz with 20ms frames = 960 samples per frame
		p.audioTimestamp += 960

		// Serialize and send
		data := rtpPacket.Marshal()

		p.packetsSent.Add(1)
		p.bytesSent.Add(int64(len(data)))
		p.metrics.RecordPacketSent(len(data))

		// Apply packet loss spike if active
		if p.shouldDropPacket() {
			p.metrics.RecordPacketLost()
			continue
		}

		// Send UDP packet
		_, err := p.udpConn.WriteToUDP(data, p.targetAddr)
		if err != nil {
			p.metrics.RecordPacketLost()
			continue
		}
	}
}

// Checks if current frame should be dropped
func (p *VirtualParticipant) shouldDropFrame() bool {
	p.activeSpikesMu.RLock()
	defer p.activeSpikesMu.RUnlock()

	for _, spike := range p.activeSpikes {
		if spike.Type == "frame_drop" {
			// Random drop based on percentage
			if dropPct, ok := spike.Params["drop_percentage"]; ok {
				var pct int
				fmt.Sscanf(dropPct, "%d", &pct)
				return rand.Intn(100) < pct
			}
		}
	}
	return false
}

// Checks if current packet should be dropped (application-level)
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

// Adds an active spike to the participant
func (p *VirtualParticipant) AddSpike(spike *pb.SpikeEvent) {
	p.activeSpikesMu.Lock()
	p.activeSpikes[spike.SpikeId] = spike
	p.activeSpikesMu.Unlock()

	// Handle bitrate reduction spike
	if spike.Type == "bitrate_reduce" {
		if newBitrate, ok := spike.Params["new_bitrate_kbps"]; ok {
			var kbps int32
			fmt.Sscanf(newBitrate, "%d", &kbps)
			p.VideoConfig.BitrateKbps = kbps
			log.Printf("[Participant %d] Reduced bitrate to %d kbps", p.ID, kbps)
		}
	}

	// Auto-remove after duration
	if spike.DurationSeconds > 0 {
		go func() {
			time.Sleep(time.Duration(spike.DurationSeconds) * time.Second)
			p.RemoveSpike(spike.SpikeId)
		}()
	}
}

// Remove an active spike
func (p *VirtualParticipant) RemoveSpike(spikeID string) {
	p.activeSpikesMu.Lock()
	delete(p.activeSpikes, spikeID)
	p.activeSpikesMu.Unlock()
}

func (p *VirtualParticipant) GetMetrics() *pb.ParticipantMetrics {
	p.activeSpikesMu.RLock()
	activeSpikeCount := int32(len(p.activeSpikes))

	// Calculate simulated jitter based on active jitter spikes
	simulatedJitter := p.metrics.GetJitter() // Base jitter from actual measurements
	for _, spike := range p.activeSpikes {
		if spike.Type == "network_jitter" {
			// Extract jitter parameters from spike
			if jitterStr, ok := spike.Params["jitter_std_dev_ms"]; ok {
				var jitterMs int
				fmt.Sscanf(jitterStr, "%d", &jitterMs)
				// Add simulated jitter variance (use half of std dev as average effect)
				simulatedJitter += float64(jitterMs) / 2.0
			}
		}
	}
	p.activeSpikesMu.RUnlock()

	return &pb.ParticipantMetrics{
		ParticipantId:      p.ID,
		FramesSent:         p.frameCount.Load(),
		BytesSent:          p.bytesSent.Load(),
		PacketsSent:        p.packetsSent.Load(),
		JitterMs:           simulatedJitter,
		PacketLossPercent:  p.metrics.GetPacketLoss(),
		NackCount:          p.metrics.GetNackCount(),
		PliCount:           p.metrics.GetPliCount(),
		MosScore:           p.metrics.GetMOS(),
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

// Return UDP transmission statistics
func (p *VirtualParticipant) GetUDPStats() (bytesSent int64, packetsSent int64) {
	return p.bytesSent.Load(), p.packetsSent.Load()
}
