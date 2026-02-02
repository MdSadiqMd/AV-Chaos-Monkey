package server

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/MdSadiqMd/AV-Chaos-Monkey/pkg/constants"
	"github.com/MdSadiqMd/AV-Chaos-Monkey/pkg/logging"
	customMiddleware "github.com/MdSadiqMd/AV-Chaos-Monkey/pkg/middleware"
	"github.com/MdSadiqMd/AV-Chaos-Monkey/pkg/pool"
	pb "github.com/MdSadiqMd/AV-Chaos-Monkey/pkg/protobuf"
	"github.com/MdSadiqMd/AV-Chaos-Monkey/pkg/scheduler"
	"github.com/MdSadiqMd/AV-Chaos-Monkey/pkg/spike"
	"github.com/MdSadiqMd/AV-Chaos-Monkey/pkg/utils"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

type HTTPServer struct {
	addr            string
	tests           map[string]*TestSession
	testsMu         sync.RWMutex
	spikeInject     *spike.Injector
	partitionID     int
	totalPartitions int
}

type TestSession struct {
	ID              string
	Pool            *pool.ParticipantPool
	Config          *pb.CreateTestRequest
	State           string
	StartTime       time.Time
	ScheduledSpikes []*pb.SpikeEvent
	SpikeScheduler  *scheduler.SpikeScheduler
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
	partitionID := utils.GetEnvInt("PARTITION_ID", 0)
	totalPartitions := utils.GetEnvInt("TOTAL_PARTITIONS", 1)
	log.Printf("[HTTP] Partition config: ID=%d, Total=%d", partitionID, totalPartitions)
	initMediaSource()
	return &HTTPServer{
		addr:            addr,
		tests:           make(map[string]*TestSession),
		spikeInject:     spike.NewInjector(),
		partitionID:     partitionID,
		totalPartitions: totalPartitions,
	}
}

func (s *HTTPServer) isMyParticipant(participantID uint32) bool {
	if s.totalPartitions <= 1 {
		return true
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
			r.Post("/ice/{participantID}", s.handleAddICECandidate)
		})
	})

	log.Printf("[HTTP] Server listening on %s", s.addr)
	return http.ListenAndServe(s.addr, r)
}

func (s *HTTPServer) getTestSession(r *http.Request) (*TestSession, error) {
	testID := chi.URLParam(r, "testID")
	if testID == "" {
		return nil, fmt.Errorf("test ID required")
	}
	s.testsMu.RLock()
	session, exists := s.tests[testID]
	s.testsMu.RUnlock()
	if !exists {
		return nil, fmt.Errorf("test not found")
	}
	return session, nil
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
	if req.Video == nil {
		req.Video = &pb.VideoConfig{Width: constants.DefaultWidth, Height: constants.DefaultHeight, Fps: constants.DefaultFPS, BitrateKbps: constants.DefaultBitrateKbps, Codec: "h264"}
	}
	if req.Audio == nil {
		req.Audio = &pb.AudioConfig{SampleRate: 48000, Channels: 1, BitrateKbps: 128, Codec: "opus"}
	}

	participantPool := pool.NewParticipantPool(req.TestId)
	configureUDPTarget(participantPool)

	basePort := 5000
	if req.BackendRtpBasePort != "" {
		fmt.Sscanf(req.BackendRtpBasePort, "%d", &basePort)
	}
	portsPerPartition := 10000
	partitionBasePort := basePort + (s.partitionID * portsPerPartition)

	participants := make([]*pb.ParticipantSetup, 0, req.NumParticipants)
	myParticipantCount := 0
	for i := int32(0); i < req.NumParticipants; i++ {
		id := uint32(1001 + i)
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

	session := &TestSession{ID: req.TestId, Pool: participantPool, Config: &req, State: "created", ScheduledSpikes: req.Spikes}
	s.testsMu.Lock()
	s.tests[req.TestId] = session
	s.testsMu.Unlock()

	resp := &pb.CreateTestResponse{TestId: req.TestId, Participants: participants, BackendPortStart: int32(partitionBasePort), BackendPortEnd: int32(partitionBasePort + myParticipantCount - 1), SfuGrpcEndpoint: "localhost:50051"}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(resp)
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

	participantCount := session.Pool.Size()
	session.State = "running"
	session.StartTime = time.Now()

	resp := &pb.StartTestResponse{Started: true, StartTimeUnixMs: session.StartTime.UnixMilli(), StatusMessage: fmt.Sprintf("Starting %d participants", participantCount)}
	w.Header().Set("Content-Type", "application/json")

	go func() {
		session.Pool.Start()
		logging.LogSuccess("Test %s: Started %d participants in background", session.ID, participantCount)
	}()

	if len(session.ScheduledSpikes) > 0 {
		spikeConfig := s.getSpikeDistributionConfig(session.Config)
		if spikeConfig.DistributionStrategy == "legacy" {
			for _, spike := range session.ScheduledSpikes {
				go s.scheduleSpike(session, spike)
			}
			logging.LogInfo("Test %s: Scheduled %d spikes with legacy timing", session.ID, len(session.ScheduledSpikes))
		} else {
			injectFunc := func(spikeEvent *pb.SpikeEvent) error {
				if session.Pool != nil {
					session.Pool.InjectSpike(spikeEvent)
				}
				return s.spikeInject.Inject(spikeEvent)
			}
			testDuration := time.Duration(session.Config.DurationSeconds) * time.Second
			if testDuration == 0 {
				testDuration = 5 * time.Minute
				for _, sp := range session.ScheduledSpikes {
					endTime := time.Duration(sp.StartOffsetSeconds+sp.DurationSeconds) * time.Second
					if endTime > testDuration {
						testDuration = endTime + time.Minute
					}
				}
			}
			session.SpikeScheduler = scheduler.NewSpikeScheduler(spikeConfig, injectFunc)
			session.SpikeScheduler.ScheduleSpikes(session.ScheduledSpikes, spikeConfig)
			session.SpikeScheduler.Start()
			logging.LogInfo("Test %s: Scheduled %d spikes with %s distribution over %v", session.ID, len(session.ScheduledSpikes), spikeConfig.DistributionStrategy, testDuration)
		}
	}

	if err := json.NewEncoder(w).Encode(resp); err != nil {
		log.Printf("[HTTP] Error encoding response for test %s: %v", session.ID, err)
		return
	}
	logging.LogInfo("Test %s start request completed (participants starting in background)", session.ID)
}

func (s *HTTPServer) getSpikeDistributionConfig(req *pb.CreateTestRequest) *scheduler.SpikeSchedulerConfig {
	config := scheduler.DefaultSpikeSchedulerConfig()
	if req.DurationSeconds > 0 {
		config.TestDuration = time.Duration(req.DurationSeconds) * time.Second
	}
	if req.SpikeDistribution != nil {
		sd := req.SpikeDistribution
		if sd.Strategy != "" {
			config.DistributionStrategy = sd.Strategy
		}
		if sd.MinSpacingSeconds > 0 {
			config.MinSpacing = time.Duration(sd.MinSpacingSeconds) * time.Second
		}
		if sd.JitterPercent > 0 {
			config.JitterPercent = int(sd.JitterPercent)
		}
	}
	return config
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
	if session.SpikeScheduler != nil {
		session.SpikeScheduler.Stop()
	}
	session.Pool.Stop()
	session.State = "stopped"
	s.spikeInject.RemoveAll()

	finalMetrics := session.Pool.GetMetrics()
	resp := &pb.StopTestResponse{Stopped: true, FinalMetrics: finalMetrics}
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

func (s *HTTPServer) GetTest(testID string) *TestSession {
	return s.tests[testID]
}
