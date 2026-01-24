package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/MdSadiqMd/AV-Chaos-Monkey/pkg/config"
	"github.com/MdSadiqMd/AV-Chaos-Monkey/pkg/utils"
)

func main() {
	var (
		numParticipants = flag.Int("participants", 250, "Number of participants")
		testDuration    = flag.Int("duration", 600, "Test duration in seconds")
		skipTest        = flag.Bool("skip-test", false, "Skip running the test")
		autoDetect      = flag.Bool("auto-detect", true, "Auto-detect optimal participant count")
		useK8s          = flag.Bool("k8s", false, "Use Kubernetes mode")
	)
	flag.Parse()

	if *useK8s {
		projectRoot := config.GetProjectRoot()
		cmd := exec.Command("go", "run", filepath.Join(projectRoot, "tools", "k8s-start", "main.go"), "--setup")
		cmd.Args = append(cmd.Args, flag.Args()...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		cmd.Run()
		return
	}

	if err := runDockerMode(*numParticipants, *testDuration, *skipTest, *autoDetect); err != nil {
		fmt.Fprintf(os.Stderr, "❌ Error: %v\n", err)
		os.Exit(1)
	}
}

func runDockerMode(numParticipants, testDuration int, skipTest, autoDetect bool) error {
	if err := utils.EnsureDockerRunning(); err != nil {
		return fmt.Errorf("Docker check failed: %w", err)
	}

	if autoDetect {
		dockerMem := utils.DetectDockerMemory()
		safeCount := calculateSafeParticipants(dockerMem)
		if numParticipants == 250 { // Only override default
			numParticipants = safeCount
			fmt.Printf("✅ Auto-detected participants: %d\n", numParticipants)
		}
	}

	fmt.Println("Configuration:")
	fmt.Printf("  Participants: %d\n", numParticipants)
	fmt.Printf("  Duration: %ds\n", testDuration)
	fmt.Printf("  Skip Test: %v\n", skipTest)
	fmt.Println()

	fmt.Println("Step 1: Checking Docker...")
	if !utils.CommandExists("docker") {
		return fmt.Errorf("Docker is not installed")
	}

	fmt.Println("Step 2: Checking for port conflicts...")
	if err := checkPortConflict(); err != nil {
		fmt.Printf("⚠️  %v\n", err)
	}

	fmt.Println("Step 3: Building orchestrator...")
	projectRoot := config.GetProjectRoot()
	cmd := exec.Command("docker-compose", "--profile", "monitoring", "build", "--no-cache", "orchestrator")
	cmd.Dir = projectRoot
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to build orchestrator: %w", err)
	}

	fmt.Println("Step 4: Starting Docker services...")
	cmd = exec.Command("docker-compose", "--profile", "monitoring", "up", "-d", "orchestrator")
	cmd.Dir = projectRoot
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("failed to start orchestrator: %w", err)
	}

	fmt.Println("Waiting for orchestrator to be healthy...")
	if err := waitForHealth("http://localhost:8080/healthz", 15); err != nil {
		return fmt.Errorf("orchestrator failed to become healthy: %w", err)
	}
	fmt.Println("✓ Orchestrator is healthy!")

	fmt.Println("Starting Prometheus and Grafana...")
	cmd = exec.Command("docker-compose", "--profile", "monitoring", "up", "-d", "prometheus", "grafana")
	cmd.Dir = projectRoot
	cmd.Run() // Optional, don't fail if this fails

	os.Setenv("NUM_PARTICIPANTS", strconv.Itoa(numParticipants))
	os.Setenv("TEST_DURATION_SECONDS", strconv.Itoa(testDuration))

	fmt.Println()
	fmt.Println("Service URLs:")
	fmt.Println("  Orchestrator: http://localhost:8080")
	fmt.Println("  Prometheus:   http://localhost:9091")
	fmt.Println("  Grafana:      http://localhost:3000 (admin/admin)")
	fmt.Println()

	if !skipTest {
		fmt.Println("Step 5: Starting chaos test...")
		scriptDir := getScriptDir()
		chaosTestPath := filepath.Join(scriptDir, "..", "..", "tools", "chaos-test", "main")
		cmd := exec.Command(chaosTestPath)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		cmd.Run()
	} else {
		fmt.Println("✓ All services ready - test skipped")
		fmt.Printf("To run the test manually: NUM_PARTICIPANTS=%d TEST_DURATION_SECONDS=%d ./tools/chaos-test/main\n",
			numParticipants, testDuration)
	}

	return nil
}

func calculateSafeParticipants(dockerMemGB int) int {
	if dockerMemGB >= 40 {
		return 500
	} else if dockerMemGB >= 32 {
		return 350
	} else if dockerMemGB >= 24 {
		return 250
	} else if dockerMemGB >= 16 {
		return 150
	} else if dockerMemGB >= 8 {
		return 50
	} else if dockerMemGB >= 4 {
		return 10
	}
	return 10
}

func checkPortConflict() error {
	cmd := exec.Command("lsof", "-i", ":8080")
	output, err := cmd.Output()
	if err != nil {
		return nil // No conflict
	}

	lines := strings.SplitSeq(string(output), "\n")
	for line := range lines {
		if strings.Contains(line, "LISTEN") {
			if !strings.Contains(line, "docker") && !strings.Contains(line, "kubectl") {
				return fmt.Errorf("Port 8080 is in use by a non-Docker/Kubernetes process")
			}
		}
	}
	return nil
}

func waitForHealth(url string, maxRetries int) error {
	for i := range maxRetries {
		cmd := exec.Command("curl", "-sf", url)
		cmd.Stdout = nil
		cmd.Stderr = nil
		if cmd.Run() == nil {
			return nil
		}
		if i < maxRetries-1 {
			fmt.Print(".")
		}
		exec.Command("sleep", "2").Run()
	}
	fmt.Println()
	return fmt.Errorf("health check failed after %d attempts", maxRetries)
}

func getScriptDir() string {
	execPath, _ := os.Executable()
	return filepath.Dir(execPath)
}
