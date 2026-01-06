package metrics

import (
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"time"
)

type RTPMetrics struct {
	ParticipantID uint32

	// Counters (atomic for thread safety)
	framesReceived  atomic.Int64
	framesDropped   atomic.Int64
	bytesReceived   atomic.Int64
	bytesSent       atomic.Int64
	packetsSent     atomic.Int64
	packetsReceived atomic.Int64
	packetsLost     atomic.Int64
	nackCount       atomic.Int64
	pliCount        atomic.Int64
	firCount        atomic.Int64

	// Jitter calculation (RFC 3550)
	mu               sync.RWMutex
	lastTimestamp    uint32
	lastArrivalTime  time.Time
	transit          int32
	jitter           float64
	jitterSamples    []float64
	interarrivalTime []time.Duration

	// Round-trip time tracking
	rttSamples []time.Duration
	avgRTT     time.Duration

	// Bitrate tracking with sliding window
	bitrateWindow    time.Duration
	bitrateSamples   []bitrateSample
	bitrateLastCheck time.Time

	// Sequence number tracking for packet loss
	expectedSeq     uint16
	receivedSeqNums map[uint16]bool
	seqWrap         int // Track sequence number wraps
}

type bitrateSample struct {
	bytes     int64
	timestamp time.Time
}

func NewRTPMetrics(participantID uint32) *RTPMetrics {
	return &RTPMetrics{
		ParticipantID:    participantID,
		bitrateWindow:    time.Second,
		bitrateSamples:   make([]bitrateSample, 0, 100),
		jitterSamples:    make([]float64, 0, 100),
		rttSamples:       make([]time.Duration, 0, 100),
		receivedSeqNums:  make(map[uint16]bool),
		bitrateLastCheck: time.Now(),
	}
}

func (m *RTPMetrics) RecordPacketSent(size int) {
	m.packetsSent.Add(1)
	m.bytesSent.Add(int64(size))

	m.mu.Lock()
	m.bitrateSamples = append(m.bitrateSamples, bitrateSample{
		bytes:     int64(size),
		timestamp: time.Now(),
	})
	// Keep only recent samples
	if len(m.bitrateSamples) > 1000 {
		m.bitrateSamples = m.bitrateSamples[len(m.bitrateSamples)-500:]
	}
	m.mu.Unlock()
}

func (m *RTPMetrics) RecordPacketReceived(rtpTimestamp uint32, seqNum uint16, size int) {
	m.packetsReceived.Add(1)
	m.bytesReceived.Add(int64(size))

	m.mu.Lock()
	defer m.mu.Unlock()

	arrivalTime := time.Now()

	// Track sequence numbers for packet loss calculation
	m.receivedSeqNums[seqNum] = true

	// Handle sequence number wrap-around
	if m.expectedSeq > 0 {
		if seqNum < 1000 && m.expectedSeq > 65000 {
			m.seqWrap++
		}
	}
	m.expectedSeq = seqNum + 1

	// RFC 3550 jitter calculation
	if m.lastTimestamp != 0 {
		// Calculate transit time difference
		// Transit = arrival_time - RTP_timestamp
		// D(i,j) = |transit_j - transit_i|
		// J(i) = J(i-1) + (D(i-1,i) - J(i-1))/16
		arrivalMs := int32(arrivalTime.UnixNano() / 1000000)
		rtpMs := int32(rtpTimestamp / 90) // Convert 90kHz RTP clock to ms

		transit := arrivalMs - rtpMs
		d := transit - m.transit
		if d < 0 {
			d = -d
		}

		// Exponential moving average
		m.jitter = m.jitter + (float64(d)-m.jitter)/16.0

		// Store jitter sample for percentile calculations
		m.jitterSamples = append(m.jitterSamples, m.jitter)
		if len(m.jitterSamples) > 1000 {
			m.jitterSamples = m.jitterSamples[len(m.jitterSamples)-500:]
		}

		// Track inter-arrival time
		m.interarrivalTime = append(m.interarrivalTime, arrivalTime.Sub(m.lastArrivalTime))
		if len(m.interarrivalTime) > 100 {
			m.interarrivalTime = m.interarrivalTime[1:]
		}

		m.transit = transit
	} else {
		m.transit = int32(arrivalTime.UnixNano()/1000000) - int32(rtpTimestamp/90)
	}

	m.lastTimestamp = rtpTimestamp
	m.lastArrivalTime = arrivalTime
}

// Records a round-trip time measurement
func (m *RTPMetrics) RecordRTT(rtt time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.rttSamples = append(m.rttSamples, rtt)
	if len(m.rttSamples) > 100 {
		m.rttSamples = m.rttSamples[1:]
	}

	// Calculate average RTT
	var total time.Duration
	for _, r := range m.rttSamples {
		total += r
	}
	m.avgRTT = total / time.Duration(len(m.rttSamples))
}

func (m *RTPMetrics) RecordPacketLost() {
	m.packetsLost.Add(1)
}

func (m *RTPMetrics) RecordFrameReceived() {
	m.framesReceived.Add(1)
}

func (m *RTPMetrics) RecordFrameDropped() {
	m.framesDropped.Add(1)
}

// Records NACK (Negative Acknowledgement) event
func (m *RTPMetrics) RecordNACK() {
	m.nackCount.Add(1)
}

// Records PLI (Picture Loss Indication) event
func (m *RTPMetrics) RecordPLI() {
	m.pliCount.Add(1)
}

// Records FIR (Full Intra Request) event
func (m *RTPMetrics) RecordFIR() {
	m.firCount.Add(1)
}

// Current jitter in milliseconds (RFC 3550)
func (m *RTPMetrics) GetJitter() float64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.jitter
}

// Returns the 99th percentile jitter
func (m *RTPMetrics) GetJitterP99() float64 {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if len(m.jitterSamples) == 0 {
		return 0
	}

	// Find 99th percentile
	sorted := make([]float64, len(m.jitterSamples))
	copy(sorted, m.jitterSamples)
	// Simple sort for percentile
	for i := range sorted {
		for j := i + 1; j < len(sorted); j++ {
			if sorted[i] > sorted[j] {
				sorted[i], sorted[j] = sorted[j], sorted[i]
			}
		}
	}

	idx := int(float64(len(sorted)) * 0.99)
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

func (m *RTPMetrics) GetPacketLossFromSequence() float64 {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if len(m.receivedSeqNums) == 0 {
		return 0
	}

	// Find min and max sequence numbers
	var minSeq, maxSeq uint16
	first := true
	for seq := range m.receivedSeqNums {
		if first {
			minSeq = seq
			maxSeq = seq
			first = false
		} else {
			if seq < minSeq {
				minSeq = seq
			}
			if seq > maxSeq {
				maxSeq = seq
			}
		}
	}

	// Calculate expected packets (handling wrap-around)
	expected := int(maxSeq) - int(minSeq) + 1 + m.seqWrap*65536
	received := len(m.receivedSeqNums)
	lost := expected - received

	if expected <= 0 {
		return 0
	}

	return float64(lost) / float64(expected) * 100.0
}

func (m *RTPMetrics) GetNackCount() int64 {
	return m.nackCount.Load()
}

func (m *RTPMetrics) GetPliCount() int64 {
	return m.pliCount.Load()
}

func (m *RTPMetrics) GetFirCount() int64 {
	return m.firCount.Load()
}

func (m *RTPMetrics) GetPacketLoss() float64 {
	sent := m.packetsSent.Load()
	lost := m.packetsLost.Load()

	if sent == 0 {
		return 0
	}
	lossPercent := float64(lost) / float64(sent) * 100.0
	// Cap at 100% - packet loss cannot exceed 100%
	if lossPercent > 100.0 {
		lossPercent = 100.0
	}
	return lossPercent
}

func (m *RTPMetrics) GetRTT() time.Duration {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.avgRTT
}

// GetMOS calculates Mean Opinion Score using ITU-T G.107 E-model
// Returns value between 1.0 (bad) and 4.5 (excellent)
func (m *RTPMetrics) GetMOS() float64 {
	packetLoss := m.GetPacketLoss()
	jitter := m.GetJitter()
	rtt := m.GetRTT().Milliseconds()

	// ITU-T G.107 E-model simplified calculation
	// R = R0 - Is - Id - Ie + A
	// R0 = 93.2 (base value)
	// Is = 0 (simultaneous impairment, ignored)
	// Id = delay impairment
	// Ie = equipment impairment (packet loss + jitter)
	// A = advantage factor (0 for wired)
	r0 := 93.2

	// Delay impairment (Id) based on one-way delay
	delay := float64(rtt) / 2 // One-way delay
	id := 0.024*delay + 0.11*(delay-177.3)*math.Max(0, 1.0-math.Exp(-(delay-177.3)/100.0))
	if delay <= 177.3 {
		id = 0.024 * delay
	}

	// Equipment impairment (Ie) based on packet loss and jitter
	// Bursty loss is worse than random loss
	ie := 0.0
	if packetLoss > 0 {
		ie = 30.0 * math.Log(1.0+15.0*packetLoss/100.0)
	}

	// Jitter impact (additional impairment)
	if jitter > 10 {
		ie += (jitter - 10) * 0.5
	}

	// Calculate R-factor
	r := r0 - id - ie

	// Bound R-factor
	if r < 0 {
		r = 0
	}
	if r > 100 {
		r = 100
	}

	// Convert R-factor to MOS (ITU-T G.107)
	var mos float64
	if r < 0 {
		mos = 1.0
	} else if r > 100 {
		mos = 4.5
	} else {
		mos = 1.0 + 0.035*r + r*(r-60)*(100-r)*7e-6
	}

	// Bound MOS
	if mos < 1.0 {
		mos = 1.0
	}
	if mos > 4.5 {
		mos = 4.5
	}

	return mos
}

// Returns current bitrate in kbps (sliding window)
func (m *RTPMetrics) GetBitrateKbps() int {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now()
	cutoff := now.Add(-m.bitrateWindow)

	// Remove old samples
	validSamples := make([]bitrateSample, 0, len(m.bitrateSamples))
	for _, s := range m.bitrateSamples {
		if s.timestamp.After(cutoff) {
			validSamples = append(validSamples, s)
		}
	}
	m.bitrateSamples = validSamples

	// Calculate bitrate
	var totalBytes int64
	for _, s := range validSamples {
		totalBytes += s.bytes
	}

	// Convert to kbps (bytes * 8 / 1000)
	return int(totalBytes * 8 / 1000)
}

func (m *RTPMetrics) GetFramesSent() int64 {
	return m.framesReceived.Load()
}

func (m *RTPMetrics) GetBytesSent() int64 {
	return m.bytesSent.Load()
}

func (m *RTPMetrics) GetPacketsSent() int64 {
	return m.packetsSent.Load()
}

// Reset all metrics
func (m *RTPMetrics) Reset() {
	m.framesReceived.Store(0)
	m.framesDropped.Store(0)
	m.bytesReceived.Store(0)
	m.bytesSent.Store(0)
	m.packetsSent.Store(0)
	m.packetsReceived.Store(0)
	m.packetsLost.Store(0)
	m.nackCount.Store(0)
	m.pliCount.Store(0)
	m.firCount.Store(0)

	m.mu.Lock()
	m.jitter = 0
	m.lastTimestamp = 0
	m.transit = 0
	m.interarrivalTime = nil
	m.jitterSamples = make([]float64, 0, 100)
	m.rttSamples = make([]time.Duration, 0, 100)
	m.bitrateSamples = make([]bitrateSample, 0, 100)
	m.receivedSeqNums = make(map[uint16]bool)
	m.seqWrap = 0
	m.avgRTT = 0
	m.mu.Unlock()
}

// exports metrics in Prometheus format
func (m *RTPMetrics) ToPrometheus(testID string) string {
	return fmt.Sprintf(`# HELP rtp_frames_sent_total Total RTP frames sent
# TYPE rtp_frames_sent_total counter
rtp_frames_sent_total{test_id="%s",participant_id="%d"} %d

# HELP rtp_frames_dropped_total Total RTP frames dropped
# TYPE rtp_frames_dropped_total counter
rtp_frames_dropped_total{test_id="%s",participant_id="%d"} %d

# HELP rtp_packets_sent_total Total RTP packets sent
# TYPE rtp_packets_sent_total counter
rtp_packets_sent_total{test_id="%s",participant_id="%d"} %d

# HELP rtp_packets_lost_total Total RTP packets lost
# TYPE rtp_packets_lost_total counter
rtp_packets_lost_total{test_id="%s",participant_id="%d"} %d

# HELP rtp_bytes_sent_total Total bytes sent
# TYPE rtp_bytes_sent_total counter
rtp_bytes_sent_total{test_id="%s",participant_id="%d"} %d

# HELP rtp_jitter_ms RTP jitter in milliseconds (RFC 3550)
# TYPE rtp_jitter_ms gauge
rtp_jitter_ms{test_id="%s",participant_id="%d"} %.3f

# HELP rtp_jitter_p99_ms 99th percentile jitter in milliseconds
# TYPE rtp_jitter_p99_ms gauge
rtp_jitter_p99_ms{test_id="%s",participant_id="%d"} %.3f

# HELP rtp_packet_loss_percent Packet loss percentage
# TYPE rtp_packet_loss_percent gauge
rtp_packet_loss_percent{test_id="%s",participant_id="%d"} %.3f

# HELP rtp_mos_score Mean Opinion Score (ITU-T G.107, 1.0-4.5)
# TYPE rtp_mos_score gauge
rtp_mos_score{test_id="%s",participant_id="%d"} %.3f

# HELP rtp_rtt_ms Round-trip time in milliseconds
# TYPE rtp_rtt_ms gauge
rtp_rtt_ms{test_id="%s",participant_id="%d"} %d

# HELP rtp_nack_count Total NACK requests
# TYPE rtp_nack_count counter
rtp_nack_count{test_id="%s",participant_id="%d"} %d

# HELP rtp_pli_count Total PLI requests
# TYPE rtp_pli_count counter
rtp_pli_count{test_id="%s",participant_id="%d"} %d

# HELP rtp_fir_count Total FIR requests
# TYPE rtp_fir_count counter
rtp_fir_count{test_id="%s",participant_id="%d"} %d

# HELP rtp_bitrate_kbps Current bitrate in kbps
# TYPE rtp_bitrate_kbps gauge
rtp_bitrate_kbps{test_id="%s",participant_id="%d"} %d
`,
		testID, m.ParticipantID, m.GetFramesSent(),
		testID, m.ParticipantID, m.framesDropped.Load(),
		testID, m.ParticipantID, m.GetPacketsSent(),
		testID, m.ParticipantID, m.packetsLost.Load(),
		testID, m.ParticipantID, m.GetBytesSent(),
		testID, m.ParticipantID, m.GetJitter(),
		testID, m.ParticipantID, m.GetJitterP99(),
		testID, m.ParticipantID, m.GetPacketLoss(),
		testID, m.ParticipantID, m.GetMOS(),
		testID, m.ParticipantID, m.GetRTT().Milliseconds(),
		testID, m.ParticipantID, m.GetNackCount(),
		testID, m.ParticipantID, m.GetPliCount(),
		testID, m.ParticipantID, m.GetFirCount(),
		testID, m.ParticipantID, m.GetBitrateKbps(),
	)
}

// Returns a point-in-time snapshot of all metrics
func (m *RTPMetrics) Snapshot() map[string]any {
	return map[string]any{
		"participant_id":      m.ParticipantID,
		"frames_sent":         m.GetFramesSent(),
		"frames_dropped":      m.framesDropped.Load(),
		"packets_sent":        m.GetPacketsSent(),
		"packets_received":    m.packetsReceived.Load(),
		"packets_lost":        m.packetsLost.Load(),
		"bytes_sent":          m.GetBytesSent(),
		"bytes_received":      m.bytesReceived.Load(),
		"jitter_ms":           m.GetJitter(),
		"jitter_p99_ms":       m.GetJitterP99(),
		"packet_loss_percent": m.GetPacketLoss(),
		"mos_score":           m.GetMOS(),
		"rtt_ms":              m.GetRTT().Milliseconds(),
		"bitrate_kbps":        m.GetBitrateKbps(),
		"nack_count":          m.GetNackCount(),
		"pli_count":           m.GetPliCount(),
		"fir_count":           m.GetFirCount(),
	}
}

// Aggregates metrics from multiple participants
type AggregateMetrics struct {
	TotalFramesSent    int64
	TotalFramesDropped int64
	TotalPacketsSent   int64
	TotalPacketsLost   int64
	TotalBytesSent     int64
	AvgJitterMs        float64
	MaxJitterMs        float64
	P99JitterMs        float64
	AvgPacketLoss      float64
	MaxPacketLoss      float64
	AvgMosScore        float64
	MinMosScore        float64
	AvgRTTMs           float64
	TotalBitrateKbps   int
	TotalNACKs         int64
	TotalPLIs          int64
	TotalFIRs          int64
}

// Aggregate combines metrics from multiple RTPMetrics instances
func Aggregate(metrics []*RTPMetrics) *AggregateMetrics {
	if len(metrics) == 0 {
		return &AggregateMetrics{}
	}

	agg := &AggregateMetrics{
		MinMosScore: 5.0, // Start high to find min
	}
	var totalJitter, totalLoss, totalMos, totalRTT float64

	for _, m := range metrics {
		agg.TotalFramesSent += m.GetFramesSent()
		agg.TotalFramesDropped += m.framesDropped.Load()
		agg.TotalPacketsSent += m.GetPacketsSent()
		agg.TotalPacketsLost += m.packetsLost.Load()
		agg.TotalBytesSent += m.GetBytesSent()
		agg.TotalBitrateKbps += m.GetBitrateKbps()
		agg.TotalNACKs += m.GetNackCount()
		agg.TotalPLIs += m.GetPliCount()
		agg.TotalFIRs += m.GetFirCount()

		jitter := m.GetJitter()
		loss := m.GetPacketLoss()
		mos := m.GetMOS()
		rtt := float64(m.GetRTT().Milliseconds())

		totalJitter += jitter
		totalLoss += loss
		totalMos += mos
		totalRTT += rtt

		if jitter > agg.MaxJitterMs {
			agg.MaxJitterMs = jitter
		}
		if loss > agg.MaxPacketLoss {
			agg.MaxPacketLoss = loss
		}
		if mos < agg.MinMosScore {
			agg.MinMosScore = mos
		}

		p99 := m.GetJitterP99()
		if p99 > agg.P99JitterMs {
			agg.P99JitterMs = p99
		}
	}

	count := float64(len(metrics))
	agg.AvgJitterMs = totalJitter / count
	agg.AvgPacketLoss = totalLoss / count
	agg.AvgMosScore = totalMos / count
	agg.AvgRTTMs = totalRTT / count

	return agg
}
