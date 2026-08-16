// bc250_panel_server.go
//
// Go port of bc250_dashboard_server.py - same hwmon-direct sensor reading,
// same JSON API shape (so the existing panel HTML needs zero changes), same
// "serve the panel itself" behavior. The point of this port is portability:
// this compiles to a single static binary with no runtime dependency at all -
// no Python interpreter version to drift out from under you on a
// rolling-release distro, nothing any package manager can break by
// upgrading something else out from under it.
//
// Build (module-aware - this needs go.mod/go.sum and bc250_terminal.go
// alongside it, not just this one file, since the Terminal tab pulls in two
// small external packages):
//
//	go build -o bc250-panel .
//
// Run:
//
//	./bc250-panel --port 8091 --interval 2 --user yourusername
//
// Reads sysfs directly (no lm-sensors dependency), same as the Python
// version: each chip is found dynamically by its hwmon driver name, and each
// value is found by matching its *_label file content - nothing hardcoded to
// a specific hwmonN path, since that numbering isn't stable across boots.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"math/bits"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ---------------- reading ----------------

type Reading struct {
	Key   string  `json:"k"`
	Value float64 `json:"v"`
	Age   int     `json:"a"`
}

var (
	latest                    []Reading
	latestMu                  sync.Mutex
	latestAt                  time.Time
	hwmonCache                = map[string]string{} // driver name -> hwmon dir path
	hwmonCacheMu              sync.Mutex
	lastCPUIdle, lastCPUTotal float64
	haveLastCPU               bool
)

func readNum(path string) (float64, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(string(b)), 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

func readStr(path string) (string, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(b)), true
}

// findHwmon locates the hwmon directory whose "name" file matches driverName,
// cached since it doesn't change at runtime.
func findHwmon(driverName string) string {
	hwmonCacheMu.Lock()
	defer hwmonCacheMu.Unlock()
	if v, ok := hwmonCache[driverName]; ok {
		return v // only ever caches successful finds now - see below
	}
	matches, _ := filepath.Glob("/sys/class/hwmon/hwmon*")
	for _, path := range matches {
		if name, ok := readStr(filepath.Join(path, "name")); ok && name == driverName {
			hwmonCache[driverName] = path
			return path
		}
	}
	// Deliberately NOT caching "" here. Some chips (isl69247 in particular,
	// on I2C) enumerate later at boot than PCI devices like amdgpu/k10temp.
	// If the very first poll ran before that finished, caching a permanent
	// "not found" here would mean this chip's data silently vanishes forever,
	// even though the hwmon directory shows up moments later - exactly what
	// was happening. Just keep rechecking each poll until it's actually found.
	return ""
}

// findInputByLabel finds e.g. hwmonDir/in3_input where in3_label matches
// labelMatch. prefix is "in", "temp", "power", "curr", or "fan". If exact is
// false, a case-insensitive substring match is used instead of an exact one.
func findInputByLabel(hwmonDir, prefix, labelMatch string, exact bool) string {
	if hwmonDir == "" {
		return ""
	}
	matches, _ := filepath.Glob(filepath.Join(hwmonDir, prefix+"*_label"))
	for _, labelFile := range matches {
		label, ok := readStr(labelFile)
		if !ok {
			continue
		}
		matched := false
		if exact {
			matched = strings.EqualFold(label, labelMatch)
		} else {
			matched = strings.Contains(strings.ToLower(label), strings.ToLower(labelMatch))
		}
		if matched {
			inputFile := strings.Replace(labelFile, "_label", "_input", 1)
			if _, err := os.Stat(inputFile); err == nil {
				return inputFile
			}
		}
	}
	return ""
}

func add(readings *[]Reading, key string, v float64, ok bool) {
	if ok {
		*readings = append(*readings, Reading{Key: key, Value: v})
	}
}

func readISL69247(readings *[]Reading) {
	hwmon := findHwmon("isl69247")
	if hwmon == "" {
		return
	}
	totalUW, found := 0.0, false
	for _, label := range []string{"pout1", "pout2"} {
		if f := findInputByLabel(hwmon, "power", label, true); f != "" {
			if v, ok := readNum(f); ok {
				totalUW += v
				found = true
			}
		}
	}
	if found {
		add(readings, "vrm", roundTo(totalUW/1_000_000, 1), true)
	}
}

var nonAlnum = regexp.MustCompile(`[^a-z0-9]+`)

func readNCT6686(readings *[]Reading) {
	hwmon := findHwmon("nct6686")
	if hwmon == "" {
		return
	}
	fans := map[string]float64{}
	matches, _ := filepath.Glob(filepath.Join(hwmon, "fan*_label"))
	for _, labelFile := range matches {
		label, ok := readStr(labelFile)
		if !ok {
			continue
		}
		inputFile := strings.Replace(labelFile, "_label", "_input", 1)
		if rpm, ok := readNum(inputFile); ok && rpm > 0 {
			safe := strings.Trim(nonAlnum.ReplaceAllString(strings.ToLower(label), "_"), "_")
			fans[safe] = rpm
		}
	}
	if len(fans) == 1 {
		for _, rpm := range fans {
			add(readings, "fan", roundTo(rpm, 0), true)
		}
	} else if len(fans) > 1 {
		sum := 0.0
		for name, rpm := range fans {
			add(readings, "fan_"+name, roundTo(rpm, 0), true)
			sum += rpm
		}
		add(readings, "fan_avg", roundTo(sum/float64(len(fans)), 0), true)
	}
	// Every detected pwmN channel, not just pwm1 - unified into a single
	// "fan_pwm" reading (their average) so the Sensors tab reflects whatever
	// is actually driving the fans right now, however many channels that is.
	pwmMatches, _ := filepath.Glob(filepath.Join(hwmon, "pwm[0-9]*"))
	pwmSum, pwmCount := 0.0, 0
	for _, f := range pwmMatches {
		base := filepath.Base(f)
		if strings.Contains(base, "_") {
			continue // skip pwmN_enable, pwmN_mode, etc. - only the bare pwmN value files
		}
		if v, ok := readNum(f); ok {
			pwmSum += v
			pwmCount++
		}
	}
	if pwmCount > 0 {
		add(readings, "fan_pwm", roundTo(pwmSum/float64(pwmCount)/255*100, 0), true)
	}
}

// ---------------- fan control ----------------
//
// The BC-250's fan header is monitored fine by the stock in-tree "nct6683"
// kernel driver (temps/RPM read-only), but that chip ID does NOT expose
// writable pwm*_enable/pwm* files - manual speed control needs the
// community "nct6687d" out-of-tree driver instead, which binds and shows up
// under hwmon name "nct6686" (same name readNCT6686 above already looks
// for). So: same hwmon dir as sensor reading, but its mere existence is
// also exactly the signal that manual control is even possible.
// https://github.com/Fred78290/nct6687d
//
// This chip can expose several independent pwmN channels (not just pwm1),
// so every pwmN_enable found gets enumerated rather than assuming a fixed
// index - the UI picks which channel to view/drive via a dropdown.

var pwmEnableRe = regexp.MustCompile(`^pwm(\d+)_enable$`)

type FanChannel struct {
	Index   int     `json:"index"`   // matches the pwm{N} filename
	Enable  int     `json:"enable"`  // raw pwmN_enable value, -1 if unreadable
	Manual  bool    `json:"manual"`  // enable == 1
	Percent float64 `json:"percent"` // current pwmN as 0-100
	RPM     float64 `json:"rpm"`     // fanN_input if a same-index channel exists, else 0
	OnCurve bool    `json:"on_curve"`
}

type FanStatus struct {
	Driver       string       `json:"driver"` // "nct6687d", "nct6683", or "none"
	Controllable bool         `json:"controllable"`
	Hint         string       `json:"hint"`
	Channels     []FanChannel `json:"channels"`
}

func detectFanDriver() (hwmonDir, driver string, controllable bool) {
	if hw := findHwmon("nct6686"); hw != "" {
		return hw, "nct6687d", true
	}
	if hw := findHwmon("nct6683"); hw != "" {
		return hw, "nct6683", false
	}
	return "", "none", false
}

func readFanStatus() FanStatus {
	hwmon, driver, controllable := detectFanDriver()
	st := FanStatus{Driver: driver, Controllable: controllable}
	switch driver {
	case "nct6687d":
		st.Hint = "nct6687d active - manual PWM control available."
	case "nct6683":
		st.Hint = "Stock nct6683 driver detected - read-only, this chip ID doesn't expose writable PWM. Install nct6687d for manual control: https://github.com/Fred78290/nct6687d"
	default:
		st.Hint = "No compatible fan controller hwmon found."
	}
	if hwmon == "" {
		return st
	}
	entries, _ := os.ReadDir(hwmon)
	var idxs []int
	for _, e := range entries {
		if m := pwmEnableRe.FindStringSubmatch(e.Name()); m != nil {
			n, _ := strconv.Atoi(m[1])
			idxs = append(idxs, n)
		}
	}
	sort.Ints(idxs)
	for _, n := range idxs {
		ch := FanChannel{Index: n, Enable: -1}
		if v, ok := readNum(filepath.Join(hwmon, fmt.Sprintf("pwm%d_enable", n))); ok {
			ch.Enable = int(v)
			ch.Manual = ch.Enable == 1
		}
		if v, ok := readNum(filepath.Join(hwmon, fmt.Sprintf("pwm%d", n))); ok {
			ch.Percent = roundTo(v/255*100, 0)
		}
		if v, ok := readNum(filepath.Join(hwmon, fmt.Sprintf("fan%d_input", n))); ok {
			ch.RPM = roundTo(v, 0)
		}
		fanCurvesMu.Lock()
		if c, ok := fanCurves[n]; ok {
			ch.OnCurve = c.Enabled
		}
		fanCurvesMu.Unlock()
		st.Channels = append(st.Channels, ch)
	}
	return st
}

// writeFan sets manual mode and/or a target percent on one pwmN channel.
// Either can be omitted (nil) to leave that aspect untouched - e.g. flipping
// back to Auto without also specifying a percent.
func writeFan(index int, enable *bool, percent *float64) error {
	hwmon, driver, controllable := detectFanDriver()
	if !controllable {
		if driver == "nct6683" {
			return fmt.Errorf("stock nct6683 driver only - install nct6687d for manual control")
		}
		return fmt.Errorf("no controllable fan hwmon found")
	}
	if index < 1 {
		return fmt.Errorf("invalid pwm channel index %d", index)
	}
	enableFile := filepath.Join(hwmon, fmt.Sprintf("pwm%d_enable", index))
	valueFile := filepath.Join(hwmon, fmt.Sprintf("pwm%d", index))
	if _, err := os.Stat(enableFile); err != nil {
		return fmt.Errorf("pwm%d_enable not found on this hwmon", index)
	}
	if enable != nil {
		// nct6687d's own documented codes (NOT the generic hwmon 1/2/3
		// convention most other chips use): 1 = manual (fixed at whatever's
		// last written to pwmN), 99 = hand back to "whatever automatic mode
		// was configured by firmware" - i.e. the actual BIOS/EC-driven
		// curve. https://github.com/Fred78290/nct6687d
		val := "99"
		if *enable {
			val = "1"
		}
		if err := os.WriteFile(enableFile, []byte(val), 0644); err != nil {
			return fmt.Errorf("write pwm%d_enable failed (%v) on %s - the panel server runs as root, so this is unexpected (read-only filesystem? immutable attribute? LSM policy blocking it?)", index, err, hwmon)
		}
	}
	if percent != nil {
		p := *percent
		if p < 0 {
			p = 0
		}
		if p > 100 {
			p = 100
		}
		raw := int(p/100*255 + 0.5)
		if err := os.WriteFile(valueFile, []byte(strconv.Itoa(raw)), 0644); err != nil {
			return fmt.Errorf("write pwm%d failed (%v) on %s - the panel server runs as root, so this is unexpected (read-only filesystem? immutable attribute? LSM policy blocking it?)", index, err, hwmon)
		}
	}
	return nil
}

// resetAllFansToStock clears every stored fan curve and sets every detected
// pwmN channel back to 99 - nct6687d's documented code for "whatever
// automatic mode was configured by firmware", i.e. genuinely handing full
// control back to the BIOS/EC curve, undoing everything this panel has ever
// configured. This is NOT the generic hwmon "2 = automatic" convention most
// other chips use - this driver is documented as using 99 specifically.
// https://github.com/Fred78290/nct6687d
func resetAllFansToStock() error {
	hwmon, _, controllable := detectFanDriver()
	if !controllable {
		return fmt.Errorf("no controllable fan hwmon found")
	}

	fanCurvesMu.Lock()
	fanCurves = map[int]*FanCurve{}
	fanCurveLastPct = map[int]float64{}
	saveFanCurvesLocked()
	fanCurvesMu.Unlock()
	fanCurveLiveMu.Lock()
	fanCurveLive = map[int]curveLivePoint{}
	fanCurveLiveMu.Unlock()

	entries, _ := os.ReadDir(hwmon)
	var firstErr error
	channelsReset := 0
	for _, e := range entries {
		m := pwmEnableRe.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		channelsReset++
		n, _ := strconv.Atoi(m[1])
		enablePath := filepath.Join(hwmon, e.Name())
		valuePath := filepath.Join(hwmon, fmt.Sprintf("pwm%d", n))

		// Order matters here, and getting it wrong is exactly what caused
		// "reset ends up in Manual instead of Auto": this driver documents
		// that writing to the plain pwmN file (the raw duty register) has
		// the side effect of switching that channel BACK to manual mode,
		// regardless of what pwmN_enable was set to moments earlier. So the
		// neutral-percent write (clearing stale leftover state - see below)
		// has to happen FIRST, and the actual enable=99 (auto) write has to
		// come LAST, so it's what actually determines the final state.
		if err := os.WriteFile(valuePath, []byte("128"), 0644); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("pwm%d: %v", n, err)
		}
		// This driver's pwmN (the raw duty register) and pwmN_enable (the
		// mode) are two independent files - resetting the mode alone
		// wouldn't touch this one at all. Left alone, whatever percent was
		// last manually set just sits there and gets read straight back by
		// readFanStatus() as if it were still meaningful, making a "reset"
		// look like it silently kept your old manual value. Writing a
		// neutral 50% here means a fresh check-in after reset always shows
		// a sane number instead of stale leftover state - and if you flip
		// back to Manual later, you get a moderate starting point instead
		// of whatever aggressive value was last set (say, 100%) blasting
		// the fan the instant you re-engage manual control.

		if err := os.WriteFile(enablePath, []byte("99"), 0644); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("pwm%d_enable: %v", n, err)
		}
		// NOT verified against a specific readback value on purpose: 99 is a
		// write-only "restore whatever firmware originally configured"
		// trigger on this driver, not a value that persists - the driver
		// immediately swaps it for the real captured mode internally (which
		// can legitimately be 1, 2, 4, 5, whatever this board's firmware
		// actually used). Comparing readback against literal 99 here was a
		// bug that manufactured false failures on a reset that had already
		// succeeded. A write I/O error above is the only real failure case.
		//
		// That said, whatever the true firmware-default code turns out to
		// be, it should never legitimately be "1" specifically - that's
		// this driver's own documented code for manual/fixed, which is by
		// definition not what "restore firmware auto default" should ever
		// produce. If it reads back as 1 anyway, something's still wrong
		// (an ordering issue we haven't anticipated, a firmware quirk,
		// whatever) - surfacing that as an error beats silently reporting
		// success for a reset that didn't actually leave this in Auto.
		if readback, ok := readNum(enablePath); ok && int(readback) == 1 && firstErr == nil {
			firstErr = fmt.Errorf("pwm%d_enable still reads back as 1 (manual) after reset - this channel may need a different approach", n)
		}
	}
	if firstErr == nil && channelsReset == 0 {
		// detectFanDriver said this hwmon was controllable, but somehow no
		// pwmN_enable files actually turned up - returning nil here would
		// silently report "success" for a reset that touched nothing at all.
		return fmt.Errorf("no pwmN_enable channels found under %s despite a controllable driver being detected - nothing was reset", hwmon)
	}
	return firstErr
}

// ---------------- fan curves ----------------
//
// Temp -> speed% curves, evaluated continuously by the poll loop (not the
// browser) so a curve keeps running even with the tab closed - same as
// FanControl (github.com/Rem0o/FanControl.Releases). A channel with an
// enabled curve is exclusively curve-driven; a plain manual/auto write from
// the Fan Control tab (POST /api/fan) auto-disables that channel's curve
// first so the two can't fight over the same pwmN file every poll cycle.

type CurvePoint struct {
	Temp    float64 `json:"temp"`
	Percent float64 `json:"percent"`
}

type FanCurve struct {
	Channel int          `json:"channel"`
	Sensor  string       `json:"sensor"` // sensor key from /api/sensors, e.g. "cpu", "gpu", "nvme_temp"
	Points  []CurvePoint `json:"points"` // sorted ascending by Temp, >=2 points
	Enabled bool         `json:"enabled"`
}

type curveLivePoint struct {
	Temp    float64 `json:"temp"`
	Percent float64 `json:"percent"`
}

var (
	fanCurves       = map[int]*FanCurve{} // channel index -> curve
	fanCurvesMu     sync.Mutex
	fanCurvesPath   string              // set in main(), next to the binary so it survives reinstalls in place
	fanCurveLastPct = map[int]float64{} // channel -> last percent actually written, to skip no-op writes
	fanCurveLiveMu  sync.Mutex
	fanCurveLive    = map[int]curveLivePoint{} // channel -> most recent (sensor temp, computed percent), for the UI's live curve marker
)

func loadFanCurves() {
	fanCurvesMu.Lock()
	defer fanCurvesMu.Unlock()
	b, err := os.ReadFile(fanCurvesPath)
	if err != nil {
		return
	}
	var list []*FanCurve
	if json.Unmarshal(b, &list) != nil {
		return
	}
	for _, c := range list {
		fanCurves[c.Channel] = c
	}
}

func saveFanCurvesLocked() {
	list := make([]*FanCurve, 0, len(fanCurves))
	for _, c := range fanCurves {
		list = append(list, c)
	}
	b, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return
	}
	os.WriteFile(fanCurvesPath, b, 0644)
}

// evalCurve linearly interpolates percent for a given temp, clamping to the
// first/last point outside the defined range. points must be pre-sorted.
func evalCurve(points []CurvePoint, temp float64) float64 {
	if len(points) == 0 {
		return 0
	}
	if temp <= points[0].Temp {
		return points[0].Percent
	}
	last := points[len(points)-1]
	if temp >= last.Temp {
		return last.Percent
	}
	for i := 0; i < len(points)-1; i++ {
		a, b := points[i], points[i+1]
		if temp >= a.Temp && temp <= b.Temp {
			if b.Temp == a.Temp {
				return a.Percent
			}
			t := (temp - a.Temp) / (b.Temp - a.Temp)
			return a.Percent + t*(b.Percent-a.Percent)
		}
	}
	return last.Percent
}

// applyFanCurves runs every poll tick. sensorVals is this tick's freshly-read
// sensor snapshot (key -> value) - reusing it instead of re-reading avoids a
// second sysfs pass and keeps the curve reacting to the exact same sample
// the Sensors tab is showing.
func applyFanCurves(sensorVals map[string]float64) {
	fanCurvesMu.Lock()
	curves := make([]*FanCurve, 0, len(fanCurves))
	for _, c := range fanCurves {
		curves = append(curves, c)
	}
	fanCurvesMu.Unlock()

	for _, c := range curves {
		if !c.Enabled || len(c.Points) < 2 {
			continue
		}
		temp, ok := sensorVals[c.Sensor]
		if !ok {
			continue // sensor not present this tick - leave the fan as-is rather than guess
		}
		target := roundTo(evalCurve(c.Points, temp), 0)

		fanCurveLiveMu.Lock()
		fanCurveLive[c.Channel] = curveLivePoint{Temp: temp, Percent: target}
		fanCurveLiveMu.Unlock()

		fanCurvesMu.Lock()
		last, had := fanCurveLastPct[c.Channel]
		fanCurvesMu.Unlock()
		if had && last == target {
			continue // no meaningful change - skip the sysfs write
		}
		enable := true
		if err := writeFan(c.Channel, &enable, &target); err == nil {
			fanCurvesMu.Lock()
			fanCurveLastPct[c.Channel] = target
			fanCurvesMu.Unlock()
		} else {
			fmt.Println("fan curve write failed:", err)
		}
	}
}

// disableCurveForChannel is called before any manual/auto write from the
// Fan Control tab lands, so a stored curve doesn't immediately overwrite the
// user's manual choice on the next poll tick.
func disableCurveForChannel(index int) {
	fanCurvesMu.Lock()
	defer fanCurvesMu.Unlock()
	if c, ok := fanCurves[index]; ok && c.Enabled {
		c.Enabled = false
		saveFanCurvesLocked()
	}
	// Clear the "last value this curve actually wrote" cache too - without
	// this, re-enabling the same curve later (without the sensor temp
	// having changed) could compute the exact same target percent, see it
	// matches the stale cache, and skip the write entirely as a no-op -
	// leaving the hardware sitting in whatever the manual override left it
	// at (e.g. still Auto) even though the UI now says the curve is active.
	delete(fanCurveLastPct, index)
}

// ---------------- process list ----------------

type ProcInfo struct {
	PID   int     `json:"pid"`
	Name  string  `json:"name"`
	CPU   float64 `json:"cpu"` // percent, 0-100*NCPU
	MemMB float64 `json:"mem_mb"`
	User  string  `json:"user"`
}

// ticks: last utime+stime for the pid. start: that pid's /proc/[pid]/stat
// starttime field (jiffies since boot the process was created). starttime is
// what lets us tell "same process, still running" apart from "PID got
// recycled by a brand-new process" - see readProcesses.
type procSample struct {
	ticks float64
	start float64
}

var (
	procCPUTimeMu sync.Mutex
	procCPUTime   = map[int]procSample{}
	procCPUAt     time.Time
	uidNameCache  = map[string]string{}
	uidNameMu     sync.Mutex

	logicalCPUsOnce  sync.Once
	logicalCPUsCount int
)

const userHZ = 100.0 // USER_HZ - standard on x86/x86_64 Linux, no cgo sysconf available

// logicalCPUs returns the number of logical CPUs (threads), used only to put
// a sane ceiling on a single process's reported CPU% (100% per logical CPU
// it could possibly be scheduled on). Cached since topology can't change
// while the server is running.
func logicalCPUs() int {
	logicalCPUsOnce.Do(func() {
		_, threads, _, ok := readCPUTopology()
		if ok && threads > 0 {
			logicalCPUsCount = threads
		} else {
			logicalCPUsCount = 1
		}
	})
	return logicalCPUsCount
}

func uidToName(uid string) string {
	uidNameMu.Lock()
	defer uidNameMu.Unlock()
	if n, ok := uidNameCache[uid]; ok {
		return n
	}
	name := uid
	if b, err := os.ReadFile("/etc/passwd"); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			parts := strings.Split(line, ":")
			if len(parts) >= 3 && parts[2] == uid {
				name = parts[0]
				break
			}
		}
	}
	uidNameCache[uid] = name
	return name
}

func readProcesses() []ProcInfo {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	now := time.Now()
	procCPUTimeMu.Lock()
	prevAt := procCPUAt
	elapsed := now.Sub(prevAt).Seconds()
	// Elapsed windows under this are too short to divide by reliably - any
	// jitter in the poll loop's own timing (a slow sensor read pushing this
	// cycle out, GC pause, etc.) gets massively amplified once you divide a
	// tick delta by a near-zero elapsed. Treat those as a fresh first sample
	// instead of computing a spike we'd have to show the user.
	const minReliableElapsed = 0.25
	firstSample := prevAt.IsZero() || elapsed < minReliableElapsed
	newCPUTime := map[int]procSample{}
	procCPUTimeMu.Unlock()
	maxPct := roundTo(100*float64(logicalCPUs()), 1)

	var out []ProcInfo
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		statPath := filepath.Join("/proc", e.Name(), "stat")
		statContent, ok := readStr(statPath)
		if !ok {
			continue
		}
		// comm is inside parens and may itself contain spaces/parens, so
		// split on the LAST ')' rather than naive whitespace fields.
		lastParen := strings.LastIndex(statContent, ")")
		if lastParen < 0 || lastParen+2 > len(statContent) {
			continue // short/truncated read, usually a process that exited mid-read - skip it, not a crash
		}
		firstParen := strings.Index(statContent, "(")
		if firstParen < 0 || firstParen+1 > lastParen {
			continue
		}
		comm := statContent[firstParen+1 : lastParen]
		rest := strings.Fields(statContent[lastParen+2:])
		// need through field 22 (starttime) - index 19 in rest, see comment below
		if len(rest) < 20 {
			continue
		}
		utime, _ := strconv.ParseFloat(rest[11], 64)
		stime, _ := strconv.ParseFloat(rest[12], 64)
		// starttime (field 22, jiffies-since-boot the process was created) -
		// stable per process instance, so it's what we use to detect PID
		// reuse below. rest[] starts at field 3, so field 22 is rest[19].
		starttime, _ := strconv.ParseFloat(rest[19], 64)
		total := utime + stime
		newCPUTime[pid] = procSample{ticks: total, start: starttime}

		cpuPct := 0.0
		if !firstSample && elapsed > 0 {
			procCPUTimeMu.Lock()
			prev, had := procCPUTime[pid]
			procCPUTimeMu.Unlock()
			// had-but-different-starttime means this pid number got recycled
			// by an unrelated process since the last sample (very common on
			// this hardware - games/emulators/helper processes launching and
			// exiting constantly). Diffing this process's ticks against the
			// old occupant's last-known ticks produces a meaningless delta
			// (that's what was showing up as processes at 1000%+ CPU) - so
			// treat a recycled pid as a first sample instead, same as a
			// genuinely brand-new process.
			if had && prev.start == starttime {
				cpuPct = roundTo(100*(total-prev.ticks)/userHZ/elapsed, 1)
				if cpuPct < 0 {
					cpuPct = 0
				}
				if cpuPct > maxPct {
					cpuPct = maxPct
				}
			}
		}

		memMB := 0.0
		uid := ""
		if statusContent, ok := readStr(filepath.Join("/proc", e.Name(), "status")); ok {
			for _, line := range strings.Split(statusContent, "\n") {
				if strings.HasPrefix(line, "VmRSS:") {
					f := strings.Fields(line)
					if len(f) >= 2 {
						kb, _ := strconv.ParseFloat(f[1], 64)
						memMB = roundTo(kb/1024, 1)
					}
				} else if strings.HasPrefix(line, "Uid:") {
					f := strings.Fields(line)
					if len(f) >= 2 {
						uid = f[1]
					}
				}
			}
		}

		out = append(out, ProcInfo{PID: pid, Name: comm, CPU: cpuPct, MemMB: memMB, User: uidToName(uid)})
	}

	procCPUTimeMu.Lock()
	procCPUTime = newCPUTime
	procCPUAt = now
	procCPUTimeMu.Unlock()

	return out
}

func killProcess(pid int, sig string) error {
	if pid <= 1 {
		return fmt.Errorf("refusing to signal pid %d", pid)
	}
	s := syscall.SIGTERM
	if strings.EqualFold(sig, "KILL") {
		s = syscall.SIGKILL
	}
	if err := syscall.Kill(pid, s); err != nil {
		return fmt.Errorf("signal failed: %v (needs matching user or root)", err)
	}
	return nil
}

func readNVMe(readings *[]Reading) {
	matches, _ := filepath.Glob("/sys/class/hwmon/hwmon*")
	for _, path := range matches {
		name, ok := readStr(filepath.Join(path, "name"))
		if !ok || !strings.HasPrefix(name, "nvme") {
			continue
		}
		f := findInputByLabel(path, "temp", "composite", true)
		if f == "" {
			f = filepath.Join(path, "temp1_input")
		}
		if v, ok := readNum(f); ok {
			add(readings, "nvme_temp", roundTo(v/1000, 0), true)
		}
		break
	}

	// Live disk read/write RATE in MB/s (not a lifetime total) - delta between
	// this poll and the last one, divided by elapsed time. Ported directly
	// from the user's own proven reference script: same field indices (2=read
	// sectors, 6=write sectors, always 512 bytes/sector regardless of the
	// drive's real sector size), same aggregation across every real block
	// device (skipping loop/ram), same per-device previous-value tracking.
	now := time.Now()
	interval := now.Sub(diskPrevTime).Seconds()
	diskPrevTime = now
	totalReadBytes, totalWriteBytes := 0.0, 0.0
	entries, _ := os.ReadDir("/sys/block")
	for _, entry := range entries {
		dev := entry.Name()
		if strings.HasPrefix(dev, "loop") || strings.HasPrefix(dev, "ram") {
			continue
		}
		statPath := filepath.Join("/sys/block", dev, "stat")
		content, ok := readStr(statPath)
		if !ok {
			continue
		}
		fields := strings.Fields(content)
		if len(fields) < 7 {
			continue
		}
		readSectors, err1 := strconv.ParseFloat(fields[2], 64)
		writeSectors, err2 := strconv.ParseFloat(fields[6], 64)
		if err1 != nil || err2 != nil {
			continue
		}
		readBytes := readSectors * 512
		writeBytes := writeSectors * 512
		diskPrevMu.Lock()
		prev, seen := diskPrev[dev]
		diskPrev[dev] = [2]float64{readBytes, writeBytes}
		diskPrevMu.Unlock()
		if seen {
			if d := readBytes - prev[0]; d > 0 {
				totalReadBytes += d
			}
			if d := writeBytes - prev[1]; d > 0 {
				totalWriteBytes += d
			}
		}
	}
	if interval > 0 {
		add(readings, "disk_read", roundTo(totalReadBytes/interval/(1024*1024), 1), true)
		add(readings, "disk_write", roundTo(totalWriteBytes/interval/(1024*1024), 1), true)
	}
}

// CU count - three-tier fallback chain, in priority order:
//  1. Live umr masked register read (readUMRActiveCU) - the real live
//     active-CU figure straight off the GPU's own SPI_PG_ENABLE_STATIC_WGP_MASK
//     register, same four masked reads as bc250-cu-count-passthru.sh's
//     write_cu_count(). Needs root (umr needs direct MMIO/debugfs access)
//     and the umr binary on PATH. Re-read fresh every poll, never cached -
//     this can genuinely change at runtime as WGPs get power-gated, unlike
//     the two fallbacks below.
//  2. /tmp/bc250_cu_count - written by the user's separate root-privileged
//     passthru script, for when this server itself isn't running as root
//     and can't do the umr read directly. Also re-read fresh every poll.
//  3. vulkaninfo's static num_cu - the physical CU count. A fixed hardware
//     spec, not a live figure, but works with zero privilege and no extra
//     scripts. Cached forever once found, since it can't change at runtime.
var cuCountCache float64 = -1
var cuCountCacheMu sync.Mutex

var diskPrev = map[string][2]float64{} // device name -> [readBytes, writeBytes] from last poll
var diskPrevMu sync.Mutex
var diskPrevTime = time.Now()

var umrHexRe = regexp.MustCompile(`0x[0-9a-fA-F]+`)

// readUMRActiveCU runs the exact same four masked
// mmSPI_PG_ENABLE_STATIC_WGP_MASK reads as the reference
// bc250-cu-count-passthru.sh write_cu_count(), and sums their popcount*2 -
// the live active CU count straight from the GPU's register state. Returns
// ok=false (rather than erroring) on anything that stops this from working -
// not root, umr missing, a read failing - so the caller falls through to the
// next method in the chain instead of surfacing a partial/bogus value.
func readUMRActiveCU() (int, bool) {
	if os.Geteuid() != 0 {
		return 0, false
	}
	if _, err := exec.LookPath("umr"); err != nil {
		return 0, false
	}
	total := 0
	for _, bMask := range [][]string{{"0", "0", "0"}, {"0", "1", "0"}, {"1", "0", "0"}, {"1", "1", "0"}} {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		args := append([]string{"-r", "*.gfx1013.mmSPI_PG_ENABLE_STATIC_WGP_MASK", "-b"}, bMask...)
		out, err := exec.CommandContext(ctx, "umr", args...).CombinedOutput()
		cancel()
		if err != nil {
			return 0, false
		}
		hex := umrHexRe.FindString(string(out))
		if hex == "" {
			return 0, false
		}
		v, err := strconv.ParseUint(hex, 0, 64)
		if err != nil {
			return 0, false
		}
		total += bits.OnesCount64(v) * 2
	}
	return total, true
}

func readCUCount(readings *[]Reading) {
	if v, ok := readUMRActiveCU(); ok {
		add(readings, "cu_active", float64(v), true)
		return
	}

	if content, ok := readStr("/tmp/bc250_cu_count"); ok {
		if v, err := strconv.Atoi(strings.TrimSpace(content)); err == nil && v > 0 {
			add(readings, "cu_active", float64(v), true)
			return
		}
	}

	// Last resort: vulkaninfo, if neither the live umr read nor the tmp
	// passthru file worked.
	cuCountCacheMu.Lock()
	cached := cuCountCache
	cuCountCacheMu.Unlock()
	if cached >= 0 {
		add(readings, "cu_active", cached, true)
		return
	}

	cmd := exec.Command("vulkaninfo", "--summary")
	cmd.Env = append(os.Environ(), "RADV_DEBUG=info")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "num_cu") && !strings.Contains(line, "per_sh") {
			parts := strings.Split(line, "=")
			if len(parts) > 0 {
				if v, err := strconv.Atoi(strings.TrimSpace(parts[len(parts)-1])); err == nil {
					cuCountCacheMu.Lock()
					cuCountCache = float64(v)
					cuCountCacheMu.Unlock()
					add(readings, "cu_active", float64(v), true)
				}
			}
			return
		}
	}
}

func readK10temp(readings *[]Reading) {
	hwmon := findHwmon("k10temp")
	if hwmon == "" {
		return
	}
	f := findInputByLabel(hwmon, "temp", "tctl", true)
	if f == "" {
		f = filepath.Join(hwmon, "temp1_input")
	}
	if v, ok := readNum(f); ok {
		add(readings, "cpu", roundTo(v/1000, 0), true)
	}
}

var sclkRe = regexp.MustCompile(`(\d+)Mhz`)

func readAMDGPU(readings *[]Reading) {
	hwmon := findHwmon("amdgpu")
	if hwmon == "" {
		return
	}
	// CPU/APU core voltages - read from amdgpu's own reported rails (in0 =
	// vddgfx/APU, in1 = vddnb/CPU), matching the user's proven reference
	// exactly - positional indexing, same as the reference script does.
	if v, ok := readNum(filepath.Join(hwmon, "in0_input")); ok {
		add(readings, "apu_mv", roundTo(v, 0), true)
	}
	if v, ok := readNum(filepath.Join(hwmon, "in1_input")); ok {
		add(readings, "cpu_mv", roundTo(v, 0), true)
	}

	f := findInputByLabel(hwmon, "temp", "edge", true)
	if f == "" {
		f = filepath.Join(hwmon, "temp1_input")
	}
	if v, ok := readNum(f); ok {
		add(readings, "gpu", roundTo(v/1000, 0), true)
	}

	// The "device" symlink lives INSIDE the hwmon dir, not as its sibling.
	deviceDir := filepath.Join(hwmon, "device")

	// Prefer freq1_input - a plain current-clock readout straight from the
	// hwmon interface, in Hz (e.g. 2000000000 == 2000MHz, hence the /1e6
	// below to get MHz out of it). Falls back to parsing the active line out
	// of pp_dpm_sclk (e.g. "1: 2000Mhz *") on cards/drivers that don't expose
	// freq1_input.
	if v, ok := readNum(filepath.Join(hwmon, "freq1_input")); ok {
		add(readings, "gpu_freq", roundTo(v/1_000_000, 0), true)
	} else if content, ok := readStr(filepath.Join(deviceDir, "pp_dpm_sclk")); ok {
		for _, line := range strings.Split(content, "\n") {
			if strings.Contains(line, "*") {
				if m := sclkRe.FindStringSubmatch(line); m != nil {
					if mhz, err := strconv.Atoi(m[1]); err == nil {
						add(readings, "gpu_freq", float64(mhz), true)
					}
				}
				break
			}
		}
	}

	pptPath := findInputByLabel(hwmon, "power", "ppt", false)
	if pptPath == "" {
		pptPath = filepath.Join(hwmon, "power1_average")
	}
	if v, ok := readNum(pptPath); ok {
		add(readings, "ppt", roundTo(v/1_000_000, 1), true)
	}

	// gpu_busy_percent returns "Operation not supported" on this board/driver
	// combo - gpu_metrics (a binary telemetry blob) works instead. Byte offset
	// 28:30 as little-endian uint16 is the load field, per confirmed-working
	// reference code for this exact hardware. 65535 means "no data yet".
	if data, err := os.ReadFile(filepath.Join(deviceDir, "gpu_metrics")); err == nil && len(data) >= 30 {
		raw := int(data[28]) | int(data[29])<<8
		if raw != 65535 {
			load := float64(raw)
			if load > 100 {
				load = load / 100
			}
			if load > 100 {
				load = 100
			}
			add(readings, "gpu_usage", roundTo(load, 0), true)
		}
	}

	// VRAM alone under-reports on this APU's unified memory architecture -
	// a large chunk of dynamic graphics memory usage shows up as GTT (system
	// RAM mapped for GPU use), not the VRAM counter. Confirmed against the
	// user's own working reference script - this is why it looked frozen.
	vram, vramOk := readNum(filepath.Join(deviceDir, "mem_info_vram_used"))
	gtt, gttOk := readNum(filepath.Join(deviceDir, "mem_info_gtt_used"))
	if vramOk || gttOk {
		total := 0.0
		if vramOk {
			total += vram
		}
		if gttOk {
			total += gtt
		}
		add(readings, "vram_used", roundTo(total/(1024*1024), 0), true)
	}
}

var cpuMHzRe = regexp.MustCompile(`cpu MHz\s*:\s*([\d.]+)`)

func readCPUFreqAvg() (float64, bool) {
	content, ok := readStr("/proc/cpuinfo")
	if !ok {
		return 0, false
	}
	matches := cpuMHzRe.FindAllStringSubmatch(content, -1)
	if len(matches) == 0 {
		return 0, false
	}
	sum := 0.0
	for _, m := range matches {
		v, _ := strconv.ParseFloat(m[1], 64)
		sum += v
	}
	return roundTo(sum/float64(len(matches)), 0), true
}

func readCPUUsagePercent() (float64, bool) {
	content, ok := readStr("/proc/stat")
	if !ok {
		return 0, false
	}
	firstLine := strings.Split(content, "\n")[0]
	fields := strings.Fields(firstLine)[1:]
	nums := make([]float64, len(fields))
	total := 0.0
	for i, f := range fields {
		v, _ := strconv.ParseFloat(f, 64)
		nums[i] = v
		total += v
	}
	if len(nums) < 5 {
		return 0, false
	}
	idle := nums[3] + nums[4]
	if !haveLastCPU {
		lastCPUIdle, lastCPUTotal = idle, total
		haveLastCPU = true
		return 0, false
	}
	dIdle := idle - lastCPUIdle
	dTotal := total - lastCPUTotal
	lastCPUIdle, lastCPUTotal = idle, total
	if dTotal <= 0 {
		return 0, false
	}
	return roundTo(100*(1-dIdle/dTotal), 0), true
}

func readCPUTopology() (cores, threads int, coresOk, threadsOk bool) {
	content, ok := readStr("/proc/cpuinfo")
	if !ok {
		return 0, 0, false, false
	}
	coreIDs := map[string]bool{}
	threadCount := 0
	for _, line := range strings.Split(content, "\n") {
		if strings.HasPrefix(line, "processor") {
			threadCount++
		} else if strings.HasPrefix(line, "core id") {
			parts := strings.SplitN(line, ":", 2)
			if len(parts) == 2 {
				coreIDs[strings.TrimSpace(parts[1])] = true
			}
		}
	}
	return len(coreIDs), threadCount, len(coreIDs) > 0, threadCount > 0
}

var memTotalRe = regexp.MustCompile(`MemTotal:\s+(\d+)`)
var memAvailRe = regexp.MustCompile(`MemAvailable:\s+(\d+)`)

func readRAMUsedMB() (float64, bool) {
	content, ok := readStr("/proc/meminfo")
	if !ok {
		return 0, false
	}
	tm := memTotalRe.FindStringSubmatch(content)
	am := memAvailRe.FindStringSubmatch(content)
	if tm == nil || am == nil {
		return 0, false
	}
	total, _ := strconv.ParseFloat(tm[1], 64)
	avail, _ := strconv.ParseFloat(am[1], 64)
	return roundTo((total-avail)/1024, 0), true
}

func roundTo(v float64, decimals int) float64 {
	mult := 1.0
	for i := 0; i < decimals; i++ {
		mult *= 10
	}
	return float64(int(v*mult+0.5)) / mult
}

func readAllSensors() []Reading {
	var readings []Reading
	readK10temp(&readings)
	if v, ok := readCPUFreqAvg(); ok {
		add(&readings, "cpu_freq", v, true)
	}
	if v, ok := readCPUUsagePercent(); ok {
		add(&readings, "cpu_usage", v, true)
	}
	if cores, threads, cOk, tOk := readCPUTopology(); true {
		if cOk {
			add(&readings, "cpu_cores", float64(cores), true)
		}
		if tOk {
			add(&readings, "cpu_threads", float64(threads), true)
		}
	}
	if v, ok := readRAMUsedMB(); ok {
		add(&readings, "ram_used", v, true)
	}
	readAMDGPU(&readings)
	readISL69247(&readings)
	readNCT6686(&readings)
	readNVMe(&readings)
	readCUCount(&readings)
	return readings
}

var (
	latestProcs   []ProcInfo
	latestProcsMu sync.Mutex
)

func pollLoop(interval time.Duration) {
	for {
		func() {
			// Recovers from any panic in a single poll cycle (sensors, fan
			// curves, or process listing) so one bad /proc read or hwmon
			// glitch can't take down the entire server - it just logs and
			// tries again next tick instead of crash-looping systemd.
			defer func() {
				if r := recover(); r != nil {
					fmt.Println("pollLoop: recovered from panic:", r)
				}
			}()

			readings := readAllSensors()
			latestMu.Lock()
			latest = readings
			latestAt = time.Now()
			latestMu.Unlock()

			sensorVals := make(map[string]float64, len(readings))
			for _, r := range readings {
				sensorVals[r.Key] = r.Value
			}
			applyFanCurves(sensorVals)

			procs := readProcesses()
			latestProcsMu.Lock()
			latestProcs = procs
			latestProcsMu.Unlock()
		}()
		time.Sleep(interval)
	}
}

// ---------------- HTTP server ----------------

var panelHTMLPath string

func withCORS(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
}

func main() {
	port := flag.Int("port", 8091, "port to serve on")
	intervalSec := flag.Float64("interval", 2.0, "sensor poll interval in seconds")
	termUser := flag.String("user", "", "user account the Terminal tab's shell drops privileges to (falls back to root if unset/not found)")
	flag.Parse()
	terminalUser = *termUser

	exePath, _ := os.Executable()
	panelHTMLPath = filepath.Join(filepath.Dir(exePath), "bc250_control_panel.html")
	fanCurvesPath = filepath.Join(filepath.Dir(exePath), "bc250_fan_curves.json")
	loadFanCurves()

	go pollLoop(time.Duration(*intervalSec * float64(time.Second)))

	mux := http.NewServeMux()

	mux.HandleFunc("/api/sensors", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions {
			withCORS(w)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		latestMu.Lock()
		age := -1
		if !latestAt.IsZero() {
			age = int(time.Since(latestAt).Seconds())
		}
		out := make([]Reading, len(latest))
		copy(out, latest)
		latestMu.Unlock()
		for i := range out {
			out[i].Age = age
		}
		withCORS(w)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"sensors": out})
	})

	mux.HandleFunc("/api/fan", func(w http.ResponseWriter, r *http.Request) {
		withCORS(w)
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method == http.MethodGet {
			json.NewEncoder(w).Encode(readFanStatus())
			return
		}
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			Index   int      `json:"index"`
			Enabled *bool    `json:"enabled"`
			Percent *float64 `json:"percent"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "bad request body"})
			return
		}
		disableCurveForChannel(body.Index) // manual/auto override always wins over a stored curve
		if err := writeFan(body.Index, body.Enabled, body.Percent); err != nil {
			w.WriteHeader(http.StatusConflict)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(readFanStatus())
	})

	mux.HandleFunc("/api/fan/reset", func(w http.ResponseWriter, r *http.Request) {
		withCORS(w)
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if err := resetAllFansToStock(); err != nil {
			w.WriteHeader(http.StatusConflict)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(readFanStatus())
	})

	mux.HandleFunc("/api/fancurves", func(w http.ResponseWriter, r *http.Request) {
		withCORS(w)
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		switch r.Method {
		case http.MethodGet:
			fanCurvesMu.Lock()
			list := make([]*FanCurve, 0, len(fanCurves))
			for _, c := range fanCurves {
				list = append(list, c)
			}
			fanCurvesMu.Unlock()
			sort.Slice(list, func(i, j int) bool { return list[i].Channel < list[j].Channel })

			fanCurveLiveMu.Lock()
			live := make(map[string]curveLivePoint, len(fanCurveLive))
			for ch, v := range fanCurveLive {
				live[strconv.Itoa(ch)] = v
			}
			fanCurveLiveMu.Unlock()

			json.NewEncoder(w).Encode(map[string]interface{}{"curves": list, "live": live})

		case http.MethodPost:
			var body FanCurve
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				json.NewEncoder(w).Encode(map[string]string{"error": "bad request body"})
				return
			}
			if body.Channel < 1 {
				w.WriteHeader(http.StatusBadRequest)
				json.NewEncoder(w).Encode(map[string]string{"error": "invalid channel"})
				return
			}
			if body.Enabled && (body.Sensor == "" || len(body.Points) < 2) {
				w.WriteHeader(http.StatusBadRequest)
				json.NewEncoder(w).Encode(map[string]string{"error": "a curve needs a sensor and at least 2 points before it can be enabled"})
				return
			}
			for i, p := range body.Points {
				if p.Percent < 0 || p.Percent > 100 {
					w.WriteHeader(http.StatusBadRequest)
					json.NewEncoder(w).Encode(map[string]string{"error": fmt.Sprintf("point %d: percent must be 0-100", i)})
					return
				}
			}
			sort.Slice(body.Points, func(i, j int) bool { return body.Points[i].Temp < body.Points[j].Temp })

			fanCurvesMu.Lock()
			fanCurves[body.Channel] = &body
			delete(fanCurveLastPct, body.Channel) // see disableCurveForChannel comment - any curve replacement must force a real write next tick, not risk a stale-cache skip
			saveFanCurvesLocked()
			fanCurvesMu.Unlock()

			// Turning a curve off (or replacing it while disabled) should hand
			// the channel back to plain Auto rather than leaving it stuck at
			// whatever percent the curve last wrote.
			if !body.Enabled {
				enable := false
				writeFan(body.Channel, &enable, nil)
			}
			json.NewEncoder(w).Encode(map[string]bool{"ok": true})

		case http.MethodDelete:
			ch, _ := strconv.Atoi(r.URL.Query().Get("channel"))
			if ch < 1 {
				w.WriteHeader(http.StatusBadRequest)
				json.NewEncoder(w).Encode(map[string]string{"error": "invalid channel"})
				return
			}
			fanCurvesMu.Lock()
			delete(fanCurves, ch)
			delete(fanCurveLastPct, ch)
			saveFanCurvesLocked()
			fanCurvesMu.Unlock()
			enable := false
			writeFan(ch, &enable, nil)
			json.NewEncoder(w).Encode(map[string]bool{"ok": true})

		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})

	mux.HandleFunc("/api/processes", func(w http.ResponseWriter, r *http.Request) {
		withCORS(w)
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		latestProcsMu.Lock()
		procs := make([]ProcInfo, len(latestProcs))
		copy(procs, latestProcs)
		latestProcsMu.Unlock()
		// Sort by CPU desc, then mem desc, purely so that IF the list needs
		// truncating below (only in pathological cases - hundreds of
		// processes), the ones cut are the least relevant. The client always
		// re-sorts by whatever column the person actually picked (Name/PID/
		// CPU/RAM, either direction) - this is not the final display order.
		sort.Slice(procs, func(i, j int) bool {
			if procs[i].CPU != procs[j].CPU {
				return procs[i].CPU > procs[j].CPU
			}
			return procs[i].MemMB > procs[j].MemMB
		})
		if len(procs) > 500 {
			// Was capped at 60 - fine when the server's CPU-sort was also the
			// only display order, but once the client can sort by Name/PID/RAM
			// instead, truncating to a CPU-biased top-60 BEFORE that client
			// sort meant a Name-sorted view only ever showed a partial,
			// CPU-skewed slice of processes - sorting alphabetically should
			// cover everything, not just the heaviest 60. 500 is just a
			// sanity ceiling for pathological cases (fork bombs, hundreds of
			// containers); virtually no normal desktop ever reaches it.
			procs = procs[:500]
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"processes": procs})
	})

	mux.HandleFunc("/api/kill", func(w http.ResponseWriter, r *http.Request) {
		withCORS(w)
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			PID    int    `json:"pid"`
			Signal string `json:"signal"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.PID == 0 {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "bad request body"})
			return
		}
		if err := killProcess(body.PID, body.Signal); err != nil {
			w.WriteHeader(http.StatusConflict)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(map[string]bool{"ok": true})
	})

	mux.HandleFunc("/api/terminal", handleTerminalWS)

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" && !strings.HasPrefix(r.URL.Path, "/index") {
			withCORS(w)
			http.NotFound(w, r)
			return
		}
		body, err := os.ReadFile(panelHTMLPath)
		if err != nil {
			withCORS(w)
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte("bc250_control_panel.html not found next to this binary"))
			return
		}
		withCORS(w)
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate") // a stale cached copy of the JS after an update is a real failure mode otherwise
		w.Write(body)
	})

	addr := fmt.Sprintf(":%d", *port) // bind both IPv4 and IPv6 - 0.0.0.0 alone is IPv4-only, and
	// browsers resolving "localhost" to ::1 first would get refused
	fmt.Printf("BC-250 panel + sensors serving at http://%s/\n", addr)
	fmt.Printf("Sensor API at http://%s/api/sensors\n", addr)
	fmt.Println("hwmon directly.")
	if err := http.ListenAndServe(addr, mux); err != nil {
		fmt.Println("Server error:", err)
		os.Exit(1)
	}
}
