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
}

func NewParticipantPool(testID string) *ParticipantPool {
	pp := &ParticipantPool{
		participants: make(map[uint32]*VirtualParticipant),
		testID:       testID,
		targetHost:   "127.0.0.1", // Default to localhost
	}
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
				continue
			}

			// Serialize RTP packet
			data := pkt.Marshal()

			// Send real UDP packet if enabled
			if p.udpEnabled && p.udpConn != nil && p.targetAddr != nil {
				n, err := p.udpConn.WriteToUDP(data, p.targetAddr)
				if err != nil {
					continue
				}
				p.packetsSent.Add(1)
				p.bytesSent.Add(int64(n))
			} else {
				p.packetsSent.Add(1)
				p.bytesSent.Add(int64(len(data)))
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

// Return the participant setup info
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
