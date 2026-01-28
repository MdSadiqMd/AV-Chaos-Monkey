package k8s

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os/exec"
	"sync/atomic"
	"time"

	"github.com/MdSadiqMd/AV-Chaos-Monkey/pkg/constants"
	"github.com/MdSadiqMd/AV-Chaos-Monkey/pkg/logging"
	"github.com/MdSadiqMd/AV-Chaos-Monkey/pkg/utils"
)

func SetupPortForwarding() {
	logging.LogInfo("Setting up port forwarding...")

	exec.Command("pkill", "-9", "-f", "kubectl port-forward").Run()
	exec.Command("pkill", "-9", "-f", "udp-relay-binary").Run()
	time.Sleep(2 * time.Second)

	kubectlCmd, _ := utils.FindCommand("kubectl")

	services := []struct {
		service, localPort, remotePort string
	}{
		{"orchestrator", "8080", "8080"},
		{"prometheus", "9091", "9090"},
		{"grafana", "3000", "3000"},
	}

	for _, svc := range services {
		cmd := exec.Command(kubectlCmd, "port-forward", fmt.Sprintf("svc/%s", svc.service), fmt.Sprintf("%s:%s", svc.localPort, svc.remotePort))
		cmd.Start()
		time.Sleep(1 * time.Second)
	}

	SetupUDPRelayChain(kubectlCmd)

	logging.LogSuccess("Port forwarding active")
	fmt.Printf("  Orchestrator: %shttp://localhost:8080%s\n", utils.CYAN, utils.NC)
	fmt.Printf("  Prometheus:   %shttp://localhost:9091%s\n", utils.CYAN, utils.NC)
	fmt.Printf("  Grafana:      %shttp://localhost:3000%s (admin/admin)\n", utils.CYAN, utils.NC)
	fmt.Printf("  UDP Receiver: %sUDP localhost:%d%s (run: go run ./examples/go/udp_receiver.go %d)\n", utils.CYAN, constants.DefaultTargetPort, utils.NC, constants.DefaultTargetPort)
}

func SetupUDPRelayChain(kubectlCmd string) {
	logging.LogInfo("Setting up UDP relay chain...")

	waitCmd := exec.Command(kubectlCmd, "wait", "--for=condition=ready", "pod/udp-relay", "--timeout=30s")
	if err := waitCmd.Run(); err != nil {
		logging.LogWarning("UDP relay pod not ready, skipping UDP relay chain: %v", err)
		return
	}

	pfCmd := exec.Command(kubectlCmd, "port-forward", "pod/udp-relay", "15001:5001")
	if err := pfCmd.Start(); err != nil {
		logging.LogWarning("Failed to start UDP relay port-forward: %v", err)
		return
	}
	time.Sleep(2 * time.Second)

	go RunLocalUDPRelay()

	logging.LogSuccess("UDP relay chain active (Kubernetes -> localhost:%d)", constants.DefaultTargetPort)
}

func RunLocalUDPRelay() {
	const tcpAddr = "localhost:15001"
	udpTarget := fmt.Sprintf("localhost:%d", constants.DefaultTargetPort)

	udpAddr, err := net.ResolveUDPAddr("udp", udpTarget)
	if err != nil {
		logging.LogWarning("UDP relay: failed to resolve UDP target: %v", err)
		return
	}

	localAddr, err := net.ResolveUDPAddr("udp", fmt.Sprintf("%s:0", constants.DefaultTargetHost))
	if err != nil {
		logging.LogWarning("UDP relay: failed to resolve local address: %v", err)
		return
	}
	udpConn, err := net.ListenUDP("udp", localAddr)
	if err != nil {
		logging.LogWarning("UDP relay: failed to create UDP socket: %v", err)
		return
	}
	defer udpConn.Close()

	var packetCount int64
	var lastLogTime time.Time

	for {
		err := relayTCPToUDP(tcpAddr, udpConn, udpAddr, &packetCount, &lastLogTime)
		if err != nil {
			if time.Since(lastLogTime) > 30*time.Second {
				logging.LogInfo("UDP relay: reconnecting... (%v)", err)
				lastLogTime = time.Now()
			}
		}
		time.Sleep(2 * time.Second)
	}
}

func relayTCPToUDP(tcpAddr string, udpConn *net.UDPConn, udpAddr *net.UDPAddr, packetCount *int64, lastLogTime *time.Time) error {
	conn, err := net.DialTimeout("tcp", tcpAddr, 5*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()

	logging.LogInfo("UDP relay: connected to TCP %s, forwarding to UDP", tcpAddr)

	lenBuf := make([]byte, 2)
	dataBuf := make([]byte, 65536)

	for {
		_, err := io.ReadFull(conn, lenBuf)
		if err != nil {
			return err
		}

		length := binary.BigEndian.Uint16(lenBuf)
		if length == 0 {
			continue
		}

		_, err = io.ReadFull(conn, dataBuf[:length])
		if err != nil {
			return err
		}

		_, err = udpConn.WriteToUDP(dataBuf[:length], udpAddr)
		if err != nil {
			continue
		}

		count := atomic.AddInt64(packetCount, 1)
		if count == 1 || count%10000 == 0 {
			*lastLogTime = time.Now()
		}
	}
}
