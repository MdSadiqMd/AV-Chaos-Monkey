package metrics

import "fmt"

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
