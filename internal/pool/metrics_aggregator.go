package pool

import (
	"time"

	"github.com/MdSadiqMd/AV-Chaos-Monkey/pkg/constants"
	"github.com/MdSadiqMd/AV-Chaos-Monkey/pkg/logging"
	pb "github.com/MdSadiqMd/AV-Chaos-Monkey/internal/protobuf"
	"github.com/MdSadiqMd/AV-Chaos-Monkey/pkg/utils"
)

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

func (pp *ParticipantPool) updateCachedMetrics() {
	pp.mu.RLock()
	participantCount := len(pp.participants)
	var totalFrames, totalPackets, totalBitrate int64
	var totalJitter, totalLoss, totalMos float64
	var realMetricsCount float64

	participantsList := make([]*VirtualParticipant, 0, participantCount)
	for _, p := range pp.participants {
		participantsList = append(participantsList, p)
		totalFrames += p.frameCount.Load()
		totalPackets += p.packetsSent.Load()
		totalBitrate += int64(p.VideoConfig.BitrateKbps)

		ssrc := p.ID * 1000
		if pp.rtcpFeedback != nil {
			loss, jitter, rtt := pp.rtcpFeedback.GetMetrics(ssrc)
			if loss > 0 || jitter > 0 || rtt > 0 {
				totalLoss += loss
				totalJitter += jitter
				totalMos += utils.CalculateMOS(loss, jitter, rtt)
				realMetricsCount++
				continue
			}
		}
		pm := p.GetMetrics()
		totalMos += pm.MosScore
		totalLoss += pm.PacketLossPercent
		totalJitter += pm.JitterMs
	}
	pp.mu.RUnlock()

	pp.webrtcParticipantsMu.RLock()
	webrtcCount := len(pp.webrtcParticipants)
	for _, wp := range pp.webrtcParticipants {
		loss, jitter, rtt := wp.GetRealMetrics()
		if loss > 0 || jitter > 0 || rtt > 0 {
			totalLoss += loss
			totalJitter += jitter
			totalMos += utils.CalculateMOS(loss, jitter, rtt)
			realMetricsCount++
		}
	}
	pp.webrtcParticipantsMu.RUnlock()

	totalCount := float64(participantCount + webrtcCount)
	if totalCount == 0 {
		totalCount = 1
	}
	if realMetricsCount > 0 {
		logging.LogInfo("Using real RTCP metrics for %.0f/%.0f participants", realMetricsCount, totalCount)
	}

	avgLoss := totalLoss / totalCount
	if avgLoss > 100.0 {
		avgLoss = 100.0
	}

	elapsed := int64(0)
	if !pp.startTime.IsZero() {
		elapsed = int64(time.Since(pp.startTime).Seconds())
	}

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
				pm := p.GetMetricsWithRTCP(pp.rtcpFeedback)
				if pm != nil {
					participantMetrics = append(participantMetrics, pm)
				}
			}
		}
	}

	metricsResp := &pb.MetricsResponse{
		TestId:         pp.testID,
		ElapsedSeconds: elapsed,
		Participants:   participantMetrics,
		Aggregate: &pb.AggregateMetrics{
			TotalFramesSent:  totalFrames,
			TotalPacketsSent: totalPackets,
			AvgJitterMs:      totalJitter / totalCount,
			AvgPacketLoss:    avgLoss,
			AvgMosScore:      totalMos / totalCount,
			TotalBitrateKbps: totalBitrate,
		},
	}

	pp.cachedMetricsMu.Lock()
	pp.cachedMetrics = metricsResp
	pp.cachedMetricsTime = time.Now()
	pp.cachedMetricsMu.Unlock()
}

func (pp *ParticipantPool) GetMetrics() *pb.MetricsResponse {
	pp.cachedMetricsMu.RLock()
	cached := pp.cachedMetrics
	cacheTime := pp.cachedMetricsTime
	pp.cachedMetricsMu.RUnlock()

	if cached != nil && time.Since(cacheTime) < pp.metricsCacheTTL {
		elapsed := int64(0)
		if !pp.startTime.IsZero() {
			elapsed = int64(time.Since(pp.startTime).Seconds())
		}
		return &pb.MetricsResponse{
			TestId:         cached.TestId,
			ElapsedSeconds: elapsed,
			Participants:   cached.Participants,
			Aggregate:      cached.Aggregate,
		}
	}

	pp.updateCachedMetrics()
	pp.cachedMetricsMu.RLock()
	defer pp.cachedMetricsMu.RUnlock()
	return pp.cachedMetrics
}
