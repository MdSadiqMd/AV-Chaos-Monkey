package server

import (
	"encoding/json"
	"fmt"
	"net/http"

	pb "github.com/MdSadiqMd/AV-Chaos-Monkey/pkg/protobuf"
)

func (s *HTTPServer) handleGetAllMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	if len(s.tests) == 0 {
		fmt.Fprintf(w, "# No active tests\n")
		return
	}

	fmt.Fprintf(w, `# HELP rtp_frames_sent_total Total RTP frames sent
# TYPE rtp_frames_sent_total counter
# HELP rtp_packets_sent_total Total RTP packets sent
# TYPE rtp_packets_sent_total counter
# HELP rtp_jitter_ms RTP jitter in milliseconds
# TYPE rtp_jitter_ms gauge
# HELP rtp_packet_loss_percent Packet loss percentage
# TYPE rtp_packet_loss_percent gauge
# HELP rtp_mos_score Mean Opinion Score (1-4.5)
# TYPE rtp_mos_score gauge
# HELP rtp_bitrate_kbps Total bitrate in kbps
# TYPE rtp_bitrate_kbps gauge
# HELP rtp_test_state Test state (0=created, 1=running, 2=stopped)
# TYPE rtp_test_state gauge
# HELP rtp_active_participants Number of active participants
# TYPE rtp_active_participants gauge
`)

	for testID, session := range s.tests {
		if session.Pool == nil {
			continue
		}
		metrics := session.Pool.GetMetrics()
		if metrics == nil {
			continue
		}
		if metrics.Aggregate != nil {
			fmt.Fprintf(w, `rtp_frames_sent_total{test_id="%s",partition="%d"} %d
rtp_packets_sent_total{test_id="%s",partition="%d"} %d
rtp_jitter_ms{test_id="%s",partition="%d"} %.2f
rtp_packet_loss_percent{test_id="%s",partition="%d"} %.2f
rtp_mos_score{test_id="%s",partition="%d"} %.2f
rtp_bitrate_kbps{test_id="%s",partition="%d"} %d
rtp_test_state{test_id="%s",partition="%d"} %d
rtp_active_participants{test_id="%s",partition="%d"} %d
`,
				testID, s.partitionID, metrics.Aggregate.TotalFramesSent,
				testID, s.partitionID, metrics.Aggregate.TotalPacketsSent,
				testID, s.partitionID, metrics.Aggregate.AvgJitterMs,
				testID, s.partitionID, metrics.Aggregate.AvgPacketLoss,
				testID, s.partitionID, metrics.Aggregate.AvgMosScore,
				testID, s.partitionID, metrics.Aggregate.TotalBitrateKbps,
				testID, s.partitionID, session.StateValue(),
				testID, s.partitionID, len(metrics.Participants),
			)
		}
	}
}

func (s *HTTPServer) handleGetMetrics(w http.ResponseWriter, r *http.Request) {
	session, err := s.getTestSession(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if session.Pool == nil {
		http.Error(w, "Test session not available", http.StatusServiceUnavailable)
		return
	}

	format := r.URL.Query().Get("format")
	metrics := session.Pool.GetMetrics()
	if metrics == nil {
		http.Error(w, "Metrics not available", http.StatusServiceUnavailable)
		return
	}

	if format == "prometheus" {
		w.Header().Set("Content-Type", "text/plain")
		output := formatPrometheusMetrics(session, metrics)
		w.Write([]byte(output))
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(metrics)
}

func formatPrometheusMetrics(session *TestSession, metrics *pb.MetricsResponse) string {
	agg := metrics.Aggregate
	output := fmt.Sprintf(`# HELP rtp_frames_sent_total Total RTP frames sent
# TYPE rtp_frames_sent_total counter
rtp_frames_sent_total{test_id="%s",type="aggregate"} %d

# HELP rtp_packets_sent_total Total RTP packets sent
# TYPE rtp_packets_sent_total counter
rtp_packets_sent_total{test_id="%s",type="aggregate"} %d

# HELP rtp_bytes_sent_total Total bytes sent
# TYPE rtp_bytes_sent_total counter
rtp_bytes_sent_total{test_id="%s",type="aggregate"} %d

# HELP rtp_jitter_ms RTP jitter in milliseconds
# TYPE rtp_jitter_ms gauge
rtp_jitter_ms{test_id="%s",type="aggregate"} %.2f

# HELP rtp_packet_loss_percent Packet loss percentage
# TYPE rtp_packet_loss_percent gauge
rtp_packet_loss_percent{test_id="%s",type="aggregate"} %.2f

# HELP rtp_mos_score Mean Opinion Score (1-4.5)
# TYPE rtp_mos_score gauge
rtp_mos_score{test_id="%s",type="aggregate"} %.2f

# HELP rtp_bitrate_kbps Total bitrate in kbps
# TYPE rtp_bitrate_kbps gauge
rtp_bitrate_kbps{test_id="%s",type="aggregate"} %d

# HELP rtp_test_state Test state (0=created, 1=running, 2=stopped)
# TYPE rtp_test_state gauge
rtp_test_state{test_id="%s"} %d

# HELP rtp_test_elapsed_seconds Test elapsed time in seconds
# TYPE rtp_test_elapsed_seconds gauge
rtp_test_elapsed_seconds{test_id="%s"} %d

`,
		session.ID, agg.TotalFramesSent,
		session.ID, agg.TotalPacketsSent,
		session.ID, agg.TotalBitrateKbps*1000/8,
		session.ID, agg.AvgJitterMs,
		session.ID, agg.AvgPacketLoss,
		session.ID, agg.AvgMosScore,
		session.ID, agg.TotalBitrateKbps,
		session.ID, session.StateValue(),
		session.ID, metrics.ElapsedSeconds,
	)

	if len(metrics.Participants) > 0 {
		for _, p := range metrics.Participants {
			output += fmt.Sprintf(`rtp_frames_sent_total{test_id="%s",participant_id="%d"} %d
rtp_jitter_ms{test_id="%s",participant_id="%d"} %.2f
rtp_packet_loss_percent{test_id="%s",participant_id="%d"} %.2f
rtp_mos_score{test_id="%s",participant_id="%d"} %.2f

`,
				session.ID, p.ParticipantId, p.FramesSent,
				session.ID, p.ParticipantId, p.JitterMs,
				session.ID, p.ParticipantId, p.PacketLossPercent,
				session.ID, p.ParticipantId, p.MosScore,
			)
		}
	}

	return output
}
