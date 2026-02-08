package client

import "github.com/MdSadiqMd/AV-Chaos-Monkey/pkg/logging"

func CollectStats(connections []*ParticipantConnection) map[string]any {
	stats := map[string]any{
		"total":     len(connections),
		"connected": 0,
		"video":     int64(0),
		"audio":     int64(0),
	}

	connected := 0
	var totalVideo, totalAudio int64

	for _, conn := range connections {
		totalVideo += conn.VideoPackets.Load()
		totalAudio += conn.AudioPackets.Load()
		if conn.Connected.Load() {
			connected++
		}
	}

	stats["connected"] = connected
	stats["video"] = totalVideo
	stats["audio"] = totalAudio
	return stats
}

func PrintStats(connections []*ParticipantConnection) {
	if len(connections) == 0 {
		logging.LogWarning("No connections to display")
		return
	}

	var totalVideo, totalAudio, totalVideoBytes, totalAudioBytes int64
	connected := 0

	for _, conn := range connections {
		totalVideo += conn.VideoPackets.Load()
		totalAudio += conn.AudioPackets.Load()
		totalVideoBytes += conn.VideoBytes.Load()
		totalAudioBytes += conn.AudioBytes.Load()
		if conn.Connected.Load() {
			connected++
		}
	}

	logging.LogSimple("═══════════════════════════════════════════════════════════")
	logging.LogSimple("                     WEBRTC RECEIVER STATS           ")
	logging.LogSimple("═══════════════════════════════════════════════════════════")
	logging.LogMetrics("Total Participants: %d", len(connections))
	logging.LogMetrics("  Connected: %d (%.1f%%)", connected, float64(connected)*100/float64(len(connections)))
	logging.LogSimple("")
	logging.LogMetrics("Total Packets: %d", totalVideo+totalAudio)
	logging.LogMetrics("  Video: %d packets (%.2f MB)", totalVideo, float64(totalVideoBytes)/1024/1024)
	logging.LogMetrics("  Audio: %d packets (%.2f MB)", totalAudio, float64(totalAudioBytes)/1024/1024)
	logging.LogSimple("═══════════════════════════════════════════════════════════")
}

func CloseConnections(connections []*ParticipantConnection) {
	for _, conn := range connections {
		if conn != nil {
			conn.Close()
		}
	}
}
