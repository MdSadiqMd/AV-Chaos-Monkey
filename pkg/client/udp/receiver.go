package udp

import (
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/MdSadiqMd/AV-Chaos-Monkey/pkg/client/rtp"
	"github.com/MdSadiqMd/AV-Chaos-Monkey/pkg/logging"
)

type Config struct {
	Port       string
	BufferSize int
}

func DefaultConfig() *Config {
	return &Config{
		Port:       "5000",
		BufferSize: 4096,
	}
}

type Stats struct {
	VideoPackets       int
	AudioPackets       int
	OtherPackets       int
	TotalBytes         int64
	VideoBytes         int64
	AudioBytes         int64
	NALTypes           map[byte]int
	UniqueSSRCs        map[uint32]bool
	ParticipantPackets map[uint32]int
	StartTime          time.Time
	TotalPackets       int
	mu                 sync.Mutex
}

func NewStats() *Stats {
	return &Stats{
		NALTypes:           make(map[byte]int),
		UniqueSSRCs:        make(map[uint32]bool),
		ParticipantPackets: make(map[uint32]int),
		StartTime:          time.Now(),
	}
}

func (s *Stats) RecordPacket(header *rtp.Header, packetSize int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.TotalPackets++
	s.TotalBytes += int64(packetSize)
	s.UniqueSSRCs[header.SSRC] = true

	if header.ParticipantID > 0 {
		s.ParticipantPackets[header.ParticipantID]++
	}

	switch {
	case header.IsVideoPacket():
		s.VideoPackets++
		s.VideoBytes += int64(len(header.Payload))
		if len(header.Payload) > 0 {
			nalType := header.GetNALType()
			s.NALTypes[nalType]++
		}
	case header.IsAudioPacket():
		s.AudioPackets++
		s.AudioBytes += int64(len(header.Payload))
	default:
		s.OtherPackets++
	}
}

func (s *Stats) GetTotalPackets() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.TotalPackets
}

type Receiver struct {
	config *Config
	conn   *net.UDPConn
	stats  *Stats
}

func NewReceiver(config *Config) *Receiver {
	if config == nil {
		config = DefaultConfig()
	}
	return &Receiver{
		config: config,
		stats:  NewStats(),
	}
}

func (r *Receiver) Start() error {
	addr, err := net.ResolveUDPAddr("udp", "0.0.0.0:"+r.config.Port)
	if err != nil {
		return fmt.Errorf("failed to resolve address: %w", err)
	}

	r.conn, err = net.ListenUDP("udp", addr)
	if err != nil {
		return fmt.Errorf("failed to listen on port %s: %w", r.config.Port, err)
	}

	logging.LogInfo("Listening for RTP packets on UDP port 0.0.0.0:%s", r.config.Port)
	return nil
}

func (r *Receiver) Close() {
	if r.conn != nil {
		r.conn.Close()
	}
}

func (r *Receiver) GetStats() *Stats {
	return r.stats
}

type PacketHandler func(header *rtp.Header, clientAddr *net.UDPAddr, packetNum int)

func (r *Receiver) ReceivePackets(handler PacketHandler) {
	buffer := make([]byte, r.config.BufferSize)

	for {
		n, clientAddr, err := r.conn.ReadFromUDP(buffer)
		if err != nil {
			if r.conn == nil {
				return
			}
			logging.LogError("Error reading UDP: %v", err)
			continue
		}

		header, err := rtp.ParsePacket(buffer[:n])
		if err != nil {
			logging.LogError("Error parsing RTP packet: %v", err)
			continue
		}

		r.stats.RecordPacket(header, n)

		if handler != nil {
			handler(header, clientAddr, r.stats.GetTotalPackets())
		}
	}
}

func PrintStats(stats *Stats) {
	stats.mu.Lock()
	defer stats.mu.Unlock()

	elapsed := time.Since(stats.StartTime)
	totalPackets := stats.TotalPackets

	logging.LogSimple("")
	logging.LogSimple("═══════════════════════════════════════════════════════════")
	logging.LogSimple("                    PACKET STATISTICS                      ")
	logging.LogSimple("═══════════════════════════════════════════════════════════")

	if totalPackets == 0 {
		logging.LogWarning("No packets received")
		logging.LogSimple("═══════════════════════════════════════════════════════")
		return
	}

	logging.LogMetrics("Duration: %v", elapsed.Round(time.Second))
	logging.LogMetrics("Total Packets: %d (%.1f pkt/s)", totalPackets, float64(totalPackets)/elapsed.Seconds())
	logging.LogMetrics("Total Bytes: %.2f MB (%.2f Mbps)", float64(stats.TotalBytes)/1024/1024, float64(stats.TotalBytes)*8/elapsed.Seconds()/1000000)
	logging.LogSimple("")
	logging.LogInfo("Media Type Breakdown:")
	logging.LogMetrics("  Video (H.264): %d packets (%.1f%%)", stats.VideoPackets, float64(stats.VideoPackets)*100/float64(totalPackets))
	logging.LogMetrics("  Audio (Opus):  %d packets (%.1f%%)", stats.AudioPackets, float64(stats.AudioPackets)*100/float64(totalPackets))
	logging.LogSimple("")
	logging.LogMetrics("Unique Streams (SSRCs): %d", len(stats.UniqueSSRCs))
	logging.LogMetrics("Unique Participants: %d", len(stats.ParticipantPackets))
	logging.LogSimple("═══════════════════════════════════════════════════════════")
}

func LogPacketDetails(header *rtp.Header, clientAddr *net.UDPAddr, packetNum int) {
	logging.LogInfo("Packet #%d from %s:", packetNum, clientAddr)
	logging.LogInfo("  Participant ID: %d", header.ParticipantID)
	logging.LogInfo("  Payload Type: %d", header.PayloadType)
	logging.LogInfo("  Sequence: %d", header.Sequence)
	logging.LogInfo("  Timestamp: %d", header.Timestamp)
	logging.LogInfo("  SSRC: %d", header.SSRC)
	logging.LogInfo("  Payload Size: %d bytes", len(header.Payload))
}
