package dashboard

import (
	"strings"
	"testing"

	"github.com/deungjaho/beacon/internal/collector"
	"github.com/deungjaho/beacon/internal/state"
)

func TestDashboardRender(t *testing.T) {
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
			ProcCount:       842,
			ProcCountOK:     true,
			ThreadCount:     4500,
			ThreadCountOK:   true,
			CompressorMemOK: true,
			CompressorMem:   "3.3G",
			CompressorMemKB: 3300 * 1024,
			TotalMem:        "1.2G",
			DiskOK:          true,
			DiskUsed:        "195G",
			DiskTotal:       "460G",
			DiskUsedKB:      195 * 1024 * 1024,
			DiskTotalKB:     460 * 1024 * 1024,
			DiskAvailableKB: 265 * 1024 * 1024,
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
		"L1 (Fair / 轻度温控降频)",
		"4.12 (1m)",
		"842 procs / 4500 threads",
		"35% \033[32m(Normal / 正常)",
		"1.1G",
		"3.3G",
		"内存换页压缩活跃",
		"1.2G",
		"195G / 460G",
		"%1",
		"analyzing metrics",
		"tests passed",
	}

	for _, check := range checks {
		if !strings.Contains(out, check) {
			t.Errorf("rendered dashboard missing expected text: %q\nDashboard output:\n%s", check, out)
		}
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
