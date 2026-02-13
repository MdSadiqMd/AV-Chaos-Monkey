package utils

import (
	"os/exec"
	"runtime"
	"strconv"
	"strings"
)

func DetectSystemMemory() (int, error) {
	if runtime.GOOS == "darwin" {
		cmd := exec.Command("sysctl", "-n", "hw.memsize")
		output, err := cmd.Output()
		if err != nil {
			return 0, err
		}
		memBytes, err := strconv.ParseInt(strings.TrimSpace(string(output)), 10, 64)
		if err != nil {
			return 0, err
		}
		return int(memBytes / 1024 / 1024 / 1024), nil
	}

	cmd := exec.Command("grep", "MemTotal", "/proc/meminfo")
	output, err := cmd.Output()
	if err != nil {
		return 0, err
	}
	parts := strings.Fields(string(output))
	if len(parts) < 2 {
		return 0, nil
	}
	memKB, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, err
	}
	return int(memKB / 1024 / 1024), nil
}

func DetectDockerMemory() int {
	cmd := exec.Command("docker", "info")
	output, err := cmd.Output()
	if err != nil {
		return 8
	}

	lines := strings.SplitSeq(string(output), "\n")
	for line := range lines {
		if strings.Contains(line, "Total Memory") {
			parts := strings.Fields(line)
			for i, part := range parts {
				if strings.HasSuffix(part, "GiB") {
					memStr := strings.TrimSuffix(part, "GiB")
					if mem, err := strconv.ParseFloat(memStr, 64); err == nil {
						return int(mem)
					}
				}
				if i > 0 && strings.Contains(part, "GiB") {
					prev := parts[i-1]
					if mem, err := strconv.ParseFloat(prev, 64); err == nil {
						return int(mem)
					}
				}
			}
		}
	}
	return 8
}

func DetectCPUCores() int {
	if runtime.GOOS == "darwin" {
		cmd := exec.Command("sysctl", "-n", "hw.ncpu")
		output, err := cmd.Output()
		if err != nil {
			return 4
		}
		cores, err := strconv.Atoi(strings.TrimSpace(string(output)))
		if err != nil {
			return 4
		}
		return cores
	}

	cmd := exec.Command("nproc")
	output, err := cmd.Output()
	if err != nil {
		return 4
	}
	cores, err := strconv.Atoi(strings.TrimSpace(string(output)))
	if err != nil {
		return 4
	}
	return cores
}

func CalculateOptimalReplicas(dockerMemGB int) int {
	// Calculate based on available memory after reserving for monitoring
	// Monitoring (Prometheus + Grafana + Coturn) needs ~1-2GB depending on config
	// Each orchestrator pod needs 256Mi-2Gi depending on Docker memory
	if dockerMemGB >= 16 {
		return 10 // High memory: 10 pods with 2GB each
	} else if dockerMemGB >= 12 {
		return 8 // 12GB+: 8 pods
	} else if dockerMemGB >= 8 {
		return 6 // 8GB+: 6 pods with 1GB each
	} else if dockerMemGB >= 6 {
		return 4 // 6GB+: 4 pods
	} else if dockerMemGB >= 4 {
		return 2 // 4GB+: 2 pods with 512Mi each
	}
	return 1 // <4GB: 1 pod with minimal resources
}

func CalculateParticipantsPerPod() int {
	return 150
}
