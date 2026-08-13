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
// Build:
//   go build -o bc250-panel bc250_panel_server.go
// Run:
//   ./bc250-panel --port 8091 --interval 2
//
// Reads sysfs directly (no lm-sensors dependency), same as the Python
// version: each chip is found dynamically by its hwmon driver name, and each
// value is found by matching its *_label file content - nothing hardcoded to
// a specific hwmonN path, since that numbering isn't stable across boots.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------------- reading ----------------

type Reading struct {
	Key   string  `json:"k"`
	Value float64 `json:"v"`
	Age   int     `json:"a"`
}

var (
	latest      []Reading
	latestMu    sync.Mutex
	latestAt    time.Time
	hwmonCache  = map[string]string{} // driver name -> hwmon dir path
	hwmonCacheMu sync.Mutex
	lastCPUIdle, lastCPUTotal float64
	haveLastCPU bool
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
	if pwm, ok := readNum(filepath.Join(hwmon, "pwm1")); ok {
		add(readings, "fan_pwm", roundTo(pwm/255*100, 0), true)
	}
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

// CU count - vulkaninfo reports the physical CU count (a fixed hardware spec,
// not a live "currently active" figure), same approach already proven
// working in the user's own reference script. Live per-CU active/idle
// masking via umr would need the exact command - not implemented here yet.
var cuCountCache float64 = -1
var cuCountCacheMu sync.Mutex

var diskPrev = map[string][2]float64{} // device name -> [readBytes, writeBytes] from last poll
var diskPrevMu sync.Mutex
var diskPrevTime = time.Now()

// CU count - reads /tmp/bc250_cu_count, written by the user's own separate
// root-privileged script that runs the actual umr register read. This
// server never invokes umr or needs any elevated privilege itself - it just
// reads a plain file, matching the reference script's exact get_cu_count()
// approach. Falls back to vulkaninfo's static count if that file is missing
// or invalid, same fallback order as the reference.
func readCUCount(readings *[]Reading) {
	cuCountCacheMu.Lock()
	cached := cuCountCache
	cuCountCacheMu.Unlock()
	if cached >= 0 {
		add(readings, "cu_active", cached, true)
		return
	}

	if content, ok := readStr("/tmp/bc250_cu_count"); ok {
		if v, err := strconv.Atoi(strings.TrimSpace(content)); err == nil && v > 0 {
			cuCountCacheMu.Lock()
			cuCountCache = float64(v)
			cuCountCacheMu.Unlock()
			add(readings, "cu_active", float64(v), true)
			return
		}
	}

	// Fallback: vulkaninfo, if the tmp file isn't there or isn't valid yet.
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

	if content, ok := readStr(filepath.Join(deviceDir, "pp_dpm_sclk")); ok {
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

func pollLoop(interval time.Duration) {
	for {
		readings := readAllSensors()
		latestMu.Lock()
		latest = readings
		latestAt = time.Now()
		latestMu.Unlock()
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
	flag.Parse()

	exePath, _ := os.Executable()
	panelHTMLPath = filepath.Join(filepath.Dir(exePath), "bc250_argb_panel.html")

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
			w.Write([]byte("bc250_argb_panel.html not found next to this binary"))
			return
		}
		withCORS(w)
		w.Header().Set("Content-Type", "text/html")
		w.Write(body)
	})

	addr := fmt.Sprintf(":%d", *port) // bind both IPv4 and IPv6 - 0.0.0.0 alone is IPv4-only, and
	                                    // browsers resolving "localhost" to ::1 first would get refused
	fmt.Printf("BC-250 panel + sensors serving at http://%s/\n", addr)
	fmt.Printf("Sensor API at http://%s/api/sensors\n", addr)
	fmt.Println("Reading hwmon directly - no lm-sensors dependency, single static binary.")
	if err := http.ListenAndServe(addr, mux); err != nil {
		fmt.Println("Server error:", err)
		os.Exit(1)
	}
}
