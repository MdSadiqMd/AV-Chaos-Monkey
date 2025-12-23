package server

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"

	customMiddleware "github.com/MdSadiqMd/AV-Chaos-Monkey/pkg/middleware"
	"github.com/MdSadiqMd/AV-Chaos-Monkey/pkg/pb"
	"github.com/MdSadiqMd/AV-Chaos-Monkey/pkg/pool"
	"github.com/MdSadiqMd/AV-Chaos-Monkey/pkg/spike"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

type HTTPServer struct {
	addr        string
	tests       map[string]*TestSession
	spikeInject *spike.Injector
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

func NewHTTPServer(addr string) *HTTPServer {
	return &HTTPServer{
		addr:        addr,
		tests:       make(map[string]*TestSession),
		spikeInject: spike.NewInjector(),
	}
}

func (s *HTTPServer) Start() error {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(customMiddleware.LoggingMiddleware)
	r.Use(middleware.Recoverer)
	r.Use(customMiddleware.CorsMiddleware)

	r.Get("/healthz", s.handleHealth)
	r.Route("/api/v1", func(r chi.Router) {
		r.Post("/test/create", s.handleCreateTest)
		r.Route("/test/{testID}", func(r chi.Router) {
			r.Get("/", s.handleGetTest)
			r.Post("/start", s.handleStartTest)
			r.Post("/stop", s.handleStopTest)
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
	json.NewEncoder(w).Encode(map[string]string{"status": "healthy"})
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
	basePort := 5000
	if req.BackendRtpBasePort != "" {
		fmt.Sscanf(req.BackendRtpBasePort, "%d", &basePort)
	}

	participants := make([]*pb.ParticipantSetup, 0, req.NumParticipants)
	for i := int32(0); i < req.NumParticipants; i++ {
		id := uint32(1001 + i)
		port := basePort + int(i)

		p, err := participantPool.AddParticipant(id, req.Video, req.Audio, port)
		if err != nil {
			http.Error(w, "Failed to create participant: "+err.Error(), http.StatusInternalServerError)
			return
		}

		participants = append(participants, p.GetSetup())
	}

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
		BackendPortStart: int32(basePort),
		BackendPortEnd:   int32(basePort + int(req.NumParticipants) - 1),
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
	resp := &pb.StopTestResponse{
		Stopped: true,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// Injects a spike during runtime
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
