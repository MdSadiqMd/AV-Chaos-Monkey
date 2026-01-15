package main

import (
	"encoding/binary"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"
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

type PacketStats struct {
	VideoPackets int
	AudioPackets int
	OtherPackets int
	TotalBytes   int64
	VideoBytes   int64
	AudioBytes   int64
	NALTypes     map[byte]int
	UniqueSSRCs  map[uint32]bool
	StartTime    time.Time
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

	// Initialize stats
	stats := &PacketStats{
		NALTypes:    make(map[byte]int),
		UniqueSSRCs: make(map[uint32]bool),
		StartTime:   time.Now(),
	}

	go func() {
		<-sigChan
		printStats(stats, packetCount)
		os.Exit(0)
	}()

	for {
		n, clientAddr, err := conn.ReadFromUDP(buffer)
		if err != nil {
			log.Printf("Error reading UDP: %v", err)
			continue
		}

		packetCount++
		stats.TotalBytes += int64(n)

		header, err := parseRTPPacket(buffer[:n])
		if err != nil {
			log.Printf("Error parsing RTP packet: %v", err)
			continue
		}

		// Track unique SSRCs (participants)
		stats.UniqueSSRCs[header.SSRC] = true

		// Track packet types
		switch header.PayloadType {
		case 96: // H.264 video
			stats.VideoPackets++
			stats.VideoBytes += int64(len(header.Payload))
			if len(header.Payload) > 0 {
				nalType := header.Payload[0] & 0x1F
				stats.NALTypes[nalType]++
			}
		case 111: // Opus audio
			stats.AudioPackets++
			stats.AudioBytes += int64(len(header.Payload))
		default:
			stats.OtherPackets++
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

			// For H.264 video, show NAL unit type
			if header.PayloadType == 96 && len(header.Payload) > 0 {
				nalType := header.Payload[0] & 0x1F
				nalTypeName := getNALTypeName(nalType)
				log.Printf("  NAL Type: %d (%s)", nalType, nalTypeName)

				// Show first few bytes of payload for verification
				previewLen := min(16, len(header.Payload))
				log.Printf("  Payload Preview: % x", header.Payload[:previewLen])
			}

			// For Opus audio, show packet info
			if header.PayloadType == 111 && len(header.Payload) > 0 {
				// Opus TOC byte analysis
				tocByte := header.Payload[0]
				config := (tocByte >> 3) & 0x1F
				stereo := (tocByte >> 2) & 0x01
				frameCount := tocByte & 0x03
				log.Printf("  Opus Config: %d, Stereo: %d, Frames: %d", config, stereo, frameCount)

				previewLen := min(16, len(header.Payload))
				log.Printf("  Payload Preview: % x", header.Payload[:previewLen])
			}
			log.Println()
		}
	}
}

func printStats(stats *PacketStats, totalPackets int) {
	elapsed := time.Since(stats.StartTime)
	log.Println()
	log.Println("═══════════════════════════════════════════════════════════")
	log.Println("                    PACKET STATISTICS                       ")
	log.Println("═══════════════════════════════════════════════════════════")
	log.Printf("Duration: %v", elapsed.Round(time.Second))
	log.Printf("Total Packets: %d (%.1f pkt/s)", totalPackets, float64(totalPackets)/elapsed.Seconds())
	log.Printf("Total Bytes: %.2f MB (%.2f Mbps)", float64(stats.TotalBytes)/1024/1024, float64(stats.TotalBytes)*8/elapsed.Seconds()/1000000)
	log.Println()
	log.Println("Media Type Breakdown:")
	log.Printf("  Video (H.264): %d packets (%.1f%%)", stats.VideoPackets, float64(stats.VideoPackets)*100/float64(totalPackets))
	log.Printf("  Audio (Opus):  %d packets (%.1f%%)", stats.AudioPackets, float64(stats.AudioPackets)*100/float64(totalPackets))
	if stats.OtherPackets > 0 {
		log.Printf("  Other:         %d packets", stats.OtherPackets)
	}
	log.Println()
	log.Printf("Unique Streams (SSRCs): %d", len(stats.UniqueSSRCs))
	log.Println()
	log.Println("H.264 NAL Unit Types:")
	for nalType, count := range stats.NALTypes {
		log.Printf("  %s (type %d): %d packets", getNALTypeName(nalType), nalType, count)
	}
	log.Println("═══════════════════════════════════════════════════════════")
}

// Returns a human-readable name for H.264 NAL unit types
func getNALTypeName(nalType byte) string {
	switch nalType {
	case 1:
		return "Non-IDR Slice (P-frame)"
	case 5:
		return "IDR Slice (Keyframe)"
	case 6:
		return "SEI"
	case 7:
		return "SPS"
	case 8:
		return "PPS"
	case 9:
		return "Access Unit Delimiter"
	case 28:
		return "FU-A (Fragmented)"
	default:
		return "Other"
	}
}
