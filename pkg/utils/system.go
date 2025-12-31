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
	// Memory per pod: 2GB (matches StatefulSet resource limits)
	// Reserve 2GB for Prometheus + Grafana + system
	availableMem := dockerMemGB - 2

	if availableMem >= 20 {
		return 10 // 20GB+ → 10 pods
	} else if availableMem >= 16 {
		return 8 // 16GB+ → 8 pods
	} else if availableMem >= 12 {
		return 6 // 12GB+ → 6 pods
	} else if availableMem >= 8 {
		return 4 // 8GB+ → 4 pods
	} else if availableMem >= 6 {
		return 3 // 6GB+ → 3 pods
	} else if availableMem >= 4 {
		return 2 // 4GB+ → 2 pods
	}
	return 1 // <4GB → 1 pod
}

func CalculateParticipantsPerPod() int {
	return 150
}
