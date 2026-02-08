package server

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"

	pb "github.com/MdSadiqMd/AV-Chaos-Monkey/internal/protobuf"
	"github.com/MdSadiqMd/AV-Chaos-Monkey/internal/spike"
	webrtc "github.com/MdSadiqMd/AV-Chaos-Monkey/internal/webrtc"
	"github.com/go-chi/chi/v5"
)

func (s *HTTPServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]any{
		"status":           "healthy",
		"partition_id":     s.partitionID,
		"total_partitions": s.totalPartitions,
	})
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

	go func() {
		session.Pool.InjectSpike(&spikeEvent)
		if err := s.spikeInject.Inject(&spikeEvent); err != nil {
			log.Printf("[Spike] Warning: %v", err)
		}
	}()

	resp := &pb.InjectSpikeResponse{
		Injected: true,
		Message:  fmt.Sprintf("Spike %s queued for %d seconds", spikeEvent.SpikeId, spikeEvent.DurationSeconds),
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
	if session.State == "stopped" {
		http.Error(w, "Test is stopped, cannot create WebRTC connection", http.StatusBadRequest)
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

	webrtcParticipant := session.Pool.GetWebRTCParticipant(participantID)
	if webrtcParticipant != nil {
		shouldRecreate, recreateReason := webrtcParticipant.NeedsRecreation()
		if shouldRecreate {
			log.Printf("[HTTP] WebRTC participant %d: %s, recreating...", participantID, recreateReason)
			webrtcParticipant.Close()
			session.Pool.RemoveWebRTCParticipant(participantID)
			webrtcParticipant = nil
		}
	}

	if webrtcParticipant == nil {
		udpParticipant := session.Pool.GetParticipant(participantID)
		if udpParticipant == nil {
			http.Error(w, "Participant not found", http.StatusNotFound)
			return
		}
		videoConfig := webrtc.VideoConfig{
			Width:       int(udpParticipant.VideoConfig.Width),
			Height:      int(udpParticipant.VideoConfig.Height),
			FPS:         int(udpParticipant.VideoConfig.Fps),
			BitrateKbps: int(udpParticipant.VideoConfig.BitrateKbps),
			Codec:       "H264",
		}
		newWebRTCParticipant, err := webrtc.NewParticipant(participantID, videoConfig)
		if err != nil {
			log.Printf("[HTTP] Failed to create WebRTC participant %d: %v", participantID, err)
			http.Error(w, fmt.Sprintf("Failed to create WebRTC participant: %v", err), http.StatusInternalServerError)
			return
		}
		session.Pool.AddWebRTCParticipant(participantID, newWebRTCParticipant)
		webrtcParticipant = newWebRTCParticipant
		log.Printf("[HTTP] Created WebRTC participant %d with real peer connection (test state: %s)", participantID, session.State)
	}

	sdpOffer, err := webrtcParticipant.CreateOffer()
	if err != nil {
		log.Printf("[HTTP] Failed to create SDP offer for participant %d: %v", participantID, err)
		http.Error(w, fmt.Sprintf("Failed to create SDP offer: %v", err), http.StatusInternalServerError)
		return
	}

	resp := map[string]any{
		"participant_id": participantID,
		"sdp_offer":      sdpOffer,
		"test_state":     session.State,
		"trickle_ice":    true,
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
	log.Printf("[HTTP] SDP offer returned for participant %d (test state: %s)", participantID, session.State)
}

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

	webrtcParticipant := session.Pool.GetWebRTCParticipant(participantID)
	if webrtcParticipant == nil {
		http.Error(w, "WebRTC participant not found. Call /sdp first to create the participant.", http.StatusNotFound)
		return
	}

	var req struct {
		SDPAnswer string `json:"sdp_answer"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	if err := webrtcParticipant.SetRemoteAnswer(req.SDPAnswer); err != nil {
		log.Printf("[HTTP] Failed to set SDP answer for participant %d: %v", participantID, err)
		http.Error(w, fmt.Sprintf("Failed to set SDP answer: %v", err), http.StatusInternalServerError)
		return
	}

	if session.State == "running" {
		log.Printf("[HTTP] SDP answer set for participant %d (test is running)", participantID)
	}
	log.Printf("[HTTP] WebRTC participant %d: SDP answer set, connection establishing...", participantID)

	pc := webrtcParticipant.GetPeerConnection()
	var iceState, connectionState string
	if pc != nil {
		type connectionStateGetter interface {
			ICEConnectionState() interface{ String() string }
			ConnectionState() interface{ String() string }
		}
		if csg, ok := pc.(connectionStateGetter); ok {
			iceState = csg.ICEConnectionState().String()
			connectionState = csg.ConnectionState().String()
		} else {
			iceState = "unknown"
			connectionState = "unknown"
		}
	}

	resp := map[string]any{
		"connected":        true,
		"ice_state":        iceState,
		"connection_state": connectionState,
		"message":          "SDP answer accepted, WebRTC connection establishing",
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (s *HTTPServer) handleAddICECandidate(w http.ResponseWriter, r *http.Request) {
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

	webrtcParticipant := session.Pool.GetWebRTCParticipant(participantID)
	if webrtcParticipant == nil {
		http.Error(w, "WebRTC participant not found. Call /sdp first to create the participant.", http.StatusNotFound)
		return
	}

	var req struct {
		Candidate     string `json:"candidate"`
		SDPMLineIndex *int   `json:"sdpMLineIndex,omitempty"`
		SDPMid        string `json:"sdpMid,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}
	if req.Candidate == "" {
		http.Error(w, "Candidate is required", http.StatusBadRequest)
		return
	}

	if err := webrtcParticipant.AddICECandidate(req.Candidate); err != nil {
		log.Printf("[HTTP] Failed to add ICE candidate for participant %d: %v", participantID, err)
		http.Error(w, fmt.Sprintf("Failed to add ICE candidate: %v", err), http.StatusInternalServerError)
		return
	}

	log.Printf("[HTTP] WebRTC participant %d: Added ICE candidate from client", participantID)
	resp := map[string]any{
		"added":   true,
		"message": "ICE candidate added successfully",
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}
