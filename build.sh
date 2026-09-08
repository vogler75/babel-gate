#!/usr/bin/env bash
set -euo pipefail

# Determine repository root
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "${SCRIPT_DIR}"

# Terminal color codes
GREEN="\033[0;32m"
BLUE="\033[0;34m"
YELLOW="\033[1;33m"
RED="\033[0;31m"
NC="\033[0m"

# Default options
RUN_TESTS=false
CLEAN=false
RELEASE=false

usage() {
    echo "Usage: ./build.sh [OPTIONS]"
    echo ""
    echo "Build BabelGate / LLM-Router locally"
    echo ""
    echo "Options:"
    echo "  -t, --test       Run unit tests before building"
    echo "  -r, --release    Build optimized release binary (stripped symbols)"
    echo "  -c, --clean      Clean previous build artifacts before building"
    echo "  -h, --help       Show this help message"
    exit 0
}

# Parse flags
while [[ $# -gt 0 ]]; do
    case "$1" in
        -t|--test)
            RUN_TESTS=true
            shift
            ;;
        -r|--release)
            RELEASE=true
            shift
            ;;
        -c|--clean)
            CLEAN=true
            shift
            ;;
        -h|--help)
            usage
            ;;
        *)
            echo -e "${RED}Unknown option: $1${NC}"
            usage
            ;;
    esac
done

if [ "$CLEAN" = true ]; then
    echo -e "${BLUE}==>${NC} Cleaning bin/ directory..."
    rm -rf bin/
fi

if [ "$RUN_TESTS" = true ]; then
    echo -e "${BLUE}==>${NC} Running unit tests..."
    go test ./...
    echo -e "${GREEN}✓ All tests passed!${NC}"
fi

echo -e "${BLUE}==>${NC} Building babelgate..."
mkdir -p bin

LDFLAGS=""
if [ "$RELEASE" = true ]; then
    LDFLAGS="-s -w"
fi

if [ -n "$LDFLAGS" ]; then
    go build -trimpath -ldflags="${LDFLAGS}" -o bin/babelgate ./cmd/router
else
    go build -o bin/babelgate ./cmd/router
fi

BIN_SIZE=$(du -h bin/babelgate | awk '{print $1}')
echo -e "${GREEN}✓ Successfully built bin/babelgate (${BIN_SIZE})${NC}"
echo ""
echo "Run with:"
echo "  ./bin/babelgate"
