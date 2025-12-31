#!/usr/bin/env bash
# Build AV Chaos Monkey for a specific architecture or all architectures

set -e

# Available architectures
ARCHITECTURES=(
  "x86_64-linux"
  "aarch64-linux"
  "x86_64-darwin"
  "aarch64-darwin"
)

# Show usage information
show_usage() {
  cat << EOF
Usage: $0 [ARCHITECTURE|all|--list|--help]

Build AV Chaos Monkey for a specific architecture.

Arguments:
  ARCHITECTURE    Build for the specified architecture (e.g., x86_64-linux)
  all             Build for all architectures (default behavior)
  --list          List all available architectures
  --help          Show this help message

Examples:
  $0 x86_64-linux          # Build for Linux x86_64
  $0 aarch64-darwin        # Build for macOS Apple Silicon
  $0 all                   # Build for all architectures
  $0 --list                # List available architectures

Available architectures:
  - x86_64-linux      Linux (Intel/AMD 64-bit)
  - aarch64-linux     Linux ARM64 (Raspberry Pi, AWS Graviton)
  - x86_64-darwin     macOS Intel
  - aarch64-darwin    macOS Apple Silicon (M1/M2/M3)

Note: Cross-compilation from macOS to Linux requires remote builders.
EOF
}

# List available architectures
list_architectures() {
  echo "Available architectures:"
  for arch in "${ARCHITECTURES[@]}"; do
    echo "  - $arch"
  done
  exit 0
}

# Validate architecture
is_valid_arch() {
  local arch=$1
  for valid_arch in "${ARCHITECTURES[@]}"; do
    if [[ "$arch" == "$valid_arch" ]]; then
      return 0
    fi
  done
  return 1
}

# Handle command line arguments
TARGET_ARCH="${1:-}"

case "$TARGET_ARCH" in
  --help|-h)
    show_usage
    exit 0
    ;;
  --list|-l)
    list_architectures
    ;;
  "")
    # No argument - show usage
    show_usage
    exit 1
    ;;
  all)
    # Build for all architectures (original behavior)
    TARGET_ARCH="all"
    ;;
  *)
    # Check if it's a valid architecture
    if ! is_valid_arch "$TARGET_ARCH"; then
      echo "❌ Error: Invalid architecture '$TARGET_ARCH'"
      echo ""
      list_architectures
      exit 1
    fi
    ;;
esac

CURRENT_SYSTEM=$(nix eval --impure --expr 'builtins.currentSystem' --raw 2>/dev/null || echo "unknown")

# Build function
build_for_arch() {
  local arch=$1
  local out_link="result-$arch"

  echo "📦 Building for $arch..."

  # Check if this is cross-compilation that might not work
  if [[ "$CURRENT_SYSTEM" == *"darwin"* ]] && [[ "$arch" == *"linux"* ]]; then
    echo "   ⚠️  Cross-compilation from macOS to Linux - may require remote builders"
  elif [[ "$CURRENT_SYSTEM" == *"linux"* ]] && [[ "$arch" == *"darwin"* ]]; then
    echo "   ⚠️  Cross-compilation from Linux to macOS - may require remote builders"
  fi

  if nix build ".#packages.$arch.av-chaos-monkey" --out-link "$out_link" 2>&1; then
    echo "✅ Built successfully: $out_link"

    # Show binary info
    local binary_path
    if [ -L "$out_link" ]; then
      binary_path=$(readlink -f "$out_link" 2>/dev/null || readlink "$out_link")/bin/main
      if [ -f "$binary_path" ]; then
        local file_info=$(file "$binary_path" 2>/dev/null || echo "binary")
        echo "   📄 Binary: $binary_path"
        echo "   📋 Info: $file_info"
      fi
    fi
    return 0
  else
    echo "❌ Failed to build for $arch"

    # Provide helpful error message
    if [[ "$CURRENT_SYSTEM" == *"darwin"* ]] && [[ "$arch" == *"linux"* ]]; then
      echo "   💡 Tip: Set up Nix remote builders or build on a Linux machine"
      echo "   💡 See CROSS_COMPILATION.md for more information"
    elif [[ "$CURRENT_SYSTEM" == *"linux"* ]] && [[ "$arch" == *"darwin"* ]]; then
      echo "   💡 Tip: Set up Nix remote builders or build on a macOS machine"
      echo "   💡 See CROSS_COMPILATION.md for more information"
    fi
    return 1
  fi
}

# Main build logic
if [[ "$TARGET_ARCH" == "all" ]]; then
  # Build for all architectures
  echo "🔨 Building AV Chaos Monkey for all architectures..."
  echo ""
  echo "ℹ️  Note: Cross-compilation from macOS to Linux requires remote builders."
  echo "   Building for your current system will work, others may require setup."
  echo ""

  SUCCESS_COUNT=0
  FAILED_ARCHS=()

  for arch in "${ARCHITECTURES[@]}"; do
    if build_for_arch "$arch"; then
      SUCCESS_COUNT=$((SUCCESS_COUNT + 1))
    else
      FAILED_ARCHS+=("$arch")
    fi
    echo ""
  done

  echo "🎉 Build summary:"
  echo "   ✅ Successful: $SUCCESS_COUNT/${#ARCHITECTURES[@]}"
  if [ ${#FAILED_ARCHS[@]} -gt 0 ]; then
    echo "   ❌ Failed: ${FAILED_ARCHS[*]}"
  fi
else
  # Build for single architecture
  echo "🔨 Building AV Chaos Monkey for $TARGET_ARCH..."
  echo ""

  if build_for_arch "$TARGET_ARCH"; then
    echo ""
    echo "✅ Build complete! Binary available at: result-$TARGET_ARCH/bin/main"
    exit 0
  else
    echo ""
    echo "❌ Build failed for $TARGET_ARCH"
    exit 1
  fi
fi
