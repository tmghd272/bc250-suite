# BC-250 Control Suite

ARGB fan controller + system sensor dashboard for a BC-250 running linux, with an ESP32-driven RGB controller for the case fans.

## Install

```bash
git clone https://github.com/tmghd272/bc250-suite.git
cd bc250-suite
chmod +x install_bc250_panel_go.sh
./install_bc250_panel_go.sh
```

## Components

| File | What it is |
|---|---|
| `bc250_argb_ble.ino` | ESP32 firmware — drives the ARGB LEDs, serves the recovery/management web console, handles BLE + WiFi control, E1.31 external control mode |
| `bc250_argb_panel.html` | The full control panel UI — RGB effects, gradient editor, system sensors dashboard. Talks to the ESP32 (BLE or WiFi) and to the BC-250's own sensor server |
| `bc250_panel_server.go` | BC-250-side server (Go, single static binary, no runtime dependency) — reads hwmon sensors directly and hosts the panel HTML itself on the LAN |
| `install_bc250_panel_go.sh` | Installer — builds the Go binary, installs it, sets up a systemd user service so it survives reboots |

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
# same folder as bc250_panel_server.go + bc250_argb_panel.html
./install_bc250_panel_go.sh
```

Builds the binary, installs to `~/.local/share/bc250-panel/`, sets up `systemctl --user` auto-start (falls back to manual instructions on non-systemd distros). Serves both the panel and `/api/sensors` from the same origin/port (default `8091`), so BLE only ever works from a browser on the BC-250 itself via `localhost` — remote origins can't get Web Bluetooth without a Chrome flag override (`chrome://flags/#unsafely-treat-insecure-origin-as-secure`), and even then it depends on the browser build. WiFi control mode has no such restriction and works from anywhere on the LAN.

To rebuild after any firmware/panel change:
```bash
go build -o ~/.local/share/bc250-panel/bc250-panel bc250_panel_server.go
systemctl --user restart bc250-panel
```

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
