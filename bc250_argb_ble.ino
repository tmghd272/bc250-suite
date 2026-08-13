/*
  BC-250 ARGB Controller v4
  --------------------------
  Drives WS2812/ARGB fan LEDs on GPIO16.

  v4 changes from v3:
    - Removed the per-fan dual-zone color system (was tied to daisy-chain
      wiring and caused sync bugs). Replaced with a simpler Color A / Color B
      model used contextually by whichever effect needs one or two colors.
    - Added a custom multi-stop gradient/palette (like Aura Sync / Armoury
      Crate's gradient picker) that several effects can animate through.
    - Added 8 new effects: Theater 2-Color, Meteor, Bounce, Strobe, Confetti,
      Palette Flow, on top of the v3 set. 31 effects total.
    - Speed changes are now idempotent even if a stale command arrives late
      (see SPD handling) - most of the "stuck on fast" issue was actually a
      web-panel bug (unthrottled slider spam), fixed there, but this adds a
      firmware-side safety net too.

  Control paths: BLE (Web Bluetooth panel), WiFi AP direct-connect, WiFi STA
  (home router, provisioned over BLE). Mode-exclusion logic unchanged from v3.

  Wiring:
    GPIO16 -> Fan DATA (DI)    [optional: a 330ohm series resistor here helps
                                 signal integrity on longer runs, but isn't
                                 required - works fine direct-wired on short runs]
    5V (VIN)  -> fan V+
    GND       -> shared GND

  Libraries needed (Arduino IDE > Tools > Manage Libraries):
    - FastLED (by Daniel Garcia)
  Everything else (WiFi, WebServer, BLE, Preferences, ESPmDNS) is built into
  the ESP32 core.

  Board setup: Tools > Board > "ESP32 Dev Module" (not "DOIT DEVKIT V1" -
  that profile hides the Partition Scheme menu you need).
  Tools > Partition Scheme > "Huge APP (3MB No OTA/1MB SPIFFS)"
*/

#include <FastLED.h>
#include <BLEDevice.h>
#include <BLEServer.h>
#include <BLEUtils.h>
#include <BLE2902.h>
#include <WiFi.h>
#include <WebServer.h>
#include <Preferences.h>
#include <ESPmDNS.h>
#include <DNSServer.h>
#include <WiFiUdp.h>

// ---------- CONFIG - EDIT THESE ----------
#define LED_PIN           16
#define MAX_LEDS          60      // compile-time buffer size - generous upper bound, safe to exceed real count
#define LED_TYPE          WS2812B
#define COLOR_ORDER       GRB
#define BLE_DEVICE_NAME   "BC250-ARGB"

#define AP_SSID           "BC250-ARGB-Direct"
#define AP_PASSWORD        "argbfans"   // min 8 chars, edit this

#define TEMP_MIN 30.0    // deg C mapped to "cool" end of Thermal gradient
#define TEMP_MAX 80.0    // deg C mapped to "hot" end of Thermal gradient
#define TEMP_STALE_MS 15000
// ------------------------------------------

#define SERVICE_UUID        "6e400001-b5a3-f393-e0a9-e50e24dcca9e"
#define CHARACTERISTIC_UUID "6e400002-b5a3-f393-e0a9-e50e24dcca9e"
#define STATUS_UUID         "6e400003-b5a3-f393-e0a9-e50e24dcca9e"

#define MAX_PALETTE_STOPS 6
#define PALETTE_LUT_SIZE  256

CRGB leds[MAX_LEDS];
uint16_t activeLedCount = 8; // real addressable count - confirmed 8 on this hardware (fans mirror each
                              // other, so 16 "positions" never existed as unique pixels). Runtime-configurable
                              // via LEDCOUNT command, persisted through saveLedState()/loadLedState().
CRGB pixelColors[MAX_LEDS]; // independent per-LED colors for the "Pixel Colors" effect - OpenRGB-style direct control
CRGB paletteLUT[PALETTE_LUT_SIZE]; // precomputed gradient built from user's palette stops
Preferences prefs;
Preferences ledPrefs; // separate object/namespace from wifi_cfg so they don't collide
WebServer server(80);
DNSServer dnsServer;
#define DNS_PORT 53

// ---------------- E1.31 (sACN) receiver - external system mode ----------------
// This is a completely separate subsystem from the WebServer management layer
// (restart/WiFi/OTA/etc all keep working no matter which system mode is active).
// It's also WiFi-only by nature - E1.31 is a UDP network protocol, there's no
// way to run it over BLE.
WiFiUDP e131Udp;
#define E131_PORT 5568
#define SYSMODE_NATIVE 0
#define SYSMODE_E131   1
uint8_t systemMode = SYSMODE_NATIVE;
unsigned long lastE131Packet = 0;
#define E131_STALE_MS 5000 // purely informational now - only affects the panel's "live/stale" status badge, not what the LEDs actually show

BLECharacteristic *pCharacteristic;
BLECharacteristic *pStatusCharacteristic;
BLEServer *pServer;
bool deviceConnected  = false;
bool bleAdvertising   = false;

// Live sensor telemetry (pushed in from the host script via SENS,key,value)
// Genuinely dynamic - any key you send just shows up, no firmware changes needed.
#define MAX_SENSORS 10
struct SensorEntry { String key; float value; unsigned long updatedAt; };
SensorEntry sensorTable[MAX_SENSORS];
uint8_t sensorCount = 0;
#define SENS_STALE_MS 15000

int findOrCreateSensor(const String &key) {
  for (uint8_t i = 0; i < sensorCount; i++) if (sensorTable[i].key == key) return i;
  if (sensorCount < MAX_SENSORS) { sensorTable[sensorCount].key = key; return sensorCount++; }
  return -1; // table full
}

float getSensorValue(const String &key) {
  for (uint8_t i = 0; i < sensorCount; i++) if (sensorTable[i].key == key) return sensorTable[i].value;
  return NAN;
}

unsigned long getSensorAge(const String &key) {
  for (uint8_t i = 0; i < sensorCount; i++) if (sensorTable[i].key == key) return millis() - sensorTable[i].updatedAt;
  return 0xFFFFFFFF;
}

// WiFi state
String   staSSID = "", staPass = "";
bool     staCredsSet   = false;
bool     staWasConnectedBeforeBLE = false;
unsigned long lastWifiPoll = 0;

// Live LED state
bool     powerOn     = true;
uint8_t  brightness  = 128;
CRGB     colorA = CRGB(0x4d,0xe3,0xd1);
CRGB     colorB = CRGB(0xff,0x2e,0x63);
uint8_t  effect       = 0;   // 0-21, see EFFECT LIST below
uint8_t  effectSpeed  = 10;  // 1-20
uint32_t speedCmdSeq  = 0;   // increments each SPD command - lets us ignore stale/late writes

CRGB     paletteStops[MAX_PALETTE_STOPS];
uint8_t  paletteCount = 2; // defaults to colorA -> colorB until user defines a custom palette

// LED state persistence - explicit save only (not auto-save on every change,
// to avoid wearing out flash with constant writes while dragging sliders etc).
// Without this, every power cycle reset back to the hardcoded defaults below
// (which is why it always came back to that aqua color - that IS the default).
void saveLedState() {
  ledPrefs.begin("led_cfg", false);
  ledPrefs.putUChar("caR", colorA.r); ledPrefs.putUChar("caG", colorA.g); ledPrefs.putUChar("caB", colorA.b);
  ledPrefs.putUChar("cbR", colorB.r); ledPrefs.putUChar("cbG", colorB.g); ledPrefs.putUChar("cbB", colorB.b);
  ledPrefs.putUChar("effect", effect);
  ledPrefs.putUChar("bri", brightness);
  ledPrefs.putUChar("spd", effectSpeed);
  ledPrefs.putBool("pwr", powerOn);
  ledPrefs.putUShort("ledCount", activeLedCount);
  ledPrefs.putUChar("sysMode", systemMode);
  // Full palette (was only ever saving colorA/colorB before - any 3rd+ stop
  // silently vanished on every reboot, which is exactly what was reported).
  ledPrefs.putUChar("palCount", paletteCount);
  for (uint8_t i = 0; i < paletteCount; i++) {
    String key = "pal" + String(i);
    ledPrefs.putUChar((key + "r").c_str(), paletteStops[i].r);
    ledPrefs.putUChar((key + "g").c_str(), paletteStops[i].g);
    ledPrefs.putUChar((key + "b").c_str(), paletteStops[i].b);
  }
  ledPrefs.putBool("saved", true);
  ledPrefs.end();
  Serial.println("[LED] State saved to flash");
}

void loadLedState() {
  ledPrefs.begin("led_cfg", true); // read-only
  if (ledPrefs.getBool("saved", false)) {
    colorA = CRGB(ledPrefs.getUChar("caR"), ledPrefs.getUChar("caG"), ledPrefs.getUChar("caB"));
    colorB = CRGB(ledPrefs.getUChar("cbR"), ledPrefs.getUChar("cbG"), ledPrefs.getUChar("cbB"));
    effect = ledPrefs.getUChar("effect", 0);
    if (effect > 31) effect = 0; // guard against a stale/corrupt value pointing past the current effect list
    brightness = ledPrefs.getUChar("bri", 128);
    effectSpeed = ledPrefs.getUChar("spd", 10);
    powerOn = ledPrefs.getBool("pwr", true);
    activeLedCount = ledPrefs.getUShort("ledCount", 8);
    if (activeLedCount < 1 || activeLedCount > MAX_LEDS) activeLedCount = 8; // guard against corrupt/out-of-range saved values
    systemMode = ledPrefs.getUChar("sysMode", SYSMODE_NATIVE);

    uint8_t savedPalCount = ledPrefs.getUChar("palCount", 0);
    if (savedPalCount >= 2 && savedPalCount <= MAX_PALETTE_STOPS) {
      for (uint8_t i = 0; i < savedPalCount; i++) {
        String key = "pal" + String(i);
        paletteStops[i] = CRGB(
          ledPrefs.getUChar((key + "r").c_str(), colorA.r),
          ledPrefs.getUChar((key + "g").c_str(), colorA.g),
          ledPrefs.getUChar((key + "b").c_str(), colorA.b)
        );
      }
      paletteCount = savedPalCount;
    } else {
      // Older saved state from before palette persistence existed - fall back
      // to the 2-color version rather than leaving the array uninitialized.
      paletteStops[0] = colorA; paletteStops[1] = colorB; paletteCount = 2;
    }
    Serial.println("[LED] Restored saved state from flash");
  } else {
    Serial.println("[LED] No saved state yet - using defaults until Save is pressed");
    paletteStops[0] = colorA; paletteStops[1] = colorB; paletteCount = 2; // safe fallback - without this, paletteStops stayed at its zero-initialized (black) default
  }
  ledPrefs.end();
}

/* EFFECT LIST
   0 Solid        1 Rainbow      2 Breathe       3 Chase (comet)
   4 Gradient     5 Twinkle      6 Wipe          7 Theater Chase
   8 Theater 2-Color              9 Scanner      10 Fire
   11 Sparkle     12 ColorLoop   13 Meteor       14 Bounce
   15 Strobe      16 Confetti    17 Palette Flow 18 Wipe Random
   19 Dual Scan   20 Running Lights 21 Saw       22 Dissolve
   23 Alert Flash 24 Sinelon     25 Popcorn      26 Ripple
   27 Glitter     28 Candle      29 Heartbeat    30 Gradient Flow
   31 Pixel Colors
*/

const uint16_t TICK_MS = 16;
unsigned long lastTick = 0;

uint8_t  huePhase     = 0;
uint8_t  breathePhase = 0;
uint16_t chasePos     = 0;
uint16_t wipePos      = 0;
int8_t   wipeDir      = 1;
uint16_t scannerPos   = 0;
int8_t   scannerDir   = 1;
uint8_t  loopHue      = 0;
uint16_t meteorPos    = 0;
uint16_t bouncePos    = 0;
int8_t   bounceDir    = 1;
bool     bounceUseB   = false;
bool     strobeOn     = false;
uint16_t flowPhase    = 0;
CRGB     fireHeat[MAX_LEDS];
uint16_t dualScanPos1 = 0, dualScanPos2 = activeLedCount - 1;
int8_t   dualScanDir1 = 1, dualScanDir2 = -1;
uint16_t runningPhase = 0, sawPhase = 0;
CRGB     dissolveState[MAX_LEDS];
bool     policePhase = false;
float    sinelonPos = 0;
int8_t   sinelonDir = 1;
CRGB     popcornState[MAX_LEDS];
float    ripplePos = 0;
int16_t  rippleOrigin = -1;
uint8_t  heartbeatPhase = 0;
uint16_t gradFlowPhase  = 0;

void handleCommand(String cmd);

String colorHex(CRGB c) {
  char buf[7];
  sprintf(buf, "%02x%02x%02x", c.r, c.g, c.b);
  return String(buf);
}

String buildStatusJson() {
  unsigned long now = millis();
  String j = "{\"sensors\":[";
  for (uint8_t i = 0; i < sensorCount; i++) {
    if (i > 0) j += ",";
    j += "{\"k\":\"" + sensorTable[i].key + "\",";
    j += "\"v\":" + String(sensorTable[i].value, 1) + ",";
    j += "\"a\":" + String((now - sensorTable[i].updatedAt) / 1000) + "}";
  }
  j += "],";
  // Actual current LED state - lets the panel sync its UI to reality on
  // connect/reconnect instead of always assuming JS defaults, which was the
  // "preview doesn't match after restart" complaint.
  j += "\"led\":{";
  j += "\"power\":" + String(powerOn ? "true" : "false") + ",";
  j += "\"effect\":" + String(effect) + ",";
  j += "\"brightness\":" + String(brightness) + ",";
  j += "\"speed\":" + String(effectSpeed) + ",";
  j += "\"ledCount\":" + String(activeLedCount) + ",";
  j += "\"sysMode\":\"" + String(systemMode == SYSMODE_E131 ? "E131" : "NATIVE") + "\",";
  j += "\"colorA\":\"" + colorHex(colorA) + "\",";
  j += "\"colorB\":\"" + colorHex(colorB) + "\",";
  j += "\"palette\":[";
  for (uint8_t i = 0; i < paletteCount; i++) {
    if (i > 0) j += ",";
    j += "\"" + colorHex(paletteStops[i]) + "\"";
  }
  j += "]}";
  j += "}";
  return j;
}

void pushStatus() {
  if (pStatusCharacteristic == nullptr) return;
  String j = buildStatusJson();
  pStatusCharacteristic->setValue(j.c_str());
  if (deviceConnected) pStatusCharacteristic->notify();
}

CRGB hexToColor(String hex) {
  long rgb = strtol(hex.c_str(), NULL, 16);
  return CRGB((rgb >> 16) & 0xFF, (rgb >> 8) & 0xFF, rgb & 0xFF);
}

// Parses one incoming E1.31 (sACN) packet and paints the LEDs directly from
// its DMX channel data, bypassing the native effects engine entirely. Standard
// E1.31 packet layout: 126-byte header (root + framing + DMP layers) followed
// by up to 512 bytes of DMX channel data. We only care about the universe
// number (for a sanity check) and the raw channel bytes - 3 per LED (R,G,B).
#define E131_HEADER_SIZE 126
void handleE131() {
  int packetSize = e131Udp.parsePacket();
  if (packetSize <= 0) return;

  uint8_t buf[638]; // 126 header + up to 512 channels
  int len = e131Udp.read(buf, sizeof(buf));
  if (len < E131_HEADER_SIZE + 1) return; // too short to be a real E1.31 packet

  // Universe number sits at a fixed offset in the framing layer.
  uint16_t universe = (buf[113] << 8) | buf[114];
  (void)universe; // not currently filtering by universe - single-universe setup assumed

  uint8_t startCode = buf[125];
  if (startCode != 0) return; // non-zero start code = not plain DMX data, ignore

  int dmxLen = len - E131_HEADER_SIZE;
  int ledsAvailable = dmxLen / 3;
  int count = min((int)activeLedCount, ledsAvailable);

  for (int i = 0; i < count; i++) {
    int base = E131_HEADER_SIZE + i * 3;
    leds[i] = CRGB(buf[base], buf[base + 1], buf[base + 2]);
  }
  // If the sender is driving fewer channels than we have LEDs, leave the rest
  // as whatever they last were rather than guessing - avoids flicker on the tail.

  lastE131Packet = millis();
  FastLED.show();
}

// Rebuilds the 256-entry lookup table from the current palette stops,
// evenly spaced and linearly interpolated. Called whenever the palette changes.
void rebuildPaletteLUT() {
  if (paletteCount < 2) {
    for (int i = 0; i < PALETTE_LUT_SIZE; i++) paletteLUT[i] = colorA;
    return;
  }
  float segLen = (float)PALETTE_LUT_SIZE / (paletteCount - 1);
  for (int i = 0; i < PALETTE_LUT_SIZE; i++) {
    float pos = i / segLen;
    int seg = (int)pos;
    if (seg >= paletteCount - 1) seg = paletteCount - 2;
    uint8_t blend = (uint8_t)((pos - seg) * 255);
    paletteLUT[i] = CRGB(
      lerp8by8(paletteStops[seg].r, paletteStops[seg+1].r, blend),
      lerp8by8(paletteStops[seg].g, paletteStops[seg+1].g, blend),
      lerp8by8(paletteStops[seg].b, paletteStops[seg+1].b, blend)
    );
  }
}

// ---------------- WiFi / BLE exclusion state machine ----------------

void wifiSuspendAll(bool rememberStaState) {
  if (rememberStaState) staWasConnectedBeforeBLE = (WiFi.status() == WL_CONNECTED);
  dnsServer.stop();
  WiFi.softAPdisconnect(true);
  WiFi.disconnect(false, false);
  Serial.println("[MODE] WiFi suspended (BLE active)");
}

void wifiResumeAll() {
  WiFi.softAP(AP_SSID, AP_PASSWORD);
  dnsServer.start(DNS_PORT, "*", WiFi.softAPIP());
  if (staCredsSet && staWasConnectedBeforeBLE) {
    WiFi.begin(staSSID.c_str(), staPass.c_str());
    Serial.println("[MODE] WiFi resumed - reconnecting STA + AP back up");
  } else {
    Serial.println("[MODE] WiFi resumed - AP back up");
  }
}

void bleStopAdvertisingAndKick() {
  if (deviceConnected && pServer != nullptr) pServer->disconnect(0);
  BLEDevice::stopAdvertising();
  bleAdvertising = false;
  Serial.println("[MODE] BLE disabled (AP client present)");
}

void bleResumeAdvertising() {
  if (!bleAdvertising) {
    BLEDevice::startAdvertising();
    bleAdvertising = true;
    Serial.println("[MODE] BLE advertising resumed");
  }
}

volatile bool pendingWifiSuspend = false;
volatile bool pendingRestart = false;
unsigned long restartDueAt = 0;
volatile bool pendingWifiResume  = false;
unsigned long wifiSuspendDueAt = 0;
#define BLE_SETTLE_MS 2000 // grace period after connect before tearing WiFi down,
                            // so service discovery on the browser side has time to finish

class ServerCallbacks: public BLEServerCallbacks {
  void onConnect(BLEServer* srv) override {
    deviceConnected = true;
    Serial.println("[MODE] BLE client connected -> WiFi suspend queued (delayed)");
    pendingWifiSuspend = true;
    wifiSuspendDueAt = millis() + BLE_SETTLE_MS;
  }
  void onDisconnect(BLEServer* srv) override {
    deviceConnected = false;
    Serial.println("[MODE] BLE client disconnected -> WiFi resume queued");
    pendingWifiResume = true;
    srv->getAdvertising()->start();
    bleAdvertising = true;
  }
};

class CommandCallbacks: public BLECharacteristicCallbacks {
  void onWrite(BLECharacteristic *pChar) override {
    String value = pChar->getValue().c_str();
    if (value.length() == 0) return;
    handleCommand(value);
  }
};

void handleCommand(String cmd) {
  cmd.trim();
  int comma = cmd.indexOf(',');
  String type = comma == -1 ? cmd : cmd.substring(0, comma);
  String rest = comma == -1 ? ""  : cmd.substring(comma + 1);

  Serial.print("RX: "); Serial.println(cmd);

  if (type == "PWR")      powerOn = (rest.toInt() == 1);
  else if (type == "BRI") { brightness = constrain(rest.toInt(), 0, 255); FastLED.setBrightness(brightness); }
  else if (type == "CA")  { colorA = hexToColor(rest); if (paletteCount == 2) { paletteStops[0] = colorA; rebuildPaletteLUT(); } }
  else if (type == "CB")  { colorB = hexToColor(rest); if (paletteCount == 2) { paletteStops[1] = colorB; rebuildPaletteLUT(); } }
  else if (type == "EFF") effect = constrain(rest.toInt(), 0, 31);
  else if (type == "SPD") {
    effectSpeed = constrain(rest.toInt(), 1, 20);
    speedCmdSeq++; // marks this as the latest intended value
  }
  else if (type == "PAL") {
    // format: PAL,hex1,hex2,...  (2-6 stops)
    paletteCount = 0;
    int start = 0;
    while (paletteCount < MAX_PALETTE_STOPS) {
      int next = rest.indexOf(',', start);
      String hex = (next == -1) ? rest.substring(start) : rest.substring(start, next);
      if (hex.length() > 0) { paletteStops[paletteCount++] = hexToColor(hex); }
      if (next == -1) break;
      start = next + 1;
    }
    if (paletteCount < 2) { paletteStops[0] = colorA; paletteStops[1] = colorB; paletteCount = 2; }
    rebuildPaletteLUT();
  }
  else if (type == "TEMP") { // legacy alias for SENS,cpu,x
    int idx = findOrCreateSensor("cpu");
    if (idx >= 0) { sensorTable[idx].value = rest.toFloat(); sensorTable[idx].updatedAt = millis(); pushStatus(); }
  }
  else if (type == "SENS") {
    // format: SENS,key,value  - key can be anything, table grows dynamically (up to MAX_SENSORS)
    int c2 = rest.indexOf(',');
    if (c2 != -1) {
      String key = rest.substring(0, c2);
      float val = rest.substring(c2 + 1).toFloat();
      int idx = findOrCreateSensor(key);
      if (idx >= 0) { sensorTable[idx].value = val; sensorTable[idx].updatedAt = millis(); pushStatus(); }
    }
  }
  else if (type == "WIFI") {
    int c2 = rest.indexOf(',');
    if (c2 != -1) {
      staSSID = rest.substring(0, c2);
      staPass = rest.substring(c2 + 1);
      staCredsSet = true;
      prefs.putString("ssid", staSSID);
      prefs.putString("pass", staPass);
      Serial.print("[WIFI] Credentials saved for SSID: "); Serial.println(staSSID);
      if (!deviceConnected) WiFi.begin(staSSID.c_str(), staPass.c_str());
      staWasConnectedBeforeBLE = true;
    }
  }
  else if (type == "WIFIOFF") {
    // Deliberately drop the router connection. AP is untouched - it keeps
    // broadcasting regardless, so this can never strand the device unreachable.
    WiFi.disconnect(false, false);
    staWasConnectedBeforeBLE = false; // don't auto-reconnect on next BLE disconnect cycle
    Serial.println("[WIFI] Disconnected from router by request (AP unaffected)");
  }
  else if (type == "SAVE") {
    saveLedState();
  }
  else if (type == "LEDCOUNT") {
    int n = rest.toInt();
    if (n >= 1 && n <= MAX_LEDS) {
      if (n < activeLedCount) { for (int i = n; i < activeLedCount; i++) leds[i] = CRGB::Black; FastLED.show(); } // clear anything beyond the new (smaller) count
      activeLedCount = n;
      rebuildPaletteLUT(); // some effects size things off the LED count
      Serial.print("[LED] Active LED count set to "); Serial.println(activeLedCount);
    }
  }
  else if (type == "PIXELS") {
    // format: PIXELS,hex1,hex2,...,hexN - one color per LED, same comma-list
    // pattern as the PAL command. Missing entries at the end just stay whatever
    // they were before (no need to resend the whole strip every time).
    int i = 0, start = 0;
    while (i < MAX_LEDS) {
      int next = rest.indexOf(',', start);
      String hex = (next == -1) ? rest.substring(start) : rest.substring(start, next);
      if (hex.length() > 0) { pixelColors[i++] = hexToColor(hex); }
      if (next == -1) break;
      start = next + 1;
    }
  }
  else if (type == "RESET") {
    // Factory reset - wipes LED profile, system mode, and saved WiFi credentials.
    // Used by the recovery webpage so a bad config can always be cleared without
    // physically re-flashing or opening the case.
    ledPrefs.begin("led_cfg", false); ledPrefs.clear(); ledPrefs.end();
    prefs.begin("wifi_cfg", false); prefs.clear(); prefs.end();
    Serial.println("[SYS] Factory reset requested - restarting in ~500ms");
    pendingRestart = true;
    restartDueAt = millis() + 500;
  }
  else if (type == "RESTART") {
    Serial.println("[SYS] Restart requested from panel - restarting in ~500ms");
    pendingRestart = true;
    restartDueAt = millis() + 500; // give the response time to actually reach the client first
  }
  else if (type == "SYSMODE") {
    if (rest == "E131") {
      systemMode = SYSMODE_E131;
      lastE131Packet = 0; // force "waiting" state until the first real packet arrives
      Serial.println("[SYS] Switched to E1.31 external control mode - native effects paused");
    } else {
      systemMode = SYSMODE_NATIVE;
      Serial.println("[SYS] Switched to Native mode - web panel effects resumed");
    }
  }
}

// ---------------- Simple HTTP control page for AP/STA (Web Bluetooth won't work here) ----------------
const char HTTP_PAGE[] PROGMEM = R"HTML(
<!DOCTYPE html><html><head><meta name=viewport content="width=device-width,initial-scale=1">
<title>BC-250 Recovery Console</title>
<style>
body{background:#0d0f0d;color:#e9e6da;font-family:-apple-system,sans-serif;padding:20px;max-width:440px;margin:auto}
h2{color:#ffb000;font-size:18px} .sub{color:#8d8f85;font-size:12px;margin-bottom:20px}
label{display:block;margin-top:16px;font-size:11px;text-transform:uppercase;letter-spacing:0.05em;color:#8d8f85}
input[type=text],input[type=password],input[type=file]{width:100%;box-sizing:border-box;background:#1a1c19;border:1px solid #444;color:#e9e6da;padding:8px;border-radius:6px;margin-top:4px}
button{background:#3a3c37;color:#e9e6da;border:1px solid #000;padding:10px 14px;margin:4px 4px 4px 0;border-radius:8px;cursor:pointer;font-size:13px}
button.danger{background:#4a2a26;color:#ffb0a3}
button.primary{background:#2a4a3e;color:#7ee8c0}
.status{font-size:12px;color:#7ee8c0;margin-top:8px;min-height:16px}
.card{background:#161815;border:1px solid #2a2c28;border-radius:10px;padding:14px 16px;margin-top:16px}
</style></head><body>
<h2>BC-250 Recovery Console</h2>
<div class="sub">Always-on management page, independent of the main ARGB panel. If a flash goes bad or the main panel breaks, this stays reachable via the AP hotspot (192.168.4.1) or the router IP.</div>

<div class="card">
  <label>System Mode</label>
  <button class="primary" onclick="cmd('SYSMODE,NATIVE')">Native (Web Panel)</button>
  <button onclick="cmd('SYSMODE,E131')">E1.31 (External)</button>
</div>

<div class="card">
  <label>Router Connection</label>
  <input type=text id=ssid placeholder="Router SSID">
  <input type=password id=pass placeholder="Router password">
  <button class="primary" onclick="connectRouter()">Connect to Router</button>
  <button class="danger" onclick="cmd('WIFIOFF')">Disconnect from Router</button>
  <div class="status" id=wifiStatus>checking…</div>
</div>

<div class="card">
  <label>Device</label>
  <button onclick="if(confirm('Restart the ESP32?'))cmd('RESTART')">Restart</button>
  <button class="danger" onclick="if(confirm('Factory reset? This clears saved colors/effect, system mode, and WiFi credentials.'))cmd('RESET')">Factory Reset</button>
</div>

<script>
function cmd(c){ return fetch('/cmd?c='+encodeURIComponent(c)); }
function connectRouter(){
  const s=document.getElementById('ssid').value, p=document.getElementById('pass').value;
  if(!s){alert('Enter a network name');return;}
  cmd('WIFI,'+s+','+p);
  document.getElementById('wifiStatus').textContent='sent — connecting…';
}
async function pollStatus(){
  try{
    const res=await fetch('/wifistatus',{cache:'no-store'});
    const s=await res.json();
    document.getElementById('wifiStatus').textContent = s.staConnected
      ? ('Router: '+s.staSsid+' ('+s.staIp+')  ·  AP: '+s.apClients+' client(s)')
      : ('Router: not connected  ·  AP: "'+s.apSsid+'" active, '+s.apClients+' client(s)');
  }catch(e){ document.getElementById('wifiStatus').textContent='can\'t read status'; }
}
pollStatus(); setInterval(pollStatus, 4000);
</script></body></html>
)HTML";

void setupWebServer() {
  server.on("/", HTTP_GET, []() {
    server.send_P(200, "text/html", HTTP_PAGE);
  });
  server.on("/cmd", HTTP_GET, []() {
    server.sendHeader("Access-Control-Allow-Origin", "*");
    if (server.hasArg("c")) handleCommand(server.arg("c"));
    server.send(200, "text/plain", "OK");
  });
  server.on("/status", HTTP_GET, []() {
    server.sendHeader("Access-Control-Allow-Origin", "*");
    server.send(200, "application/json", buildStatusJson());
  });
  server.on("/wifistatus", HTTP_GET, []() {
    server.sendHeader("Access-Control-Allow-Origin", "*");
    bool staConnected = (WiFi.status() == WL_CONNECTED);
    unsigned long e131Age = lastE131Packet ? (millis() - lastE131Packet) : 0xFFFFFFFF;
    String j = "{";
    j += "\"apActive\":true,"; // AP is always up in this firmware unless a BLE client is connected
    j += "\"apSsid\":\"" + String(AP_SSID) + "\",";
    j += "\"apIp\":\"" + WiFi.softAPIP().toString() + "\",";
    j += "\"apClients\":" + String(WiFi.softAPgetStationNum()) + ",";
    j += "\"staConnected\":" + String(staConnected ? "true" : "false") + ",";
    j += "\"staSsid\":\"" + (staConnected ? WiFi.SSID() : String("")) + "\",";
    j += "\"staIp\":\"" + (staConnected ? WiFi.localIP().toString() : String("")) + "\",";
    j += "\"sysMode\":\"" + String(systemMode == SYSMODE_E131 ? "E131" : "NATIVE") + "\",";
    j += "\"e131Live\":" + String((systemMode == SYSMODE_E131 && e131Age < E131_STALE_MS) ? "true" : "false");
    j += "}";
    server.send(200, "application/json", j);
  });

  // --- Captive portal probes ---
  // Each OS pings a specific URL expecting a specific "internet is fine" response.
  // Answering with our own page instead (or a non-matching response) is what makes
  // the OS conclude "this network needs sign-in" and auto-open the browser.
  server.on("/hotspot-detect.html", HTTP_GET, []() { server.send_P(200, "text/html", HTTP_PAGE); }); // Apple/iOS/macOS
  server.on("/generate_204", HTTP_GET, []() { server.send_P(200, "text/html", HTTP_PAGE); });        // Android
  server.on("/gen_204", HTTP_GET, []() { server.send_P(200, "text/html", HTTP_PAGE); });              // Android (alt)
  server.on("/connecttest.txt", HTTP_GET, []() { server.send_P(200, "text/html", HTTP_PAGE); });      // Windows NCSI
  server.on("/ncsi.txt", HTTP_GET, []() { server.send_P(200, "text/html", HTTP_PAGE); });             // Windows (alt)
  server.on("/library/test/success.html", HTTP_GET, []() { server.send_P(200, "text/html", HTTP_PAGE); }); // Apple (alt)

  // Catch-all: any other unmatched path also gets our page, so a probe URL we
  // didn't explicitly list still resolves to something useful instead of a 404.
  server.onNotFound([]() {
    server.send_P(200, "text/html", HTTP_PAGE);
  });

  server.begin();
}

void setup() {
  Serial.begin(115200);

  FastLED.addLeds<LED_TYPE, LED_PIN, COLOR_ORDER>(leds, MAX_LEDS);

  loadLedState(); // restores colors/effect/brightness/speed/power/palette, or defaults if none saved yet
  FastLED.setBrightness(brightness);

  // paletteStops/paletteCount are already set correctly by loadLedState() above
  // (either the real saved palette, or the 2-color colorA/colorB fallback) -
  // this used to unconditionally overwrite them back to just 2 colors here,
  // which is exactly why saved 3+ stop gradients never survived a reboot.
  rebuildPaletteLUT();
  for (uint16_t i = 0; i < MAX_LEDS; i++) pixelColors[i] = colorA; // default starting point for the Pixel Colors effect
  fill_solid(leds, activeLedCount, colorA);
  FastLED.show();

  prefs.begin("wifi_cfg", false);
  staSSID = prefs.getString("ssid", "");
  staPass = prefs.getString("pass", "");
  staCredsSet = staSSID.length() > 0;

  WiFi.mode(WIFI_AP_STA);
  WiFi.softAP(AP_SSID, AP_PASSWORD);
  Serial.print("[WIFI] AP started: "); Serial.println(AP_SSID);
  Serial.print("[WIFI] AP IP: "); Serial.println(WiFi.softAPIP());

  dnsServer.start(DNS_PORT, "*", WiFi.softAPIP()); // captive portal: every domain resolves to us
  Serial.println("[WIFI] Captive portal DNS active - connecting phones/PCs to the AP should auto-prompt to sign in");

  // ESP32's mDNS responder doesn't automatically pick up a STA interface that
  // connects AFTER mDNS.begin() was first called - it needs to be re-announced
  // once the router link actually comes up, or the hostname silently stops
  // resolving until a full reboot. This event handler is what fixes that.
  WiFi.onEvent([](WiFiEvent_t event) {
    if (event == ARDUINO_EVENT_WIFI_STA_GOT_IP) {
      Serial.print("[WIFI] STA got IP: "); Serial.println(WiFi.localIP());
      MDNS.end();
      if (MDNS.begin("bc250-argb")) {
        Serial.println("[MDNS] Re-announced on new STA interface - bc250-argb.local should resolve now");
      }
    }
  });

  if (staCredsSet) {
    WiFi.begin(staSSID.c_str(), staPass.c_str());
    Serial.print("[WIFI] Attempting STA connect to: "); Serial.println(staSSID);
  }

  setupWebServer();
  if (MDNS.begin("bc250-argb")) {
    Serial.println("[MDNS] Reachable as: http://bc250-argb.local");
  }

  e131Udp.begin(E131_PORT);
  Serial.print("[E1.31] Listening on UDP port "); Serial.println(E131_PORT);

  BLEDevice::init(BLE_DEVICE_NAME);
  // Default BLE ATT MTU is 23 bytes (20 usable after the header) - way too small
  // for commands like PAL with 3+ color stops ("PAL,4de3d1,ff2e63,ffffff" alone
  // is already 24 bytes) or long WiFi passwords. Writes beyond the negotiated
  // MTU get silently truncated, not rejected, which is exactly why 3rd+ palette
  // stops looked broken - the command was arriving cut off. Requesting a much
  // larger MTU here fixes this at the source.
  BLEDevice::setMTU(247);
  pServer = BLEDevice::createServer();
  pServer->setCallbacks(new ServerCallbacks());

  BLEService *pService = pServer->createService(SERVICE_UUID);
  pCharacteristic = pService->createCharacteristic(
                      CHARACTERISTIC_UUID,
                      BLECharacteristic::PROPERTY_READ |
                      BLECharacteristic::PROPERTY_WRITE |
                      BLECharacteristic::PROPERTY_NOTIFY
                    );
  pCharacteristic->addDescriptor(new BLE2902());
  pCharacteristic->setCallbacks(new CommandCallbacks());

  pStatusCharacteristic = pService->createCharacteristic(
                      STATUS_UUID,
                      BLECharacteristic::PROPERTY_READ |
                      BLECharacteristic::PROPERTY_NOTIFY
                    );
  pStatusCharacteristic->addDescriptor(new BLE2902());
  pStatusCharacteristic->setValue(buildStatusJson().c_str());

  pService->start();

  BLEAdvertising *pAdvertising = BLEDevice::getAdvertising();
  pAdvertising->addServiceUUID(SERVICE_UUID);
  pAdvertising->setScanResponse(true);
  BLEDevice::startAdvertising();
  bleAdvertising = true;

  Serial.println("Setup complete.");
}

void pollModeExclusion() {
  unsigned long now = millis();
  if (now - lastWifiPoll < 1000) return;
  lastWifiPoll = now;

  int apClients = WiFi.softAPgetStationNum();
  if (!deviceConnected) {
    if (apClients > 0 && bleAdvertising) bleStopAdvertisingAndKick();
    else if (apClients == 0 && !bleAdvertising) bleResumeAdvertising();
  }
}

void loop() {
  if (pendingWifiSuspend && millis() >= wifiSuspendDueAt) { pendingWifiSuspend = false; wifiSuspendAll(true); }
  if (pendingWifiResume)  { pendingWifiResume  = false; wifiResumeAll(); }
  if (pendingRestart && millis() >= restartDueAt) { ESP.restart(); }

  // Management layer - always runs, completely independent of which system
  // mode is currently driving the LEDs. Restart/WiFi/OTA/etc never go away.
  server.handleClient();
  dnsServer.processNextRequest();
  pollModeExclusion();

  if (systemMode == SYSMODE_E131) {
    handleE131(); // paints + calls FastLED.show() itself the moment a packet arrives
    unsigned long now = millis();
    if (now - lastTick >= TICK_MS) {
      lastTick = now;
      // Only show "waiting" grey before the very first packet ever arrives.
      // Once real data lands, hold it indefinitely - senders like OpenRGB only
      // transmit when a color actually changes, not continuously, so reverting
      // to a fallback after a few quiet seconds was wrongly overwriting valid,
      // still-current color data.
      if (lastE131Packet == 0) {
        fill_solid(leds, activeLedCount, CRGB(10, 10, 10)); // dim grey = "never received data yet"
        FastLED.show();
      }
    }
    return; // native effects engine below is entirely skipped in this mode
  }

  unsigned long now = millis();
  if (now - lastTick < TICK_MS) return;
  lastTick = now;

  if (!powerOn) {
    fill_solid(leds, activeLedCount, CRGB::Black);
    FastLED.show();
    return;
  }

  switch (effect) {
    case 0: // solid
      fill_solid(leds, activeLedCount, colorA);
      break;

    case 1: // rainbow
      static uint8_t accum = 0; accum += effectSpeed;
      if (accum >= 10) { accum = 0; huePhase += 4; }
      fill_rainbow(leds, activeLedCount, huePhase, 255 / activeLedCount);
      break;

    case 2: { // breathe
      static uint8_t accum = 0; accum += effectSpeed;
      if (accum >= 60) { accum = 0; breathePhase += 8; }
      uint8_t level = sin8(breathePhase);
      CRGB c = colorA; c.nscale8(level);
      fill_solid(leds, activeLedCount, c);
      break;
    }

    case 3: { // chase (comet trail)
      fadeToBlackBy(leds, activeLedCount, 40);
      leds[chasePos % activeLedCount] = colorA;
      static uint8_t accum = 0; accum += effectSpeed;
      if (accum >= 160) { accum = 0; chasePos++; }
      break;
    }

    case 4: // gradient - static blend across the strip, now reflects ALL palette
             // stops (was only ever blending colorA->colorB directly, ignoring
             // any middle stops - paletteLUT already interpolates correctly)
      for (uint16_t i = 0; i < activeLedCount; i++) {
        uint8_t idx = (uint8_t)((uint32_t)i * 255 / (activeLedCount - 1));
        leds[i] = paletteLUT[idx];
      }
      break;

    case 5: { // twinkle
      fadeToBlackBy(leds, activeLedCount, 15);
      static uint8_t accum = 0; accum += effectSpeed;
      if (accum >= 120) { accum = 0; leds[random16(activeLedCount)] = colorA; }
      break;
    }

    case 6: { // wipe
      fill_solid(leds, activeLedCount, CRGB::Black);
      for (uint16_t i = 0; i <= wipePos && i < activeLedCount; i++) leds[i] = colorA;
      static uint8_t accum = 0; accum += effectSpeed;
      if (accum >= 48) { accum = 0; wipePos += wipeDir; if (wipePos >= activeLedCount - 1 || wipePos == 0) wipeDir *= -1; }
      break;
    }

    case 7: { // theater chase - single color marquee
      fill_solid(leds, activeLedCount, CRGB::Black);
      static uint8_t offset = 0;
      for (uint16_t i = offset; i < activeLedCount; i += 3) leds[i] = colorA;
      static uint8_t accum = 0; accum += effectSpeed;
      if (accum >= 160) { accum = 0; offset = (offset + 1) % 3; }
      break;
    }

    case 8: { // theater chase 2-color - alternating groups of A and B
      static uint8_t offset = 0;
      for (uint16_t i = 0; i < activeLedCount; i++) {
        bool lit = ((i + offset) % 3 == 0);
        bool groupB = (((i + offset) / 3) % 2 == 1);
        leds[i] = lit ? (groupB ? colorB : colorA) : CRGB::Black;
      }
      static uint8_t accum = 0; accum += effectSpeed;
      if (accum >= 160) { accum = 0; offset = (offset + 1) % 3; }
      break;
    }

    case 9: { // scanner - single dot ping-pongs
      fadeToBlackBy(leds, activeLedCount, 60);
      leds[scannerPos] = colorA;
      static uint8_t accum = 0; accum += effectSpeed;
      if (accum >= 48) { accum = 0; scannerPos += scannerDir; if (scannerPos >= activeLedCount - 1 || scannerPos == 0) scannerDir *= -1; }
      break;
    }

    case 10: { // fire - tinted by colorA
      static uint8_t accum = 0; accum += effectSpeed;
      if (accum >= 96) {
        accum = 0;
        for (uint16_t i = 0; i < activeLedCount; i++) fireHeat[i].r = qsub8(fireHeat[i].r, random8(0, 20));
        for (uint16_t i = activeLedCount - 1; i >= 2; i--) fireHeat[i].r = (fireHeat[i-1].r + fireHeat[i-2].r + fireHeat[i-2].r) / 3;
        if (random8() < 120) { uint16_t sparkPos = random8(2); fireHeat[sparkPos].r = qadd8(fireHeat[sparkPos].r, random8(160, 255)); }
        for (uint16_t i = 0; i < activeLedCount; i++) {
          uint8_t heat = fireHeat[i].r;
          leds[i] = CRGB(scale8(colorA.r, heat), scale8(colorA.g, heat / 3), scale8(colorA.b, heat / 6));
        }
      }
      break;
    }

    case 11: { // sparkle - base color + random white flash
      fill_solid(leds, activeLedCount, colorA);
      static uint8_t accum = 0; accum += effectSpeed;
      if (accum >= 80) { accum = 0; leds[random16(activeLedCount)] = CRGB::White; }
      break;
    }

    case 12: { // colorloop
      CHSV hsv(loopHue, 255, 255);
      CRGB c; hsv2rgb_rainbow(hsv, c);
      fill_solid(leds, activeLedCount, c);
      static uint8_t accum = 0; accum += effectSpeed;
      if (accum >= 60) { accum = 0; loopHue += 3; }
      break;
    }

    case 13: { // meteor - bright head, fading trail, occasional sparkle in trail
      fadeToBlackBy(leds, activeLedCount, 35);
      leds[meteorPos % activeLedCount] = colorA;
      if (random8() < 40) leds[(meteorPos + activeLedCount - random8(4)) % activeLedCount] += CRGB(20,20,20);
      static uint8_t accum = 0; accum += effectSpeed;
      if (accum >= 48) { accum = 0; meteorPos++; }
      break;
    }

    case 14: { // bounce - single bar bounces end to end, alternating color each bounce
      fill_solid(leds, activeLedCount, CRGB::Black);
      CRGB c = bounceUseB ? colorB : colorA;
      for (int8_t o = -1; o <= 1; o++) {
        int16_t p = bouncePos + o;
        if (p >= 0 && p < activeLedCount) leds[p] = c;
      }
      static uint8_t accum = 0; accum += effectSpeed;
      if (accum >= 48) {
        accum = 0;
        bouncePos += bounceDir;
        if (bouncePos >= activeLedCount - 1 || bouncePos <= 0) { bounceDir *= -1; bounceUseB = !bounceUseB; }
      }
      break;
    }

    case 15: { // strobe
      static uint8_t accum = 0; accum += effectSpeed;
      if (accum >= 120) { accum = 0; strobeOn = !strobeOn; }
      fill_solid(leds, activeLedCount, strobeOn ? colorA : CRGB::Black);
      break;
    }

    case 16: { // confetti - random sparkles sampled from the custom palette
      fadeToBlackBy(leds, activeLedCount, 10);
      static uint8_t accum = 0; accum += effectSpeed;
      if (accum >= 64) { accum = 0; leds[random16(activeLedCount)] = paletteLUT[random8()]; }
      break;
    }

    case 17: { // palette flow - smooth animated wave through the custom gradient (Aura/Armoury-Crate style)
      for (uint16_t i = 0; i < activeLedCount; i++) {
        uint8_t idx = (uint8_t)((flowPhase + i * (256 / activeLedCount)) % 256);
        leds[i] = paletteLUT[idx];
      }
      static uint8_t accum = 0; accum += effectSpeed;
      if (accum >= 8) { accum = 0; flowPhase += 4; }
      break;
    }

    case 18: { // wipe random - like Wipe, but picks a fresh random palette color each pass
      static CRGB currentColor = colorA;
      fill_solid(leds, activeLedCount, CRGB::Black);
      for (uint16_t i = 0; i <= wipePos && i < activeLedCount; i++) leds[i] = currentColor;
      static uint8_t accum = 0; accum += effectSpeed;
      if (accum >= 48) {
        accum = 0; wipePos += wipeDir;
        if (wipePos >= activeLedCount - 1 || wipePos == 0) {
          wipeDir *= -1;
          currentColor = paletteLUT[random8()];
        }
      }
      break;
    }

    case 19: { // dual scan - two scanner dots sweeping in opposite directions
      fadeToBlackBy(leds, activeLedCount, 60);
      leds[dualScanPos1] = colorA;
      leds[dualScanPos2] = colorB;
      static uint8_t accum = 0; accum += effectSpeed;
      if (accum >= 48) {
        accum = 0;
        dualScanPos1 += dualScanDir1;
        if (dualScanPos1 >= activeLedCount - 1 || dualScanPos1 == 0) dualScanDir1 *= -1;
        dualScanPos2 += dualScanDir2;
        if (dualScanPos2 >= activeLedCount - 1 || dualScanPos2 == 0) dualScanDir2 *= -1;
      }
      break;
    }

    case 20: { // running lights - sine-wave brightness ripple travels down the strip
      for (uint16_t i = 0; i < activeLedCount; i++) {
        uint8_t level = sin8((runningPhase + i * (255 / activeLedCount)) % 256);
        CRGB c = colorA; c.nscale8(level);
        leds[i] = c;
      }
      static uint8_t accum = 0; accum += effectSpeed;
      if (accum >= 10) { accum = 0; runningPhase += 4; }
      break;
    }

    case 21: { // saw - sawtooth brightness ramp travels down the strip (sharp edge, not smooth like running lights)
      for (uint16_t i = 0; i < activeLedCount; i++) {
        uint8_t level = (uint8_t)((sawPhase + i * (256 / activeLedCount)) % 256);
        CRGB c = colorA; c.nscale8(level);
        leds[i] = c;
      }
      static uint8_t accum = 0; accum += effectSpeed;
      if (accum >= 10) { accum = 0; sawPhase += 4; }
      break;
    }

    case 22: { // dissolve - random pixels randomly flip on to colorA then fade back out
      static uint8_t accum = 0; accum += effectSpeed;
      if (accum >= 48) {
        accum = 0;
        uint16_t i = random16(activeLedCount);
        dissolveState[i] = (dissolveState[i].r == 0 && dissolveState[i].g == 0 && dissolveState[i].b == 0) ? colorA : CRGB::Black;
      }
      for (uint16_t i = 0; i < activeLedCount; i++) leds[i] = dissolveState[i];
      break;
    }

    case 23: { // alert flash - whole strip synced, alternates fully between Color A and Color B.
      // Replaces the old Police effect, which split the strip into two halves -
      // that looks broken on Y-split/parallel-wired fans since both fans just
      // mirror the same half-red-half-blue image instead of showing solid
      // colors. This version lights the ENTIRE strip one color, then the
      // entire strip the other color - correct regardless of fan wiring.
      static uint8_t accum = 0; accum += effectSpeed;
      if (accum >= 160) { accum = 0; policePhase = !policePhase; }
      fill_solid(leds, activeLedCount, policePhase ? colorA : colorB);
      break;
    }

    case 24: { // sinelon - bright dot with fading trail, smooth sine-driven back-and-forth motion
      fadeToBlackBy(leds, activeLedCount, 30);
      uint16_t pos = (uint16_t)(((sin(sinelonPos) + 1.0) / 2.0) * (activeLedCount - 1));
      leds[pos] = colorA;
      sinelonPos += (effectSpeed * 0.02);
      break;
    }

    case 25: { // popcorn - random "kernels" flash bright then fade, scattered timing
      static uint8_t accum = 0; accum += effectSpeed;
      fadeToBlackBy(leds, activeLedCount, 20);
      if (accum >= 48) {
        accum = 0;
        if (random8() < 90) {
          uint16_t i = random16(activeLedCount);
          leds[i] = paletteLUT[random8()];
        }
      }
      break;
    }

    case 26: { // ripple - expanding ring of brightness from a random origin point, fades as it travels
      static uint8_t accum = 0; accum += effectSpeed;
      if (rippleOrigin < 0) { rippleOrigin = random16(activeLedCount); ripplePos = 0; }
      fill_solid(leds, activeLedCount, CRGB::Black);
      int16_t left = rippleOrigin - (int16_t)ripplePos;
      int16_t right = rippleOrigin + (int16_t)ripplePos;
      uint8_t fade = qsub8(255, (uint8_t)(ripplePos * (255 / activeLedCount)));
      if (left >= 0 && left < activeLedCount) { CRGB c = colorA; c.nscale8(fade); leds[left] = c; }
      if (right >= 0 && right < activeLedCount) { CRGB c = colorA; c.nscale8(fade); leds[right] = c; }
      if (accum >= 48) {
        accum = 0; ripplePos += 1;
        if (ripplePos > activeLedCount) rippleOrigin = -1; // pick a new origin next frame
      }
      break;
    }

    case 27: { // glitter - solid colorA background, always lit, with random white sparkle overlay
      fill_solid(leds, activeLedCount, colorA);
      static uint8_t accum = 0; accum += effectSpeed;
      if (accum >= 64) { accum = 0; if (random8() < 120) leds[random16(activeLedCount)] = CRGB::White; }
      break;
    }

    case 28: { // candle - gentle warm flicker, subtler/slower than Fire, single-flame feel
      static uint8_t accum = 0; accum += effectSpeed;
      if (accum >= 80) {
        accum = 0;
        for (uint16_t i = 0; i < activeLedCount; i++) {
          uint8_t flicker = 180 + random8(75); // stays mostly bright, jitters a bit
          CRGB c = colorA; c.nscale8(flicker);
          leds[i] = c;
        }
      }
      break;
    }

    case 29: { // heartbeat - double-pulse "thump-thump" brightness rhythm
      static uint8_t accum = 0; accum += effectSpeed;
      if (accum >= 40) { accum = 0; heartbeatPhase += 3; }
      uint8_t phase = heartbeatPhase % 256;
      uint8_t level;
      if (phase < 40) level = sin8(phase * 6);        // first thump
      else if (phase < 70) level = 0;                  // gap
      else if (phase < 100) level = sin8((phase - 70) * 8); // second thump
      else level = 0;                                  // rest before next cycle
      CRGB c = colorA; c.nscale8(level);
      fill_solid(leds, activeLedCount, c);
      break;
    }

    case 30: { // gradient flow - travels through the full custom palette (was only
               // ever blending colorA->colorB directly, same bug as Gradient had)
      for (uint16_t i = 0; i < activeLedCount; i++) {
        uint8_t idx = triwave8((uint8_t)((gradFlowPhase + i * (256 / activeLedCount)) % 256));
        leds[i] = paletteLUT[idx];
      }
      static uint8_t accum = 0; accum += effectSpeed;
      if (accum >= 10) { accum = 0; gradFlowPhase += 4; }
      break;
    }

    case 31: // pixel colors - independent per-LED color, set individually like
             // OpenRGB's Direct mode. Deliberately just a display of whatever's
             // in pixelColors[] - no timing, no gating, nothing to desync.
      for (uint16_t i = 0; i < activeLedCount; i++) leds[i] = pixelColors[i];
      break;
  }

  FastLED.show();
}
