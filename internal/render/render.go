// Package render assembles the tmux status-right string from host metrics.
// It performs no subprocess calls and no file writes; it is purely a
// read-and-format operation designed to complete in under 5ms.
//
// Agent status (working/waiting/blocked/completed) is NOT shown in
// status-right. Instead, unacknowledged notifications display as a 🔔
// bell in the session/window/pane name (handled by the dotfile tmux
// status scripts). This renderer shows only resource metrics in a
// strict order: CPU, memory pressure, process count, pane memory,
// window memory, session memory, total tmux memory, root disk usage.
//
// All available metrics are always rendered in fixed order. The renderer
// does NOT hide segments based on terminal width — insufficient space is
// handled by tmux's native status-right truncation.
package render

import (
	"fmt"
	"os"
	"strings"

	"github.com/deungjaho/beacon/internal/collector"
)

// Args holds the tmux context passed to status-tmux.
type Args struct {
	Width       int
	StatusBG    string
	SessionName string
	WindowIndex string
	PaneID      string
	WindowID    string
	Compact     bool
	WithTmuxMem bool
}

// Segment is a single Powerline segment.
type Segment struct {
	FG   string
	BG   string
	Text string
	Bold bool
}

const (
	iconCPU         = "\uf4bc"
	iconThermal     = "\uf2c9"
	iconLoad        = "\uf0e4"
	iconPaneMem     = "\ue266"
	iconWindowMem   = "\U000F05B2"
	iconSessionMem  = "\uebc8"
	iconTotalMem    = "\U000F035B"
	iconProcCount   = "\uf46c"
	iconMemPressure = "\uf080"
	iconFreeMem     = "\uf2db"
	iconCompressor  = "\U000F061A"
	iconDisk        = "\uf0a0" // disk icon (U+F0A0)
)

// Render produces the final tmux status-right string.
// All available metrics are always output in fixed order; tmux handles
// truncation when the terminal is too narrow.
func Render(args Args, m collector.Metrics) string {
	if !args.Compact && os.Getenv("BEACON_COMPACT_STATUS") == "1" {
		args.Compact = true
	}
	statusBG := args.StatusBG
	if statusBG == "" || statusBG == "default" {
		statusBG = "black"
	}

	segments := buildSegments(args, m)
	if len(segments) == 0 {
		return ""
	}
	return formatPowerline(statusBG, segments)
}

// buildSegments builds resource-only segments in strict order:
// CPU, thermal, load, memory pressure, real free memory, compressor,
// process count, pane memory, window memory, session memory, total tmux memory,
// root disk usage.
// In compact mode, the 7 host health metrics (CPU, thermal, load, memory pressure,
// real free memory, compressor, process count) are rendered for status-right.
// WithTmuxMem additionally renders pane/window/session memory in compact mode.
// In full mode, all 12 segments are rendered.
func buildSegments(args Args, m collector.Metrics) []Segment {
	var segs []Segment

	// 1. CPU
	if m.CPUOK {
		segs = append(segs, Segment{
			FG:   "#1d1f21",
			BG:   collector.CPUBGColor(m.CPUPercent),
			Text: fmt.Sprintf("%s %s", iconCPU, collector.FormatUsagePercent(m.CPUPercent)),
			Bold: true,
		})
	}

	// 2. Thermal pressure level (full mode only)
	if !args.Compact && m.ThermalOK {
		segs = append(segs, Segment{
			FG:   "#1d1f21",
			BG:   thermalBGColor(m.ThermalLevel),
			Text: fmt.Sprintf("%s L%d", iconThermal, m.ThermalLevel),
			Bold: collector.ThermalSeverity(m.ThermalLevel) != collector.SeverityOK,
		})
	}

	// 3. Memory pressure
	if m.MemPressureOK {
		segs = append(segs, Segment{
			FG:   "#1d1f21",
			BG:   collector.MemPressureColor(m.MemPressure),
			Text: fmt.Sprintf("%s %d%%", iconMemPressure, m.MemPressure),
			Bold: true,
		})
	}

	// 4. System Load average (1m)
	if m.LoadAvgOK {
		segs = append(segs, Segment{
			FG:   "#1d1f21",
			BG:   loadAvgBGColor(m.LoadAvg1m),
			Text: fmt.Sprintf("%s %.1f", iconLoad, m.LoadAvg1m),
			Bold: collector.LoadSeverity(m.LoadAvg1m) == collector.SeverityCrit,
		})
	}

	// 5. Real free physical memory (full mode only)
	if !args.Compact && m.RealFreeMemOK && m.RealFreeMem != "" {
		segs = append(segs, Segment{
			FG:   "#1d1f21",
			BG:   realFreeBGColor(m.RealFreeMemKB),
			Text: fmt.Sprintf("%s %s", iconFreeMem, m.RealFreeMem),
			Bold: collector.RealFreeSeverity(m.RealFreeMemKB) == collector.SeverityCrit,
		})
	}

	// 6. Memory compressor usage
	if m.CompressorMemOK && m.CompressorMem != "" {
		segs = append(segs, Segment{
			FG:   "#1d1f21",
			BG:   compressorBGColor(m.CompressorMemKB),
			Text: fmt.Sprintf("%s %s", iconCompressor, m.CompressorMem),
			Bold: collector.CompressorSeverity(m.CompressorMemKB) == collector.SeverityCrit,
		})
	}

	// 7. Process count, full mode only (fall back to thread count)
	if !args.Compact && m.ProcCountOK && m.ProcCount > 0 {
		segs = append(segs, Segment{
			FG:   "#1d1f21",
			BG:   "#B48EAD",
			Text: fmt.Sprintf("%s %d", iconProcCount, m.ProcCount),
			Bold: true,
		})
	} else if !args.Compact && m.ThreadCountOK && m.ThreadCount > 0 {
		segs = append(segs, Segment{
			FG:   "#1d1f21",
			BG:   "#B48EAD",
			Text: fmt.Sprintf("%s %d", iconProcCount, m.ThreadCount),
			Bold: true,
		})
	}

	if !args.Compact || args.WithTmuxMem {
		// 8. Pane memory
		if args.PaneID != "" {
			if v, ok := m.PaneMem[args.PaneID]; ok && v != "" {
				segs = append(segs, Segment{
					FG:   "#1d1f21",
					BG:   "#7CB8BB",
					Text: fmt.Sprintf("%s %s", iconPaneMem, v),
				})
			}
		}

		// 9. Window memory
		wkey := args.SessionName + ":" + args.WindowIndex
		if v, ok := m.WindowMem[wkey]; ok && v != "" {
			segs = append(segs, Segment{
				FG:   "#F4F4E6",
				BG:   "#5A8A8A",
				Text: fmt.Sprintf("%s %s", iconWindowMem, v),
			})
		}

		// 10. Session memory
		if v, ok := m.SessionMem[args.SessionName]; ok && v != "" {
			segs = append(segs, Segment{
				FG:   "#F4F4E6",
				BG:   "#4A7A7A",
				Text: fmt.Sprintf("%s %s", iconSessionMem, v),
			})
		}
	}

	if !args.Compact {
		// 11. Total tmux memory
		if m.TotalMem != "" {
			segs = append(segs, Segment{
				FG:   "#F4F4E6",
				BG:   "#3A6A6A",
				Text: fmt.Sprintf("%s %s", iconTotalMem, m.TotalMem),
			})
		}

		// 12. Root disk usage
		if m.DiskOK && m.DiskUsed != "" {
			segs = append(segs, Segment{
				FG:   "#1d1f21",
				BG:   diskBGColor(m.DiskAvailableKB),
				Text: fmt.Sprintf("%s %s", iconDisk, m.DiskUsed),
			})
		}
	}

	return segs
}

// Segment palettes share collector.Severity thresholds; only the hex
// mapping differs per metric.
func thermalBGColor(level int) string {
	switch collector.ThermalSeverity(level) {
	case collector.SeverityWarn:
		return "#F0DFAF"
	case collector.SeverityCrit:
		return "#CC9393"
	default:
		return "#7F9F7F"
	}
}

func loadAvgBGColor(load float64) string {
	switch collector.LoadSeverity(load) {
	case collector.SeverityWarn:
		return "#F0DFAF"
	case collector.SeverityCrit:
		return "#CC9393"
	default:
		return "#7F9F7F"
	}
}

func realFreeBGColor(freeKB uint64) string {
	switch collector.RealFreeSeverity(freeKB) {
	case collector.SeverityWarn:
		return "#DFAF8F"
	case collector.SeverityCrit:
		return "#CC9393"
	default:
		return "#5A8A8A"
	}
}

func compressorBGColor(compKB uint64) string {
	switch collector.CompressorSeverity(compKB) {
	case collector.SeverityWarn:
		return "#DFAF8F"
	case collector.SeverityCrit:
		return "#CC9393"
	default:
		return "#4A7A7A"
	}
}

// diskBGColor returns the disk segment background color: green-brown
// when plenty, yellow below 20 GiB, red below 5 GiB. A failed sample is
// expressed by DiskOK=false (segment not rendered), not by a zero value.
func diskBGColor(availableKB uint64) string {
	switch collector.DiskSeverity(availableKB) {
	case collector.SeverityWarn:
		return "#F0DFAF"
	case collector.SeverityCrit:
		return "#CC9393"
	default:
		return "#DFAF8F"
	}
}

func formatPowerline(statusBG string, segs []Segment) string {
	const separator = "\ue0b2"
	const rightCap = "█"
	prevBG := statusBG
	var b strings.Builder
	for _, seg := range segs {
		b.WriteString(fmt.Sprintf("#[fg=%s,bg=%s]%s#[fg=%s,bg=%s", seg.BG, prevBG, separator, seg.FG, seg.BG))
		if seg.Bold {
			b.WriteString(",bold")
		}
		b.WriteString("]")
		b.WriteString(seg.Text)
		b.WriteString(" ")
		prevBG = seg.BG
	}
	b.WriteString(fmt.Sprintf("#[fg=%s,bg=%s]%s", prevBG, statusBG, rightCap))
	return b.String()
}
