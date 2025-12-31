package utils

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"time"
)

func EnsureDockerRunning() error {
	cmd := exec.Command("docker", "info")
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Run(); err == nil {
		return nil
	}

	if runtime.GOOS == "darwin" {
		return ensureDockerMacOS()
	}

	return fmt.Errorf("Docker is not running. Please start Docker manually")
}

func ensureDockerMacOS() error {
	var dockerApp string
	possiblePaths := []string{
		"/Applications/Docker.app",
		"/Applications/Docker Desktop.app",
	}

	for _, path := range possiblePaths {
		if _, err := os.Stat(path); err == nil {
			dockerApp = path
			break
		}
	}

	if dockerApp == "" {
		return fmt.Errorf("Docker Desktop not found. Please install Docker Desktop")
	}

	cmd := exec.Command("pgrep", "-f", "Docker Desktop")
	cmd.Stdout = nil
	cmd.Stderr = nil
	if cmd.Run() == nil {
		fmt.Println("Docker Desktop is starting...")
	} else {
		fmt.Println("Starting Docker Desktop...")
		exec.Command("open", dockerApp).Run()
	}

	// Wait for Docker to be ready (up to 90 seconds)
	fmt.Print("Waiting for Docker to be ready")
	maxWait := 90
	waited := 0
	for waited < maxWait {
		cmd := exec.Command("docker", "info")
		cmd.Stdout = nil
		cmd.Stderr = nil
		if cmd.Run() == nil {
			fmt.Println()
			return nil
		}
		if waited%10 == 0 {
			fmt.Print(".")
		}
		time.Sleep(1 * time.Second)
		waited++
	}
	fmt.Println()
	fmt.Println("⚠️  Docker is taking longer than expected to start")
	fmt.Println("   This might be the first launch - please check Docker Desktop window")

	// Final check
	cmd = exec.Command("docker", "info")
	cmd.Stdout = nil
	cmd.Stderr = nil
	if cmd.Run() == nil {
		return nil
	}

	return fmt.Errorf("Docker is not running. Please start Docker Desktop manually, then run the command again")
}
