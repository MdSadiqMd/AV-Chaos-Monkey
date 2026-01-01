package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
)

var architectures = []string{
	"x86_64-linux",
	"aarch64-linux",
	"x86_64-darwin",
	"aarch64-darwin",
}

func main() {
	var (
		list = flag.Bool("list", false, "List available architectures")
		help = flag.Bool("help", false, "Show help message")
	)
	flag.Parse()

	if *help {
		showUsage()
		os.Exit(0)
	}

	if *list {
		listArchitectures()
		os.Exit(0)
	}

	targetArch := ""
	if flag.NArg() > 0 {
		targetArch = flag.Arg(0)
	} else {
		showUsage()
		os.Exit(1)
	}

	if targetArch == "all" {
		buildAll()
	} else if isValidArch(targetArch) {
		buildForArch(targetArch)
	} else {
		fmt.Fprintf(os.Stderr, "❌ Error: Invalid architecture '%s'\n\n", targetArch)
		listArchitectures()
		os.Exit(1)
	}
}

func showUsage() {
	fmt.Println("Usage: build [ARCHITECTURE|all|--list|--help]")
	fmt.Println()
	fmt.Println("Build AV Chaos Monkey for a specific architecture.")
	fmt.Println()
	fmt.Println("Arguments:")
	fmt.Println("  ARCHITECTURE    Build for the specified architecture (e.g., x86_64-linux)")
	fmt.Println("  all             Build for all architectures (default behavior)")
	fmt.Println("  --list          List all available architectures")
	fmt.Println("  --help          Show this help message")
	fmt.Println()
	fmt.Println("Examples:")
	fmt.Println("  build x86_64-linux          # Build for Linux x86_64")
	fmt.Println("  build aarch64-darwin        # Build for macOS Apple Silicon")
	fmt.Println("  build all                   # Build for all architectures")
	fmt.Println("  build --list                # List available architectures")
	fmt.Println()
	fmt.Println("Available architectures:")
	for _, arch := range architectures {
		var desc string
		switch arch {
		case "x86_64-linux":
			desc = "Linux (Intel/AMD 64-bit)"
		case "aarch64-linux":
			desc = "Linux ARM64 (Raspberry Pi, AWS Graviton)"
		case "x86_64-darwin":
			desc = "macOS Intel"
		case "aarch64-darwin":
			desc = "macOS Apple Silicon (M1/M2/M3)"
		}
		fmt.Printf("  - %-20s %s\n", arch, desc)
	}
	fmt.Println()
	fmt.Println("Note: Cross-compilation from macOS to Linux requires remote builders.")
}

func listArchitectures() {
	fmt.Println("Available architectures:")
	for _, arch := range architectures {
		fmt.Printf("  - %s\n", arch)
	}
}

func isValidArch(arch string) bool {
	return slices.Contains(architectures, arch)
}

func buildAll() {
	fmt.Println("🔨 Building AV Chaos Monkey for all architectures...")
	fmt.Println()
	fmt.Println("ℹ️  Note: Cross-compilation from macOS to Linux requires remote builders.")
	fmt.Println("   Building for your current system will work, others may require setup.")
	fmt.Println()

	successCount := 0
	var failedArchs []string

	for _, arch := range architectures {
		if buildForArch(arch) == nil {
			successCount++
		} else {
			failedArchs = append(failedArchs, arch)
		}
		fmt.Println()
	}

	fmt.Println("🎉 Build summary:")
	fmt.Printf("   ✅ Successful: %d/%d\n", successCount, len(architectures))
	if len(failedArchs) > 0 {
		fmt.Printf("   ❌ Failed: %s\n", strings.Join(failedArchs, ", "))
	}
}

func buildForArch(arch string) error {
	outLink := fmt.Sprintf("result-%s", arch)

	fmt.Printf("📦 Building for %s...\n", arch)

	// Get current system
	cmd := exec.Command("nix", "eval", "--impure", "--expr", "builtins.currentSystem", "--raw")
	currentSystem, _ := cmd.Output()
	currentSystemStr := strings.TrimSpace(string(currentSystem))

	// Check if this is cross-compilation
	if strings.Contains(currentSystemStr, "darwin") && strings.Contains(arch, "linux") {
		fmt.Println("   ⚠️  Cross-compilation from macOS to Linux - may require remote builders")
	} else if strings.Contains(currentSystemStr, "linux") && strings.Contains(arch, "darwin") {
		fmt.Println("   ⚠️  Cross-compilation from Linux to macOS - may require remote builders")
	}

	// Build
	cmd = exec.Command("nix", "build", fmt.Sprintf(".#packages.%s.av-chaos-monkey", arch), "--out-link", outLink)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Printf("❌ Failed to build for %s\n", arch)

		if strings.Contains(currentSystemStr, "darwin") && strings.Contains(arch, "linux") {
			fmt.Println("   💡 Tip: Set up Nix remote builders or build on a Linux machine")
			fmt.Println("   💡 See CROSS_COMPILATION.md for more information")
		} else if strings.Contains(currentSystemStr, "linux") && strings.Contains(arch, "darwin") {
			fmt.Println("   💡 Tip: Set up Nix remote builders or build on a macOS machine")
			fmt.Println("   💡 See CROSS_COMPILATION.md for more information")
		}
		return err
	}
	fmt.Printf("✅ Built successfully: %s\n", outLink)

	// Show binary info
	if link, err := os.Readlink(outLink); err == nil {
		binaryPath := link + "/bin/main"
		if info, err := os.Stat(binaryPath); err == nil && !info.IsDir() {
			fmt.Printf("   📄 Binary: %s\n", binaryPath)
			cmd = exec.Command("file", binaryPath)
			if output, err := cmd.Output(); err == nil {
				fmt.Printf("   📋 Info: %s\n", strings.TrimSpace(string(output)))
			}
		}
	}

	return nil
}
