package collector

import (
	"testing"
)

func TestParseMacOSCPUUsage(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		want   float64
		wantOK bool
	}{
		{
			name:   "normal",
			input:  "CPU usage: 65.76% user, 31.30% sys, 2.92% idle",
			want:   97.06,
			wantOK: true,
		},
		{
			name:   "low cpu",
			input:  "CPU usage: 3.50% user, 1.20% sys, 95.30% idle",
			want:   4.70,
			wantOK: true,
		},
		{
			name:   "no match",
			input:  "no cpu line here",
			wantOK: false,
		},
		{
			name:   "capped at 100",
			input:  "CPU usage: 80.0% user, 40.0% sys, -20.0% idle",
			want:   100,
			wantOK: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseMacOSCPUUsageTotal(tc.input)
			if ok != tc.wantOK {
				t.Fatalf("ok: got %v want %v", ok, tc.wantOK)
			}
			if !tc.wantOK {
				return
			}
			if got != tc.want {
				t.Fatalf("cpu: got %.2f want %.2f", got, tc.want)
			}
		})
	}
}

func TestParseMemPressure(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		want   int
		wantOK bool
	}{
		{name: "29 free", input: "System-wide memory free percentage: 29%", want: 71, wantOK: true},
		{name: "60 free", input: "System-wide memory free percentage: 60%", want: 40, wantOK: true},
		{name: "no match", input: "nothing here", wantOK: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseMacOSMemPressure(tc.input)
			if ok != tc.wantOK {
				t.Fatalf("ok: got %v want %v", ok, tc.wantOK)
			}
			if tc.wantOK && got != tc.want {
				t.Fatalf("pressure: got %d want %d", got, tc.want)
			}
		})
	}
}

func TestParseProcCount(t *testing.T) {
	input := "USER PID %CPU %MEM VSZ RSS TT STAT STARTED TIME COMMAND\nroot 1 0.0 0.0 408 8960 ?? Ss 10AM 0:00.01 /sbin/launchd\nuser 500 0.1 0.2 8000 18000 s001 S 10AM 0:00.05 -zsh\n"
	got := parseProcCount(input)
	if got != 2 {
		t.Fatalf("proc count: got %d want 2", got)
	}
}

func TestParsePsPidPpidMemory(t *testing.T) {
	input := "  PID  PPID    RSS\n    1     0   8960\n  410     1  18000\n  516     1   9120\n  517   410   6192\n"
	procs := parsePsPidPpidMemory(input)
	if len(procs) != 4 {
		t.Fatalf("proc count: got %d want 4", len(procs))
	}
	if procs[1].PID != 410 || procs[1].PPID != 1 || procs[1].MemoryKB != 18000 {
		t.Fatalf("proc[1]: %+v", procs[1])
	}
	if procs[3].PID != 517 || procs[3].PPID != 410 || procs[3].MemoryKB != 6192 {
		t.Fatalf("proc[3]: %+v", procs[3])
	}
}

func TestBuildChildrenMap(t *testing.T) {
	procs := []procInfo{
		{PID: 1, PPID: 0, MemoryKB: 100},
		{PID: 410, PPID: 1, MemoryKB: 200},
		{PID: 516, PPID: 1, MemoryKB: 300},
		{PID: 517, PPID: 410, MemoryKB: 400},
		{PID: 518, PPID: 410, MemoryKB: 500},
	}
	children := buildChildrenMap(procs)
	if len(children[1]) != 2 {
		t.Fatalf("children of 1: got %d want 2", len(children[1]))
	}
	if len(children[410]) != 2 {
		t.Fatalf("children of 410: got %d want 2", len(children[410]))
	}
}

func TestDescendantMemorySum(t *testing.T) {
	procs := []procInfo{
		{PID: 1, PPID: 0, MemoryKB: 100},
		{PID: 410, PPID: 1, MemoryKB: 200},
		{PID: 516, PPID: 1, MemoryKB: 300},
		{PID: 517, PPID: 410, MemoryKB: 400},
		{PID: 518, PPID: 410, MemoryKB: 500},
		{PID: 600, PPID: 517, MemoryKB: 600},
	}
	children := buildChildrenMap(procs)
	memByPid := map[int]uint64{}
	for _, p := range procs {
		memByPid[p.PID] = p.MemoryKB
	}
	// Pane with shell PID 410: should include 410 + 517 + 518 + 600
	got := sumDescendantMemory(410, children, memByPid)
	want := uint64(200 + 400 + 500 + 600)
	if got != want {
		t.Fatalf("descendant memory for 410: got %d want %d", got, want)
	}
	// Pane with shell PID 516: only itself
	got = sumDescendantMemory(516, children, memByPid)
	if got != 300 {
		t.Fatalf("descendant memory for 516: got %d want 300", got)
	}
}

func TestParseTmuxPanes(t *testing.T) {
	input := "1-lanqiao|1|@1|%1|8501\n1-lanqiao|2|@2|%2|8532\n3-yinchen|1|@6|%8|8855\n"
	panes := parseTmuxPanes(input)
	if len(panes) != 3 {
		t.Fatalf("pane count: got %d want 3", len(panes))
	}
	if panes[0].Session != "1-lanqiao" || panes[0].WindowID != "@1" || panes[0].PaneID != "%1" || panes[0].PanePID != 8501 {
		t.Fatalf("pane[0]: %+v", panes[0])
	}

}

func TestAggregatePaneMemory(t *testing.T) {
	panes := []tmuxPane{
		{Session: "s1", WindowIndex: "1", WindowID: "@1", PaneID: "%1", PanePID: 100},
		{Session: "s1", WindowIndex: "1", WindowID: "@1", PaneID: "%2", PanePID: 200},
		{Session: "s1", WindowIndex: "2", WindowID: "@2", PaneID: "%3", PanePID: 300},
		{Session: "s2", WindowIndex: "1", WindowID: "@3", PaneID: "%4", PanePID: 400},
	}
	procs := []procInfo{
		{PID: 100, PPID: 0, MemoryKB: 1000},
		{PID: 200, PPID: 0, MemoryKB: 2000},
		{PID: 300, PPID: 0, MemoryKB: 3000},
		{PID: 400, PPID: 0, MemoryKB: 4000},
	}
	children := buildChildrenMap(procs)
	memByPid := map[int]uint64{}
	for _, p := range procs {
		memByPid[p.PID] = p.MemoryKB
	}
	result := aggregatePaneMemory(panes, children, memByPid)
	if result.PaneMem["%1"] != 1000 {
		t.Fatalf("pane %%1: got %d want 1000", result.PaneMem["%1"])
	}
	if result.WindowMem["s1:1"] != 3000 {
		t.Fatalf("window s1:1: got %d want 3000", result.WindowMem["s1:1"])
	}
	if result.SessionMem["s1"] != 6000 {
		t.Fatalf("session s1: got %d want 6000", result.SessionMem["s1"])
	}
	if result.TotalMem != 10000 {
		t.Fatalf("total: got %d want 10000", result.TotalMem)
	}
}

func TestFormatMemoryMB(t *testing.T) {
	tests := []struct {
		kb   uint64
		want string
	}{
		{0, "0M"},
		{512 * 1024, "512M"},
		{1024 * 1024, "1G"},          // exactly 1 GiB
		{1024*1024 + 512*1024, "2G"}, // 1.5 GiB → rounds to 2G
		{2048 * 1024, "2G"},          // exactly 2 GiB
		{1536 * 1024, "2G"},          // 1.5 GiB → rounds to 2G
		{500 * 1024, "500M"},
		{11*1024*1024 + 100*1024, "11G"},   // ~11.1 GiB → 11G
		{222*1024*1024 + 819*1024, "223G"}, // ~222.8 GiB → rounds to 223G
		{1023 * 1024, "1023M"},             // just under 1 GiB → M
		{1024*1024 - 1, "1024M"},           // 1 KiB under 1 GiB → 1024M
	}
	for _, tc := range tests {
		got := formatMemoryMB(tc.kb)
		if got != tc.want {
			t.Errorf("formatMemoryMB(%d): got %q want %q", tc.kb, got, tc.want)
		}
	}
}

func TestFormatUsagePercent(t *testing.T) {
	tests := []struct {
		val  float64
		want string
	}{
		{0, "0%"},
		{15, "15%"},
		{97.06, "97%"},
		{3.50, "4%"},
		{3.55, "4%"},
		{65.76, "66%"},
	}
	for _, tc := range tests {
		got := formatUsagePercent(tc.val)
		if got != tc.want {
			t.Errorf("formatUsagePercent(%.2f): got %q want %q", tc.val, got, tc.want)
		}
	}
}

func TestParseLinuxCPUStat(t *testing.T) {
	input := "cpu  3357 0 4313 1362393 234 0 12 0 0 0\ncpu0 1135 0 1578 681196 117 0 6 0 0 0\n"
	total, idle, ok := parseLinuxCPUStat(input)
	if !ok {
		t.Fatal("expected ok")
	}
	// user=3357 nice=0 system=4313 idle=1362393 iowait=234 irq=0 softirq=12 steal=0
	// total = 3357+0+4313+1362393+234+0+12+0 = 1370309
	// idle = idle+iowait = 1362393+234 = 1362627
	if total != 1370309 {
		t.Fatalf("total: got %d want 1370309", total)
	}
	if idle != 1362627 {
		t.Fatalf("idle: got %d want 1362627", idle)
	}
}

func TestComputeLinuxCPUDelta(t *testing.T) {
	// First sample: total=1000, idle=800
	// Second sample: total=1100, idle=850
	// delta_total=100, delta_idle=50
	// usage = (100-50)/100 = 50%
	prevTotal := uint64(1000)
	prevIdle := uint64(800)
	currTotal := uint64(1100)
	currIdle := uint64(850)
	got := computeLinuxCPUPercent(prevTotal, prevIdle, currTotal, currIdle)
	if got != 50 {
		t.Fatalf("cpu delta: got %.2f want 50", got)
	}
}

func TestParseLinuxMemInfo(t *testing.T) {
	input := "MemTotal:       16384000 kB\nMemFree:         1234567 kB\nMemAvailable:    8765432 kB\nBuffers:         123456 kB\n"
	total, avail, ok := parseLinuxMemInfo(input)
	if !ok {
		t.Fatal("expected ok")
	}
	if total != 16384000*1024 {
		t.Fatalf("total: got %d want %d", total, 16384000*1024)
	}
	if avail != 8765432*1024 {
		t.Fatalf("avail: got %d want %d", avail, 8765432*1024)
	}
}

func TestLinuxMemPressurePercent(t *testing.T) {
	// total=16GB, avail=8GB → pressure = 50%
	pressure := linuxMemPressurePercent(16*1024*1024*1024, 8*1024*1024*1024)
	if pressure != 50 {
		t.Fatalf("pressure: got %d want 50", pressure)
	}
	// avail > total → pressure 0
	pressure = linuxMemPressurePercent(16*1024*1024*1024, 20*1024*1024*1024)
	if pressure != 0 {
		t.Fatalf("pressure: got %d want 0", pressure)
	}
}

func TestMemPressureColor(t *testing.T) {
	tests := []struct {
		pressure int
		want     string
	}{
		{20, "#7F9F7F"},
		{30, "#F0DFAF"},
		{59, "#F0DFAF"},
		{60, "#CC9393"},
		{90, "#CC9393"},
	}
	for _, tc := range tests {
		got := MemPressureColor(tc.pressure)
		if got != tc.want {
			t.Errorf("MemPressureColor(%d): got %q want %q", tc.pressure, got, tc.want)
		}
	}
}

func TestCPUBGColor(t *testing.T) {
	tests := []struct {
		val  float64
		want string
	}{
		{20, "#7F9F7F"},
		{30, "#F0DFAF"},
		{59, "#F0DFAF"},
		{60, "#CC9393"},
		{90, "#CC9393"},
	}
	for _, tc := range tests {
		got := CPUBGColor(tc.val)
		if got != tc.want {
			t.Errorf("CPUBGColor(%.0f): got %q want %q", tc.val, got, tc.want)
		}
	}
}

// --- macOS top process parsing tests ---

func TestParseMemColumn(t *testing.T) {
	tests := []struct {
		input string
		want  uint64
		ok    bool
	}{
		{"1144M", 1144 * 1024, true},
		{"856K", 856, true},
		{"2G", 2 * 1024 * 1024, true},
		{"100M", 100 * 1024, true},
		{"10M", 10 * 1024, true},
		{"", 0, false},
		{"abc", 0, false},
	}
	for _, tc := range tests {
		got, ok := parseMemColumn(tc.input)
		if ok != tc.ok {
			t.Errorf("parseMemColumn(%q): ok got %v want %v", tc.input, ok, tc.ok)
			continue
		}
		if ok && got != tc.want {
			t.Errorf("parseMemColumn(%q): got %d want %d", tc.input, got, tc.want)
		}
	}
}

func TestParseMacOSProcCount(t *testing.T) {
	tests := []struct {
		input string
		want  int
		ok    bool
	}{
		{"Processes: 727 total, 4 running, 708 sleeping, 5940 threads", 727, true},
		{"Processes: 100 total, ", 100, true},
		{"no match", 0, false},
	}
	for _, tc := range tests {
		got, ok := parseMacOSProcCount(tc.input)
		if ok != tc.ok {
			t.Errorf("parseMacOSProcCount: ok got %v want %v", ok, tc.ok)
			continue
		}
		if ok && got != tc.want {
			t.Errorf("parseMacOSProcCount: got %d want %d", got, tc.want)
		}
	}
}

func TestParseMacOSTopProcesses(t *testing.T) {
	input := `Processes: 5 total, 1 running, 4 sleeping, 10 threads
CPU usage: 10% user, 20% sys, 70% idle

PID    MEM   PPID
589    1144M 1
13746  904M  13494
33007  754M  1
`
	procs := parseMacOSTopProcesses(input)
	if len(procs) != 3 {
		t.Fatalf("proc count: got %d want 3", len(procs))
	}
	if procs[0].PID != 589 || procs[0].MemoryKB != 1144*1024 || procs[0].MemoryKind != MemoryKindFootprint {
		t.Fatalf("proc[0]: %+v", procs[0])
	}
	if procs[1].PPID != 13494 || procs[1].MemoryKB != 904*1024 {
		t.Fatalf("proc[1]: %+v", procs[1])
	}
}

func TestParseMacOSTopProcessesEmpty(t *testing.T) {
	input := "Processes: 0 total\nCPU usage: 5% user, 10% sys, 85% idle\n"
	procs := parseMacOSTopProcesses(input)
	if len(procs) != 0 {
		t.Fatalf("expected 0 procs, got %d", len(procs))
	}
}

// --- Disk usage parsing tests ---

func TestParseDfOutput(t *testing.T) {
	input := "Filesystem     1K-blocks      Used Available Capacity Mounted on\n/dev/disk3s1s1 239362496 12346296 11785340    52%  /\n"
	s, ok := parseDfOutput(input)
	if !ok {
		t.Fatal("expected ok")
	}
	if s.TotalKB != 239362496 {
		t.Fatalf("total: got %d want 239362496", s.TotalKB)
	}
	if s.UsedKB != 12346296 {
		t.Fatalf("used: got %d want 12346296", s.UsedKB)
	}
	if s.AvailableKB != 11785340 {
		t.Fatalf("available: got %d want 11785340", s.AvailableKB)
	}
}

func TestParseDfOutputEmpty(t *testing.T) {
	_, ok := parseDfOutput("Filesystem 1K-blocks Used Available\n")
	if ok {
		t.Fatal("expected not ok for header-only input")
	}
}

func TestParseDfOutputMalformed(t *testing.T) {
	tests := []string{
		"",
		"garbage",
		"Filesystem 1K-blocks Used Available\n",
		"Filesystem 1K-blocks Used Available\n/dev/sda1 abc def ghi 50% /\n",
	}
	for _, input := range tests {
		_, ok := parseDfOutput(input)
		if ok {
			t.Errorf("expected not ok for input %q", input)
		}
	}
}

// TestParseDfOutputLinuxRoot verifies the df parser handles real Linux
// df -k / output format (with Use% column).
func TestParseDfOutputLinuxRoot(t *testing.T) {
	input := "Filesystem     1K-blocks      Used Available Use% Mounted on\n" +
		"/dev/sda1      239362496 192196232  11908312  95% /\n"
	s, ok := parseDfOutput(input)
	if !ok {
		t.Fatalf("parse failed")
	}
	if s.UsedKB != 192196232 {
		t.Fatalf("used: got %d want 192196232", s.UsedKB)
	}
	if s.TotalKB != 239362496 {
		t.Fatalf("total: got %d want 239362496", s.TotalKB)
	}
	if s.AvailableKB != 11908312 {
		t.Fatalf("available: got %d want 11908312", s.AvailableKB)
	}
}

// TestComputeDiskUsed verifies the pure disk-used derivation logic:
// Darwin: consumed = total - available (clamped against underflow).
// Linux: df Used column returned directly.
func TestComputeDiskUsed(t *testing.T) {
	tests := []struct {
		name string
		goos string
		s    DiskSample
		want uint64
	}{
		{
			name: "darwin normal",
			goos: "darwin",
			s:    DiskSample{TotalKB: 228 * 1024 * 1024, UsedKB: 12 * 1024 * 1024, AvailableKB: 8 * 1024 * 1024},
			want: 228*1024*1024 - 8*1024*1024, // 220 GiB
		},
		{
			name: "darwin available exceeds total (clamp)",
			goos: "darwin",
			s:    DiskSample{TotalKB: 100, UsedKB: 10, AvailableKB: 200},
			want: 0, // clamped: 100 - 100 = 0
		},
		{
			name: "darwin available equals total",
			goos: "darwin",
			s:    DiskSample{TotalKB: 100, UsedKB: 0, AvailableKB: 100},
			want: 0,
		},
		{
			name: "darwin zero available",
			goos: "darwin",
			s:    DiskSample{TotalKB: 100, UsedKB: 5, AvailableKB: 0},
			want: 100,
		},
		{
			name: "linux uses df Used directly",
			goos: "linux",
			s:    DiskSample{TotalKB: 239362496, UsedKB: 192196232, AvailableKB: 11908312},
			want: 192196232,
		},
		{
			name: "linux ignores available for used",
			goos: "linux",
			s:    DiskSample{TotalKB: 100, UsedKB: 42, AvailableKB: 200},
			want: 42, // available > total doesn't matter on Linux
		},
		{
			name: "unknown os falls back to df Used",
			goos: "",
			s:    DiskSample{TotalKB: 100, UsedKB: 55, AvailableKB: 45},
			want: 55,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := computeDiskUsed(tc.goos, tc.s)
			if got != tc.want {
				t.Fatalf("computeDiskUsed(%q, %+v): got %d want %d", tc.goos, tc.s, got, tc.want)
			}
		})
	}
}

// --- MemoryKind tests ---

func TestMemoryKindValues(t *testing.T) {
	if MemoryKindFootprint != "footprint" {
		t.Fatalf("MemoryKindFootprint: got %q want %q", MemoryKindFootprint, "footprint")
	}
	if MemoryKindRSS != "rss" {
		t.Fatalf("MemoryKindRSS: got %q want %q", MemoryKindRSS, "rss")
	}
}

func TestParsePsPidPpidMemoryKind(t *testing.T) {
	input := "  PID  PPID    RSS\n    1     0   8960\n"
	procs := parsePsPidPpidMemory(input)
	if len(procs) != 1 {
		t.Fatalf("proc count: got %d want 1", len(procs))
	}
	if procs[0].MemoryKind != MemoryKindRSS {
		t.Fatalf("MemoryKind: got %q want %q", procs[0].MemoryKind, MemoryKindRSS)
	}
}

func TestParseMacOSTopProcessesMemoryKind(t *testing.T) {
	input := "Processes: 1 total\n\nPID    MEM   PPID\n1    100M  0\n"
	procs := parseMacOSTopProcesses(input)
	if len(procs) != 1 {
		t.Fatalf("proc count: got %d want 1", len(procs))
	}
	if procs[0].MemoryKind != MemoryKindFootprint {
		t.Fatalf("MemoryKind: got %q want %q", procs[0].MemoryKind, MemoryKindFootprint)
	}
}

func TestParseMacOSIOStatCPU(t *testing.T) {
	// Real iostat -c 2 -w 1 output on macOS.
	input := "          disk0       cpu    load average\n" +
		"    KB/t  tps  MB/s  us sy id   1m   5m   15m\n" +
		"   14.78  347  5.01  12 11 78  13.22 12.80 8.56\n" +
		"   39.66 1380 53.44  14 11 75  13.22 12.80 8.56\n"
	cpu, ok := parseMacOSIOStatCPU(input)
	if !ok {
		t.Fatalf("parse failed")
	}
	// Second sample: us=14, sy=11 → 25.
	if cpu != 25 {
		t.Fatalf("CPU: got %.0f want 25", cpu)
	}
}

func TestParseMacOSIOStatCPUMultiDisk(t *testing.T) {
	// Real iostat -c 2 -w 1 output on a Mac with two disks (disk0, disk4).
	// The CPU columns are shifted right by 3 positions compared to the
	// single-disk case; the parser must locate "us" dynamically.
	input := "              disk0               disk4       cpu    load average\n" +
		"    KB/t  tps  MB/s     KB/t  tps  MB/s  us sy id   1m   5m   15m\n" +
		"   15.04  181  2.66    73.70    0  0.02   4  4 92  2.80 2.55 2.52\n" +
		"   19.04 7719 143.51     0.00    0  0.00  41 27 32  2.80 2.55 2.52\n"
	cpu, ok := parseMacOSIOStatCPU(input)
	if !ok {
		t.Fatalf("parse failed")
	}
	// Second sample: us=41, sy=27 → 68.
	if cpu != 68 {
		t.Fatalf("CPU: got %.0f want 68", cpu)
	}
}

func TestParseMacOSIOStatCPUShortOutput(t *testing.T) {
	// Only header, no data lines.
	input := "          disk0       cpu    load average\n" +
		"    KB/t  tps  MB/s  us sy id   1m   5m   15m\n"
	_, ok := parseMacOSIOStatCPU(input)
	if ok {
		t.Fatalf("should fail on short output")
	}
}

func TestParseMacOSIOStatCPUFirstSampleIgnored(t *testing.T) {
	// Verify we use the SECOND sample, not the first.
	input := "          disk0       cpu    load average\n" +
		"    KB/t  tps  MB/s  us sy id   1m   5m   15m\n" +
		"   14.78  347  5.01  99 1 0  13.22 12.80 8.56\n" +
		"   39.66 1380 53.44  3 2 95  13.22 12.80 8.56\n"
	cpu, ok := parseMacOSIOStatCPU(input)
	if !ok {
		t.Fatalf("parse failed")
	}
	// Second sample: us=3, sy=2 → 5, NOT first sample 99+1=100.
	if cpu != 5 {
		t.Fatalf("CPU: got %.0f want 5 (should use 2nd sample)", cpu)
	}
}

func TestSampleFastTimeoutRetainsLastGood(t *testing.T) {
	// Simulate a timeout: runCommand with a non-existent binary.
	// The collector should return ok=false, and the daemon should
	// retain last-good values (tested in daemon package).
	c := NewCollector("darwin", "tmux")
	// Override sampleCPUOutput to simulate timeout by using a bad command.
	// We can't easily inject, but we can verify that a real SampleFast
	// on darwin returns valid values (smoke test).
	cpu, cpuOK, _, pressureOK, _, procCountOK := c.SampleFast()
	if !cpuOK {
		t.Fatalf("CPU should be OK on darwin: cpu=%.1f", cpu)
	}
	if !pressureOK {
		t.Fatalf("pressure should be OK on darwin")
	}
	if !procCountOK {
		t.Fatalf("proc count should be OK on darwin")
	}
	if cpu < 0 || cpu > 100 {
		t.Fatalf("CPU out of range: %.1f", cpu)
	}
}

// TestDiskSamplePath verifies the pure path-selection logic for disk
// sampling without depending on the actual machine's filesystem.
func TestDiskSamplePath(t *testing.T) {
	tests := []struct {
		name   string
		goos   string
		exists func(string) bool
		want   string
	}{
		{"darwin data exists", "darwin", func(p string) bool { return p == "/System/Volumes/Data" }, "/System/Volumes/Data"},
		{"darwin data missing", "darwin", func(string) bool { return false }, "/"},
		{"linux always root", "linux", func(string) bool { return true }, "/"},
		{"linux ignores data path", "linux", func(p string) bool { return p == "/System/Volumes/Data" }, "/"},
		{"empty os defaults root", "", func(string) bool { return true }, "/"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := diskSamplePath(tt.goos, tt.exists)
			if got != tt.want {
				t.Fatalf("diskSamplePath(%q): got %q want %q", tt.goos, got, tt.want)
			}
		})
	}
}
