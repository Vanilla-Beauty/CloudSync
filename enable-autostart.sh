#!/usr/bin/env bash
# enable-autostart.sh — Register cloudsyncd as a background service
#
# Linux: systemd user service (starts on login; --linger for boot-time start)
# macOS: launchd LaunchAgent (starts on login)
#
# Usage:
#   ./enable-autostart.sh             # install & enable
#   ./enable-autostart.sh --uninstall # remove service
#   ./enable-autostart.sh --status    # show current status
#   ./enable-autostart.sh --linger    # (Linux only) also enable boot-time start
set -euo pipefail

OS="$(uname -s)"
SERVICE_NAME="cloudsyncd"

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

find_binary() {
  local bin="$1"
  local script_dir
  script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
  if [[ -x "$script_dir/$bin" ]]; then
    echo "$script_dir/$bin"; return
  fi
  if command -v "$bin" &>/dev/null; then
    command -v "$bin"; return
  fi
  return 1
}

# ── parse args ────────────────────────────────────────────────────────────────

MODE="install"

usage() {
  cat <<EOF
Usage: $0 [option]

Options:
  (none)         Install and enable cloudsyncd autostart
  --linger       (Linux) Enable lingering so cloudsyncd starts at boot without login
  --uninstall    Stop, disable, and remove the autostart entry
  --status       Show the current service status
  -h, --help     Show this help

Detected OS: $OS
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

# ── Windows guard ─────────────────────────────────────────────────────────────

case "$OS" in
  MINGW*|MSYS*|CYGWIN*)
    error "Detected Windows shell. Use enable-autostart.ps1 instead."
    exit 1
    ;;
esac

# ══════════════════════════════════════════════════════════════════════════════
# macOS — LaunchAgent
# ══════════════════════════════════════════════════════════════════════════════

if [[ "$OS" == "Darwin" ]]; then

  LAUNCH_DIR="$HOME/Library/LaunchAgents"
  PLIST="$LAUNCH_DIR/com.cloudsync.cloudsyncd.plist"

  # ── status (macOS) ──────────────────────────────────────────────────────────
  if [[ "$MODE" == "status" ]]; then
    echo ""
    if launchctl list | grep -q "com.cloudsync.cloudsyncd"; then
      info "cloudsyncd is loaded"
      launchctl list com.cloudsync.cloudsyncd 2>/dev/null || true
    else
      warn "cloudsyncd is not loaded"
    fi
    [[ -f "$PLIST" ]] && info "Plist: $PLIST" || warn "Plist not found: $PLIST"
    exit 0
  fi

  # ── uninstall (macOS) ───────────────────────────────────────────────────────
  if [[ "$MODE" == "uninstall" ]]; then
    section "Removing cloudsyncd LaunchAgent..."
    launchctl unload "$PLIST" 2>/dev/null && info "Unloaded" || true
    [[ -f "$PLIST" ]] && rm -f "$PLIST" && info "Removed $PLIST" || warn "Plist not found"
    exit 0
  fi

  # ── install (macOS) ─────────────────────────────────────────────────────────
  section "Locating cloudsyncd binary..."
  DAEMON_PATH=""
  if DAEMON_PATH=$(find_binary "cloudsyncd"); then
    info "Found: $DAEMON_PATH"
  else
    error "cloudsyncd binary not found. Run ./install.sh first."
    exit 1
  fi

  LOG_DIR="${HOME}/.config/cloudsync"
  mkdir -p "$LOG_DIR"

  section "Writing LaunchAgent plist..."
  mkdir -p "$LAUNCH_DIR"

  cat > "$PLIST" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN"
  "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>com.cloudsync.cloudsyncd</string>
    <key>ProgramArguments</key>
    <array>
        <string>${DAEMON_PATH}</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <dict>
        <key>SuccessfulExit</key>
        <false/>
    </dict>
    <key>StandardOutPath</key>
    <string>${LOG_DIR}/cloudsyncd.stdout.log</string>
    <key>StandardErrorPath</key>
    <string>${LOG_DIR}/cloudsyncd.stderr.log</string>
    <key>ThrottleInterval</key>
    <integer>5</integer>
</dict>
</plist>
EOF
  info "Written: $PLIST"

  section "Loading LaunchAgent..."
  launchctl unload "$PLIST" 2>/dev/null || true
  launchctl load -w "$PLIST"
  info "Loaded and enabled"

  echo ""
  echo -e "${BOLD}Done.${RESET}"
  echo ""
  echo "  Status:    launchctl list com.cloudsync.cloudsyncd"
  echo "  Logs:      tail -f $LOG_DIR/cloudsyncd.stdout.log"
  echo "  Stop:      launchctl unload $PLIST"
  echo "  Remove:    ./enable-autostart.sh --uninstall"
  echo ""
  exit 0
fi

# ══════════════════════════════════════════════════════════════════════════════
# Linux — systemd user service
# ══════════════════════════════════════════════════════════════════════════════

UNIT_DIR="${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user"
UNIT_FILE="$UNIT_DIR/${SERVICE_NAME}.service"

require_systemd_user() {
  if ! command -v systemctl &>/dev/null; then
    error "systemctl not found. This script requires systemd."
    exit 1
  fi
  if ! systemctl --user status &>/dev/null 2>&1; then
    warn "systemd user session may not be active yet. Service will start on next login."
  fi
}

# ── status (Linux) ────────────────────────────────────────────────────────────
if [[ "$MODE" == "status" ]]; then
  require_systemd_user
  echo ""
  systemctl --user status "$SERVICE_NAME" --no-pager || true
  echo ""
  if loginctl show-user "$USER" 2>/dev/null | grep -q "Linger=yes"; then
    info "Lingering is enabled — service starts at boot."
  else
    warn "Lingering is disabled — service starts only on login."
  fi
  exit 0
fi

# ── uninstall (Linux) ─────────────────────────────────────────────────────────
if [[ "$MODE" == "uninstall" ]]; then
  require_systemd_user
  section "Removing cloudsyncd autostart..."
  systemctl --user stop    "$SERVICE_NAME" 2>/dev/null && info "Stopped" || true
  systemctl --user disable "$SERVICE_NAME" 2>/dev/null && info "Disabled" || true
  if [[ -f "$UNIT_FILE" ]]; then
    rm -f "$UNIT_FILE"
    info "Removed $UNIT_FILE"
  else
    warn "Unit file not found (already removed?)"
  fi
  systemctl --user daemon-reload
  info "Daemon reloaded"

  if loginctl show-user "$USER" 2>/dev/null | grep -q "Linger=yes"; then
    echo ""
    read -r -p "Disable lingering for $USER? [y/N] " REPLY
    if [[ "${REPLY,,}" == "y" ]]; then
      loginctl disable-linger "$USER"
      info "Lingering disabled"
    fi
  fi
  echo ""; info "Autostart removed."
  exit 0
fi

# ── install (Linux) ───────────────────────────────────────────────────────────
require_systemd_user

section "Locating cloudsyncd binary..."
DAEMON_PATH=""
if DAEMON_PATH=$(find_binary "cloudsyncd"); then
  info "Found: $DAEMON_PATH"
else
  error "cloudsyncd binary not found."
  echo ""; echo "  Build and install first:"; echo "    ./install.sh --gobin"
  exit 1
fi

section "Writing systemd unit file..."
mkdir -p "$UNIT_DIR"

cat > "$UNIT_FILE" <<EOF
[Unit]
Description=CloudSync Daemon
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=${DAEMON_PATH}
Restart=on-failure
RestartSec=5
StandardOutput=journal
StandardError=journal
TimeoutStopSec=10

[Install]
WantedBy=default.target
EOF
info "Written: $UNIT_FILE"

section "Enabling service..."
systemctl --user daemon-reload
info "Daemon reloaded"
systemctl --user enable "$SERVICE_NAME"
info "Enabled"

if systemctl --user is-active --quiet "$SERVICE_NAME"; then
  info "$SERVICE_NAME is already running"
else
  if systemctl --user start "$SERVICE_NAME"; then
    info "Started $SERVICE_NAME"
  else
    warn "Could not start right now — will start on next login."
  fi
fi

# ── linger (Linux) ────────────────────────────────────────────────────────────
if [[ "$MODE" == "linger" ]]; then
  section "Enabling lingering for $USER..."
  if loginctl enable-linger "$USER"; then
    info "Lingering enabled — $SERVICE_NAME will start at boot without login"
  else
    error "Failed to enable lingering. Try: sudo loginctl enable-linger $USER"
    exit 1
  fi
fi

echo ""
echo -e "${BOLD}Done.${RESET}"
echo ""
echo "  Status:  systemctl --user status $SERVICE_NAME"
echo "  Logs:    journalctl --user -u $SERVICE_NAME -f"
echo "  Stop:    systemctl --user stop $SERVICE_NAME"
echo "  Remove:  ./enable-autostart.sh --uninstall"
echo ""
if loginctl show-user "$USER" 2>/dev/null | grep -q "Linger=yes"; then
  info "Lingering is ON  — starts at boot."
else
  warn "Lingering is OFF — starts on login only."
  echo "  To start at boot: ./enable-autostart.sh --linger"
fi
echo ""
