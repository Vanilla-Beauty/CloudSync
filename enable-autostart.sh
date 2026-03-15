#!/usr/bin/env bash
# enable-autostart.sh — Register cloudsyncd as a systemd user service so it
# starts automatically on login (and optionally on boot via lingering).
#
# Usage:
#   ./enable-autostart.sh             # install & enable
#   ./enable-autostart.sh --uninstall # remove service & disable
#   ./enable-autostart.sh --status    # show current status
#   ./enable-autostart.sh --linger    # also enable boot-time start (no login needed)
set -euo pipefail

# ── constants ─────────────────────────────────────────────────────────────────

SERVICE_NAME="cloudsyncd"
UNIT_DIR="${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user"
UNIT_FILE="$UNIT_DIR/${SERVICE_NAME}.service"

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

require_systemd_user() {
  if ! command -v systemctl &>/dev/null; then
    error "systemctl not found. This script requires systemd."
    exit 1
  fi
  # Check that the user session bus is reachable
  if ! systemctl --user status &>/dev/null 2>&1; then
    # Non-fatal: lingering or headless sessions may still work
    warn "systemd user session may not be active yet. Service will start on next login."
  fi
}

find_binary() {
  local bin="$1"
  # 1. Same directory as this script
  local script_dir
  script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
  if [[ -x "$script_dir/$bin" ]]; then
    echo "$script_dir/$bin"
    return
  fi
  # 2. PATH
  if command -v "$bin" &>/dev/null; then
    command -v "$bin"
    return
  fi
  return 1
}

# ── parse args ────────────────────────────────────────────────────────────────

MODE="install"  # install | uninstall | status | linger

usage() {
  cat <<EOF
Usage: $0 [option]

Options:
  (none)         Install and enable the cloudsyncd systemd user service
  --linger       Also enable lingering so cloudsyncd starts at boot without login
  --uninstall    Stop, disable, and remove the service unit file
  --status       Show the current service status
  -h, --help     Show this help

About systemd user services:
  A 'user service' runs as your own user, without root, and starts when you
  log in. With --linger it starts at boot even if you never log in (useful
  for servers / headless machines).
EOF
  exit 0
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --uninstall) MODE="uninstall"; shift ;;
    --status)    MODE="status"; shift ;;
    --linger)    MODE="linger"; shift ;;
    -h|--help)   usage ;;
    *) error "Unknown option: $1"; usage ;;
  esac
done

# ── status ────────────────────────────────────────────────────────────────────

if [[ "$MODE" == "status" ]]; then
  require_systemd_user
  echo ""
  systemctl --user status "$SERVICE_NAME" --no-pager || true
  echo ""
  # Show whether lingering is on
  if loginctl show-user "$USER" 2>/dev/null | grep -q "Linger=yes"; then
    info "Lingering is enabled — service starts at boot."
  else
    warn "Lingering is disabled — service starts only on login."
  fi
  exit 0
fi

# ── uninstall ─────────────────────────────────────────────────────────────────

if [[ "$MODE" == "uninstall" ]]; then
  require_systemd_user
  section "Removing cloudsyncd autostart..."

  systemctl --user stop    "$SERVICE_NAME" 2>/dev/null && info "Stopped $SERVICE_NAME" || true
  systemctl --user disable "$SERVICE_NAME" 2>/dev/null && info "Disabled $SERVICE_NAME" || true

  if [[ -f "$UNIT_FILE" ]]; then
    rm -f "$UNIT_FILE"
    info "Removed $UNIT_FILE"
  else
    warn "Unit file not found: $UNIT_FILE (already removed?)"
  fi

  systemctl --user daemon-reload
  info "systemd user daemon reloaded"

  # Offer to disable lingering
  if loginctl show-user "$USER" 2>/dev/null | grep -q "Linger=yes"; then
    echo ""
    read -r -p "Disable lingering for $USER? [y/N] " REPLY
    if [[ "${REPLY,,}" == "y" ]]; then
      loginctl disable-linger "$USER"
      info "Lingering disabled"
    fi
  fi

  echo ""
  info "Autostart removed. cloudsyncd will no longer start automatically."
  exit 0
fi

# ── install (default) and linger ──────────────────────────────────────────────

require_systemd_user

section "Locating cloudsyncd binary..."

DAEMON_PATH=""
if DAEMON_PATH=$(find_binary "cloudsyncd"); then
  info "Found: $DAEMON_PATH"
else
  error "cloudsyncd binary not found."
  echo ""
  echo "  Build and install it first:"
  echo "    ./install.sh --gobin"
  echo "  or:"
  echo "    go build -o ~/.local/bin/cloudsyncd ./cmd/cloudsyncd/"
  exit 1
fi

CLOUDSYNC_PATH=""
if CLOUDSYNC_PATH=$(find_binary "cloudsync"); then
  info "Found: $CLOUDSYNC_PATH"
else
  warn "cloudsync (CLI) binary not found in PATH — autostart will still work, but you won't be able to control the daemon from the command line until it is installed."
fi

section "Writing systemd unit file..."

mkdir -p "$UNIT_DIR"

cat > "$UNIT_FILE" <<EOF
[Unit]
Description=CloudSync Daemon
Documentation=https://codebuddy.woa.com
# Start after the network is up so COS uploads don't fail immediately
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=${DAEMON_PATH}
Restart=on-failure
RestartSec=5
# Log to the systemd journal (also mirrors to cloudsyncd.log via the daemon)
StandardOutput=journal
StandardError=journal
# Give the daemon 10 s to shut down cleanly before SIGKILL
TimeoutStopSec=10

[Install]
WantedBy=default.target
EOF

info "Written: $UNIT_FILE"

section "Enabling service..."

systemctl --user daemon-reload
info "Daemon reloaded"

systemctl --user enable "$SERVICE_NAME"
info "Enabled (will start on next login)"

# Start it now if not already running
if systemctl --user is-active --quiet "$SERVICE_NAME"; then
  info "$SERVICE_NAME is already running"
else
  if systemctl --user start "$SERVICE_NAME"; then
    info "Started $SERVICE_NAME"
  else
    warn "Could not start $SERVICE_NAME right now (no active user session?)"
    warn "It will start automatically on next login."
  fi
fi

# ── lingering (boot-time start) ───────────────────────────────────────────────

if [[ "$MODE" == "linger" ]]; then
  section "Enabling lingering for $USER..."
  if loginctl enable-linger "$USER"; then
    info "Lingering enabled — $SERVICE_NAME will start at boot without login"
  else
    error "Failed to enable lingering. You may need sudo:"
    echo "    sudo loginctl enable-linger $USER"
    exit 1
  fi
fi

# ── summary ───────────────────────────────────────────────────────────────────

echo ""
echo -e "${BOLD}Done.${RESET}"
echo ""
echo "  Check status:    systemctl --user status $SERVICE_NAME"
echo "  View logs:       journalctl --user -u $SERVICE_NAME -f"
echo "  Stop:            systemctl --user stop $SERVICE_NAME"
echo "  Disable:         ./enable-autostart.sh --uninstall"
echo ""

if loginctl show-user "$USER" 2>/dev/null | grep -q "Linger=yes"; then
  info "Lingering is ON  — starts at boot."
else
  warn "Lingering is OFF — starts on login only."
  echo "  To start at boot (no login required), run:"
  echo "    ./enable-autostart.sh --linger"
fi
echo ""
