package dashboard

import (
	"strings"
	"testing"
	"time"

	"github.com/deungjaho/beacon/internal/collector"
	"github.com/deungjaho/beacon/internal/state"
)

func TestDashboardRender(t *testing.T) {
	t.Setenv("BEACON_SHOW_AGENTS", "1")
	d := Data{
		Metrics: collector.Metrics{
			SampledAt:       1727500000,
			CPUPercent:      42.5,
			CPUOK:           true,
			MemPressure:     35,
			MemPressureOK:   true,
			RealFreeMemOK:   true,
			RealFreeMem:     "1.1G",
			RealFreeMemKB:   1100 * 1024,
			ThermalOK:       true,
			ThermalLevel:    1,
			LoadAvgOK:       true,
			LoadAvg1m:       4.12,
			LoadAvg5m:       3.5,
			LoadAvg15m:      2.9,
			ProcCount:       842,
			ProcCountOK:     true,
			ThreadCount:     4500,
			ThreadCountOK:   true,
			CompressorMemOK: true,
			CompressorMem:   "3.3G",
			CompressorMemKB: 3300 * 1024,
			TotalMem:        "1.2G",
			PaneMemKB:       map[string]uint64{"%3": 377 * 1024, "%1": 6 * 1024},
			PaneMem:         map[string]string{"%3": "377M", "%1": "6M"},
			SessionMemKB:    map[string]uint64{"mise": 900 * 1024, "ebiz": 300 * 1024},
			SessionMem:      map[string]string{"mise": "900M", "ebiz": "300M"},
			DiskOK:          true,
			DiskUsed:        "195G",
			DiskTotal:       "460G",
			DiskUsedKB:      195 * 1024 * 1024,
			DiskTotalKB:     460 * 1024 * 1024,
			DiskAvailableKB: 265 * 1024 * 1024,
			PaneMeta: map[string]collector.PaneMeta{
				"%3": {Session: "mise", WindowIndex: "2", WindowName: "p2p", Cmd: "devin", Path: "/x/beacon"},
				"%1": {Session: "ebiz", WindowIndex: "1", WindowName: "web", Cmd: "zsh", Path: "/srv/ebiz"},
			},
			PaneCPU:           map[string]float64{"%3": 12.0, "%1": 2.0},
			PaneMemBaselineKB: map[string]uint64{"%3": 100 * 1024, "%1": 6 * 1024},
			PhysMemOK:         true,
			PhysMemUsedKB:     11800 * 1024,
			PhysMemTotalKB:    16384 * 1024,
			SwapOK:            true,
			SwapUsed:          "512M",
			SwapTotal:         "2.0G",
			SwapUsedKB:        512 * 1024,
			SwapTotalKB:       2048 * 1024,
			BootTimeSec:       time.Now().Add(-49 * time.Hour).Unix(),
			BatteryOK:         true,
			BatteryPct:        87,
			BatteryState:      "discharging",
			NetOK:             true,
			NetRxKBs:          12.4,
			NetTxKBs:          4.4,
			CPUSplitOK:        true,
			CPUUsr:            28,
			CPUSys:            15,
			CPUIdle:           57,
			DiskIOOK:          true,
			DiskTps:           180,
			DiskMBs:           4.2,
			PageIOOK:          true,
			PageInRate:        12,
			PageOutRate:       0,
			PhysWiredKB:       2400 * 1024,
			PhysAppKB:         8400 * 1024,
		},
		State: &state.State{
			Panes: map[string]state.PaneRecord{
				"%1": {
					Session: "camt",
					Window:  "1",
					Status:  "working",
					Summary: "analyzing metrics",
				},
			},
			LastCompleted: &state.LastCompleted{
				Pane:    "%0",
				Session: "camt",
				Window:  "0",
				Summary: "tests passed",
				Time:    1727499900,
			},
		},
		DaemonRunning: true,
		Hostname:      "test-host",
	}

	out := Render(d)

	// Check core metrics are rendered
	checks := []string{
		"Beacon System Monitor",
		"test-host",
		"Daemon: \033[32mRunning",
		"42.5%",
		"L1降频",
		"负载  \033[33m4.12",
		"/3.50/2.90",
		"线程  4500",
		"↓ 12K/s",
		"↑ 4K/s",
		"用户  28%",
		"系统  15%",
		"空闲  57%",
		"IO    180tps · 4.2M/s",
		"应用  8.2G",
		"wired  2.3G",
		"换入  12/s",
		"换出  0/s",
		"Δ+277M",
		"Δ+0M",
		"11.5G/16.0G",
		"压力  \033[33m35%",
		"空闲  \033[32m1.1G",
		"压缩  \033[33m3.3G",
		"swap  \033[32m512M/2.0G",
		"195G/460G",
		"可用  \033[32m265G",
		"[Tmux 内存分布]",
		"总 1.2G",
		"p2p",
		"%3 devin 12% 377M",
		"web",
		"%1 zsh 2% 6M",
		"mise",
		"900M",
		"ebiz",
		"300M",
		"analyzing metrics",
		"tests passed",
	}

	for _, check := range checks {
		if !strings.Contains(out, check) {
			t.Errorf("rendered dashboard missing expected text: %q\nDashboard output:\n%s", check, out)
		}
	}
}

func TestDashboardAgentSectionHiddenByDefault(t *testing.T) {
	d := Data{
		Metrics: collector.Metrics{},
		State: &state.State{
			Panes: map[string]state.PaneRecord{
				"%1": {Session: "s", Window: "1", Status: "working", Summary: "x"},
			},
		},
		DaemonRunning: true,
		Hostname:      "h",
	}
	out := Render(d)
	if strings.Contains(out, "[Agent 任务状态]") {
		t.Fatalf("agent section must be hidden without BEACON_SHOW_AGENTS:\n%s", out)
	}
}

func TestDashboardRenderZero(t *testing.T) {
	d := Data{
		Metrics:       collector.Metrics{},
		State:         nil,
		DaemonRunning: false,
		Hostname:      "empty-host",
	}

	out := Render(d)
	if !strings.Contains(out, "empty-host") {
		t.Errorf("missing hostname in empty state")
	}
	if !strings.Contains(out, "Stopped") {
		t.Errorf("expected stopped daemon status")
	}
}
