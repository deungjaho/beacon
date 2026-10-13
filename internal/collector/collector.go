// Package collector implements bounded host and tmux resource sampling.
// It never blocks the caller: all commands have timeouts and all parsers
// degrade safely on missing or malformed input.
//
// Sampling is split into three tiers with independent cadence:
//
//   - Fast (4s): CPU usage, memory pressure, process count.
//     Uses top -l 1 -n 0 (macOS) or /proc/stat + ps (Linux).
//     Does NOT scan the full process list.
//   - Footprint (10s): per-pane/window/session/total tmux memory.
//     Uses top -l 1 -n 999 (macOS physical footprint) or ps -eo (Linux RSS).
//     May also update CPU/proc count as a bonus but does not replace fast.
//   - Slow (60s): root disk usage.
//     Uses df -k (macOS: /System/Volumes/Data, Linux: /).
//
// Each tier writes its own fields with its own sampled_at timestamp.
// The daemon merges all tiers into a single snapshot and retains
// last-good values with a stale flag when a tier fails.
package collector

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Metrics is the full resource snapshot written by the daemon.
// Fields are grouped by sampling tier; each tier has its own
// sampled_at and stale metadata.
type Metrics struct {
	SampledAt int64 `json:"sampled_at"` // overall snapshot time

	// --- Fast tier (4s) ---
	CPUPercent    float64 `json:"cpu_percent"`
	CPUOK         bool    `json:"cpu_ok"`
	CPUSampledAt  int64   `json:"cpu_sampled_at"`
	CPUStale      bool    `json:"cpu_stale"`
	MemPressure   int     `json:"mem_pressure"`
	MemPressureOK bool    `json:"mem_pressure_ok"`
	ProcCount     int     `json:"proc_count"`
	ProcCountOK   bool    `json:"proc_count_ok"`
	ThreadCount   int     `json:"thread_count,omitempty"`
	ThreadCountOK bool    `json:"thread_count_ok,omitempty"`

	// Thermal pressure level (macOS: 0=nominal, 1=moderate, 2=heavy, 3=trapping)
	ThermalLevel int  `json:"thermal_level"`
	ThermalOK    bool `json:"thermal_ok"`

	// System load averages (5m/15m available on macOS/Linux)
	LoadAvg1m  float64 `json:"load_avg_1m"`
	LoadAvg5m  float64 `json:"load_avg_5m,omitempty"`
	LoadAvg15m float64 `json:"load_avg_15m,omitempty"`
	LoadAvgOK  bool    `json:"load_avg_ok"`

	// Real free physical memory
	RealFreeMemKB uint64 `json:"real_free_mem_kb"`
	RealFreeMem   string `json:"real_free_mem"`
	RealFreeMemOK bool   `json:"real_free_mem_ok"`

	// Memory compressor usage
	CompressorMemKB uint64 `json:"compressor_mem_kb"`
	CompressorMem   string `json:"compressor_mem"`
	CompressorMemOK bool   `json:"compressor_mem_ok"`

	// --- Footprint tier (10s) ---
	// Per-pane/window/session/total tmux memory.
	// macOS: physical footprint from top MEM column.
	// Linux: RSS from ps.
	PaneMemKB      map[string]uint64 `json:"pane_mem_kb"`
	WindowMemKB    map[string]uint64 `json:"window_mem_kb"`
	SessionMemKB   map[string]uint64 `json:"session_mem_kb"`
	TotalMemKB     uint64            `json:"total_mem_kb"`
	PaneMem        map[string]string `json:"pane_mem"`
	WindowMem      map[string]string `json:"window_mem"`
	SessionMem     map[string]string `json:"session_mem"`
	TotalMem       string            `json:"total_mem"`
	FootprintOK    bool              `json:"footprint_ok"`
	FootprintAt    int64             `json:"footprint_sampled_at"`
	FootprintStale bool              `json:"footprint_stale"`

	// --- Slow tier (60s) ---
	// Root disk usage. DiskAvailableKB is the raw df "Available" column,
	// retained for render-time color judgment (red when < 5 GiB).
	// DiskUsed is consumed space (macOS: total-available, Linux: df Used).
	DiskUsedKB      uint64 `json:"disk_used_kb"`
	DiskTotalKB     uint64 `json:"disk_total_kb"`
	DiskAvailableKB uint64 `json:"disk_available_kb"`
	DiskOK          bool   `json:"disk_ok"`
	DiskSampledAt   int64  `json:"disk_sampled_at"`
	DiskStale       bool   `json:"disk_stale"`
	DiskUsed        string `json:"disk_used"`
	DiskTotal       string `json:"disk_total"`

	// --- Fast tier extras ---
	// Top processes by CPU / by memory (up to 5 each).
	TopCPUProcs []ProcStat `json:"top_cpu_procs,omitempty"`
	TopMemProcs []ProcStat `json:"top_mem_procs,omitempty"`

	// Swap usage (macOS vm.swapusage, Linux /proc/meminfo).
	SwapTotalKB uint64 `json:"swap_total_kb,omitempty"`
	SwapUsedKB  uint64 `json:"swap_used_kb,omitempty"`
	SwapTotal   string `json:"swap_total,omitempty"`
	SwapUsed    string `json:"swap_used,omitempty"`
	SwapOK      bool   `json:"swap_ok,omitempty"`

	// --- Slow tier extras ---
	// Boot time as unix seconds; uptime = now - BootTimeSec (live, no drift).
	BootTimeSec  int64  `json:"boot_time_sec,omitempty"`
	BatteryPct   int    `json:"battery_pct,omitempty"`
	BatteryState string `json:"battery_state,omitempty"` // charging|discharging|charged|ac
	BatteryOK    bool   `json:"battery_ok,omitempty"`

	// --- Footprint tier extras ---
	// Per-pane metadata for display joins (cmd/path/window name).
	PaneMeta    map[string]PaneMeta `json:"pane_meta,omitempty"`
	WindowNames map[string]string   `json:"window_names,omitempty"`

	// Per-pane aggregated %CPU of each pane's process subtree (macOS only).
	PaneCPU map[string]float64 `json:"pane_cpu,omitempty"`
	// Pane memory baseline captured ~5min ago by the daemon; used to flag
	// panes whose footprint grew sharply.
	PaneMemBaselineKB map[string]uint64 `json:"pane_mem_baseline_kb,omitempty"`

	// Physical memory decomposition (fast tier): Activity-Monitor-style
	// "Memory Used" — macOS: anon-purgeable+wired+compressed (vm_stat);
	// Linux: MemTotal-MemAvailable. Wired/App break down macOS usage.
	PhysMemUsedKB  uint64 `json:"phys_mem_used_kb,omitempty"`
	PhysMemTotalKB uint64 `json:"phys_mem_total_kb,omitempty"`
	PhysMemOK      bool   `json:"phys_mem_ok,omitempty"`
	PhysWiredKB    uint64 `json:"phys_wired_kb,omitempty"`
	PhysAppKB      uint64 `json:"phys_app_kb,omitempty"`

	// CPU breakdown (fast tier, iostat us/sy/id columns; macOS only).
	CPUUsr     float64 `json:"cpu_usr,omitempty"`
	CPUSys     float64 `json:"cpu_sys,omitempty"`
	CPUIdle    float64 `json:"cpu_idle,omitempty"`
	CPUSplitOK bool    `json:"cpu_split_ok,omitempty"`

	// Disk I/O throughput (fast tier, iostat tps/MB/s; macOS only).
	DiskTps  float64 `json:"disk_tps,omitempty"`
	DiskMBs  float64 `json:"disk_mbs,omitempty"`
	DiskIOOK bool    `json:"disk_io_ok,omitempty"`

	// Paging rates (/s), derived by the daemon from cumulative vm_stat
	// Pageins/Pageouts (Linux: pgpgin/pgpgout) between fast samples.
	PageInRate  float64 `json:"page_in_rate,omitempty"`
	PageOutRate float64 `json:"page_out_rate,omitempty"`
	PageIOOK    bool    `json:"page_io_ok,omitempty"`

	// Network throughput (KB/s), derived by the daemon from cumulative
	// counter deltas between fast samples.
	NetRxKBs float64 `json:"net_rx_kbs,omitempty"`
	NetTxKBs float64 `json:"net_tx_kbs,omitempty"`
	NetOK    bool    `json:"net_ok,omitempty"`
}

// ProcStat is a single process entry for top-N listings.
type ProcStat struct {
	PID   int     `json:"pid"`
	Name  string  `json:"name"`
	CPU   float64 `json:"cpu,omitempty"`
	MemKB uint64  `json:"mem_kb,omitempty"`
	Mem   string  `json:"mem,omitempty"`
}

// PaneMeta describes a tmux pane's display context captured alongside
// the memory footprint sample.
type PaneMeta struct {
	Session     string `json:"session"`
	WindowIndex string `json:"window_index"`
	WindowName  string `json:"window_name"`
	Cmd         string `json:"cmd"`
	Path        string `json:"path"`
}

// tmuxPane is a single pane entry from tmux list-panes.
type tmuxPane struct {
	Session     string
	WindowIndex string
	WindowID    string
	PaneID      string
	PanePID     int
	Cmd         string
	Path        string
	WindowName  string
}

// MemoryKind identifies what MemoryKB represents on the current platform.
type MemoryKind string

const (
	// MemoryKindFootprint: macOS physical footprint from top's MEM column.
	// This is the actual physical memory consumed by the process, not
	// MEM+CMPRS (which would double-count compressed pages).
	MemoryKindFootprint MemoryKind = "footprint"
	// MemoryKindRSS: Linux resident set size from ps. This is the standard
	// per-process memory metric on Linux.
	MemoryKindRSS MemoryKind = "rss"
)

// procInfo is a single process entry from ps or top.
// MemoryKB is the unified per-process memory metric; MemoryKind identifies
// what it represents (footprint on macOS, RSS on Linux).
type procInfo struct {
	PID        int
	PPID       int
	MemoryKB   uint64     // per-process memory in KB
	MemoryKind MemoryKind // semantic of MemoryKB (platform-dependent)
	CPU        float64    // %CPU (0 where not sampled, e.g. Linux ps path)
}

// paneMemoryResult holds the aggregated memory maps (in KB).
// All values use the same MemoryKind as the source process entries.
type paneMemoryResult struct {
	PaneMem    map[string]uint64
	WindowMem  map[string]uint64
	SessionMem map[string]uint64
	TotalMem   uint64
	Kind       MemoryKind // what the aggregated values represent
	// PaneMeta/WindowNames carry display context for the same panes,
	// keyed by pane_id and "session:window_index" respectively.
	PaneMeta    map[string]PaneMeta
	WindowNames map[string]string
	// PaneCPU is the aggregated %CPU of each pane's process subtree
	// (macOS only; empty on Linux where top -stats cpu is unavailable).
	PaneCPU map[string]float64
}

// FastResult holds the sampled metrics for the fast tier.
type FastResult struct {
	CPU            float64
	CPUOK          bool
	Pressure       int
	PressureOK     bool
	ProcCount      int
	ProcCountOK    bool
	ThreadCount    int
	ThreadCountOK  bool
	LoadAvg1m      float64
	LoadAvg5m      float64
	LoadAvg15m     float64
	LoadAvgOK      bool
	ThermalLevel   int
	ThermalOK      bool
	RealFreeKB     uint64
	RealFreeOK     bool
	CompressorKB   uint64
	CompressorOK   bool
	TopCPU         []ProcStat
	TopMem         []ProcStat
	SwapTotalKB    uint64
	SwapUsedKB     uint64
	SwapOK         bool
	PhysMemUsedKB  uint64
	PhysMemTotalKB uint64
	PhysMemOK      bool
	PhysWiredKB    uint64
	PhysAppKB      uint64
	CPUUsr         float64
	CPUSys         float64
	CPUIdle        float64
	DiskTps        float64
	DiskMBs        float64
	DiskIOOK       bool
	PageIns        uint64
	PageOuts       uint64
	PageIOOK       bool
	NetRxBytes     uint64
	NetTxBytes     uint64
	NetOK          bool
}

var cpuUsagePattern = regexp.MustCompile(`CPU usage:\s*([0-9.]+)% user,\s*([0-9.]+)% sys,`)
var memPressurePattern = regexp.MustCompile(`System-wide memory free percentage:\s*(\d+)%`)
var procCountPattern = regexp.MustCompile(`Processes:\s*(\d+)\s+total,`)
var pageSizePattern = regexp.MustCompile(`page size of\s*(\d+)`)
var pagesFreePattern = regexp.MustCompile(`Pages free:\s*(\d+)`)
var pagesCompressorPattern = regexp.MustCompile(`Pages used by compressor:\s*(\d+)`)

// commandTimeout is the max wait for any subprocess.
// iostat -c 2 -w 1 takes ~1s; allow headroom.
const commandTimeout = 5 * time.Second

// runCommand runs a command with a bounded timeout and returns its stdout.
// Uses context cancellation so the kill is race-free with Cmd.Start.
func runCommand(name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	// Ensure a UTF-8 locale so tools like ps emit real UTF-8 instead of
	// M-x caret escapes when the daemon runs under a bare launchd env.
	if os.Getenv("LC_ALL") == "" && os.Getenv("LANG") == "" {
		cmd.Env = append(os.Environ(), "LC_ALL=en_US.UTF-8")
	}
	return cmd.Output()
}

// --- Shared parsers ---

// parseMacOSCPUUsageTotal extracts user+sys from top output (legacy fallback).
func parseMacOSCPUUsageTotal(output string) (float64, bool) {
	matches := cpuUsagePattern.FindStringSubmatch(output)
	if len(matches) != 3 {
		return 0, false
	}
	user, err := strconv.ParseFloat(matches[1], 64)
	if err != nil {
		return 0, false
	}
	system, err := strconv.ParseFloat(matches[2], 64)
	if err != nil {
		return 0, false
	}
	total := user + system
	if total < 0 {
		total = 0
	}
	if total > 100 {
		total = 100
	}
	return total, true
}

// parseMacOSIOStatCPU parses `iostat -c 2 -w 1` output and returns
// us+sy from the second sample. The output has a header line, a column
// header line, a first data line, and a second data line. The number of
// disk columns varies by machine (one per disk), so the CPU columns
// (us, sy, id) are located dynamically by scanning the column header
// line for "us".
//
// Example output (single disk):
//
//	           disk0       cpu    load average
//	 KB/t  tps  MB/s  us sy id   1m   5m   15m
//	14.78  347  5.01  12 11 78  13.22 12.80 8.56
//	39.66 1380 53.44  14 11 75  13.22 12.80 8.56
//
// Example output (multiple disks):
//
//	           disk0               disk4       cpu    load average
//	 KB/t  tps  MB/s     KB/t  tps  MB/s  us sy id   1m   5m   15m
//	15.04  181  2.66    73.70    0  0.02   4  4 92  2.80 2.55 2.52
//	19.04 7719 143.51     0.00    0  0.00  41 27 32  2.80 2.55 2.52
//
// We locate the "us" column in the column-header line, then parse the
// last data line at the same index (us) and index+1 (sy).
func parseMacOSIOStatCPU(output string) (float64, bool) {
	s := parseMacOSIOStat(output)
	return s.cpuTotal, s.cpuOK
}

// ioStatSample is everything one `iostat -c 2 -w 1` output carries:
// CPU us/sy/id split+total, disk tps+MB/s, and the load averages.
type ioStatSample struct {
	cpuUsr, cpuSys, cpuIdle, cpuTotal float64
	cpuOK                             bool
	diskTps, diskMBs                  float64
	diskIOOK                          bool
	load1m, load5m, load15m           float64
	loadOK                            bool
}

// parseMacOSIOStat parses iostat -c 2 -w 1 output. Columns are located in
// the header line by name and the last (2nd sample) data line is read.
func parseMacOSIOStat(output string) (s ioStatSample) {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) < 4 {
		return s
	}
	usIdx, syIdx, idIdx := -1, -1, -1
	var tpsIdxs, mbsIdxs []int
	loadIdx, load5Idx, load15Idx := -1, -1, -1
	for i, f := range strings.Fields(lines[1]) {
		switch f {
		case "us":
			usIdx = i
		case "sy":
			syIdx = i
		case "id":
			idIdx = i
		case "tps":
			tpsIdxs = append(tpsIdxs, i)
		case "MB/s":
			mbsIdxs = append(mbsIdxs, i)
		case "1m":
			loadIdx = i
		case "5m":
			load5Idx = i
		case "15m":
			load15Idx = i
		}
	}
	fields := strings.Fields(strings.TrimSpace(lines[len(lines)-1]))
	at := func(idx int) (float64, bool) {
		if idx >= 0 && len(fields) > idx {
			if v, err := strconv.ParseFloat(fields[idx], 64); err == nil {
				return v, true
			}
		}
		return 0, false
	}
	us, okUs := at(usIdx)
	sy, okSy := at(syIdx)
	id, okId := at(idIdx)
	if okUs && okSy {
		s.cpuUsr, s.cpuSys = us, sy
		s.cpuTotal = us + sy
		if s.cpuTotal < 0 {
			s.cpuTotal = 0
		}
		if s.cpuTotal > 100 {
			s.cpuTotal = 100
		}
		s.cpuOK = true
	}
	if okId {
		s.cpuIdle = id
	}
	// One tps/MB/s column group per disk — sum across disks.
	for _, i := range tpsIdxs {
		if v, ok := at(i); ok {
			s.diskTps += v
			s.diskIOOK = true
		}
	}
	for _, i := range mbsIdxs {
		if v, ok := at(i); ok {
			s.diskMBs += v
		}
	}
	if v, ok := at(loadIdx); ok {
		s.load1m = v
		s.loadOK = true
	}
	if v, ok := at(load5Idx); ok {
		s.load5m = v
	}
	if v, ok := at(load15Idx); ok {
		s.load15m = v
	}
	return
}

// parseMacOSMemPressure extracts the free percentage and returns pressure (100 - free).
func parseMacOSMemPressure(output string) (int, bool) {
	matches := memPressurePattern.FindStringSubmatch(output)
	if len(matches) != 2 {
		return 0, false
	}
	free, err := strconv.Atoi(matches[1])
	if err != nil {
		return 0, false
	}
	pressure := 100 - free
	if pressure < 0 {
		pressure = 0
	}
	if pressure > 100 {
		pressure = 100
	}
	return pressure, true
}

func parseMacOSMemoryPressureDetailed(output string) (pressure int, pressureOK bool, freeKB uint64, freeOK bool, compKB uint64, compOK bool) {
	pressure, pressureOK = parseMacOSMemPressure(output)

	pageSize := uint64(16384)
	if m := pageSizePattern.FindStringSubmatch(output); len(m) == 2 {
		if ps, err := strconv.ParseUint(m[1], 10, 64); err == nil && ps > 0 {
			pageSize = ps
		}
	}

	if m := pagesFreePattern.FindStringSubmatch(output); len(m) == 2 {
		if pages, err := strconv.ParseUint(m[1], 10, 64); err == nil {
			freeKB = (pages * pageSize) / 1024
			freeOK = true
		}
	}

	if m := pagesCompressorPattern.FindStringSubmatch(output); len(m) == 2 {
		if pages, err := strconv.ParseUint(m[1], 10, 64); err == nil {
			compKB = (pages * pageSize) / 1024
			compOK = true
		}
	}
	return
}

func parseMacOSThermalLevel(output string) (int, bool) {
	fields := strings.Fields(strings.TrimSpace(output))
	if len(fields) >= 2 && strings.HasPrefix(fields[0], "com.apple.system.thermalpressurelevel") {
		lvl, err := strconv.Atoi(fields[1])
		if err == nil {
			return lvl, true
		}
	}
	return 0, false
}

func parseLinuxLoadAvg(output string) (l1, l5, l15 float64, loadOK bool, threads int, threadsOK bool) {
	fields := strings.Fields(strings.TrimSpace(output))
	for i, dst := range []*float64{&l1, &l5, &l15} {
		if len(fields) <= i {
			break
		}
		if v, err := strconv.ParseFloat(fields[i], 64); err == nil {
			*dst = v
			loadOK = true
		}
	}
	if len(fields) >= 4 {
		slash := strings.Index(fields[3], "/")
		if slash >= 0 {
			t, err := strconv.Atoi(fields[3][slash+1:])
			if err == nil && t > 0 {
				threads = t
				threadsOK = true
			}
		}
	}
	return
}

// parseMacOSThreadAndProcCount parses `ps -M -A` output on macOS.
// Lines starting with a non-space character (USER) indicate a distinct process.
// Total non-header lines indicate total threads.
func parseMacOSThreadAndProcCount(output string) (procs int, procsOK bool, threads int, threadsOK bool) {
	scanner := bufio.NewScanner(strings.NewReader(output))
	first := true
	for scanner.Scan() {
		line := scanner.Text()
		if first {
			first = false
			continue
		}
		if strings.TrimSpace(line) == "" {
			continue
		}
		threads++
		if len(line) > 0 && line[0] != ' ' && line[0] != '\t' {
			procs++
		}
	}
	if procs > 0 {
		procsOK = true
	}
	if threads > 0 {
		threadsOK = true
	}
	return
}

// parseMacOSProcCount extracts the process count from top's "Processes: N total" line.
func parseMacOSProcCount(output string) (int, bool) {
	matches := procCountPattern.FindStringSubmatch(output)
	if len(matches) != 2 {
		return 0, false
	}
	n, err := strconv.Atoi(matches[1])
	if err != nil {
		return 0, false
	}
	return n, true
}

// parseProcCount counts non-header lines from ps aux.
func parseProcCount(output string) int {
	scanner := bufio.NewScanner(strings.NewReader(output))
	count := 0
	first := true
	for scanner.Scan() {
		if first {
			first = false
			continue
		}
		line := strings.TrimSpace(scanner.Text())
		if line != "" {
			count++
		}
	}
	return count
}

// parseMacOSTopProcesses parses `top -l 1 -n 999 -o mem -stats pid,mem,ppid,cpu`
// output. Returns process list with physical footprint (MEM column) in KB
// and %CPU. The MEM column uses suffixes: K (KB), M (MB), G (GB).
func parseMacOSTopProcesses(output string) []procInfo {
	var procs []procInfo
	scanner := bufio.NewScanner(strings.NewReader(output))
	inProcessSection := false
	for scanner.Scan() {
		line := scanner.Text()
		// Each "PID MEM PPID CPU" header starts a new sample frame; keep
		// only the last one (top -l 2's second frame has real CPU%).
		if strings.HasPrefix(strings.TrimSpace(line), "PID") && strings.Contains(line, "MEM") {
			inProcessSection = true
			procs = procs[:0]
			continue
		}
		if !inProcessSection {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		footprintKB, ok := parseMemColumn(fields[1])
		if !ok {
			continue
		}
		ppid, err := strconv.Atoi(fields[2])
		if err != nil {
			continue
		}
		var cpu float64
		if len(fields) > 3 {
			cpu, _ = strconv.ParseFloat(fields[3], 64)
		}
		procs = append(procs, procInfo{
			PID:        pid,
			PPID:       ppid,
			MemoryKB:   footprintKB,
			MemoryKind: MemoryKindFootprint,
			CPU:        cpu,
		})
	}
	return procs
}

// parseMemColumn parses a top MEM column value (e.g. "1144M", "856K", "2G") into KB.
func parseMemColumn(s string) (uint64, bool) {
	if len(s) == 0 {
		return 0, false
	}
	suffix := s[len(s)-1]
	numPart := s
	multiplier := uint64(1)
	switch suffix {
	case 'K', 'k':
		multiplier = 1
		numPart = s[:len(s)-1]
	case 'M', 'm':
		multiplier = 1024
		numPart = s[:len(s)-1]
	case 'G', 'g':
		multiplier = 1024 * 1024
		numPart = s[:len(s)-1]
	}
	val, err := strconv.ParseFloat(numPart, 64)
	if err != nil {
		return 0, false
	}
	return uint64(val * float64(multiplier)), true
}

// parsePsPidPpidMemory parses `ps -eo pid,ppid,rss` output.
// On Linux, RSS is the standard per-process memory metric and is stored
// directly in MemoryKB with MemoryKind=MemoryKindRSS.
func parsePsPidPpidMemory(output string) []procInfo {
	var procs []procInfo
	scanner := bufio.NewScanner(strings.NewReader(output))
	first := true
	for scanner.Scan() {
		if first {
			first = false
			continue
		}
		fields := strings.Fields(scanner.Text())
		if len(fields) < 3 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		ppid, err := strconv.Atoi(fields[1])
		if err != nil {
			continue
		}
		memKB, err := strconv.ParseUint(fields[2], 10, 64)
		if err != nil {
			continue
		}
		procs = append(procs, procInfo{PID: pid, PPID: ppid, MemoryKB: memKB, MemoryKind: MemoryKindRSS})
	}
	return procs
}

// buildChildrenMap builds a parent → children map from process list.
func buildChildrenMap(procs []procInfo) map[int][]int {
	children := make(map[int][]int, len(procs))
	for _, p := range procs {
		children[p.PPID] = append(children[p.PPID], p.PID)
	}
	return children
}

// sumDescendantMemory sums MemoryKB of the given root PID and all its
// descendants. Each process is counted at most once.
func sumDescendantMemory(rootPID int, children map[int][]int, memByPid map[int]uint64) uint64 {
	visited := map[int]bool{rootPID: true}
	var total uint64
	if mem, ok := memByPid[rootPID]; ok {
		total += mem
	}
	stack := []int{rootPID}
	for len(stack) > 0 {
		parent := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		for _, child := range children[parent] {
			if visited[child] {
				continue
			}
			visited[child] = true
			total += memByPid[child]
			stack = append(stack, child)
		}
	}
	return total
}

// parseTmuxPanes parses `tmux list-panes -a -F` output with `|` separator.
func parseTmuxPanes(output string) []tmuxPane {
	var panes []tmuxPane
	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		parts := strings.Split(line, "|")
		if len(parts) < 5 {
			continue
		}
		pid, err := strconv.Atoi(parts[4])
		if err != nil {
			continue
		}
		p := tmuxPane{
			Session:     parts[0],
			WindowIndex: parts[1],
			WindowID:    parts[2],
			PaneID:      parts[3],
			PanePID:     pid,
		}
		if len(parts) > 5 {
			p.Cmd = parts[5]
		}
		if len(parts) > 6 {
			p.Path = parts[6]
		}
		if len(parts) > 7 {
			p.WindowName = parts[7]
		}
		panes = append(panes, p)
	}
	return panes
}

// aggregatePaneMemory walks each pane's process tree and aggregates MemoryKB
// plus %CPU. Each process is counted under exactly one pane (the one whose
// tree contains it).
func aggregatePaneMemory(panes []tmuxPane, children map[int][]int, memByPid map[int]uint64, cpuByPid map[int]float64) paneMemoryResult {
	result := paneMemoryResult{
		PaneMem:    map[string]uint64{},
		WindowMem:  map[string]uint64{},
		SessionMem: map[string]uint64{},
		PaneCPU:    map[string]float64{},
	}
	claimed := map[int]bool{}
	for _, pane := range panes {
		total, cpu := sumDescendantClaimed(pane.PanePID, children, memByPid, cpuByPid, claimed)
		result.PaneMem[pane.PaneID] = total
		result.PaneCPU[pane.PaneID] = cpu
		wkey := pane.Session + ":" + pane.WindowIndex
		result.WindowMem[wkey] += total
		result.SessionMem[pane.Session] += total
		result.TotalMem += total
	}
	return result
}

// sumDescendantClaimed walks a pane's process subtree, aggregating both
// MemoryKB and %CPU while skipping processes already claimed by another pane.
func sumDescendantClaimed(rootPID int, children map[int][]int, memByPid map[int]uint64, cpuByPid map[int]float64, claimed map[int]bool) (mem uint64, cpu float64) {
	if claimed[rootPID] {
		return 0, 0
	}
	visited := map[int]bool{rootPID: true}
	claimed[rootPID] = true
	mem += memByPid[rootPID]
	cpu += cpuByPid[rootPID]
	stack := []int{rootPID}
	for len(stack) > 0 {
		parent := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		for _, child := range children[parent] {
			if visited[child] || claimed[child] {
				continue
			}
			visited[child] = true
			claimed[child] = true
			mem += memByPid[child]
			cpu += cpuByPid[child]
			stack = append(stack, child)
		}
	}
	return mem, cpu
}

// FormatMemoryMB converts KB to a human-readable string (M or G).
func FormatMemoryMB(kb uint64) string {
	return formatMemoryMB(kb)
}

// FormatMemoryCompact formats KB into a compact string with 1 decimal place if in GB (e.g. 1.1G, 3.2G),
// or whole MB if in MB (e.g. 425M).
func FormatMemoryCompact(kb uint64) string {
	mb := float64(kb) / 1024.0
	if mb >= 1024.0 {
		return strconv.FormatFloat(mb/1024.0, 'f', 1, 64) + "G"
	}
	return strconv.FormatFloat(mb, 'f', 0, 64) + "M"
}

// formatMemoryMB converts KB to a human-readable string (M or G).
func formatMemoryMB(kb uint64) string {
	mb := float64(kb) / 1024.0
	if mb >= 1024 {
		return strconv.FormatFloat(mb/1024.0, 'f', 0, 64) + "G"
	}
	return strconv.FormatFloat(mb, 'f', 0, 64) + "M"
}

// formatUsagePercent formats a CPU percentage, rounding to whole unless < 10
// and has a fractional part.
func formatUsagePercent(value float64) string {
	return strconv.FormatFloat(value, 'f', 0, 64) + "%"
}

// CPUBGColor returns the background color for a CPU percentage.
func CPUBGColor(val float64) string {
	switch CPUSeverity(val) {
	case SeverityCrit:
		return "#CC9393"
	case SeverityWarn:
		return "#F0DFAF"
	default:
		return "#7F9F7F"
	}
}

// MemPressureColor returns the background color for a memory pressure percentage.
func MemPressureColor(pressure int) string {
	switch PressureSeverity(pressure) {
	case SeverityCrit:
		return "#CC9393"
	case SeverityWarn:
		return "#F0DFAF"
	default:
		return "#7F9F7F"
	}
}

// FormatUsagePercent formats a CPU percentage string.
func FormatUsagePercent(value float64) string {
	return formatUsagePercent(value)
}

// --- Linux parsers ---

// parseLinuxCPUStat parses /proc/stat first cpu line.
// Returns (total, idle, ok).
func parseLinuxCPUStat(output string) (uint64, uint64, bool) {
	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 5 {
			return 0, 0, false
		}
		var total, idle uint64
		for i, f := range fields[1:] {
			val, err := strconv.ParseUint(f, 10, 64)
			if err != nil {
				return 0, 0, false
			}
			total += val
			if i == 3 || i == 4 { // idle, iowait
				idle += val
			}
		}
		return total, idle, true
	}
	return 0, 0, false
}

// computeLinuxCPUPercent computes CPU usage percentage between two samples.
func computeLinuxCPUPercent(prevTotal, prevIdle, currTotal, currIdle uint64) float64 {
	dt := currTotal - prevTotal
	di := currIdle - prevIdle
	if dt == 0 {
		return 0
	}
	pct := float64(dt-di) / float64(dt) * 100
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	return pct
}

// parseLinuxMemInfo parses /proc/meminfo and returns (totalBytes, availableBytes, ok).
func parseLinuxMemInfo(output string) (uint64, uint64, bool) {
	var total, avail uint64
	var totalOK, availOK bool
	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		switch {
		case strings.HasPrefix(line, "MemTotal:"):
			total = parseMemInfoKB(line)
			totalOK = total > 0
		case strings.HasPrefix(line, "MemAvailable:"):
			avail = parseMemInfoKB(line)
			availOK = true
		}
	}
	if !totalOK || !availOK {
		return 0, 0, false
	}
	return total, avail, true
}

func parseMemInfoKB(line string) uint64 {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return 0
	}
	val, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil {
		return 0
	}
	return val * 1024
}

// linuxMemPressurePercent computes pressure from total and available bytes.
func linuxMemPressurePercent(total, avail uint64) int {
	if total == 0 {
		return 0
	}
	if avail > total {
		avail = total
	}
	used := total - avail
	pct := used * 100 / total
	return int(pct)
}

// --- Disk usage parser ---

// DiskSample holds the raw df output fields in KB.
type DiskSample struct {
	TotalKB     uint64
	UsedKB      uint64 // df "Used" column
	AvailableKB uint64 // df "Available" column
}

// parseDfOutput parses `df -k <path>` output and returns total/used/available.
// Expected format: "Filesystem 1K-blocks Used Available ...\n/dev/... N N N ..."
// Returns ok=false if no valid data line is found.
func parseDfOutput(output string) (DiskSample, bool) {
	scanner := bufio.NewScanner(strings.NewReader(output))
	first := true
	for scanner.Scan() {
		if first {
			first = false
			continue
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		total, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			continue
		}
		used, err := strconv.ParseUint(fields[2], 10, 64)
		if err != nil {
			continue
		}
		avail, err := strconv.ParseUint(fields[3], 10, 64)
		if err != nil {
			continue
		}
		return DiskSample{TotalKB: total, UsedKB: used, AvailableKB: avail}, true
	}
	return DiskSample{}, false
}

// --- Collector ---

// Collector samples host and tmux metrics across three cadence tiers.
// It is safe for concurrent use.
type Collector struct {
	tmuxBin       string
	os            string
	prevLinuxCPU  uint64
	prevLinuxIdle uint64
	mu            sync.Mutex
}

// NewCollector creates a Collector for the given OS ("darwin" or "linux").
func NewCollector(osName, tmuxBin string) *Collector {
	return &Collector{os: osName, tmuxBin: tmuxBin}
}

// SampleFast collects fast-tier metrics: CPU, memory pressure, process count.
// It does NOT scan the full process list.
//
// macOS sources (low CPU overhead):
//   - CPU: iostat -c 2 -w 1 (~1s wall, ~0 CPU; us+sy from 2nd sample)
//   - process count: ps aux (~0.02s)
//   - memory pressure: memory_pressure (~0s)
//
// Linux sources:
//   - CPU: /proc/stat (delta from previous sample)
//   - process count: ps aux
//   - memory pressure: /proc/meminfo (MemTotal - MemAvailable)
func (c *Collector) SampleFast() FastResult {
	var res FastResult
	var wg sync.WaitGroup
	var cpuOut, pressureOut, procOut, thermalOut, loadOut, topOut, swapOut []byte
	var cpuErr, pressureErr, procErr, thermalErr, loadErr, topErr, swapErr error
	var phys physMemSample
	var netRxBytes, netTxBytes uint64
	var netOK bool

	wg.Add(7)
	go func() {
		defer wg.Done()
		cpuOut, cpuErr = c.sampleCPUOutput()
	}()
	go func() {
		defer wg.Done()
		pressureOut, pressureErr = c.sampleMemPressureOutput()
	}()
	go func() {
		defer wg.Done()
		procOut, procErr = c.sampleProcCountOutput()
	}()
	go func() {
		defer wg.Done()
		topOut, topErr = runCommand("ps", "-Ao", "pid=,pcpu=,rss=,comm=")
	}()
	go func() {
		defer wg.Done()
		swapOut, swapErr = c.sampleSwapOutput()
	}()
	go func() {
		defer wg.Done()
		phys = c.samplePhysMem()
	}()
	go func() {
		defer wg.Done()
		netRxBytes, netTxBytes, netOK = c.sampleNetBytes()
	}()

	if c.os == "darwin" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			thermalOut, thermalErr = c.sampleThermalOutput()
		}()
	} else if c.os == "linux" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			loadOut, loadErr = c.sampleLoadOutput()
		}()
	}
	wg.Wait()

	// Parse CPU & Load.
	if cpuErr == nil {
		switch c.os {
		case "darwin":
			s := parseMacOSIOStat(string(cpuOut))
			res.CPU, res.CPUOK = s.cpuTotal, s.cpuOK
			res.CPUUsr, res.CPUSys, res.CPUIdle = s.cpuUsr, s.cpuSys, s.cpuIdle
			res.DiskTps, res.DiskMBs, res.DiskIOOK = s.diskTps, s.diskMBs, s.diskIOOK
			res.LoadAvg1m, res.LoadAvg5m, res.LoadAvg15m, res.LoadAvgOK = s.load1m, s.load5m, s.load15m, s.loadOK
		case "linux":
			res.CPU, res.CPUOK = c.parseLinuxCPU(string(cpuOut))
		}
	}
	if !res.LoadAvgOK && loadErr == nil && c.os == "linux" {
		res.LoadAvg1m, res.LoadAvg5m, res.LoadAvg15m, res.LoadAvgOK, res.ThreadCount, res.ThreadCountOK = parseLinuxLoadAvg(string(loadOut))
	}

	// Parse memory pressure, real free, and compressor.
	if pressureErr == nil {
		switch c.os {
		case "darwin":
			res.Pressure, res.PressureOK, res.RealFreeKB, res.RealFreeOK, res.CompressorKB, res.CompressorOK = parseMacOSMemoryPressureDetailed(string(pressureOut))
		case "linux":
			total, avail, ok := parseLinuxMemInfo(string(pressureOut))
			if ok {
				res.Pressure = linuxMemPressurePercent(total, avail)
				res.PressureOK = true
				res.RealFreeKB = avail
				res.RealFreeOK = true
			}
		}
	}

	// Parse process and thread count.
	if procErr == nil {
		switch c.os {
		case "darwin":
			res.ProcCount, res.ProcCountOK, res.ThreadCount, res.ThreadCountOK = parseMacOSThreadAndProcCount(string(procOut))
			if !res.ProcCountOK {
				res.ProcCount = parseProcCount(string(procOut))
				res.ProcCountOK = true
			}
		default:
			res.ProcCount = parseProcCount(string(procOut))
			res.ProcCountOK = true
		}
	}

	// Parse thermal.
	if thermalErr == nil && c.os == "darwin" {
		res.ThermalLevel, res.ThermalOK = parseMacOSThermalLevel(string(thermalOut))
	}

	// Parse top processes (CPU/MEM) — one ps table, two rankings.
	if topErr == nil {
		procs := parseProcStats(string(topOut))
		res.TopCPU = topProcs(procs, func(p ProcStat) float64 { return p.CPU }, 5)
		res.TopMem = topProcs(procs, func(p ProcStat) float64 { return float64(p.MemKB) }, 5)
	}

	if phys.ok {
		res.PhysMemUsedKB, res.PhysMemTotalKB, res.PhysMemOK = phys.usedKB, phys.totalKB, true
		res.PhysWiredKB, res.PhysAppKB = phys.wiredKB, phys.appKB
	}
	if phys.pageOK {
		res.PageIns, res.PageOuts, res.PageIOOK = phys.pageIns, phys.pageOuts, true
	}
	if netOK {
		res.NetRxBytes, res.NetTxBytes, res.NetOK = netRxBytes, netTxBytes, true
	}

	// Parse swap.
	if swapErr == nil {
		res.SwapTotalKB, res.SwapUsedKB, res.SwapOK = c.parseSwap(string(swapOut))
	}

	return res
}

// sampleSwapOutput returns raw swap usage output.
// macOS: sysctl vm.swapusage. Linux: /proc/meminfo (SwapTotal/SwapFree).
func (c *Collector) sampleSwapOutput() ([]byte, error) {
	switch c.os {
	case "darwin":
		return runCommand("sysctl", "vm.swapusage")
	case "linux":
		return runCommand("cat", "/proc/meminfo")
	}
	return nil, exec.ErrNotFound
}

func (c *Collector) parseSwap(output string) (totalKB, usedKB uint64, ok bool) {
	switch c.os {
	case "darwin":
		return parseMacOSSwapusage(output)
	case "linux":
		return parseLinuxSwap(output)
	}
	return 0, 0, false
}

// parseProcStats parses `ps -Ao pid=,pcpu=,rss=,comm=` output.
// comm may contain spaces; pid/pcpu/rss lead, rest is the command line.
func parseProcStats(output string) []ProcStat {
	scanner := bufio.NewScanner(strings.NewReader(output))
	var procs []ProcStat
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 4 {
			continue
		}
		pid, err1 := strconv.Atoi(fields[0])
		pcpu, err2 := strconv.ParseFloat(fields[1], 64)
		rss, err3 := strconv.Atoi(fields[2])
		if err1 != nil || err2 != nil || err3 != nil || pid <= 0 {
			continue
		}
		name := fields[3]
		if base := path.Base(name); base != "" {
			name = base
		}
		procs = append(procs, ProcStat{
			PID:   pid,
			Name:  name,
			CPU:   pcpu,
			MemKB: uint64(rss),
			Mem:   FormatMemoryCompact(uint64(rss)),
		})
	}
	return procs
}

// topProcs returns the n entries with the highest score.
func topProcs(procs []ProcStat, score func(ProcStat) float64, n int) []ProcStat {
	sorted := make([]ProcStat, len(procs))
	copy(sorted, procs)
	sort.Slice(sorted, func(i, j int) bool {
		a, b := score(sorted[i]), score(sorted[j])
		if a != b {
			return a > b
		}
		return sorted[i].PID < sorted[j].PID
	})
	if len(sorted) > n {
		sorted = sorted[:n]
	}
	return sorted
}

var (
	pagesAnonPattern     = regexp.MustCompile(`Anonymous pages:\s*(\d+)`)
	pagesPurgeablePat    = regexp.MustCompile(`Pages purgeable:\s*(\d+)`)
	pagesWiredPattern    = regexp.MustCompile(`Pages wired down:\s*(\d+)`)
	pagesOccupiedPattern = regexp.MustCompile(`Pages occupied by compressor:\s*(\d+)`)
	memTotalPattern      = regexp.MustCompile(`(?m)^MemTotal:\s*(\d+)`)
	memAvailPattern      = regexp.MustCompile(`(?m)^MemAvailable:\s*(\d+)`)
)

// physMemSample is one vm_stat reading: Activity-Monitor-style "Memory
// Used" plus the wired/app split and cumulative paging counters.
type physMemSample struct {
	usedKB, totalKB, wiredKB, appKB uint64
	ok                              bool
	pageIns, pageOuts               uint64
	pageOK                          bool
}

var pageInsPattern = regexp.MustCompile(`Pageins:\s*(\d+)`)
var pageOutsPattern = regexp.MustCompile(`Pageouts:\s*(\d+)`)
var pgpginPattern = regexp.MustCompile(`pgpgin\s+(\d+)`)
var pgpgoutPattern = regexp.MustCompile(`pgpgout\s+(\d+)`)

// samplePhysMem collects Activity-Monitor-style "Memory Used" plus the
// wired/app breakdown and paging counters.
// macOS: (anonymous - purgeable + wired + compressor-occupied) * page size.
// Linux: MemTotal - MemAvailable; paging from /proc/vmstat.
func (c *Collector) samplePhysMem() (s physMemSample) {
	switch c.os {
	case "darwin":
		out, err := runCommand("vm_stat")
		if err != nil {
			return s
		}
		s.usedKB, s.wiredKB, s.appKB = parseMacOSPhysMemUsed(string(out))
		s.pageIns, s.pageOuts = parseMacOSPageCounters(string(out))
		s.pageOK = s.pageIns > 0 || s.pageOuts > 0
		if tout, err := runCommand("sysctl", "-n", "hw.memsize"); err == nil {
			if b, err := strconv.ParseUint(strings.TrimSpace(string(tout)), 10, 64); err == nil {
				s.totalKB = b / 1024
			}
		}
		s.ok = s.usedKB > 0
		return s
	case "linux":
		data, err := os.ReadFile("/proc/meminfo")
		if err == nil {
			s.usedKB, s.totalKB, s.ok = parseLinuxPhysMem(string(data))
		}
		if vdata, err := os.ReadFile("/proc/vmstat"); err == nil {
			s.pageIns, s.pageOuts = parseLinuxPageCounters(string(vdata))
			s.pageOK = s.pageIns > 0 || s.pageOuts > 0
		}
		return s
	}
	return s
}

func parseMacOSPageCounters(output string) (ins, outs uint64) {
	if m := pageInsPattern.FindStringSubmatch(output); len(m) == 2 {
		ins, _ = strconv.ParseUint(m[1], 10, 64)
	}
	if m := pageOutsPattern.FindStringSubmatch(output); len(m) == 2 {
		outs, _ = strconv.ParseUint(m[1], 10, 64)
	}
	return
}

func parseLinuxPageCounters(output string) (ins, outs uint64) {
	if m := pgpginPattern.FindStringSubmatch(output); len(m) == 2 {
		ins, _ = strconv.ParseUint(m[1], 10, 64)
	}
	if m := pgpgoutPattern.FindStringSubmatch(output); len(m) == 2 {
		outs, _ = strconv.ParseUint(m[1], 10, 64)
	}
	return
}

func parseMacOSPhysMemUsed(output string) (usedKB, wiredKB, appKB uint64) {
	pageSize := uint64(16384)
	if m := pageSizePattern.FindStringSubmatch(output); len(m) == 2 {
		if ps, err := strconv.ParseUint(m[1], 10, 64); err == nil && ps > 0 {
			pageSize = ps
		}
	}
	pages := func(re *regexp.Regexp) uint64 {
		m := re.FindStringSubmatch(output)
		if len(m) != 2 {
			return 0
		}
		v, _ := strconv.ParseUint(m[1], 10, 64)
		return v
	}
	anon := pages(pagesAnonPattern)
	purgeable := pages(pagesPurgeablePat)
	if purgeable > anon {
		purgeable = anon
	}
	app := anon - purgeable
	wired := pages(pagesWiredPattern)
	used := app + wired + pages(pagesOccupiedPattern)
	return used * pageSize / 1024, wired * pageSize / 1024, app * pageSize / 1024
}

func parseLinuxPhysMem(output string) (usedKB, totalKB uint64, ok bool) {
	tm := memTotalPattern.FindStringSubmatch(output)
	am := memAvailPattern.FindStringSubmatch(output)
	if len(tm) != 2 || len(am) != 2 {
		return 0, 0, false
	}
	t, e1 := strconv.ParseUint(tm[1], 10, 64)
	a, e2 := strconv.ParseUint(am[1], 10, 64)
	if e1 != nil || e2 != nil || a > t {
		return 0, 0, false
	}
	return t - a, t, true
}

// sampleNetBytes returns cumulative RX/TX byte counters across non-loopback
// interfaces; the daemon diffs successive samples into KB/s rates.
func (c *Collector) sampleNetBytes() (rxBytes, txBytes uint64, ok bool) {
	switch c.os {
	case "darwin":
		out, err := runCommand("netstat", "-ibn")
		if err != nil {
			return 0, 0, false
		}
		return parseNetstatIB(string(out))
	case "linux":
		data, err := os.ReadFile("/proc/net/dev")
		if err != nil {
			return 0, 0, false
		}
		return parseLinuxNetDev(string(data))
	}
	return 0, 0, false
}

// parseNetstatIB sums Ibytes/Obytes over the first (<Link#>) row of each
// interface in `netstat -ibn`, skipping loopback and downed (*) interfaces.
func parseNetstatIB(output string) (rxBytes, txBytes uint64, ok bool) {
	seen := make(map[string]bool)
	for _, line := range strings.Split(output, "\n") {
		f := strings.Fields(line)
		if len(f) < 10 || f[0] == "Name" || strings.HasSuffix(f[0], "*") || seen[f[0]] {
			continue
		}
		seen[f[0]] = true
		if f[0] == "lo0" {
			continue
		}
		rx, e1 := strconv.ParseUint(f[6], 10, 64)
		tx, e2 := strconv.ParseUint(f[9], 10, 64)
		if e1 == nil && e2 == nil {
			rxBytes += rx
			txBytes += tx
			ok = true
		}
	}
	return
}

// parseLinuxNetDev sums RX/TX bytes over non-lo lines of /proc/net/dev.
func parseLinuxNetDev(output string) (rxBytes, txBytes uint64, ok bool) {
	for _, line := range strings.Split(output, "\n") {
		i := strings.IndexByte(line, ':')
		if i < 0 {
			continue
		}
		if name := strings.TrimSpace(line[:i]); name == "" || name == "lo" {
			continue
		}
		f := strings.Fields(line[i+1:])
		if len(f) < 9 {
			continue
		}
		rx, e1 := strconv.ParseUint(f[0], 10, 64)
		tx, e2 := strconv.ParseUint(f[8], 10, 64)
		if e1 == nil && e2 == nil {
			rxBytes += rx
			txBytes += tx
			ok = true
		}
	}
	return
}

var swapLinePattern = regexp.MustCompile(`(total|used)\s*=\s*([0-9.]+)\s*([MG])`)

// parseMacOSSwapusage parses `sysctl vm.swapusage`:
// "total = 2048.00M  used = 512.00M  free = 1536.00M  (encrypted)"
func parseMacOSSwapusage(output string) (totalKB, usedKB uint64, ok bool) {
	for _, m := range swapLinePattern.FindAllStringSubmatch(output, -1) {
		v, err := strconv.ParseFloat(m[2], 64)
		if err != nil {
			continue
		}
		kb := uint64(v * 1024)
		if m[3] == "G" {
			kb = uint64(v * 1024 * 1024)
		}
		switch m[1] {
		case "total":
			totalKB = kb
		case "used":
			usedKB = kb
		}
	}
	return totalKB, usedKB, totalKB > 0
}

var swapTotalPattern = regexp.MustCompile(`(?m)^SwapTotal:\s*(\d+)`)
var swapFreePattern = regexp.MustCompile(`(?m)^SwapFree:\s*(\d+)`)

func parseLinuxSwap(output string) (totalKB, usedKB uint64, ok bool) {
	m := swapTotalPattern.FindStringSubmatch(output)
	if len(m) != 2 {
		return 0, 0, false
	}
	t, err := strconv.ParseUint(m[1], 10, 64)
	if err != nil {
		return 0, 0, false
	}
	totalKB = t
	if f := swapFreePattern.FindStringSubmatch(output); len(f) == 2 {
		if fv, err := strconv.ParseUint(f[1], 10, 64); err == nil && fv <= t {
			usedKB = t - fv
		}
	}
	return totalKB, usedKB, true
}

func (c *Collector) sampleThermalOutput() ([]byte, error) {
	switch c.os {
	case "darwin":
		return runCommand("notifyutil", "-g", "com.apple.system.thermalpressurelevel")
	case "linux":
		return runCommand("cat", "/sys/class/thermal/thermal_zone0/temp")
	}
	return nil, exec.ErrNotFound
}

func (c *Collector) sampleLoadOutput() ([]byte, error) {
	switch c.os {
	case "linux":
		return runCommand("cat", "/proc/loadavg")
	}
	return nil, exec.ErrNotFound
}

// sampleCPUOutput returns raw CPU output for parsing.
// macOS: iostat -c 2 -w 1. Linux: /proc/stat.
func (c *Collector) sampleCPUOutput() ([]byte, error) {
	switch c.os {
	case "darwin":
		return runCommand("iostat", "-c", "2", "-w", "1")
	case "linux":
		return runCommand("cat", "/proc/stat")
	}
	return nil, exec.ErrNotFound
}

// sampleProcCountOutput returns raw process count output.
// macOS: ps -M -A (includes threads); Linux: ps aux.
func (c *Collector) sampleProcCountOutput() ([]byte, error) {
	if c.os == "darwin" {
		if out, err := runCommand("ps", "-M", "-A"); err == nil {
			return out, nil
		}
	}
	return runCommand("ps", "aux")
}

// sampleMemPressureOutput returns raw pressure output.
// macOS: memory_pressure. Linux: /proc/meminfo.
func (c *Collector) sampleMemPressureOutput() ([]byte, error) {
	switch c.os {
	case "darwin":
		return runCommand("memory_pressure")
	case "linux":
		return runCommand("cat", "/proc/meminfo")
	}
	return nil, exec.ErrNotFound
}

// parseLinuxCPU parses /proc/stat and computes CPU percentage using delta.
func (c *Collector) parseLinuxCPU(output string) (float64, bool) {
	total, idle, ok := parseLinuxCPUStat(output)
	if !ok {
		return 0, false
	}
	c.mu.Lock()
	prevTotal, prevIdle := c.prevLinuxCPU, c.prevLinuxIdle
	c.prevLinuxCPU = total
	c.prevLinuxIdle = idle
	c.mu.Unlock()
	if prevTotal > 0 {
		return computeLinuxCPUPercent(prevTotal, prevIdle, total, idle), true
	}
	return 0, false
}

// SampleFootprint collects per-pane/window/session/total tmux memory.
// macOS: top -l 1 -n 999 -o mem -stats pid,mem,ppid (physical footprint).
// Linux: ps -eo pid,ppid,rss (RSS).
// Returns nil result and false on failure. The result.Kind field indicates
// what the memory values represent (footprint on macOS, RSS on Linux).
func (c *Collector) SampleFootprint() (paneMemoryResult, bool) {
	panesOut, err := runCommand(c.tmuxBin, "list-panes", "-a", "-F",
		"#{session_name}|#{window_index}|#{window_id}|#{pane_id}|#{pane_pid}|#{pane_current_command}|#{pane_current_path}|#{window_name}")
	if err != nil {
		return paneMemoryResult{}, false
	}
	panes := parseTmuxPanes(string(panesOut))
	if len(panes) == 0 {
		return paneMemoryResult{}, true // no panes, but not an error
	}

	var procs []procInfo
	var kind MemoryKind
	switch c.os {
	case "darwin":
		// -l 2: the first top frame reports %CPU 0.0; the second frame has
		// the interval CPU%. The parser keeps the last table.
		topOut, err := runCommand("top", "-l", "2", "-n", "999", "-o", "mem", "-stats", "pid,mem,ppid,cpu")
		if err != nil {
			return paneMemoryResult{}, false
		}
		procs = parseMacOSTopProcesses(string(topOut))
		kind = MemoryKindFootprint
	case "linux":
		psOut, err := runCommand("ps", "-eo", "pid,ppid,rss")
		if err != nil {
			return paneMemoryResult{}, false
		}
		procs = parsePsPidPpidMemory(string(psOut))
		kind = MemoryKindRSS
	default:
		return paneMemoryResult{}, false
	}
	if len(procs) == 0 {
		return paneMemoryResult{}, false
	}

	children := buildChildrenMap(procs)
	memByPid := make(map[int]uint64, len(procs))
	cpuByPid := make(map[int]float64, len(procs))
	for _, p := range procs {
		memByPid[p.PID] = p.MemoryKB
		cpuByPid[p.PID] = p.CPU
	}
	result := aggregatePaneMemory(panes, children, memByPid, cpuByPid)
	result.Kind = kind
	result.PaneMeta = make(map[string]PaneMeta, len(panes))
	result.WindowNames = make(map[string]string, len(panes))
	for _, p := range panes {
		result.PaneMeta[p.PaneID] = PaneMeta{
			Session:     p.Session,
			WindowIndex: p.WindowIndex,
			WindowName:  p.WindowName,
			Cmd:         p.Cmd,
			Path:        p.Path,
		}
		wkey := p.Session + ":" + p.WindowIndex
		if p.WindowName != "" {
			result.WindowNames[wkey] = p.WindowName
		}
	}
	return result, true
}

// diskSamplePath returns the df argument for disk usage sampling.
// On macOS, the root mount point is a read-only System volume; the real
// user data lives on /System/Volumes/Data (the APFS/Data container).
// If that path exists, use it; otherwise fall back to / (Linux or older macOS).
func diskSamplePath(goos string, exists func(string) bool) string {
	if goos == "darwin" && exists("/System/Volumes/Data") {
		return "/System/Volumes/Data"
	}
	return "/"
}

// pathExists is the default existence check for diskSamplePath.
func pathExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// computeDiskUsed derives the "used" KB from a raw df sample.
// On macOS (darwin), the APFS/Data container's df "Used" column reports
// only snapshot/clone space — not actual consumed space. The real consumed
// space is total - available. Available may exceed total on sparse
// filesystems; it is clamped to total to prevent underflow.
// On Linux, df "Used" is the standard metric and is returned directly.
func computeDiskUsed(goos string, s DiskSample) uint64 {
	switch goos {
	case "darwin":
		avail := s.AvailableKB
		if avail > s.TotalKB {
			avail = s.TotalKB
		}
		return s.TotalKB - avail
	default:
		return s.UsedKB
	}
}

// SampleDisk collects root disk usage: used, total, and available (all in KB).
// On macOS, the APFS/Data container reports Used (df column 3) which is
// the space used by snapshots and clones — not the actual consumed space.
// The real consumed space is total - available. We return that as Used.
// On Linux, df Used is the standard metric and is returned directly.
// Available is always the raw df "Available" column, used by render for
// low-disk color judgment (red when < 5 GiB).
func (c *Collector) SampleDisk() (used, total, available uint64, ok bool) {
	path := diskSamplePath(c.os, pathExists)
	out, err := runCommand("df", "-k", path)
	if err != nil {
		return 0, 0, 0, false
	}
	sample, ok := parseDfOutput(string(out))
	if !ok {
		return 0, 0, 0, false
	}
	used = computeDiskUsed(c.os, sample)
	return used, sample.TotalKB, sample.AvailableKB, true
}

// SystemInfo holds slow-tier host facts: boot time and battery state.
type SystemInfo struct {
	BootTimeSec  int64
	BatteryPct   int
	BatteryState string // discharging|charging|charged|ac|""
	BatteryOK    bool
}

var bootTimePattern = regexp.MustCompile(`sec\s*=\s*(\d+)`)
var battPctPattern = regexp.MustCompile(`(\d+)%`)
var battStatePattern = regexp.MustCompile(`(discharging|charging|charged|finishing charge|not charging)`)

// SampleSystemInfo collects boot time and battery state (slow tier).
// Each sub-sample degrades independently.
func (c *Collector) SampleSystemInfo() SystemInfo {
	var info SystemInfo

	switch c.os {
	case "darwin":
		if out, err := runCommand("sysctl", "kern.boottime"); err == nil {
			if m := bootTimePattern.FindStringSubmatch(string(out)); len(m) == 2 {
				info.BootTimeSec, _ = strconv.ParseInt(m[1], 10, 64)
			}
		}
	case "linux":
		if out, err := runCommand("cat", "/proc/uptime"); err == nil {
			fields := strings.Fields(strings.TrimSpace(string(out)))
			if len(fields) > 0 {
				if up, err := strconv.ParseFloat(fields[0], 64); err == nil && up > 0 {
					info.BootTimeSec = time.Now().Unix() - int64(up)
				}
			}
		}
	}

	switch c.os {
	case "darwin":
		if out, err := runCommand("pmset", "-g", "batt"); err == nil {
			s := string(out)
			if m := battPctPattern.FindStringSubmatch(s); len(m) == 2 {
				info.BatteryPct, _ = strconv.Atoi(m[1])
				info.BatteryOK = true
			}
			if m := battStatePattern.FindStringSubmatch(s); len(m) == 2 {
				info.BatteryState = m[1]
			}
			if strings.Contains(s, "AC Power") && info.BatteryState == "" {
				info.BatteryState = "ac"
			}
		}
	case "linux":
		if matches, _ := filepath.Glob("/sys/class/power_supply/BAT*/capacity"); len(matches) > 0 {
			if data, err := os.ReadFile(matches[0]); err == nil {
				if pct, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
					info.BatteryPct = pct
					info.BatteryOK = true
				}
			}
			if data, err := os.ReadFile(strings.TrimSuffix(matches[0], "capacity") + "status"); err == nil {
				info.BatteryState = strings.ToLower(strings.TrimSpace(string(data)))
			}
		}
	}

	return info
}
