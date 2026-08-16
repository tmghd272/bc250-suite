# BC-250 Control Suite

ARGB fan controller + system sensor dashboard + fan PWM control + process manager + remote terminal for a BC-250 running linux, with an ESP32-driven RGB controller for the case fans.

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
| `bc250_control_panel.html` | The full control panel UI — RGB effects, gradient editor, system sensors dashboard, fan PWM control (per-channel), process manager, remote terminal. Talks to the ESP32 (BLE or WiFi) and to the BC-250's own panel server. Opens on System Sensors by default and remembers whichever tab you were last on after that |
| `bc250_panel_server.go` | BC-250-side server (Go) — reads hwmon sensors directly, controls every detected fan header PWM channel (when nct6687d is loaded) including background temp→speed curve evaluation, lists/kills processes, and hosts the panel HTML itself on the LAN |
| `bc250_terminal.go` | The Terminal tab's backend — a real PTY-backed shell streamed over a WebSocket (`/api/terminal`), using `creack/pty` + `gorilla/websocket` |
| `go.mod` / `go.sum` | Go module files — needed because the Terminal tab pulls in the two small libraries above. Both still compile straight into one static binary; nothing extra is required on the machine at runtime |
| `bc250_panel_go.sh` | Installer/uninstaller — `install` builds the Go binary (via `go build .`, module-aware) and sets up a **root** systemd system service so it survives reboots and can actually write to hwmon; `uninstall` tears it down (and cleans up a prior --user-service install if it finds one) |

## Hardware

- ESP32 DevKit V1, GPIO16 → any WS2812-compatible ARGB LEDs (built/tested on 2 fans wired in parallel, 8 physically unique addressable LEDs — some fan daisy-chain setups mirror rather than truly chain, worth testing your own strip's actual addressable count rather than assuming)
- isl69247 VRM chip on I2C bus 4 for combined power draw (board-specific — BC-250)
- Standard Linux hwmon sensors (`k10temp`, `amdgpu`, `nct6686`, `nvme`) — read via generic sysfs, not distro-specific. Built/tested on CachyOS, but the Go binary itself has no distro dependency; label names are matched against actual driver output rather than hardcoded to one system

## Firmware setup

1. Flash `bc250_argb_ble.ino` via Arduino IDE (ESP32 board support required)
2. Partition scheme: **"Huge APP (3MB No OTA/1MB SPIFFS)"** — deliberately no OTA slot, maximizes code space. Firmware updates are USB-only by design; the recovery web console (WiFi AP hotspot, `192.168.4.1`) survives independently for router/mode/reset access even if something else breaks
3. First boot creates a WiFi AP (`bc250-argb`) for initial setup; can also join your router via the panel or recovery console

## Panel setup

Just needs to be reachable in a browser — either opened locally or served by the Go binary (see below). No build step, no dependencies.

## Sensor server (Go) setup

```bash
# same folder as bc250_panel_server.go + bc250_control_panel.html
./bc250_panel_go.sh install
```

Builds the binary, installs to `/opt/bc250-panel/`, and runs it as a **root system service** (`systemctl status bc250-panel`, not `--user`) — running as root is what lets the Fan Control tab write to hwmon without any udev rule. Falls back to plain manual-start instructions on non-systemd distros. Serves both the panel and `/api/sensors` (plus `/api/fan`, `/api/processes`, `/api/kill`) from the same origin/port (default `8091`), so BLE only ever works from a browser on the BC-250 itself via `localhost` — remote origins can't get Web Bluetooth without a Chrome flag override (`chrome://flags/#unsafely-treat-insecure-origin-as-secure`), and even then it depends on the browser build. WiFi control mode has no such restriction and works from anywhere on the LAN.

Running as root does mean the panel's HTTP API — including fan control and process kill — has no auth layer, so anything on your LAN that can reach the port can drive it. Fine for a trusted home network; worth knowing if this box sits anywhere less trusted.

To rebuild after any firmware/panel change:
```bash
./bc250_panel_go.sh install   # safe to re-run - rebuilds and restarts the service
```

## Fan Control tab

Reads/writes every `pwmN`/`pwmN_enable` pair found on the same `nct6686`-named hwmon (i.e. the [nct6687d](https://github.com/Fred78290/nct6687d) driver) that the sensor dashboard already reads fan RPM from — not just `pwm1`. If your board exposes multiple independent channels, they all show up in a dropdown (`PWM 1`, `PWM 2`, ...), defaulting to PWM 2 (where the CPU fan is routed) once channels are detected. Each channel gets its own Auto/Manual/Curve toggle and speed slider (drag, +/- step buttons, or type an exact number — the track fills to show the current value). The stock in-tree `nct6683` driver only exposes read-only monitoring for this chip — no writable `pwmN` files at all — so manual control needs nct6687d loaded; the tab detects which one (if either) is present and disables the controls with an explanation if it's not the controllable one.

Because the panel server now runs as root (see install above), writes to `pwmN`/`pwmN_enable` just work — no udev rule needed.

The dropdown also labels each Linux `pwmN` channel with its corresponding BIOS Monitor-screen fan number, since the `nct6686` driver renumbers them:

| BIOS Fan # | Linux `pwmN` |
|---|---|
| 1 (CPU_FAN1) | 2 |
| 2 | 3 |
| 3 | 4 |
| 4 | 5 |
| 5 | 1 |

**Reset All Fans to Stock** (top of the tab) clears every saved curve and manual override on every channel and sets each `pwmN_enable` back to `2` (Auto) — full BIOS-native behavior, undoing everything the panel has ever configured.

### Fan curves

A per-channel temp → speed% curve, evaluated server-side every poll tick — same idea as [FanControl](https://github.com/Rem0o/FanControl.Releases), and it keeps running even with the browser tab closed since the curve lives in the Go server, not the page. Pick a sensor source (CPU/APU/NVMe temp), then build the curve on the graph: click empty space to add a point, drag a point to move it, click to select then remove it. Edits are staged, not live — the same **Save Changes** button used for the manual slider commits any pending curve edits too, so there's one save action for the whole tab instead of several scattered ones. Hit **Enable on This Channel** once you have at least 2 saved points to actually turn the curve on for that channel. Curves persist to `bc250_fan_curves.json` next to the binary, so they survive a service restart. Switching a channel back to plain Auto or Manual from the mode toggle automatically disables its curve (so the two never fight over the same `pwmN` write).

## Processes tab

Lightweight task-manager view — top processes by CPU%, RAM, PID, owning user, with a Kill button (SIGTERM) and a search box (filters by name, PID, or user). Column headers (Name/PID/CPU/RAM) are clickable, Task Manager-style — click to sort by that column, click again to flip direction; numeric columns default to descending, Name defaults ascending. Rows keep stable identity across polls instead of a full rebuild, and re-sorting pauses entirely while your mouse is over the list — so reaching for a Kill button doesn't have the row jump or swap out from under the cursor mid-click; it resumes normal sorting once you move away. Reads `/proc` directly, no external dependency. Killing another user's process needs the server to be running as root; killing your own doesn't.

The background process-listing loop is wrapped in a panic recovery, since a process exiting mid-read (common with many short-lived shell children) can produce a truncated `/proc/[pid]/stat` read — that's now handled as a skip rather than a crash that used to take the whole server down and trigger a systemd restart loop.

## Terminal tab

A real shell in the browser — a PTY-backed login shell streamed over a WebSocket at `/api/terminal`, rendered with [xterm.js](https://xtermjs.org/) (loaded from a CDN, same as this panel already does for its fonts). Connects once when you open the tab and **stays connected in the background** when you switch to other tabs — it doesn't spawn a fresh shell every time you tab back in, which is what was producing a stacked-up `[root@host ~]#` prompt on every visit before. It only actually disconnects when the browser tab/page itself closes or reloads. Resizing the browser window resizes the actual PTY (`SIGWINCH` and all) via a small JSON control message sent over the same socket.

**The shell runs as a normal user, not root**, even though the panel server itself runs as root — the installer captures whoever ran `sudo bash bc250_panel_go.sh install` (via `$SUDO_USER`) and passes it as `--user <name>` in the systemd unit's `ExecStart`. The spawned shell gets that user's full UID/GID/supplementary-group set (so it's in `wheel`/`sudo` same as a real login), meaning `sudo` prompts for a password normally instead of already being root. If the installer couldn't detect a real user (e.g. it was run while already logged in as root), it falls back to root and says so both in its own output and in `journalctl` — edit `ExecStart` in `/etc/systemd/system/bc250-panel.service` to add `--user yourusername` manually if that happens, then `systemctl daemon-reload && systemctl restart bc250-panel`.

**This is still unauthenticated shell access to anyone who can reach this port on your LAN** — dropping to a normal user narrows the blast radius but doesn't add authentication. Same category of tradeoff already accepted for fan control and process kill elsewhere in this panel. Fine on a trusted home network; don't port-forward this port to the internet.

Runs the account's **actual configured shell** (fish, zsh, whatever's set in `/etc/passwd`), not a hardcoded bash — so shell-specific features like fish's inline history autosuggestions work exactly like they do at a real terminal on the same account, and whatever that shell's own config normally runs on an interactive login (a fastfetch/neofetch banner, etc.) shows up naturally without this panel needing to inject anything itself. **Restart** next to the connection status kills the current shell outright and starts a genuinely fresh one, clearing the screen too — different from **Reconnect**, which only re-establishes a dropped socket without touching an already-running session. The terminal's own scrollback no longer bleeds into scrolling the whole page once you hit the top/bottom, and has its own themed scrollbar to match the rest of the panel.

A hard-killed browser tab (force-quit, network drop, laptop put to sleep) can't rely on a clean disconnect message ever arriving, so the server also pings the connection every 20s and gives up on it — killing the shell — after 45s of silence. Combined with the normal close-on-page-unload path, this is what keeps orphaned shell processes from piling up over time.

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

- **CU (compute unit) live reading** relies on `/tmp/bc250_cu_count`, written by [bc250-cu-count-passthru](https://github.com/tmghd272/bc250-cu-count-passthru) (separate project, root-privileged `umr` register reads). Falls back to `vulkaninfo`'s static total CU count if that file's missing/invalid.
- **No firmware OTA** — deliberate tradeoff for code space; USB reflash only.
- **BLE from external devices** is blocked by the Web Bluetooth secure-context requirement; no code-side fix possible, browser/OS-level constraint.
- Sensor values that can't be read are meant to be simply omitted from the API response, never faked/zeroed/placeholder'd — the Go server only sends a key if its read actually succeeds, and the panel only creates a tile once a key shows up. This is how the code is written and confirmed by reading it, but the "sensor genuinely missing → tile just doesn't appear" scenario specifically was never stress-tested live on real hardware.

## Planned

- Decky Loader plugin (separate project, not started)
