package main

import (
	"encoding/binary"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"
)

func init() {
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)
}

// RTPHeader represents parsed RTP packet header
type RTPHeader struct {
	Version       uint8
	PayloadType   uint8
	Sequence      uint16
	Timestamp     uint32
	SSRC          uint32
	Marker        bool
	ParticipantID uint32
	Payload       []byte
}

func parseRTPPacket(data []byte) (*RTPHeader, error) {
	if len(data) < 12 {
		return nil, fmt.Errorf("packet too short")
	}

	header := &RTPHeader{}

	// Parse first byte
	header.Version = (data[0] >> 6) & 0x03
	padding := (data[0] >> 5) & 0x01
	extension := (data[0] >> 4) & 0x01
	csrcCount := data[0] & 0x0F

	// Parse second byte
	header.Marker = ((data[1] >> 7) & 0x01) != 0
	header.PayloadType = data[1] & 0x7F

	// Parse sequence number and timestamp
	header.Sequence = binary.BigEndian.Uint16(data[2:4])
	header.Timestamp = binary.BigEndian.Uint32(data[4:8])
	header.SSRC = binary.BigEndian.Uint32(data[8:12])

	// Skip CSRCs
	offset := 12 + int(csrcCount)*4

	// Parse extension if present
	if extension != 0 {
		if len(data) < offset+4 {
			return nil, fmt.Errorf("extension header incomplete")
		}

		extID := binary.BigEndian.Uint16(data[offset : offset+2])
		extLen := binary.BigEndian.Uint16(data[offset+2:offset+4]) * 4

		if extID == 1 && extLen == 4 { // Participant ID extension
			if len(data) >= offset+8 {
				header.ParticipantID = binary.LittleEndian.Uint32(data[offset+4 : offset+8])
			}
		}

		offset += 4 + int(extLen)
	}

	// Extract payload
	if padding != 0 && len(data) > 0 {
		paddingLen := int(data[len(data)-1])
		header.Payload = data[offset : len(data)-paddingLen]
	} else {
		header.Payload = data[offset:]
	}

	return header, nil
}

func main() {
	// Get port from command line or use default
	port := "5000"
	if len(os.Args) > 1 {
		port = os.Args[1]
	}

	// Create UDP listener on all interfaces (0.0.0.0)
	addr, err := net.ResolveUDPAddr("udp", "0.0.0.0:"+port)
	if err != nil {
		log.Fatalf("Failed to resolve address: %v", err)
	}

	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		log.Printf("Find and kill the process using the port:")
		log.Printf("   lsof -i :%s | grep -v COMMAND | awk '{print $2}' | xargs kill -9", port)
		os.Exit(1)
	}
	defer conn.Close()

	log.Printf("Listening for RTP packets on UDP port 0.0.0.0:%s", port)
	log.Println("Press Ctrl+C to stop")

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	packetCount := 0
	buffer := make([]byte, 4096)

	go func() {
		<-sigChan
		log.Printf("\nReceived %d packets total", packetCount)
		os.Exit(0)
	}()

	for {
		n, clientAddr, err := conn.ReadFromUDP(buffer)
		if err != nil {
			log.Printf("Error reading UDP: %v", err)
			continue
		}

		packetCount++
		header, err := parseRTPPacket(buffer[:n])
		if err != nil {
			log.Printf("Error parsing RTP packet: %v", err)
			continue
		}

		// Print packet info every 100 packets
		if packetCount%100 == 0 {
			log.Printf("Packet #%d from %s:", packetCount, clientAddr)
			log.Printf("  Participant ID: %d", header.ParticipantID)
			log.Printf("  Payload Type: %d (96=H.264 video, 111=Opus audio)", header.PayloadType)
			log.Printf("  Sequence: %d", header.Sequence)
			log.Printf("  Timestamp: %d", header.Timestamp)
			log.Printf("  SSRC: %d", header.SSRC)
			log.Printf("  Payload Size: %d bytes", len(header.Payload))
			log.Println()
		}
	}
}
