#!/usr/bin/env bash
# bc250_panel_go.sh
# --------------------------------------
# Builds/installs OR removes the single-binary Go server.
#
# Usage:
#   ./bc250_panel_go.sh install [source_dir]
#   ./bc250_panel_go.sh uninstall
#
# Runs as a root SYSTEM service (not a --user service) so the Fan Control
# tab's pwmN writes just work - no udev rule needed, since the server
# already owns the hwmon files it's writing to. This does mean the panel's
# HTTP API (including process kill and fan control) is reachable, unauthenticated,
# by anything on your LAN that can hit the port - fine for a home network,
# worth knowing if this box is anywhere less trusted.
#
# Detects whether systemd is actually present rather than assuming it -
# falls back to plain instructions on non-systemd distros (Void/runit,
# Alpine/OpenRC, etc) instead of just failing.
# Also detects rpm-ostree/Bazzite systems (systemd works fine there, but the
# Go toolchain needs rpm-ostree layering or a distrobox instead of a plain
# dnf install).

set -euo pipefail

SCRIPT_PATH="$(readlink -f "$0")" # resolve to an absolute path up front - "$0" alone
                                    # can be a bare relative filename (e.g. when run as
                                    # `bash bc250_panel_go.sh`), and sudo looks that up
                                    # in PATH rather than the current directory, failing
                                    # with "command not found"
ACTION="${1:-}"
SRC_DIR="${2:-$(dirname "$SCRIPT_PATH")}"
INSTALL_DIR="/opt/bc250-panel"
SERVICE_NAME="bc250-panel"
SERVICE_FILE="/etc/systemd/system/${SERVICE_NAME}.service"
PORT="${BC250_PANEL_PORT:-8091}"

# Leftovers from the old --user-service installer, if this box ever ran that
# version. Cleaned up on both install (so the two don't fight over the same
# port) and uninstall (so nothing's left behind).
OLD_USER_INSTALL_DIR="$HOME/.local/share/bc250-panel"
OLD_USER_SERVICE="$HOME/.config/systemd/user/${SERVICE_NAME}.service"

usage() {
  echo "Usage: $0 install [source_dir] | uninstall"
  exit 1
}

require_sudo() {
  if [[ $EUID -ne 0 ]]; then
    echo "This step needs root - re-running with sudo:"
    # Explicitly invoked via bash rather than executed directly - sudo execs
    # the target directly rather than through a shell, so it needs the +x
    # bit set on the file; going through `bash` here means it works even on
    # a freshly-downloaded copy that hasn't been chmod +x'd yet.
    exec sudo -E bash "$SCRIPT_PATH" "$@"
  fi
}

remove_old_user_service() {
  if [[ -f "$OLD_USER_SERVICE" ]]; then
    echo "Found an old --user-service install - migrating off it..."
    systemctl --user disable --now "$SERVICE_NAME" 2>/dev/null || true
    rm -f "$OLD_USER_SERVICE"
    systemctl --user daemon-reload 2>/dev/null || true
  fi
  if [[ -d "$OLD_USER_INSTALL_DIR" ]]; then
    rm -rf "$OLD_USER_INSTALL_DIR"
  fi
}

do_install() {
  require_sudo install "$SRC_DIR"
  # Past this point we're root - either we started as root, or just re-exec'd
  # via sudo above. Building as root too (not just installing) keeps this
  # simple - no separate non-root build step to hand off between users, and
  # this file has no external module dependencies, so root's Go cache
  # doesn't need network access to build it.

  # The Terminal tab's shell drops privileges to a real user rather than
  # staying root - this is whoever actually ran the install (sudo sets
  # SUDO_USER automatically), falling back to `logname` if the script was
  # invoked while already root (no sudo wrapping to read it from).
  TERM_USER="${SUDO_USER:-$(logname 2>/dev/null || true)}"

  echo "== BC-250 Control Suite installer (Go edition, root system service) =="
  echo "Source dir:  $SRC_DIR"
  echo "Install dir: $INSTALL_DIR"
  echo

  if [[ ! -f "$SRC_DIR/bc250_panel_server.go" ]] || [[ ! -f "$SRC_DIR/bc250_terminal.go" ]] || [[ ! -f "$SRC_DIR/go.mod" ]] || [[ ! -f "$SRC_DIR/bc250_control_panel.html" ]]; then
    echo "ERROR: bc250_panel_server.go, bc250_terminal.go, go.mod, go.sum, and bc250_control_panel.html must all be in $SRC_DIR"
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
      echo "       rpm-ostree install golang"
      echo "       systemctl reboot"
      echo "       then re-run this script"
      echo
      echo "  2) Build inside a distrobox instead, so the base image stays untouched"
      echo "     (you'll still run the resulting binary directly on the host after):"
      echo "       distrobox create -n bc250-build -i fedora:latest"
      echo "       distrobox enter bc250-build -- sudo dnf install -y golang"
      echo "       distrobox enter bc250-build -- bash -c 'cd \"$SRC_DIR\" && go build -o \"$SRC_DIR/bc250-panel\" .'"
      echo "     Then place the built binary at $INSTALL_DIR/bc250-panel, copy"
      echo "     bc250_control_panel.html alongside it, and re-run this script - it"
      echo "     will skip the build step if the binary already exists."
    else
      echo "Install it via your distro's package manager, e.g.:"
      echo "  Arch/CachyOS: pacman -S go"
      echo "  Debian/Ubuntu: apt install golang-go"
      echo "  Fedora: dnf install golang"
    fi
    exit 1
  }

  remove_old_user_service

  mkdir -p "$INSTALL_DIR"
  echo "Building (fetches two small Go modules on first build - creack/pty and"
  echo "gorilla/websocket, both used only by the Terminal tab - needs internet"
  echo "the first time; both compile straight into the same static binary, no"
  echo "runtime dependency on the target system either way)..."
  ( cd "$SRC_DIR" && go build -o "$INSTALL_DIR/bc250-panel" . )
  install -m 0644 "$SRC_DIR/bc250_control_panel.html" "$INSTALL_DIR/bc250_control_panel.html"
  chmod 0755 "$INSTALL_DIR/bc250-panel"
  echo "Installed binary + panel HTML to $INSTALL_DIR (owned by root)"

  HAS_SYSTEMD=false
  if command -v systemctl >/dev/null 2>&1 && [[ -d /run/systemd/system ]]; then
    HAS_SYSTEMD=true
  fi

  TERM_USER_FLAG=""
  if [[ -n "$TERM_USER" ]]; then
    TERM_USER_FLAG=" --user $TERM_USER"
  fi

  if $HAS_SYSTEMD; then
    cat > "$SERVICE_FILE" <<EOF
[Unit]
Description=BC-250 Control Suite (RGB panel + sensors + fan control + processes + terminal)
After=network.target

[Service]
Type=simple
User=root
ExecStart=$INSTALL_DIR/bc250-panel --port $PORT$TERM_USER_FLAG
WorkingDirectory=$INSTALL_DIR
Restart=on-failure
RestartSec=3

[Install]
WantedBy=multi-user.target
EOF
    echo "Wrote systemd system service: $SERVICE_FILE"

    systemctl daemon-reload
    systemctl enable "$SERVICE_NAME"
    systemctl restart "$SERVICE_NAME"
    sleep 1

    echo
    if systemctl is-active --quiet "$SERVICE_NAME"; then
      IP=$(hostname -I 2>/dev/null | awk '{print $1}')
      echo "== Running (systemd, root) =="
      echo "Local:   http://localhost:$PORT/"
      [[ -n "${IP:-}" ]] && echo "Network: http://$IP:$PORT/"
      echo
      echo "Manage it with:"
      echo "  systemctl status  $SERVICE_NAME"
      echo "  systemctl restart $SERVICE_NAME"
      echo "  journalctl -u $SERVICE_NAME -f"
      echo
      echo "Running as root means the Fan Control tab's pwmN writes work"
      echo "immediately - no udev rule needed. It also means anything on your"
      echo "LAN that can reach port $PORT can drive fans and kill processes,"
      echo "since there's no auth layer. Fine on a trusted home network."
      echo
      echo "Fan Control tab needs the nct6687d driver for write access (stock"
      echo "nct6683 is read-only): https://github.com/Fred78290/nct6687d"
      echo
      if [[ -n "$TERM_USER" ]]; then
        echo "Terminal tab: shell runs as '$TERM_USER' (sudo works normally,"
        echo "needs your password same as at a real terminal) - not root."
      else
        echo "Terminal tab: couldn't detect a real user to drop to, so it'll"
        echo "run as root. Re-run this installer as your normal user via sudo"
        echo "(not already-root) to fix that, or edit ExecStart in"
        echo "$SERVICE_FILE directly to add: --user yourusername"
      fi
    else
      echo "Service failed to start - check logs with:"
      echo "  journalctl -u $SERVICE_NAME -e"
      exit 1
    fi

  else
    echo
    echo "No systemd detected on this system - skipping service setup."
    echo "You'll need to wire it into whatever your distro's init system uses"
    echo "for autostart. To run it directly (as root, for fan control):"
    echo
    echo "  $INSTALL_DIR/bc250-panel --port $PORT$TERM_USER_FLAG &"
    echo
    echo "For OpenRC (Alpine/Gentoo), a minimal service script would go in"
    echo "/etc/init.d/ calling that same command with start-stop-daemon."
    echo "For runit (Void), a run script in /etc/sv/$SERVICE_NAME/run doing"
    echo "the same via: exec $INSTALL_DIR/bc250-panel --port $PORT$TERM_USER_FLAG"
    echo
    echo "Starting it now in the foreground so you can confirm it works:"
    echo "(Ctrl+C to stop, then set up autostart for your init system separately)"
    echo
    exec "$INSTALL_DIR/bc250-panel" --port "$PORT" $TERM_USER_FLAG
  fi
}

do_uninstall() {
  require_sudo uninstall
  echo "== BC-250 Control Suite uninstaller =="

  remove_old_user_service

  if command -v systemctl >/dev/null 2>&1 && [[ -f "$SERVICE_FILE" ]]; then
    echo "Stopping and disabling $SERVICE_NAME..."
    systemctl disable --now "$SERVICE_NAME" 2>/dev/null || true
    rm -f "$SERVICE_FILE"
    systemctl daemon-reload
    echo "Removed $SERVICE_FILE"
  else
    # No systemd, or it was run in the foreground - just make sure nothing's
    # still running.
    pkill -f "$INSTALL_DIR/bc250-panel" 2>/dev/null || true
  fi

  if [[ -d "$INSTALL_DIR" ]]; then
    rm -rf "$INSTALL_DIR"
    echo "Removed $INSTALL_DIR"
  fi

  echo "Done. bc250-panel is fully removed."
}

case "$ACTION" in
  install)   do_install ;;
  uninstall) do_uninstall ;;
  *)         usage ;;
esac
