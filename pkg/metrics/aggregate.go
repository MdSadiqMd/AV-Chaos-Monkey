package metrics

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

func Aggregate(metrics []*RTPMetrics) *AggregateMetrics {
	if len(metrics) == 0 {
		return &AggregateMetrics{}
	}

	agg := &AggregateMetrics{MinMosScore: 5.0}
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
