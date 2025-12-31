#!/usr/bin/env bash
set -euo pipefail

if docker info &> /dev/null 2>&1; then
    exit 0
fi

if [[ "$(uname)" == "Darwin" ]]; then
    DOCKER_APP=""
    if [ -d "/Applications/Docker.app" ]; then
        DOCKER_APP="/Applications/Docker.app"
    elif [ -d "/Applications/Docker Desktop.app" ]; then
        DOCKER_APP="/Applications/Docker Desktop.app"
    fi
    
    if [ -n "$DOCKER_APP" ]; then
        if pgrep -f "Docker Desktop" &> /dev/null; then
            echo "Docker Desktop is starting..."
        else
            echo "Starting Docker Desktop..."
            open "$DOCKER_APP" 2>/dev/null || true
        fi
        
        # Wait for Docker to start (up to 90 seconds)
        echo -n "Waiting for Docker to be ready"
        for i in {1..90}; do
            sleep 1
            if docker info &> /dev/null 2>&1; then
                echo ""
                exit 0
            fi
            if [ $((i % 10)) -eq 0 ]; then
                echo -n "."
            fi
        done
        echo ""
        echo "⚠️  Docker is taking longer than expected to start"
        echo "   This might be the first launch - please check Docker Desktop window"
    fi
fi

if docker info &> /dev/null 2>&1; then
    exit 0
fi

echo "❌ Docker is not running"
echo "   Please start Docker Desktop manually, then run the script again"
exit 1
