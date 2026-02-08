// Implements RTCP (RTP Control Protocol) for metrics feedback. This provides packet loss, jitter, and RTT measurements from receivers
package rtcp

import (
	"encoding/binary"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/MdSadiqMd/AV-Chaos-Monkey/pkg/logging"
)

// RTCP packet types (RFC 3550)
const (
	TypeSR   = 200 // Sender Report
	TypeRR   = 201 // Receiver Report
	TypeSDES = 202 // Source Description
	TypeBYE  = 203 // Goodbye
	TypeAPP  = 204 // Application-defined
)

// Metrics from a receiver (RFC 3550 Section 6.4.2)
type ReceiverReport struct {
	SSRC               uint32 // SSRC of the receiver
	SourceSSRC         uint32 // SSRC of the source being reported
	FractionLost       uint8  // Fraction of packets lost (0-255, where 255 = 100%)
	CumulativeLost     int32  // Cumulative packets lost (24-bit signed)
	HighestSeqReceived uint32 // Extended highest sequence number received
	Jitter             uint32 // Interarrival jitter (in timestamp units)
	LastSR             uint32 // Last SR timestamp (middle 32 bits of NTP)
	DelaySinceLastSR   uint32 // Delay since last SR (1/65536 seconds)
	ReceivedAt         time.Time
}

// Metrics from a sender (RFC 3550 Section 6.4.1)
type SenderReport struct {
	SSRC         uint32
	NTPTimestamp uint64 // NTP timestamp
	RTPTimestamp uint32 // RTP timestamp
	PacketCount  uint32 // Total packets sent
	OctetCount   uint32 // Total bytes sent
	SentAt       time.Time
}

// RTCP feedback from receivers for each SSRC
type FeedbackStore struct {
	mu              sync.RWMutex
	receiverReports map[uint32]*ReceiverReport // keyed by source SSRC
	senderReports   map[uint32]*SenderReport   // keyed by sender SSRC
	rttSamples      map[uint32][]time.Duration // RTT samples per SSRC
}

func NewFeedbackStore() *FeedbackStore {
	return &FeedbackStore{
		receiverReports: make(map[uint32]*ReceiverReport),
		senderReports:   make(map[uint32]*SenderReport),
		rttSamples:      make(map[uint32][]time.Duration),
	}
}

var globalFeedback = NewFeedbackStore()

func GetGlobalFeedback() *FeedbackStore {
	return globalFeedback
}

func (fs *FeedbackStore) StoreReceiverReport(rr *ReceiverReport) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.receiverReports[rr.SourceSSRC] = rr
}

// Stores a sender report and calculates RTT if we have matching RR
func (fs *FeedbackStore) StoreSenderReport(sr *SenderReport) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.senderReports[sr.SSRC] = sr
}

// Calculate RTT from receiver report's LSR and DLSR fields
func (fs *FeedbackStore) CalculateRTT(rr *ReceiverReport) time.Duration {
	if rr.LastSR == 0 {
		return 0
	}

	fs.mu.RLock()
	sr, exists := fs.senderReports[rr.SourceSSRC]
	fs.mu.RUnlock()

	if !exists {
		return 0
	}

	// RTT = current_time - LSR - DLSR
	// LSR is middle 32 bits of NTP timestamp from SR
	// DLSR is delay at receiver in 1/65536 seconds
	now := time.Now()
	srTime := sr.SentAt

	// DLSR is in 1/65536 second units
	dlsr := time.Duration(rr.DelaySinceLastSR) * time.Second / 65536
	rtt := max(now.Sub(srTime)-dlsr, 0)

	// Store RTT sample
	fs.mu.Lock()
	if fs.rttSamples[rr.SourceSSRC] == nil {
		fs.rttSamples[rr.SourceSSRC] = make([]time.Duration, 0, 100)
	}
	fs.rttSamples[rr.SourceSSRC] = append(fs.rttSamples[rr.SourceSSRC], rtt)
	if len(fs.rttSamples[rr.SourceSSRC]) > 100 {
		fs.rttSamples[rr.SourceSSRC] = fs.rttSamples[rr.SourceSSRC][1:]
	}
	fs.mu.Unlock()

	return rtt
}

// Returns metrics for a given SSRC
func (fs *FeedbackStore) GetMetrics(ssrc uint32) (packetLoss float64, jitterMs float64, rttMs float64) {
	fs.mu.RLock()
	defer fs.mu.RUnlock()

	rr, exists := fs.receiverReports[ssrc]
	if !exists {
		return 0, 0, 0
	}

	// Fraction lost is 0-255 where 255 = 100%
	packetLoss = float64(rr.FractionLost) / 255.0 * 100.0

	// Jitter is in timestamp units (90kHz for video)
	// Convert to milliseconds: jitter / 90 = ms
	jitterMs = float64(rr.Jitter) / 90.0

	// Calculate average RTT
	if samples := fs.rttSamples[ssrc]; len(samples) > 0 {
		var total time.Duration
		for _, s := range samples {
			total += s
		}
		rttMs = float64(total.Milliseconds()) / float64(len(samples))
	}

	return packetLoss, jitterMs, rttMs
}

// Returns metrics for all known SSRCs
func (fs *FeedbackStore) GetAllMetrics() map[uint32]struct{ Loss, Jitter, RTT float64 } {
	fs.mu.RLock()
	defer fs.mu.RUnlock()

	result := make(map[uint32]struct{ Loss, Jitter, RTT float64 })
	for ssrc := range fs.receiverReports {
		loss, jitter, rtt := fs.GetMetrics(ssrc)
		result[ssrc] = struct{ Loss, Jitter, RTT float64 }{loss, jitter, rtt}
	}
	return result
}

// Parses an RTCP Receiver Report packet
func ParseReceiverReport(data []byte) (*ReceiverReport, error) {
	if len(data) < 32 {
		return nil, nil // Too short
	}

	// RTCP header: V(2) P(1) RC(5) PT(8) Length(16)
	header := data[0]
	pt := data[1]

	if pt != TypeRR {
		return nil, nil // Not a receiver report
	}

	version := (header >> 6) & 0x03
	if version != 2 {
		return nil, nil // Invalid version
	}

	rc := header & 0x1F // Report count
	if rc == 0 {
		return nil, nil // No reports
	}

	// Skip header (4 bytes) and reporter SSRC (4 bytes)
	if len(data) < 8+24 {
		return nil, nil
	}

	reporterSSRC := binary.BigEndian.Uint32(data[4:8])

	// Parse first report block (24 bytes)
	offset := 8
	sourceSSRC := binary.BigEndian.Uint32(data[offset : offset+4])
	fractionLost := data[offset+4]

	// Cumulative lost is 24-bit signed
	cumLost := int32(data[offset+5])<<16 | int32(data[offset+6])<<8 | int32(data[offset+7])
	if cumLost&0x800000 != 0 {
		cumLost |= ^0xFFFFFF // Sign extend
	}

	highestSeq := binary.BigEndian.Uint32(data[offset+8 : offset+12])
	jitter := binary.BigEndian.Uint32(data[offset+12 : offset+16])
	lastSR := binary.BigEndian.Uint32(data[offset+16 : offset+20])
	dlsr := binary.BigEndian.Uint32(data[offset+20 : offset+24])

	return &ReceiverReport{
		SSRC:               reporterSSRC,
		SourceSSRC:         sourceSSRC,
		FractionLost:       fractionLost,
		CumulativeLost:     cumLost,
		HighestSeqReceived: highestSeq,
		Jitter:             jitter,
		LastSR:             lastSR,
		DelaySinceLastSR:   dlsr,
		ReceivedAt:         time.Now(),
	}, nil
}

func BuildSenderReport(ssrc uint32, packetCount, octetCount uint32, rtpTimestamp uint32) []byte {
	// RTCP SR packet: header(4) + SSRC(4) + NTP(8) + RTP_TS(4) + pkt_count(4) + octet_count(4) = 28 bytes
	data := make([]byte, 28)

	// Header: V=2, P=0, RC=0, PT=200 (SR)
	data[0] = 0x80 // V=2, P=0, RC=0
	data[1] = TypeSR
	binary.BigEndian.PutUint16(data[2:4], 6) // Length in 32-bit words minus 1

	// SSRC
	binary.BigEndian.PutUint32(data[4:8], ssrc)

	// NTP timestamp (64-bit)
	now := time.Now()
	ntpSec := uint32(now.Unix() + 2208988800) // Seconds since 1900
	ntpFrac := uint32(float64(now.Nanosecond()) / 1e9 * (1 << 32))
	binary.BigEndian.PutUint32(data[8:12], ntpSec)
	binary.BigEndian.PutUint32(data[12:16], ntpFrac)

	// RTP timestamp
	binary.BigEndian.PutUint32(data[16:20], rtpTimestamp)

	// Packet count
	binary.BigEndian.PutUint32(data[20:24], packetCount)

	// Octet count
	binary.BigEndian.PutUint32(data[24:28], octetCount)

	return data
}

func BuildReceiverReport(reporterSSRC, sourceSSRC uint32, fractionLost uint8, cumLost int32, highestSeq, jitter, lastSR, dlsr uint32) []byte {
	// RTCP RR packet: header(4) + SSRC(4) + report_block(24) = 32 bytes
	data := make([]byte, 32)

	// Header: V=2, P=0, RC=1, PT=201 (RR)
	data[0] = 0x81 // V=2, P=0, RC=1
	data[1] = TypeRR
	binary.BigEndian.PutUint16(data[2:4], 7) // Length in 32-bit words minus 1

	// Reporter SSRC
	binary.BigEndian.PutUint32(data[4:8], reporterSSRC)

	// Report block
	binary.BigEndian.PutUint32(data[8:12], sourceSSRC)
	data[12] = fractionLost
	data[13] = byte((cumLost >> 16) & 0xFF)
	data[14] = byte((cumLost >> 8) & 0xFF)
	data[15] = byte(cumLost & 0xFF)
	binary.BigEndian.PutUint32(data[16:20], highestSeq)
	binary.BigEndian.PutUint32(data[20:24], jitter)
	binary.BigEndian.PutUint32(data[24:28], lastSR)
	binary.BigEndian.PutUint32(data[28:32], dlsr)

	return data
}

// Listen for RTCP feedback from receivers
type Server struct {
	mu       sync.RWMutex
	conn     *net.UDPConn
	port     int
	running  bool
	feedback *FeedbackStore
}

func NewServer(port int) *Server {
	return &Server{
		port:     port,
		feedback: GetGlobalFeedback(),
	}
}

// Listen for RTCP packets
func (s *Server) Start() error {
	addr, err := net.ResolveUDPAddr("udp", ":"+string(rune(s.port)))
	if err != nil {
		// Try with formatted port
		addr, err = net.ResolveUDPAddr("udp", fmt.Sprintf(":%d", s.port))
		if err != nil {
			return err
		}
	}

	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return err
	}

	s.mu.Lock()
	s.conn = conn
	s.running = true
	s.mu.Unlock()

	logging.LogInfo("RTCP feedback server listening on port %d", s.port)

	go s.receiveLoop()
	return nil
}

// Stop the RTCP server
func (s *Server) Stop() {
	s.mu.Lock()
	s.running = false
	if s.conn != nil {
		s.conn.Close()
	}
	s.mu.Unlock()
}

func (s *Server) receiveLoop() {
	buf := make([]byte, 1500)

	for {
		s.mu.RLock()
		running := s.running
		conn := s.conn
		s.mu.RUnlock()

		if !running || conn == nil {
			return
		}

		conn.SetReadDeadline(time.Now().Add(1 * time.Second))
		n, _, err := conn.ReadFromUDP(buf)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			continue
		}

		if n < 8 {
			continue
		}

		// Parse RTCP packet
		rr, err := ParseReceiverReport(buf[:n])
		if err == nil && rr != nil {
			s.feedback.StoreReceiverReport(rr)
			s.feedback.CalculateRTT(rr)
		}
	}
}

func (s *Server) GetFeedback() *FeedbackStore {
	return s.feedback
}
