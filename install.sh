#!/usr/bin/env bash
# install.sh — Build and install cloudsync + cloudsyncd
# Supports: Linux, macOS
# For Windows use install.ps1 instead.
set -euo pipefail

# ── config ────────────────────────────────────────────────────────────────────

BINARIES=(cloudsync cloudsyncd)
OS="$(uname -s)"

case "$OS" in
  Darwin) DEFAULT_INSTALL_DIR="/usr/local/bin" ;;
  *)      DEFAULT_INSTALL_DIR="/usr/local/bin" ;;
esac

# ── colors ────────────────────────────────────────────────────────────────────

if [ -t 1 ]; then
  BOLD="\033[1m"; GREEN="\033[32m"; YELLOW="\033[33m"; RED="\033[31m"; RESET="\033[0m"
else
  BOLD=""; GREEN=""; YELLOW=""; RED=""; RESET=""
fi

info()    { echo -e "${GREEN}[✓]${RESET} $*"; }
warn()    { echo -e "${YELLOW}[!]${RESET} $*"; }
error()   { echo -e "${RED}[✗]${RESET} $*" >&2; }
section() { echo -e "\n${BOLD}$*${RESET}"; }

# ── helpers ───────────────────────────────────────────────────────────────────

require_cmd() {
  if ! command -v "$1" &>/dev/null; then
    error "Required command not found: $1"
    exit 1
  fi
}

go_version_ok() {
  local ver
  ver=$(go version | grep -oE 'go[0-9]+\.[0-9]+' | head -1 | tr -d 'go')
  local major minor
  IFS='.' read -r major minor <<< "$ver"
  [[ "$major" -gt 1 || ( "$major" -eq 1 && "$minor" -ge 21 ) ]]
}

# ── parse args ────────────────────────────────────────────────────────────────

INSTALL_DIR="$DEFAULT_INSTALL_DIR"
USE_GOBIN=false

usage() {
  echo "Usage: $0 [options]"
  echo ""
  echo "Options:"
  echo "  --dir <path>   Install binaries to <path>  (default: $DEFAULT_INSTALL_DIR)"
  echo "  --gobin        Install to \$(go env GOBIN) or \$GOPATH/bin"
  echo "  -h, --help     Show this help"
  echo ""
  echo "OS detected: $OS"
  exit 0
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --dir)    INSTALL_DIR="$2"; shift 2 ;;
    --gobin)  USE_GOBIN=true; shift ;;
    -h|--help) usage ;;
    *) error "Unknown option: $1"; usage ;;
  esac
done

if $USE_GOBIN; then
  GOBIN_PATH=$(go env GOBIN)
  if [[ -z "$GOBIN_PATH" ]]; then
    GOBIN_PATH="$(go env GOPATH)/bin"
  fi
  INSTALL_DIR="$GOBIN_PATH"
fi

# ── preflight ─────────────────────────────────────────────────────────────────

section "Checking prerequisites..."

if [[ "$OS" == "MINGW"* || "$OS" == "MSYS"* || "$OS" == "CYGWIN"* ]]; then
  error "Detected Windows shell. Please use install.ps1 instead."
  exit 1
fi

require_cmd go

if ! go_version_ok; then
  error "Go 1.21+ is required. Current: $(go version)"
  exit 1
fi
info "Go $(go version | grep -oE 'go[0-9]+\.[0-9.]+' | head -1)"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
if [[ ! -f "$SCRIPT_DIR/go.mod" ]]; then
  error "go.mod not found. Run this script from the CloudSync project root."
  exit 1
fi
cd "$SCRIPT_DIR"

MODULE=$(grep '^module' go.mod | awk '{print $2}')
info "Module: $MODULE"
info "OS: $OS"

# ── build ─────────────────────────────────────────────────────────────────────

section "Building..."

BUILD_DIR=$(mktemp -d)
trap 'rm -rf "$BUILD_DIR"' EXIT

# Resolve version and build time for -ldflags injection
CS_VERSION=$(git describe --tags --always --dirty 2>/dev/null || echo "dev")
CS_BUILD_TIME=$(date -u +%Y-%m-%dT%H:%M:%SZ)
CS_LDFLAGS="-s -w -X main.version=${CS_VERSION} -X main.buildTime=${CS_BUILD_TIME}"

for bin in "${BINARIES[@]}"; do
  echo -n "  Building $bin ... "
  if go build -ldflags="${CS_LDFLAGS}" -o "$BUILD_DIR/$bin" "./cmd/$bin/"; then
    echo -e "${GREEN}done${RESET}"
  else
    echo -e "${RED}failed${RESET}"
    error "Build failed for $bin"
    exit 1
  fi
done

# ── install ───────────────────────────────────────────────────────────────────

section "Installing to $INSTALL_DIR ..."

if [[ ! -d "$INSTALL_DIR" ]]; then
  if ! mkdir -p "$INSTALL_DIR" 2>/dev/null; then
    warn "Cannot create $INSTALL_DIR, retrying with sudo..."
    sudo mkdir -p "$INSTALL_DIR"
  fi
fi

NEED_SUDO=false
if [[ ! -w "$INSTALL_DIR" ]]; then
  NEED_SUDO=true
  warn "No write permission to $INSTALL_DIR, using sudo..."
fi

for bin in "${BINARIES[@]}"; do
  echo -n "  Installing $bin ... "
  src="$BUILD_DIR/$bin"
  dst="$INSTALL_DIR/$bin"
  if $NEED_SUDO; then
    sudo install -m 755 "$src" "$dst"
  else
    install -m 755 "$src" "$dst"
  fi
  echo -e "${GREEN}done${RESET}"
done

# ── verify ────────────────────────────────────────────────────────────────────

section "Verifying..."

for bin in "${BINARIES[@]}"; do
  installed_path="$INSTALL_DIR/$bin"
  if [[ -x "$installed_path" ]]; then
    info "$installed_path"
  else
    error "$bin not found at $installed_path"
    exit 1
  fi
done

if ! echo "$PATH" | tr ':' '\n' | grep -qx "$INSTALL_DIR"; then
  warn "$INSTALL_DIR is not in your PATH."
  warn "Add this to your shell profile:"
  warn "  export PATH=\"$INSTALL_DIR:\$PATH\""
fi

# ── autostart hint ────────────────────────────────────────────────────────────

echo ""
info "Installation complete!"
echo ""
echo "  Quick start:"
echo "    cloudsync init          # configure COS credentials"
echo "    cloudsync start         # start the daemon"
echo "    cloudsync mount <path>  # start syncing a directory"
echo ""

case "$OS" in
  Linux)
    echo "  To start cloudsyncd automatically on login:"
    echo "    ./enable-autostart.sh"
    echo ""
    ;;
  Darwin)
    echo "  To start cloudsyncd automatically at login:"
    echo "    ./enable-autostart.sh"
    echo ""
    ;;
esac
