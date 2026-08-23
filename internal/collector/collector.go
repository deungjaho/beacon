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
	"regexp"
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
}

// tmuxPane is a single pane entry from tmux list-panes.
type tmuxPane struct {
	Session     string
	WindowIndex string
	WindowID    string
	PaneID      string
	PanePID     int
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
}

// paneMemoryResult holds the aggregated memory maps (in KB).
// All values use the same MemoryKind as the source process entries.
type paneMemoryResult struct {
	PaneMem    map[string]uint64
	WindowMem  map[string]uint64
	SessionMem map[string]uint64
	TotalMem   uint64
	Kind       MemoryKind // what the aggregated values represent
}

var cpuUsagePattern = regexp.MustCompile(`CPU usage:\s*([0-9.]+)% user,\s*([0-9.]+)% sys,`)
var memPressurePattern = regexp.MustCompile(`System-wide memory free percentage:\s*(\d+)%`)
var procCountPattern = regexp.MustCompile(`Processes:\s*(\d+)\s+total,`)

// commandTimeout is the max wait for any subprocess.
// iostat -c 2 -w 1 takes ~1s; allow headroom.
const commandTimeout = 5 * time.Second

// runCommand runs a command with a bounded timeout and returns its stdout.
// Uses context cancellation so the kill is race-free with Cmd.Start.
func runCommand(name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
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
	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) < 4 {
		return 0, false
	}
	// Line 1 is the column header: "KB/t tps MB/s ... us sy id 1m 5m 15m".
	headerFields := strings.Fields(lines[1])
	usIdx := -1
	for i, f := range headerFields {
		if f == "us" {
			usIdx = i
			break
		}
	}
	if usIdx < 0 {
		return 0, false
	}
	// The last line is the second sample.
	lastLine := strings.TrimSpace(lines[len(lines)-1])
	fields := strings.Fields(lastLine)
	if len(fields) < usIdx+2 {
		return 0, false
	}
	us, err := strconv.ParseFloat(fields[usIdx], 64)
	if err != nil {
		return 0, false
	}
	sy, err := strconv.ParseFloat(fields[usIdx+1], 64)
	if err != nil {
		return 0, false
	}
	total := us + sy
	if total < 0 {
		total = 0
	}
	if total > 100 {
		total = 100
	}
	return total, true
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

// parseMacOSTopProcesses parses `top -l 1 -n 999 -o mem -stats pid,mem,ppid` output.
// Returns process list with physical footprint (MEM column) in KB.
// The MEM column uses suffixes: K (KB), M (MB), G (GB).
func parseMacOSTopProcesses(output string) []procInfo {
	var procs []procInfo
	scanner := bufio.NewScanner(strings.NewReader(output))
	inProcessSection := false
	for scanner.Scan() {
		line := scanner.Text()
		// Detect the process table header: "PID    MEM   PPID"
		if !inProcessSection {
			if strings.HasPrefix(strings.TrimSpace(line), "PID") && strings.Contains(line, "MEM") {
				inProcessSection = true
			}
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
		procs = append(procs, procInfo{
			PID:        pid,
			PPID:       ppid,
			MemoryKB:   footprintKB,
			MemoryKind: MemoryKindFootprint,
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
		panes = append(panes, tmuxPane{
			Session:     parts[0],
			WindowIndex: parts[1],
			WindowID:    parts[2],
			PaneID:      parts[3],
			PanePID:     pid,
		})
	}
	return panes
}

// aggregatePaneMemory walks each pane's process tree and aggregates MemoryKB.
// Each process is counted under exactly one pane (the one whose tree contains it).
func aggregatePaneMemory(panes []tmuxPane, children map[int][]int, memByPid map[int]uint64) paneMemoryResult {
	result := paneMemoryResult{
		PaneMem:    map[string]uint64{},
		WindowMem:  map[string]uint64{},
		SessionMem: map[string]uint64{},
	}
	claimed := map[int]bool{}
	for _, pane := range panes {
		total := sumDescendantMemoryClaimed(pane.PanePID, children, memByPid, claimed)
		result.PaneMem[pane.PaneID] = total
		wkey := pane.Session + ":" + pane.WindowIndex
		result.WindowMem[wkey] += total
		result.SessionMem[pane.Session] += total
		result.TotalMem += total
	}
	return result
}

// sumDescendantMemoryClaimed is like sumDescendantMemory but skips processes
// already claimed by another pane.
func sumDescendantMemoryClaimed(rootPID int, children map[int][]int, memByPid map[int]uint64, claimed map[int]bool) uint64 {
	if claimed[rootPID] {
		return 0
	}
	visited := map[int]bool{rootPID: true}
	claimed[rootPID] = true
	var total uint64
	if mem, ok := memByPid[rootPID]; ok {
		total += mem
	}
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
			total += memByPid[child]
			stack = append(stack, child)
		}
	}
	return total
}

// FormatMemoryMB converts KB to a human-readable string (M or G).
func FormatMemoryMB(kb uint64) string {
	return formatMemoryMB(kb)
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
	if val >= 60 {
		return "#CC9393"
	}
	if val >= 30 {
		return "#F0DFAF"
	}
	return "#7F9F7F"
}

// MemPressureColor returns the background color for a memory pressure percentage.
func MemPressureColor(pressure int) string {
	if pressure >= 60 {
		return "#CC9393"
	}
	if pressure >= 30 {
		return "#F0DFAF"
	}
	return "#7F9F7F"
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
func (c *Collector) SampleFast() (cpu float64, cpuOK bool, pressure int, pressureOK bool, procCount int, procCountOK bool) {
	var wg sync.WaitGroup
	var cpuOut, pressureOut, procOut []byte
	var cpuErr, pressureErr, procErr error

	wg.Add(3)
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
	wg.Wait()

	// Parse CPU.
	if cpuErr == nil {
		switch c.os {
		case "darwin":
			cpu, cpuOK = parseMacOSIOStatCPU(string(cpuOut))
		case "linux":
			cpu, cpuOK = c.parseLinuxCPU(string(cpuOut))
		}
	}

	// Parse memory pressure.
	if pressureErr == nil {
		switch c.os {
		case "darwin":
			pressure, pressureOK = parseMacOSMemPressure(string(pressureOut))
		case "linux":
			total, avail, ok := parseLinuxMemInfo(string(pressureOut))
			if ok {
				pressure = linuxMemPressurePercent(total, avail)
				pressureOK = true
			}
		}
	}

	// Parse process count.
	if procErr == nil {
		procCount = parseProcCount(string(procOut))
		procCountOK = true
	}

	return
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
// Both macOS and Linux: ps aux.
func (c *Collector) sampleProcCountOutput() ([]byte, error) {
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
		"#{session_name}|#{window_index}|#{window_id}|#{pane_id}|#{pane_pid}")
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
		topOut, err := runCommand("top", "-l", "1", "-n", "999", "-o", "mem", "-stats", "pid,mem,ppid")
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
	for _, p := range procs {
		memByPid[p.PID] = p.MemoryKB
	}
	result := aggregatePaneMemory(panes, children, memByPid)
	result.Kind = kind
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
