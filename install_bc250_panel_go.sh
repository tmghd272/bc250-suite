#!/usr/bin/env bash
# install_bc250_panel_go.sh
# --------------------------------------
# Builds the single-binary Go server, installs it, and sets up auto-start.
# Detects whether systemd is actually present rather than assuming it -
# falls back to plain instructions on non-systemd distros (Void/runit,
# Alpine/OpenRC, etc) instead of just failing.
# Also detects rpm-ostree/Bazzite systems (systemd user services work fine
# there, but the Go toolchain needs rpm-ostree layering or a distrobox
# instead of a plain dnf install).

set -euo pipefail

SRC_DIR="${1:-$(dirname "$(readlink -f "$0")")}"
INSTALL_DIR="$HOME/.local/share/bc250-panel"
SERVICE_NAME="bc250-panel"
PORT="${BC250_PANEL_PORT:-8091}"

echo "== BC-250 Control Suite installer (Go edition) =="
echo "Source dir:  $SRC_DIR"
echo "Install dir: $INSTALL_DIR"
echo

if [[ ! -f "$SRC_DIR/bc250_panel_server.go" ]] || [[ ! -f "$SRC_DIR/bc250_argb_panel.html" ]]; then
  echo "ERROR: bc250_panel_server.go and bc250_argb_panel.html must be in $SRC_DIR"
  exit 1
fi

IS_OSTREE=false
if [[ -f /run/ostree-booted ]] || grep -qi '^ID=bazzite' /etc/os-release 2>/dev/null || grep -qi '^VARIANT_ID=bazzite' /etc/os-release 2>/dev/null; then
  IS_OSTREE=true
fi

command -v go >/dev/null || {
  echo "ERROR: Go toolchain not found."
  if $IS_OSTREE; then
    echo "Detected an rpm-ostree/Bazzite system - regular dnf won't work here."
    echo "Two options:"
    echo
    echo "  1) Layer it onto the base image (persists across boots, needs one reboot):"
    echo "       sudo rpm-ostree install golang"
    echo "       systemctl reboot"
    echo "       then re-run this script"
    echo
    echo "  2) Build inside a distrobox instead, so the base image stays untouched"
    echo "     (you'll still run the resulting binary directly on the host after):"
    echo "       distrobox create -n bc250-build -i fedora:latest"
    echo "       distrobox enter bc250-build -- sudo dnf install -y golang"
    echo "       distrobox enter bc250-build -- go build -o \"$SRC_DIR/bc250-panel\" \"$SRC_DIR/bc250_panel_server.go\""
    echo "     Then place the built binary at $INSTALL_DIR/bc250-panel, copy"
    echo "     bc250_argb_panel.html alongside it, and re-run this script - it will"
    echo "     skip the build step if the binary already exists."
  else
    echo "Install it via your distro's package manager, e.g.:"
    echo "  Arch/CachyOS: sudo pacman -S go"
    echo "  Debian/Ubuntu: sudo apt install golang-go"
    echo "  Fedora: sudo dnf install golang"
  fi
  exit 1
}

mkdir -p "$INSTALL_DIR"
if [[ -x "$INSTALL_DIR/bc250-panel" ]] && ! command -v go >/dev/null 2>&1; then
  echo "Found existing prebuilt binary at $INSTALL_DIR/bc250-panel and no host Go"
  echo "toolchain - assuming it was built via distrobox and skipping rebuild."
  cp "$SRC_DIR/bc250_argb_panel.html" "$INSTALL_DIR/"
else
  echo "Building..."
  go build -o "$INSTALL_DIR/bc250-panel" "$SRC_DIR/bc250_panel_server.go"
  cp "$SRC_DIR/bc250_argb_panel.html" "$INSTALL_DIR/"
  chmod +x "$INSTALL_DIR/bc250-panel"
  echo "Built single static binary at $INSTALL_DIR/bc250-panel"
fi

HAS_SYSTEMD=false
if command -v systemctl >/dev/null 2>&1 && [[ -d /run/systemd/system ]]; then
  HAS_SYSTEMD=true
fi

if $HAS_SYSTEMD; then
  SERVICE_DIR="$HOME/.config/systemd/user"
  mkdir -p "$SERVICE_DIR"
  cat > "$SERVICE_DIR/${SERVICE_NAME}.service" <<EOF
[Unit]
Description=BC-250 Control Suite (RGB panel + sensor dashboard)
After=network.target

[Service]
Type=simple
ExecStart=$INSTALL_DIR/bc250-panel --port $PORT
WorkingDirectory=$INSTALL_DIR
Restart=on-failure
RestartSec=3

[Install]
WantedBy=default.target
EOF
  echo "Wrote systemd user service: $SERVICE_DIR/${SERVICE_NAME}.service"

  if ! loginctl show-user "$USER" 2>/dev/null | grep -q "Linger=yes"; then
    echo
    echo "Enabling lingering so this survives logout (needs sudo once):"
    sudo loginctl enable-linger "$USER"
  fi

  systemctl --user daemon-reload
  systemctl --user enable "$SERVICE_NAME"
  systemctl --user restart "$SERVICE_NAME"
  sleep 1

  echo
  if systemctl --user is-active --quiet "$SERVICE_NAME"; then
    IP=$(hostname -I 2>/dev/null | awk '{print $1}')
    echo "== Running (systemd) =="
    echo "Local:   http://localhost:$PORT/"
    [[ -n "${IP:-}" ]] && echo "Network: http://$IP:$PORT/"
    echo
    echo "Manage it with:"
    echo "  systemctl --user status  $SERVICE_NAME"
    echo "  systemctl --user restart $SERVICE_NAME"
    echo "  journalctl --user -u $SERVICE_NAME -f"
  else
    echo "Service failed to start - check logs with:"
    echo "  journalctl --user -u $SERVICE_NAME -e"
    exit 1
  fi

else
  echo
  echo "No systemd detected on this system - skipping service setup."
  echo "You'll need to wire it into whatever your distro's init system uses"
  echo "for autostart. To run it directly:"
  echo
  echo "  $INSTALL_DIR/bc250-panel --port $PORT &"
  echo
  echo "For OpenRC (Alpine/Gentoo), a minimal service script would go in"
  echo "/etc/init.d/ calling that same command with start-stop-daemon."
  echo "For runit (Void), a run script in /etc/sv/$SERVICE_NAME/run doing"
  echo "the same via: exec chpst -u \$USER $INSTALL_DIR/bc250-panel --port $PORT"
  echo
  echo "Starting it now in the foreground so you can confirm it works:"
  echo "(Ctrl+C to stop, then set up autostart for your init system separately)"
  echo
  exec "$INSTALL_DIR/bc250-panel" --port "$PORT"
fi
