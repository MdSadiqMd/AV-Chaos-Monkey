package server

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	customMiddleware "github.com/MdSadiqMd/AV-Chaos-Monkey/pkg/middleware"
	"github.com/MdSadiqMd/AV-Chaos-Monkey/pkg/pool"
	pb "github.com/MdSadiqMd/AV-Chaos-Monkey/pkg/protobuf"
	"github.com/MdSadiqMd/AV-Chaos-Monkey/pkg/spike"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

type HTTPServer struct {
	addr            string
	tests           map[string]*TestSession
	spikeInject     *spike.Injector
	partitionID     int // Which partition this instance handles (0-indexed)
	totalPartitions int // Total number of partitions (pods)
}

// Active test session
type TestSession struct {
	ID              string
	Pool            *pool.ParticipantPool
	Config          *pb.CreateTestRequest
	State           string
	StartTime       time.Time
	ScheduledSpikes []*pb.SpikeEvent
}

func (ts *TestSession) StateValue() int {
	switch ts.State {
	case "created":
		return 0
	case "running":
		return 1
	case "stopped":
		return 2
	default:
		return -1
	}
}

func NewHTTPServer(addr string) *HTTPServer {
	partitionID := getEnvInt("PARTITION_ID", 0)
	totalPartitions := getEnvInt("TOTAL_PARTITIONS", 1)

	log.Printf("[HTTP] Partition config: ID=%d, Total=%d", partitionID, totalPartitions)

	return &HTTPServer{
		addr:            addr,
		tests:           make(map[string]*TestSession),
		spikeInject:     spike.NewInjector(),
		partitionID:     partitionID,
		totalPartitions: totalPartitions,
	}
}

func getEnvInt(key string, defaultValue int) int {
	if val := os.Getenv(key); val != "" {
		if i, err := strconv.Atoi(val); err == nil {
			return i
		}
	}
	return defaultValue
}

// isMyParticipant returns true if this participant belongs to this partition
func (s *HTTPServer) isMyParticipant(participantID uint32) bool {
	if s.totalPartitions <= 1 {
		return true // Single partition mode, handle all
	}
	return int(participantID)%s.totalPartitions == s.partitionID
}

func (s *HTTPServer) Start() error {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(customMiddleware.LoggingMiddleware)
	r.Use(middleware.Recoverer)
	r.Use(customMiddleware.CorsMiddleware)

	r.Get("/healthz", s.handleHealth)
	r.Get("/metrics", s.handleGetAllMetrics)
	r.Route("/api/v1", func(r chi.Router) {
		r.Post("/test/create", s.handleCreateTest)
		r.Route("/test/{testID}", func(r chi.Router) {
			r.Get("/", s.handleGetTest)
			r.Post("/start", s.handleStartTest)
			r.Post("/stop", s.handleStopTest)
			r.Get("/metrics", s.handleGetMetrics)
			r.Post("/spike", s.handleInjectSpike)
			r.Get("/sdp/{participantID}", s.handleGetSDP)
			r.Post("/answer/{participantID}", s.handleSetAnswer)
		})
	})

	log.Printf("[HTTP] Server listening on %s", s.addr)
	return http.ListenAndServe(s.addr, r)
}

func (s *HTTPServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]any{
		"status":           "healthy",
		"partition_id":     s.partitionID,
		"total_partitions": s.totalPartitions,
	})
}

func (s *HTTPServer) handleCreateTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req pb.CreateTestRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.TestId == "" {
		req.TestId = fmt.Sprintf("test_%d", time.Now().UnixNano())
	}
	if req.NumParticipants <= 0 {
		req.NumParticipants = 5
	}

	// Default video config
	if req.Video == nil {
		req.Video = &pb.VideoConfig{
			Width:       1280,
			Height:      720,
			Fps:         30,
			BitrateKbps: 2500,
			Codec:       "h264",
		}
	}

	// Default audio config
	if req.Audio == nil {
		req.Audio = &pb.AudioConfig{
			SampleRate:  48000,
			Channels:    1,
			BitrateKbps: 128,
			Codec:       "opus",
		}
	}

	participantPool := pool.NewParticipantPool(req.TestId)

	udpTargetHost := os.Getenv("UDP_TARGET_HOST")
	udpTargetPort := getEnvInt("UDP_TARGET_PORT", 0)
	if udpTargetHost != "" && udpTargetPort > 0 {
		participantPool.SetTarget(udpTargetHost, udpTargetPort)
		log.Printf("[HTTP] UDP transmission enabled: target=%s:%d", udpTargetHost, udpTargetPort)
	} else if udpTargetPort > 0 {
		// If only port is set, use localhost
		participantPool.SetTarget("127.0.0.1", udpTargetPort)
		log.Printf("[HTTP] UDP transmission enabled: target=127.0.0.1:%d", udpTargetPort)
	} else {
		log.Printf("[HTTP] UDP transmission disabled (set UDP_TARGET_HOST and UDP_TARGET_PORT to enable)")
	}

	basePort := 5000
	if req.BackendRtpBasePort != "" {
		fmt.Sscanf(req.BackendRtpBasePort, "%d", &basePort)
	}

	// Partition-aware port allocation: each partition gets its own port range
	// This prevents port conflicts when running multiple pods
	portsPerPartition := 10000
	partitionBasePort := basePort + (s.partitionID * portsPerPartition)

	participants := make([]*pb.ParticipantSetup, 0, req.NumParticipants)
	myParticipantCount := 0
	for i := int32(0); i < req.NumParticipants; i++ {
		id := uint32(1001 + i)

		// Only create participants assigned to this partition
		if !s.isMyParticipant(id) {
			continue
		}

		port := partitionBasePort + myParticipantCount
		myParticipantCount++

		p, err := participantPool.AddParticipant(id, req.Video, req.Audio, port)
		if err != nil {
			http.Error(w, "Failed to create participant: "+err.Error(), http.StatusInternalServerError)
			return
		}

		participants = append(participants, p.GetSetup())
	}

	log.Printf("[HTTP] Partition %d/%d: Created %d participants (of %d total requested)",
		s.partitionID, s.totalPartitions, myParticipantCount, req.NumParticipants)

	session := &TestSession{
		ID:              req.TestId,
		Pool:            participantPool,
		Config:          &req,
		State:           "created",
		ScheduledSpikes: req.Spikes,
	}

	s.tests[req.TestId] = session

	resp := &pb.CreateTestResponse{
		TestId:           req.TestId,
		Participants:     participants,
		BackendPortStart: int32(partitionBasePort),
		BackendPortEnd:   int32(partitionBasePort + myParticipantCount - 1),
		SfuGrpcEndpoint:  "localhost:50051",
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(resp)
}

func (s *HTTPServer) getTestSession(r *http.Request) (*TestSession, error) {
	testID := chi.URLParam(r, "testID")
	if testID == "" {
		return nil, fmt.Errorf("test ID required")
	}

	session, exists := s.tests[testID]
	if !exists {
		return nil, fmt.Errorf("test not found")
	}

	return session, nil
}

func (s *HTTPServer) handleStartTest(w http.ResponseWriter, r *http.Request) {
	session, err := s.getTestSession(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if session.Pool == nil {
		http.Error(w, "Test session not available", http.StatusServiceUnavailable)
		return
	}

	session.Pool.Start()
	session.State = "running"
	session.StartTime = time.Now()
	for _, spike := range session.ScheduledSpikes {
		go s.scheduleSpike(session, spike)
	}

	resp := &pb.StartTestResponse{
		Started:         true,
		StartTimeUnixMs: session.StartTime.UnixMilli(),
		StatusMessage:   fmt.Sprintf("Started %d participants", session.Pool.Size()),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (s *HTTPServer) scheduleSpike(session *TestSession, spikeEvent *pb.SpikeEvent) {
	if session == nil || session.Pool == nil || spikeEvent == nil {
		return
	}
	if spikeEvent.StartOffsetSeconds > 0 {
		time.Sleep(time.Duration(spikeEvent.StartOffsetSeconds) * time.Second)
	}
	if session.Pool == nil {
		return
	}

	session.Pool.InjectSpike(spikeEvent)
	s.spikeInject.Inject(spikeEvent)
}

func (s *HTTPServer) handleStopTest(w http.ResponseWriter, r *http.Request) {
	session, err := s.getTestSession(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if session.Pool == nil {
		http.Error(w, "Test session not available", http.StatusServiceUnavailable)
		return
	}

	session.Pool.Stop()
	session.State = "stopped"
	s.spikeInject.RemoveAll()

	finalMetrics := session.Pool.GetMetrics()
	resp := &pb.StopTestResponse{
		Stopped:      true,
		FinalMetrics: finalMetrics,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (s *HTTPServer) handleGetAllMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	if len(s.tests) == 0 {
		fmt.Fprintf(w, "# No active tests\n")
		return
	}

	// Write metric metadata once at the top
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

		// Output metrics in Prometheus format with partition label
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

		// Generate Prometheus format using aggregate metrics
		// Aggregate metrics have type="aggregate" to distinguish from individual participant metrics
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
			session.ID, metrics.Aggregate.TotalFramesSent,
			session.ID, metrics.Aggregate.TotalPacketsSent,
			session.ID, metrics.Aggregate.TotalBitrateKbps*1000/8, // Convert kbps to bytes (approximate)
			session.ID, metrics.Aggregate.AvgJitterMs,
			session.ID, metrics.Aggregate.AvgPacketLoss,
			session.ID, metrics.Aggregate.AvgMosScore,
			session.ID, metrics.Aggregate.TotalBitrateKbps,
			session.ID, func() int {
				switch session.State {
				case "running":
					return 1
				case "stopped":
					return 2
				default:
					return 0
				}
			}(),
			session.ID, metrics.ElapsedSeconds,
		)

		// Include individual participant metrics if available
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

		w.Write([]byte(output))
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(metrics)
}

// Injects spike during runtime
func (s *HTTPServer) handleInjectSpike(w http.ResponseWriter, r *http.Request) {
	session, err := s.getTestSession(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if session.Pool == nil {
		http.Error(w, "Test session not available", http.StatusServiceUnavailable)
		return
	}

	var spikeEvent pb.SpikeEvent
	if err := json.NewDecoder(r.Body).Decode(&spikeEvent); err != nil {
		http.Error(w, "Invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := spike.ValidateSpikeEvent(&spikeEvent); err != nil {
		http.Error(w, "Invalid spike: "+err.Error(), http.StatusBadRequest)
		return
	}

	session.Pool.InjectSpike(&spikeEvent)
	if err := s.spikeInject.Inject(&spikeEvent); err != nil {
		// Log but don't fail btw
		log.Printf("[Spike] Warning: %v", err)
	}

	resp := &pb.InjectSpikeResponse{
		Injected: true,
		Message:  fmt.Sprintf("Spike %s injected for %d seconds", spikeEvent.SpikeId, spikeEvent.DurationSeconds),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (s *HTTPServer) handleGetSDP(w http.ResponseWriter, r *http.Request) {
	session, err := s.getTestSession(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	participantIDStr := chi.URLParam(r, "participantID")
	if participantIDStr == "" {
		http.Error(w, "Participant ID required", http.StatusBadRequest)
		return
	}

	participantIDInt, err := strconv.Atoi(participantIDStr)
	if err != nil {
		http.Error(w, "Invalid participant ID", http.StatusBadRequest)
		return
	}
	participantID := uint32(participantIDInt)

	p := session.Pool.GetParticipant(participantID)
	if p == nil {
		http.Error(w, "Participant not found", http.StatusNotFound)
		return
	}

	sdp := generateSDPOffer(p)
	resp := map[string]any{
		"participant_id": participantID,
		"sdp_offer":      sdp,
		"ice_credentials": map[string]string{
			"ufrag":    p.IceUfrag,
			"password": p.IcePassword,
		},
		"srtp_keys": map[string]string{
			"master_key":  fmt.Sprintf("%x", p.SrtpMasterKey),
			"master_salt": fmt.Sprintf("%x", p.SrtpMasterSalt),
		},
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// Sets the SDP answer for a participant
func (s *HTTPServer) handleSetAnswer(w http.ResponseWriter, r *http.Request) {
	session, err := s.getTestSession(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	participantIDStr := chi.URLParam(r, "participantID")
	if participantIDStr == "" {
		http.Error(w, "Participant ID required", http.StatusBadRequest)
		return
	}

	participantIDInt, err := strconv.Atoi(participantIDStr)
	if err != nil {
		http.Error(w, "Invalid participant ID", http.StatusBadRequest)
		return
	}
	participantID := uint32(participantIDInt)

	p := session.Pool.GetParticipant(participantID)
	if p == nil {
		http.Error(w, "Participant not found", http.StatusNotFound)
		return
	}

	var req struct {
		SDPAnswer string `json:"sdp_answer"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	resp := map[string]any{
		"connected":  true,
		"ice_state":  "connected",
		"dtls_state": "connected",
		"srtp_state": "active",
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (s *HTTPServer) handleGetTest(w http.ResponseWriter, r *http.Request) {
	session, err := s.getTestSession(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	resp := map[string]any{
		"test_id":          session.ID,
		"state":            session.State,
		"num_participants": session.Pool.Size(),
		"config":           session.Config,
	}
	if !session.StartTime.IsZero() {
		resp["start_time"] = session.StartTime.Format(time.RFC3339)
		resp["elapsed_seconds"] = int64(time.Since(session.StartTime).Seconds())
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (s *HTTPServer) GetTest(testID string) *TestSession {
	return s.tests[testID]
}

// Generates an SDP offer for a participant
func generateSDPOffer(p *pool.VirtualParticipant) string {
	return fmt.Sprintf(`v=0
o=simulator %d 2 IN IP4 127.0.0.1
s=MeetingBotUDPSimulator
t=0 0
a=group:BUNDLE 0 1
a=extmap-allow-mixed
a=msid-semantic: WMS stream_%d

m=video %d RTP/AVP 96
c=IN IP4 127.0.0.1
a=rtcp:%d IN IP4 127.0.0.1
a=ice-ufrag:%s
a=ice-pwd:%s
a=ice-options:trickle
a=fingerprint:sha-256 FF:FF:FF:FF:FF:FF:FF:FF:FF:FF:FF:FF:FF:FF:FF:FF:FF:FF:FF:FF:FF:FF:FF:FF:FF:FF:FF:FF:FF:FF:FF:FF
a=setup:actpass
a=mid:0
a=extmap:1 urn:ietf:params:rtp-hdrext:abs-send-time
a=extmap:2 http://www.ietf.org/id/draft-holmer-rmcat-transport-wide-cc-extensions-01
a=rtpmap:96 H264/90000
a=rtcp-fb:96 goog-remb
a=rtcp-fb:96 transport-cc
a=rtcp-fb:96 nack
a=rtcp-fb:96 nack pli
a=fmtp:96 profile-level-id=42e01f;packetization-mode=1
a=rtcp-mux
a=ssrc:%d participant_id=%d
a=ssrc:%d cname:participant_%d

m=audio %d RTP/AVP 111
c=IN IP4 127.0.0.1
a=rtcp:%d IN IP4 127.0.0.1
a=ice-ufrag:%s
a=ice-pwd:%s
a=mid:1
a=rtpmap:111 opus/48000/2
a=rtcp-fb:111 transport-cc
a=fmtp:111 useinbandfec=1
a=rtcp-mux
a=ssrc:%d participant_id=%d
`,
		p.ID*1000,
		p.ID,
		p.BackendRTPPort,
		p.BackendRTPPort,
		p.IceUfrag,
		p.IcePassword,
		p.ID*1000, p.ID,
		p.ID*1000, p.ID,
		p.BackendRTPPort+1,
		p.BackendRTPPort+1,
		p.IceUfrag,
		p.IcePassword,
		p.ID*1000+1, p.ID,
	)
}
