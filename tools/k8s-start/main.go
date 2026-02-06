package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/MdSadiqMd/AV-Chaos-Monkey/pkg/config"
	"github.com/MdSadiqMd/AV-Chaos-Monkey/pkg/constants"
	"github.com/MdSadiqMd/AV-Chaos-Monkey/pkg/k8s"
	"github.com/MdSadiqMd/AV-Chaos-Monkey/pkg/logging"
	"github.com/MdSadiqMd/AV-Chaos-Monkey/pkg/utils"
)

type MediaConfig struct {
	VideoPath string
	AudioPath string
	MediaPath string
}

type TestConfig struct {
	ConfigPath string
}

func main() {
	var (
		replicas        = flag.Int("replicas", 0, "Number of replicas (0 = auto-detect)")
		numParticipants = flag.Int("participants", 0, "Number of participants (0 = auto-calculate)")
		cleanup         = flag.Bool("cleanup", false, "Cleanup Kubernetes resources")
		skipTest        = flag.Bool("skip-test", false, "Skip running the test")
		setup           = flag.Bool("setup", false, "Setup Kubernetes cluster")
		clusterName     = flag.String("name", k8s.DefaultClusterName, "Kubernetes cluster name")
		udpTargetHost   = flag.String("udp-target-host", "", "UDP target host (empty = in-cluster udp-receiver)")
		udpTargetPort   = flag.Int("udp-target-port", 5000, "UDP target port")
		videoPath       = flag.String("video", "", "Path to video file (mp4) for streaming")
		audioPath       = flag.String("audio", "", "Path to audio file (mp3) for streaming")
		mediaPath       = flag.String("media", "", "Path to media file (mkv/mp4) containing both video and audio")
		configPath      = flag.String("config", "", "Path to chaos-test config JSON file")
	)
	flag.Parse()

	if *setup || (flag.NArg() > 0 && flag.Arg(0) == "setup") {
		if err := k8s.SetupCluster(*clusterName); err != nil {
			fmt.Fprintf(os.Stderr, "❌ Error: %v\n", err)
			os.Exit(1)
		}
		return
	}

	if flag.NArg() > 0 && flag.Arg(0) == "cleanup-cluster" {
		if err := k8s.CleanupCluster(*clusterName); err != nil {
			fmt.Fprintf(os.Stderr, "❌ Error: %v\n", err)
			os.Exit(1)
		}
		return
	}

	args := flag.Args()
	if len(args) > 0 && (args[0] == "cleanup" || args[0] == "--cleanup") {
		k8s.CleanupResources()
		return
	}

	if *cleanup {
		k8s.CleanupResources()
		return
	}

	if env := os.Getenv("REPLICAS"); env != "" {
		if n, err := strconv.Atoi(env); err == nil {
			replicas = &n
		}
	}
	if env := os.Getenv("NUM_PARTICIPANTS"); env != "" {
		if n, err := strconv.Atoi(env); err == nil {
			numParticipants = &n
		}
	}

	projectRoot := config.GetProjectRoot()
	mediaConfig := MediaConfig{VideoPath: *videoPath, AudioPath: *audioPath, MediaPath: *mediaPath}
	testConfig := TestConfig{ConfigPath: *configPath}

	if err := runK8sDeployment(projectRoot, *replicas, *numParticipants, *skipTest, *udpTargetHost, *udpTargetPort, mediaConfig, testConfig); err != nil {
		fmt.Fprintf(os.Stderr, "❌ Error: %v\n", err)
		os.Exit(1)
	}
}

func runK8sDeployment(projectRoot string, userReplicas, userParticipants int, skipTest bool, udpTargetHost string, udpTargetPort int, mediaConfig MediaConfig, testConfig TestConfig) error {
	if err := k8s.CheckPrerequisites(""); err != nil {
		return err
	}

	if udpTargetHost == "" {
		udpTargetHost = "udp-relay"
		logging.LogInfo("Using UDP relay (UDP -> TCP -> localhost:%d)", constants.DefaultTargetPort)
	}

	systemMem, _ := utils.DetectSystemMemory()
	dockerMem := utils.DetectDockerMemory()
	cpuCores := utils.DetectCPUCores()

	logging.LogInfo("System Detection:")
	fmt.Printf("  System RAM:    %s%dGB%s\n", utils.BOLD, systemMem, utils.NC)
	fmt.Printf("  Docker Memory: %s%dGB%s\n", utils.BOLD, dockerMem, utils.NC)
	fmt.Printf("  CPU Cores:     %s%d%s\n", utils.BOLD, cpuCores, utils.NC)
	fmt.Println()

	optimalReplicas := utils.CalculateOptimalReplicas(dockerMem)
	participantsPerPod := utils.CalculateParticipantsPerPod()

	replicas := userReplicas
	if replicas == 0 {
		replicas = optimalReplicas
		logging.LogConfig("Auto-detected optimal replicas: %d", replicas)
	} else {
		logging.LogConfig("Using user-specified replicas: %d", replicas)
	}

	participants := userParticipants
	if participants == 0 {
		participants = replicas * participantsPerPod
		logging.LogConfig("Auto-calculated participants: %d", participants)
	} else {
		logging.LogConfig("Using user-specified participants: %d", participants)
	}

	printConfiguration(replicas, participants, mediaConfig)

	if err := k8s.BuildImage(projectRoot, ""); err != nil {
		return err
	}
	if err := k8s.UpdateReplicas(projectRoot, replicas); err != nil {
		return err
	}
	if err := k8s.UpdateUDPTarget(projectRoot, udpTargetHost, udpTargetPort); err != nil {
		return err
	}
	if err := k8s.DeployK8s(projectRoot); err != nil {
		return err
	}
	if err := k8s.WaitForPods(replicas); err != nil {
		return err
	}
	if err := k8s.UpdatePrometheusTargets(replicas, projectRoot); err != nil {
		logging.LogWarning("Failed to update Prometheus targets: %v", err)
	}
	if err := k8s.GenerateTargetFiles(projectRoot); err != nil {
		logging.LogWarning("Failed to generate target files: %v", err)
	}

	k8s.VerifyPartitionIDs()
	k8s.CheckMetricsPartitions(replicas)
	k8s.SetupPortForwarding()

	if !skipTest {
		chaosTestPath := filepath.Join(projectRoot, "tools", "chaos-test")
		if testConfig.ConfigPath != "" {
			logging.LogInfo("Running chaos test with config: %s", testConfig.ConfigPath)
			cmd := exec.Command("go", "run", chaosTestPath, "-config", testConfig.ConfigPath)
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			cmd.Run()
		} else {
			logging.LogError("No config file provided. Use -config flag.")
			fmt.Println("\nUsage:")
			fmt.Printf("  ./scripts/start_everything.sh run --video=<path> --config=config/config.json\n")
			fmt.Println("\nOr run chaos-test manually:")
			fmt.Printf("  go run ./tools/chaos-test -config config/config.json\n")
			return fmt.Errorf("config file required")
		}
	}

	return nil
}

func printConfiguration(replicas, participants int, mediaConfig MediaConfig) {
	fmt.Println()
	fmt.Printf("%sFinal Configuration:%s\n", utils.BOLD, utils.NC)
	fmt.Printf("  Replicas:          %s%d pods%s\n", utils.CYAN, replicas, utils.NC)
	fmt.Printf("  Participants:      %s%d%s (~%d per pod)\n", utils.CYAN, participants, utils.NC, participants/replicas)
	fmt.Printf("  Memory per pod:    %s2GB%s\n", utils.CYAN, utils.NC)
	fmt.Printf("  Total memory:      %s%dGB%s (pods + monitoring)\n", utils.CYAN, replicas*2+2, utils.NC)
	fmt.Printf("  Throughput:        %s%dx%s\n", utils.CYAN, replicas, utils.NC)

	if mediaConfig.MediaPath != "" {
		fmt.Printf("  Media:             %s%s%s (video+audio)\n", utils.CYAN, mediaConfig.MediaPath, utils.NC)
	} else if mediaConfig.VideoPath != "" || mediaConfig.AudioPath != "" {
		if mediaConfig.VideoPath != "" {
			fmt.Printf("  Video:             %s%s%s\n", utils.CYAN, mediaConfig.VideoPath, utils.NC)
		}
		if mediaConfig.AudioPath != "" {
			fmt.Printf("  Audio:             %s%s%s\n", utils.CYAN, mediaConfig.AudioPath, utils.NC)
		}
	} else {
		fmt.Printf("  Media:             %sdefault (public/rick-roll.mp4)%s\n", utils.CYAN, utils.NC)
	}
	fmt.Println()
}

func init() {
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigChan
		exec.Command("pkill", "-f", "kubectl port-forward").Run()
		os.Exit(0)
	}()
}
