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
	iconPaneMem     = "\ue266"
	iconWindowMem   = "\U000F05B2"
	iconSessionMem  = "\uebc8"
	iconTotalMem    = "\U000F035B"
	iconProcCount   = "\uf46c"
	iconMemPressure = "\uf080"
	iconDisk        = "\uf0a0" // disk icon (U+F0A0)
)

// Render produces the final tmux status-right string.
// All available metrics are always output in fixed order; tmux handles
// truncation when the terminal is too narrow.
func Render(args Args, m collector.Metrics) string {
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
// CPU, memory pressure, process count, pane memory, window memory,
// session memory, total tmux memory, root disk usage.
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

	// 2. Memory pressure
	if m.MemPressureOK {
		segs = append(segs, Segment{
			FG:   "#1d1f21",
			BG:   collector.MemPressureColor(m.MemPressure),
			Text: fmt.Sprintf("%s %d%%", iconMemPressure, m.MemPressure),
			Bold: true,
		})
	}

	// 3. Process count
	if m.ProcCountOK {
		segs = append(segs, Segment{
			FG:   "#1d1f21",
			BG:   "#B48EAD",
			Text: fmt.Sprintf("%s %d", iconProcCount, m.ProcCount),
			Bold: true,
		})
	}

	// 4. Pane memory
	if args.PaneID != "" {
		if v, ok := m.PaneMem[args.PaneID]; ok && v != "" {
			segs = append(segs, Segment{
				FG:   "#1d1f21",
				BG:   "#7CB8BB",
				Text: fmt.Sprintf("%s %s", iconPaneMem, v),
			})
		}
	}

	// 5. Window memory
	wkey := args.SessionName + ":" + args.WindowIndex
	if v, ok := m.WindowMem[wkey]; ok && v != "" {
		segs = append(segs, Segment{
			FG:   "#F4F4E6",
			BG:   "#5A8A8A",
			Text: fmt.Sprintf("%s %s", iconWindowMem, v),
		})
	}

	// 6. Session memory
	if v, ok := m.SessionMem[args.SessionName]; ok && v != "" {
		segs = append(segs, Segment{
			FG:   "#F4F4E6",
			BG:   "#4A7A7A",
			Text: fmt.Sprintf("%s %s", iconSessionMem, v),
		})
	}

	// 7. Total tmux memory
	if m.TotalMem != "" {
		segs = append(segs, Segment{
			FG:   "#F4F4E6",
			BG:   "#3A6A6A",
			Text: fmt.Sprintf("%s %s", iconTotalMem, m.TotalMem),
		})
	}

	// 8. Root disk usage (used only; total is in snapshot/status JSON)
	// Background is warm-brown by default, red when available < 5 GiB.
	// Color is judged from the raw df Available field, not total-used.
	if m.DiskOK && m.DiskUsed != "" {
		segs = append(segs, Segment{
			FG:   "#1d1f21",
			BG:   diskBGColor(m.DiskAvailableKB),
			Text: fmt.Sprintf("%s %s", iconDisk, m.DiskUsed),
		})
	}

	return segs
}

// diskLowWatermarkKB is the threshold below which the disk segment turns red.
// 5 GiB = 5 * 1024 * 1024 KB.
const diskLowWatermarkKB uint64 = 5 * 1024 * 1024

// diskBGColor returns the disk segment background color.
// Red (#CC9393) when available space is strictly below 5 GiB (including 0);
// warm-brown (#DFAF8F) when available >= 5 GiB.
// A failed sample is expressed by DiskOK=false (segment not rendered),
// not by a zero available value.
func diskBGColor(availableKB uint64) string {
	if availableKB < diskLowWatermarkKB {
		return "#CC9393"
	}
	return "#DFAF8F"
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
