# BC-250 Control Suite

ARGB fan controller + system sensor dashboard + fan PWM control + process manager + remote terminal for a BC-250 running Linux, with an ESP32-driven RGB controller for the case fans.

## Preview

<p align="center">
  <table border="0">
    <tr>
      <td align="center">
        <img src="images/BC-250 Sensor Monitor Panel Preview.png" alt="BC-250 Control Suite - desktop preview" width="800">
        <br><sub>Desktop</sub>
      </td>
      <td align="center">
        <img src="images/BC-250 Sensor Monitor Panel Mobile Preview.png" alt="BC-250 Control Suite - mobile preview" width="200">
        <br><sub>Mobile</sub>
      </td>
    </tr>
  </table>
</p>

## Install

```bash
git clone https://github.com/tmghd272/bc250-suite.git
cd bc250-suite
chmod +x bc250_panel_go.sh
./bc250_panel_go.sh install
```

To remove it later:
```bash
./bc250_panel_go.sh uninstall
```

## Components

| File | What it is |
|---|---|
| `bc250_argb_ble.ino` | ESP32 firmware — drives the ARGB LEDs, serves the recovery/management web console, handles BLE + WiFi control, E1.31 external control mode |
| `bc250_control_panel.html` | The control panel UI — RGB effects, gradient editor, system sensors dashboard, fan PWM control, process manager, remote terminal. Talks to the ESP32 (BLE or WiFi) and to the BC-250's own panel server |
| `bc250_panel_server.go` | BC-250-side server (Go) — reads hwmon sensors, controls every detected fan PWM channel (needs nct6687d), evaluates fan curves, lists/kills processes, hosts the panel HTML on the LAN |
| `bc250_terminal.go` | Terminal tab backend — a PTY-backed shell streamed over WebSocket (`/api/terminal`), using `creack/pty` + `gorilla/websocket` |
| `go.mod` / `go.sum` | Go module files for the Terminal tab's two dependencies. Still compiles to one static binary |
| `bc250_panel_go.sh` | Installer/uninstaller — `install` builds the Go binary and sets up a root systemd service; `uninstall` tears it down |

## Hardware

- ESP32 DevKit V1, GPIO16 → any WS2812-compatible ARGB LEDs (built/tested on 2 fans wired in parallel, 8 addressable LEDs — check your own strip's actual addressable count, some daisy-chain setups mirror rather than truly chain)
- isl69247 VRM chip on I2C bus 4 for combined power draw (BC-250-specific)
- Standard Linux hwmon sensors (`k10temp`, `amdgpu`, `nct6686`, `nvme`) via generic sysfs — not distro-specific. Built/tested on CachyOS

### ESP32 wiring

<p align="center">
  <img src="images/ARGB Fans ESP32 Wiring Diagram.png" alt="ESP32 to ARGB fans wiring diagram" width="600">
</p>

## Firmware setup

1. Flash `bc250_argb_ble.ino` via Arduino IDE (ESP32 board support required)
2. Partition scheme: **"Huge APP (3MB No OTA/1MB SPIFFS)"** — no OTA slot, maximizes code space. Firmware updates are USB-only; the recovery web console (WiFi AP, `192.168.4.1`) still works independently for router/mode/reset access
3. First boot creates a WiFi AP (`bc250-argb`) for initial setup; can also join your router via the panel or recovery console

## Panel setup

Just needs to be reachable in a browser — either opened locally or served by the Go binary. No build step, no dependencies.

## Sensor server (Go) setup

```bash
# same folder as bc250_panel_server.go + bc250_control_panel.html
./bc250_panel_go.sh install
```

Builds the binary, installs to `/opt/bc250-panel/`, and runs it as a **root** systemd service (`systemctl status bc250-panel`) — root is what lets Fan Control write to hwmon with no udev rule needed. Serves the panel plus `/api/sensors`, `/api/fan`, `/api/processes`, `/api/kill` from one origin/port (default `8091`). BLE only works from a browser on the BC-250 itself (`localhost`) — remote origins can't get Web Bluetooth without a Chrome flag override. WiFi control mode works from anywhere on the LAN.

Running as root means the panel's HTTP API has no auth layer — anything on your LAN that can reach the port can drive it. Fine for a trusted home network; worth knowing otherwise.

To rebuild after any change:
```bash
./bc250_panel_go.sh install   # safe to re-run - rebuilds and restarts the service
```

## Fan Control tab

Reads/writes every `pwmN`/`pwmN_enable` pair on the `nct6686`-named hwmon (the [nct6687d](https://github.com/Fred78290/nct6687d) driver) — not just `pwm1`. Multiple channels show up in a dropdown, defaulting to PWM 2 (CPU fan). Each channel gets its own Auto/Manual/Curve toggle and speed slider. The stock in-tree `nct6683` driver is read-only (no writable `pwmN`) — manual control needs nct6687d loaded, and the tab detects which one is present.

The dropdown labels each Linux `pwmN` with its BIOS Monitor-screen fan number, since `nct6686` renumbers them:

| BIOS Fan # | Linux `pwmN` |
|---|---|
| 1 (CPU_FAN1) | 2 |
| 2 | 3 |
| 3 | 4 |
| 4 | 5 |
| 5 | 1 |

**Reset All Fans to Stock** clears every saved curve and manual override and sets each `pwmN_enable` back to `2` (Auto).

### Fan curves

A per-channel temp → speed% curve, evaluated server-side every poll tick — same idea as [FanControl](https://github.com/Rem0o/FanControl.Releases), and it keeps running with the browser tab closed since it lives in the Go server. Pick a sensor source (CPU/APU/NVMe temp), build the curve on the graph (click to add a point, drag to move, click + remove to delete). Edits are staged — the same **Save Changes** button used for the manual slider commits pending curve edits too. **Enable on This Channel** needs at least 2 saved points. Curves persist to `bc250_fan_curves.json` next to the binary. Switching a channel back to Auto/Manual automatically disables its curve.

## Processes tab

Top processes by CPU%, RAM, PID, owning user, with Kill (SIGTERM) and a search box (name/PID/user). Column headers are clickable to sort. Reads `/proc` directly, no external dependency. Killing another user's process needs the server running as root; killing your own doesn't.

## Terminal tab

A real PTY-backed shell streamed over WebSocket at `/api/terminal`, rendered with [xterm.js](https://xtermjs.org/). Stays connected in the background across tab switches, only disconnecting when the page itself closes or reloads. Resizing the window resizes the actual PTY (`SIGWINCH`) over the same socket.

**Runs as a normal user, not root** — the installer captures whoever ran `sudo bash bc250_panel_go.sh install` (via `$SUDO_USER`) and passes it as `--user <name>` in the systemd unit. Falls back to root if no real user was detected (installer says so, and it's logged) — add `--user yourusername` manually to `ExecStart` in `/etc/systemd/system/bc250-panel.service` if that happens, then `systemctl daemon-reload && systemctl restart bc250-panel`.

**Still unauthenticated shell access to anyone who can reach this port on your LAN** — same tradeoff as fan control and process kill elsewhere in this panel. Fine on a trusted home network; don't port-forward it.

Runs the account's actual configured shell (fish, zsh, etc.), not a hardcoded bash. **Restart** kills the current shell and starts fresh; **Reconnect** only re-establishes a dropped socket. A 20s ping / 45s timeout cleans up orphaned shells after a hard-killed browser tab.

## Effects (32 total, index 0–31)

```
0  Solid            8  Theater 2-Color   16 Confetti        24 Sinelon
1  Rainbow          9  Scanner           17 Palette Flow    25 Popcorn
2  Breathe          10 Fire             18 Wipe Random      26 Ripple
3  Chase (comet)    11 Sparkle          19 Dual Scan        27 Glitter
4  Gradient         12 ColorLoop        20 Running Lights   28 Candle
5  Twinkle          13 Meteor           21 Saw              29 Heartbeat
6  Wipe             14 Bounce           22 Dissolve         30 Gradient Flow
7  Theater Chase    15 Strobe           23 Alert Flash      31 Pixel Colors
```

Gradient palette (2–6 stops) drives every multi-color effect. Effects are auto-categorized in the panel as needing the full palette editor, exactly 2 colors, exactly 1 color, or none at all (Rainbow/ColorLoop cycle their own hue wheel; Pixel Colors has its own independent per-LED grid).

## Firmware commands (BLE/WiFi)

`EFF` `CA` `CB` `PAL` `PIXELS` `BRI` `SPD` `PWR` `LEDCOUNT` `SYSMODE` `SAVE` `RESTART` `RESET` `WIFI` `WIFIOFF` `SENS` `TEMP`

## Known limitations / open items

- **CU (compute unit) live reading**: tries a direct `umr` masked register read first (needs root + the `umr` binary), falls back to `/tmp/bc250_cu_count` (written by [bc250-cu-count-passthru](https://github.com/tmghd272/bc250-cu-count-passthru) when this server isn't root), then falls back to `vulkaninfo`'s static total CU count.
- **No firmware OTA** — deliberate tradeoff for code space; USB reflash only.
- **BLE from external devices** is blocked by the Web Bluetooth secure-context requirement — browser/OS-level constraint, no code-side fix.
- Sensor values that can't be read are simply omitted from the API response, never faked/zeroed — the panel only creates a tile once a key shows up.

## Planned

- Decky Loader plugin (separate project, not started)
