package webrtc

import (
	"fmt"
	"log"
	"os"

	pionwebrtc "github.com/pion/webrtc/v3"
)

func buildICEServers(id uint32) []pionwebrtc.ICEServer {
	turnUsername := os.Getenv("TURN_USERNAME")
	if turnUsername == "" {
		turnUsername = "webrtc"
	}
	turnPassword := os.Getenv("TURN_PASSWORD")
	if turnPassword == "" {
		turnPassword = "webrtc123"
	}

	turnReplicas := 3
	if replicas := os.Getenv("TURN_REPLICAS"); replicas != "" {
		fmt.Sscanf(replicas, "%d", &turnReplicas)
	}
	if turnReplicas < 1 {
		turnReplicas = 1
	}

	turnIndex := int(id) % turnReplicas
	primaryTurnHost := fmt.Sprintf("coturn-%d.coturn.default.svc.cluster.local", turnIndex)

	iceServers := []pionwebrtc.ICEServer{{URLs: []string{fmt.Sprintf("stun:%s:3478", primaryTurnHost)}}}
	iceServers = append(iceServers,
		pionwebrtc.ICEServer{URLs: []string{fmt.Sprintf("turn:%s:3478", primaryTurnHost)}, Username: turnUsername, Credential: turnPassword},
		pionwebrtc.ICEServer{URLs: []string{fmt.Sprintf("turn:%s:3478?transport=tcp", primaryTurnHost)}, Username: turnUsername, Credential: turnPassword},
	)

	for i := 1; i < turnReplicas; i++ {
		fallbackIndex := (turnIndex + i) % turnReplicas
		fallbackHost := fmt.Sprintf("coturn-%d.coturn.default.svc.cluster.local", fallbackIndex)
		iceServers = append(iceServers, pionwebrtc.ICEServer{URLs: []string{fmt.Sprintf("turn:%s:3478", fallbackHost)}, Username: turnUsername, Credential: turnPassword})
	}

	turnHost := os.Getenv("TURN_HOST")
	if turnHost == "" {
		turnHost = "coturn-lb"
	}
	iceServers = append(iceServers,
		pionwebrtc.ICEServer{URLs: []string{fmt.Sprintf("stun:%s:3478", turnHost)}},
		pionwebrtc.ICEServer{URLs: []string{fmt.Sprintf("turn:%s:3478", turnHost)}, Username: turnUsername, Credential: turnPassword},
		pionwebrtc.ICEServer{URLs: []string{fmt.Sprintf("turn:%s:3478?transport=tcp", turnHost)}, Username: turnUsername, Credential: turnPassword},
	)

	log.Printf("[WebRTC] Participant %d: Using primary TURN server %s (index %d of %d)", id, primaryTurnHost, turnIndex, turnReplicas)
	return iceServers
}

func setupConnectionCallbacks(pc *pionwebrtc.PeerConnection, id uint32) {
	pc.OnConnectionStateChange(func(state pionwebrtc.PeerConnectionState) {
		log.Printf("[WebRTC] Participant %d connection state: %s", id, state.String())
	})
	pc.OnICEConnectionStateChange(func(state pionwebrtc.ICEConnectionState) {
		log.Printf("[WebRTC] Participant %d ICE state: %s", id, state.String())
		if state == pionwebrtc.ICEConnectionStateFailed {
			log.Printf("[WebRTC] Participant %d: ICE connection failed. This may be due to NAT traversal issues.", id)
		}
	})
	pc.OnICECandidate(func(candidate *pionwebrtc.ICECandidate) {
		if candidate != nil {
			log.Printf("[WebRTC] Participant %d: Generated ICE candidate [%s]: %s", id, candidate.Typ.String(), candidate.String())
		} else {
			log.Printf("[WebRTC] Participant %d: ICE candidate gathering complete", id)
		}
	})
}
