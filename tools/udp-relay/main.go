// Architecture:
// 1. In-cluster: udp-relay pod receives UDP on :5000, streams to TCP :5001 (length-prefixed)
// 2. kubectl port-forward: TCP 15001 -> udp-relay:5001
// 3. This tool: receives TCP on 15001, sends UDP to local receiver on configured port
//
// Testing:
//   Terminal 1: kubectl port-forward udp-relay 15001:5001
//   Terminal 2: go run ./tools/udp-relay/main.go
//   Terminal 3: go run ./examples/go/udp_receiver.go <port>

package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/MdSadiqMd/AV-Chaos-Monkey/pkg/constants"
)

func main() {
	var (
		tcpAddr   = flag.String("tcp-addr", constants.UDPRelayTCPAddr, "TCP address to connect to (kubectl port-forward)")
		udpTarget = flag.String("udp-target", constants.UDPRelayUDPAddr, "UDP target address (your local receiver)")
		reconnect = flag.Bool("reconnect", true, "Auto-reconnect on disconnect")
	)
	flag.Parse()

	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)
	log.Printf("UDP Relay (TCP->UDP) starting...")
	log.Printf("  TCP source: %s", *tcpAddr)
	log.Printf("  UDP target: %s", *udpTarget)
	log.Println()

	udpAddr, err := net.ResolveUDPAddr("udp", *udpTarget)
	if err != nil {
		log.Fatalf("Failed to resolve UDP target: %v", err)
	}

	// Create UDP socket for sending
	udpConn, err := net.DialUDP("udp", nil, udpAddr)
	if err != nil {
		log.Fatalf("Failed to create UDP socket: %v", err)
	}
	defer udpConn.Close()

	// Stats
	var packetCount int64
	var byteCount int64
	var errorCount int64

	go func() {
		ticker := time.NewTicker(5 * time.Second)
		for range ticker.C {
			p := atomic.LoadInt64(&packetCount)
			b := atomic.LoadInt64(&byteCount)
			e := atomic.LoadInt64(&errorCount)
			if p > 0 || e > 0 {
				log.Printf("📊 Stats: %d packets, %d bytes forwarded, %d errors", p, b, e)
			}
		}
	}()

	// Handle shutdown
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigChan
		log.Printf("\n=== Summary ===")
		log.Printf("Total: %d packets, %d bytes, %d errors",
			atomic.LoadInt64(&packetCount),
			atomic.LoadInt64(&byteCount),
			atomic.LoadInt64(&errorCount))
		os.Exit(0)
	}()

	// Connect and relay loop
	for {
		err := relayFromTCP(*tcpAddr, udpConn, &packetCount, &byteCount, &errorCount)
		if err != nil {
			log.Printf("Connection error: %v", err)
		}

		if !*reconnect {
			break
		}

		log.Printf("Reconnecting in 2 seconds...")
		time.Sleep(2 * time.Second)
	}
}

func relayFromTCP(tcpAddr string, udpConn *net.UDPConn, packetCount, byteCount, errorCount *int64) error {
	log.Printf("Connecting to TCP %s...", tcpAddr)

	conn, err := net.DialTimeout("tcp", tcpAddr, 10*time.Second)
	if err != nil {
		return fmt.Errorf("failed to connect: %w", err)
	}
	defer conn.Close()

	log.Printf("✅ Connected to TCP %s", tcpAddr)
	log.Printf("Waiting for UDP packets from Kubernetes...")

	// Read length-prefixed packets
	// Format: 2-byte big-endian length + payload
	lenBuf := make([]byte, 2)
	dataBuf := make([]byte, 65536)

	for {
		// Read length header
		_, err := io.ReadFull(conn, lenBuf)
		if err != nil {
			if err == io.EOF {
				return fmt.Errorf("connection closed by server")
			}
			return fmt.Errorf("failed to read length: %w", err)
		}

		length := binary.BigEndian.Uint16(lenBuf)
		if length == 0 {
			atomic.AddInt64(errorCount, 1)
			log.Printf("Invalid packet length: %d", length)
			continue
		}

		// Read payload
		_, err = io.ReadFull(conn, dataBuf[:length])
		if err != nil {
			return fmt.Errorf("failed to read payload: %w", err)
		}

		// Forward to UDP
		_, err = udpConn.Write(dataBuf[:length])
		if err != nil {
			atomic.AddInt64(errorCount, 1)
			log.Printf("UDP write error: %v", err)
			continue
		}

		atomic.AddInt64(packetCount, 1)
		atomic.AddInt64(byteCount, int64(length))

		// Log first few packets
		count := atomic.LoadInt64(packetCount)
		if count <= 5 || count%1000 == 0 {
			log.Printf("📦 Packet #%d: %d bytes -> UDP", count, length)
		}
	}
}
