package metrics

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/MdSadiqMd/AV-Chaos-Monkey/pkg/utils"
)

type RTPMetrics struct {
	ParticipantID uint32

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

	mu               sync.RWMutex
	lastTimestamp    uint32
	lastArrivalTime  time.Time
	transit          int32
	jitter           float64
	jitterSamples    []float64
	interarrivalTime []time.Duration

	rttSamples []time.Duration
	avgRTT     time.Duration

	bitrateWindow    time.Duration
	bitrateSamples   []bitrateSample
	bitrateLastCheck time.Time

	expectedSeq     uint16
	receivedSeqNums map[uint16]bool
	seqWrap         int
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
	m.bitrateSamples = append(m.bitrateSamples, bitrateSample{bytes: int64(size), timestamp: time.Now()})
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
	m.receivedSeqNums[seqNum] = true

	if m.expectedSeq > 0 && seqNum < 1000 && m.expectedSeq > 65000 {
		m.seqWrap++
	}
	m.expectedSeq = seqNum + 1

	if m.lastTimestamp != 0 {
		arrivalMs := int32(arrivalTime.UnixNano() / 1000000)
		rtpMs := int32(rtpTimestamp / 90)
		transit := arrivalMs - rtpMs
		d := transit - m.transit
		if d < 0 {
			d = -d
		}
		m.jitter = m.jitter + (float64(d)-m.jitter)/16.0
		m.jitterSamples = append(m.jitterSamples, m.jitter)
		if len(m.jitterSamples) > 1000 {
			m.jitterSamples = m.jitterSamples[len(m.jitterSamples)-500:]
		}
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

func (m *RTPMetrics) RecordRTT(rtt time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rttSamples = append(m.rttSamples, rtt)
	if len(m.rttSamples) > 100 {
		m.rttSamples = m.rttSamples[1:]
	}
	var total time.Duration
	for _, r := range m.rttSamples {
		total += r
	}
	m.avgRTT = total / time.Duration(len(m.rttSamples))
}

func (m *RTPMetrics) RecordPacketLost()    { m.packetsLost.Add(1) }
func (m *RTPMetrics) RecordFrameReceived() { m.framesReceived.Add(1) }
func (m *RTPMetrics) RecordFrameDropped()  { m.framesDropped.Add(1) }
func (m *RTPMetrics) RecordNACK()          { m.nackCount.Add(1) }
func (m *RTPMetrics) RecordPLI()           { m.pliCount.Add(1) }
func (m *RTPMetrics) RecordFIR()           { m.firCount.Add(1) }

func (m *RTPMetrics) GetJitter() float64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.jitter
}

func (m *RTPMetrics) GetJitterP99() float64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if len(m.jitterSamples) == 0 {
		return 0
	}
	sorted := make([]float64, len(m.jitterSamples))
	copy(sorted, m.jitterSamples)
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
	var minSeq, maxSeq uint16
	first := true
	for seq := range m.receivedSeqNums {
		if first {
			minSeq, maxSeq = seq, seq
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
	expected := int(maxSeq) - int(minSeq) + 1 + m.seqWrap*65536
	received := len(m.receivedSeqNums)
	if expected <= 0 {
		return 0
	}
	return float64(expected-received) / float64(expected) * 100.0
}

func (m *RTPMetrics) GetNackCount() int64 { return m.nackCount.Load() }
func (m *RTPMetrics) GetPliCount() int64  { return m.pliCount.Load() }
func (m *RTPMetrics) GetFirCount() int64  { return m.firCount.Load() }

func (m *RTPMetrics) GetPacketLoss() float64 {
	sent := m.packetsSent.Load()
	lost := m.packetsLost.Load()
	if sent == 0 {
		return 0
	}
	lossPercent := float64(lost) / float64(sent) * 100.0
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

func (m *RTPMetrics) GetBitrateKbps() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	cutoff := now.Add(-m.bitrateWindow)
	validSamples := make([]bitrateSample, 0, len(m.bitrateSamples))
	for _, s := range m.bitrateSamples {
		if s.timestamp.After(cutoff) {
			validSamples = append(validSamples, s)
		}
	}
	m.bitrateSamples = validSamples
	var totalBytes int64
	for _, s := range validSamples {
		totalBytes += s.bytes
	}
	return int(totalBytes * 8 / 1000)
}

func (m *RTPMetrics) GetFramesSent() int64  { return m.framesReceived.Load() }
func (m *RTPMetrics) GetBytesSent() int64   { return m.bytesSent.Load() }
func (m *RTPMetrics) GetPacketsSent() int64 { return m.packetsSent.Load() }

// GetMOS calculates Mean Opinion Score using ITU-T G.107 E-model
// Returns value between 1.0 (bad) and 4.5 (excellent)
func (m *RTPMetrics) GetMOS() float64 {
	return utils.CalculateMOS(m.GetPacketLoss(), m.GetJitter(), float64(m.GetRTT().Milliseconds()))
}

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
