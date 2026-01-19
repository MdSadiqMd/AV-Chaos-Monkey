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
	header.Version = (data[0] >> 6) & 0x03
	padding := (data[0] >> 5) & 0x01
	extension := (data[0] >> 4) & 0x01
	csrcCount := data[0] & 0x0F
	header.Marker = ((data[1] >> 7) & 0x01) != 0
	header.PayloadType = data[1] & 0x7F
	header.Sequence = binary.BigEndian.Uint16(data[2:4])
	header.Timestamp = binary.BigEndian.Uint32(data[4:8])
	header.SSRC = binary.BigEndian.Uint32(data[8:12])
	offset := 12 + int(csrcCount)*4
	if extension != 0 {
		if len(data) < offset+4 {
			return nil, fmt.Errorf("extension header incomplete")
		}
		extID := binary.BigEndian.Uint16(data[offset : offset+2])
		extLen := binary.BigEndian.Uint16(data[offset+2:offset+4]) * 4
		if extID == 1 && extLen == 4 && len(data) >= offset+8 {
			header.ParticipantID = binary.LittleEndian.Uint32(data[offset+4 : offset+8])
		}
		offset += 4 + int(extLen)
	}
	if padding != 0 && len(data) > 0 {
		paddingLen := int(data[len(data)-1])
		header.Payload = data[offset : len(data)-paddingLen]
	} else {
		header.Payload = data[offset:]
	}
	return header, nil
}

type PacketStats struct {
	VideoPackets, AudioPackets, OtherPackets int
	TotalBytes, VideoBytes, AudioBytes       int64
	NALTypes                                 map[byte]int
	UniqueSSRCs                              map[uint32]bool
	ParticipantPackets                       map[uint32]int
	StartTime                                time.Time
}

func main() {
	port := "5000"
	if len(os.Args) > 1 {
		port = os.Args[1]
	}
	addr, _ := net.ResolveUDPAddr("udp", "0.0.0.0:"+port)
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		log.Printf("Port in use. Kill with: lsof -i :%s | awk 'NR>1{print $2}' | xargs kill -9", port)
		os.Exit(1)
	}
	defer conn.Close()
	log.Printf("Listening for RTP packets on UDP port 0.0.0.0:%s", port)
	log.Println("Press Ctrl+C to stop")

	stats := &PacketStats{
		NALTypes: make(map[byte]int), UniqueSSRCs: make(map[uint32]bool),
		ParticipantPackets: make(map[uint32]int), StartTime: time.Now(),
	}
	packetCount := 0
	buffer := make([]byte, 4096)

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() { <-sigChan; printStats(stats, packetCount); os.Exit(0) }()

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
			continue
		}

		stats.UniqueSSRCs[header.SSRC] = true
		if header.ParticipantID > 0 {
			stats.ParticipantPackets[header.ParticipantID]++
		}

		switch header.PayloadType {
		case 96: // H.264 video
			stats.VideoPackets++
			stats.VideoBytes += int64(len(header.Payload))
			if len(header.Payload) > 0 {
				stats.NALTypes[header.Payload[0]&0x1F]++
			}
		case 111: // Opus audio
			stats.AudioPackets++
			stats.AudioBytes += int64(len(header.Payload))
		default:
			stats.OtherPackets++
		}

		if packetCount%100 == 0 {
			log.Printf("Packet #%d from %s:", packetCount, clientAddr)
			log.Printf("  Participant ID: %d", header.ParticipantID)
			log.Printf("  Payload Type: %d", header.PayloadType)
			log.Printf("  Sequence: %d", header.Sequence)
			log.Printf("  Timestamp: %d", header.Timestamp)
			log.Printf("  SSRC: %d", header.SSRC)
			log.Printf("  Payload Size: %d bytes", len(header.Payload))
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
		log.Printf("  Other: %d packets", stats.OtherPackets)
	}
	log.Println()
	log.Printf("Unique Streams (SSRCs): %d", len(stats.UniqueSSRCs))
	log.Printf("Unique Participants: %d", len(stats.ParticipantPackets))
	log.Println("═══════════════════════════════════════════════════════════")
}
