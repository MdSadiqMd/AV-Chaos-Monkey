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
	packetizer *rtp.H264Packetizer
	sequencer  uint16
	timestamp  uint32

	// Real UDP connection
	udpConn    *net.UDPConn
	targetAddr *net.UDPAddr
	udpEnabled bool

	// Spikes
	activeSpikesMu sync.RWMutex
	activeSpikes   map[string]*pb.SpikeEvent

	// Memory optimization: reusable frame buffer
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

	// Cached aggregate metrics for fast retrieval
	cachedMetricsMu   sync.RWMutex
	cachedMetrics     *pb.MetricsResponse
	cachedMetricsTime time.Time
	metricsCacheTTL   time.Duration
}

func NewParticipantPool(testID string) *ParticipantPool {
	pp := &ParticipantPool{
		participants:    make(map[uint32]*VirtualParticipant),
		testID:          testID,
		targetHost:      "127.0.0.1", // Default to localhost
		metricsCacheTTL: 500 * time.Millisecond,
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
	ufrag := make([]byte, 4)
	password := make([]byte, 12)
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
		ID:             id,
		VideoConfig:    video,
		AudioConfig:    audio,
		IceUfrag:       iceUfrag,
		IcePassword:    icePassword,
		SrtpMasterKey:  generateRandomBytes(32),
		SrtpMasterSalt: generateRandomBytes(14),
		BackendRTPPort: backendPort,
		metrics:        metrics.NewRTPMetrics(id),
		packetizer:     rtp.NewH264Packetizer(id, 96, 90000),
		activeSpikes:   make(map[string]*pb.SpikeEvent),
	}

	// Set up UDP socket for real packet transmission
	if pp.targetPort > 0 {
		if err := participant.SetupUDP(pp.targetHost, backendPort); err != nil {
			log.Printf("[Pool] Warning: Failed to setup UDP for participant %d: %v (falling back to simulation)", id, err)
		}
	}

	pp.participants[id] = participant
	log.Printf("[Pool] Added participant %d on port %d", id, backendPort)

	return participant, nil
}

// SetupUDP initializes the UDP connection for real packet transmission
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
	p.udpEnabled = true

	log.Printf("[Participant %d] UDP socket ready: local=%s target=%s", p.ID, conn.LocalAddr(), targetAddr)
	return nil
}

func (pp *ParticipantPool) GetParticipant(id uint32) *VirtualParticipant {
	pp.mu.RLock()
	defer pp.mu.RUnlock()
	return pp.participants[id]
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
	defer pp.mu.RUnlock()

	for _, p := range pp.participants {
		p.Start()
	}

	log.Printf("[Pool] Started %d participants", len(pp.participants))
}

func (pp *ParticipantPool) Stop() {
	pp.running.Store(false)

	pp.mu.RLock()
	defer pp.mu.RUnlock()

	for _, p := range pp.participants {
		p.Stop()
	}

	log.Printf("[Pool] Stopped all participants")
}

// Background worker to update cached metrics
func (pp *ParticipantPool) runMetricsAggregator() {
	ticker := time.NewTicker(250 * time.Millisecond)
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

		// Quick jitter/loss access - minimize lock time
		p.activeSpikesMu.RLock()
		spikeCount := len(p.activeSpikes)
		hasJitterSpike := false
		jitterValue := 0.0
		for _, spike := range p.activeSpikes {
			if spike.Type == "network_jitter" {
				hasJitterSpike = true
				if jitterStr, ok := spike.Params["jitter_std_dev_ms"]; ok {
					var jitterMs int
					fmt.Sscanf(jitterStr, "%d", &jitterMs)
					jitterValue += float64(jitterMs) / 2.0
				}
			}
		}
		p.activeSpikesMu.RUnlock()

		// Simplified MOS calculation based on spike count
		if spikeCount > 0 {
			totalMos += 3.5 // Degraded
			totalLoss += 5.0
			if hasJitterSpike {
				totalJitter += jitterValue
			} else {
				totalJitter += 10.0
			}
		} else {
			totalMos += 4.45   // Excellent
			totalJitter += 1.0 // Minimal baseline jitter
		}
	}
	pp.mu.RUnlock()

	count := float64(participantCount)
	if count == 0 {
		count = 1
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
		if participantCount > 100 {
			sampleRate = participantCount / 100 // Sample to keep ~100 participants max
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
			AvgPacketLoss:    totalLoss / count,
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

// Injects a spike for specific participants
func (pp *ParticipantPool) InjectSpike(spike *pb.SpikeEvent) error {
	pp.mu.RLock()
	defer pp.mu.RUnlock()

	targetIDs := spike.ParticipantIds
	if len(targetIDs) == 0 {
		// Apply to all participants
		for id := range pp.participants {
			targetIDs = append(targetIDs, id)
		}
	}

	for _, id := range targetIDs {
		if p, ok := pp.participants[id]; ok {
			p.AddSpike(spike)
		}
	}

	log.Printf("[Pool] Injected spike %s type=%s to %d participants",
		spike.SpikeId, spike.Type, len(targetIDs))

	return nil
}

func (pp *ParticipantPool) Size() int {
	pp.mu.RLock()
	defer pp.mu.RUnlock()
	return len(pp.participants)
}

func (p *VirtualParticipant) Start() {
	p.active.Store(true)
	go p.runFrameLoop()
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
		fps = 30
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
		p.timestamp += uint32(90000 / fps)

		for _, pkt := range packets {
			// Apply packet loss spike if active
			if p.shouldDropPacket() {
				p.metrics.RecordPacketLost()
				continue
			}

			// Serialize RTP packet
			data := pkt.Marshal()

			// Send real UDP packet if enabled
			if p.udpEnabled && p.udpConn != nil && p.targetAddr != nil {
				n, err := p.udpConn.WriteToUDP(data, p.targetAddr)
				if err != nil {
					p.metrics.RecordPacketLost()
					continue
				}
				p.packetsSent.Add(1)
				p.bytesSent.Add(int64(n))
				p.metrics.RecordPacketSent(n)
			} else {
				// Fallback: just update metrics without actual transmission
				p.packetsSent.Add(1)
				p.bytesSent.Add(int64(len(data)))
				p.metrics.RecordPacketSent(len(data))
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
		fps = 30
	}
	bitrateKbps := p.VideoConfig.BitrateKbps
	if bitrateKbps <= 0 {
		bitrateKbps = 2500
	}

	// Calculate frame size: bitrate_kbps * 1000 / 8 / fps
	frameSize := (bitrateKbps * 1000 / 8) / int32(fps)
	frameSize = max(1000, min(frameSize, 100000)) // Cap to prevent memory issues

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
	if frameNum%30 == 0 {
		// IDR frame (keyframe) every 30 frames
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

func (p *VirtualParticipant) IsUDPEnabled() bool {
	return p.udpEnabled
}

// Return UDP transmission statistics
func (p *VirtualParticipant) GetUDPStats() (bytesSent int64, packetsSent int64) {
	return p.bytesSent.Load(), p.packetsSent.Load()
}
