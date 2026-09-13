#!/bin/bash
set -e

REPO="relayced/Hexagon"
BRANCH="main"
BASE_URL="https://raw.githubusercontent.com/$REPO/$BRANCH"
VERSION_URL="$BASE_URL/version.txt"
BIN_DIR="${HOME:-.}"
LOCAL_BIN="$BIN_DIR/nefhub"

# ANSI Colors
CYAN='\033[38;5;75m'
GREEN='\033[38;5;78m'
AMBER='\033[38;5;214m'
RED='\033[38;5;203m'
GRAY='\033[38;5;244m'
NC='\033[0m'
BOLD='\033[1m'

echo -e "${CYAN}${BOLD}╔══════════════════════════════════════════════╗${NC}"
echo -e "${CYAN}${BOLD}║           NEFARIOUS HUB LAUNCHER             ║${NC}"
echo -e "${CYAN}${BOLD}║        Open-Source Termux Automation         ║${NC}"
echo -e "${CYAN}${BOLD}╚══════════════════════════════════════════════╝${NC}"
echo ""

# Architecture Detection
ARCH=$(uname -m)
case "$ARCH" in
    aarch64|arm64)
        TARGET_BIN="nefhub_arm64"
        ;;
    armv7l|armv8l|arm)
        TARGET_BIN="nefhub_arm"
        ;;
    x86_64|amd64)
        TARGET_BIN="nefhub_amd64"
        ;;
    *)
        TARGET_BIN="nefhub_arm64"
        ;;
esac

echo -e "${GRAY}[*] Device architecture: ${CYAN}$ARCH${GRAY} -> Binary: ${CYAN}$TARGET_BIN${NC}"

# Ensure curl is available
if ! command -v curl >/dev/null 2>&1; then
    echo -e "${AMBER}[!] curl not found, installing...${NC}"
    if command -v pkg >/dev/null 2>&1; then
        pkg install -y curl
    elif command -v apt-get >/dev/null 2>&1; then
        apt-get install -y curl
    fi
fi

# Fetch remote version
REMOTE_VER=$(curl -sL "$VERSION_URL" 2>/dev/null | tr -d '[:space:]')
if [ -z "$REMOTE_VER" ]; then
    REMOTE_VER="latest"
fi

echo -e "${GREEN}[✓] Latest release: v$REMOTE_VER${NC}"
echo -e "${GRAY}[*] Downloading precompiled binary...${NC}"

# Clean up old binary to ensure fresh download
rm -f "$LOCAL_BIN"

# Download precompiled binary with cache-busting query
DOWNLOAD_URL="$BASE_URL/$TARGET_BIN?t=$(date +%s)"
curl -sL -H "Cache-Control: no-cache" "$DOWNLOAD_URL" -o "$LOCAL_BIN"

if [ ! -s "$LOCAL_BIN" ]; then
    echo -e "${RED}[✗] Failed to download binary from $BASE_URL/$TARGET_BIN${NC}"
    exit 1
fi

chmod +x "$LOCAL_BIN"
echo -e "${GREEN}[✓] Binary ready! Launching Nefarious Hub...${NC}"
echo ""

# Execute with TTY attachment for interactive menu
if [ -e /dev/tty ]; then
    exec "$LOCAL_BIN" "$@" < /dev/tty
else
    exec "$LOCAL_BIN" "$@"
fi
