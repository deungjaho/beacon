package dashboard

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/deungjaho/beacon/internal/collector"
	"github.com/deungjaho/beacon/internal/state"
)

const (
	colorReset   = "\033[0m"
	colorBold    = "\033[1m"
	colorDim     = "\033[2m"
	colorGreen   = "\033[32m"
	colorYellow  = "\033[33m"
	colorRed     = "\033[31m"
	colorCyan    = "\033[36m"
	colorMagenta = "\033[35m"
	colorBlue    = "\033[34m"
)

// Data contains all state needed to render the dashboard.
type Data struct {
	Metrics       collector.Metrics
	State         *state.State
	DaemonRunning bool
	Hostname      string
}

// Render formats a full-screen system dashboard.
func Render(d Data) string {
	var sb strings.Builder

	// Header
	host := d.Hostname
	if host == "" {
		host, _ = os.Hostname()
	}
	daemonStatus := colorRed + "Stopped" + colorReset
	if d.DaemonRunning {
		daemonStatus = colorGreen + "Running" + colorReset
	}

	snapTime := "just now"
	if d.Metrics.SampledAt > 0 {
		diff := time.Since(time.Unix(d.Metrics.SampledAt, 0)).Round(time.Second)
		if diff < time.Second {
			snapTime = "just now"
		} else {
			snapTime = fmt.Sprintf("%s ago", diff)
		}
	}

	sb.WriteString(fmt.Sprintf("%s%sBeacon System Monitor%s  %s•%s  %s (%s/%s)  %s•%s  Daemon: %s  %s(%s)%s\n",
		colorBold, colorCyan, colorReset,
		colorDim, colorReset,
		host, runtime.GOOS, runtime.GOARCH,
		colorDim, colorReset,
		daemonStatus,
		colorDim, snapTime, colorReset,
	))
	sb.WriteString(colorDim + strings.Repeat("─", 78) + colorReset + "\n")

	// CPU & System
	sb.WriteString(fmt.Sprintf("%s%s[CPU & 系统状态]%s\n", colorBold, colorBlue, colorReset))

	cpuBar := renderBar(d.Metrics.CPUPercent, 100.0, 18, cpuColor(d.Metrics.CPUPercent))
	cpuStr := "n/a"
	if d.Metrics.CPUOK {
		cpuStr = fmt.Sprintf("%.1f%%", d.Metrics.CPUPercent)
	}
	sb.WriteString(fmt.Sprintf("  CPU 使用率:       %s  %s\n", cpuBar, cpuStr))

	thermalStr := "n/a"
	if d.Metrics.ThermalOK {
		thermalStr = formatThermal(d.Metrics.ThermalLevel)
	}
	sb.WriteString(fmt.Sprintf("  温控降频等级:     %s\n", thermalStr))

	loadStr := "n/a"
	if d.Metrics.LoadAvgOK {
		loadStr = fmt.Sprintf("%s%.2f (1m)%s", loadColor(d.Metrics.LoadAvg1m), d.Metrics.LoadAvg1m, colorReset)
	}
	sb.WriteString(fmt.Sprintf("  系统平均负载:     %s\n", loadStr))

	procStr := "n/a"
	if d.Metrics.ProcCountOK && d.Metrics.ThreadCountOK && d.Metrics.ThreadCount > 0 {
		procStr = fmt.Sprintf("%d procs / %d threads", d.Metrics.ProcCount, d.Metrics.ThreadCount)
	} else if d.Metrics.ThreadCountOK && d.Metrics.ThreadCount > 0 {
		procStr = fmt.Sprintf("%d threads", d.Metrics.ThreadCount)
	} else if d.Metrics.ProcCountOK {
		procStr = fmt.Sprintf("%d procs", d.Metrics.ProcCount)
	}
	sb.WriteString(fmt.Sprintf("  系统进程/线程:   %s\n", procStr))
	sb.WriteString("\n")

	// Memory Subsystem
	sb.WriteString(fmt.Sprintf("%s%s[内存与压缩器]%s\n", colorBold, colorMagenta, colorReset))

	memPressureStr := "n/a"
	memBar := renderBar(float64(d.Metrics.MemPressure), 100.0, 18, pressureColor(d.Metrics.MemPressure))
	if d.Metrics.MemPressureOK {
		memPressureStr = fmt.Sprintf("%d%% %s", d.Metrics.MemPressure, formatPressureLabel(d.Metrics.MemPressure))
	}
	sb.WriteString(fmt.Sprintf("  内存压力:         %s  %s\n", memBar, memPressureStr))

	freeMemStr := "n/a"
	if d.Metrics.RealFreeMemOK && d.Metrics.RealFreeMem != "" {
		freeMemStr = fmt.Sprintf("%s%s%s", freeMemColor(d.Metrics.RealFreeMemKB), d.Metrics.RealFreeMem, colorReset)
	}
	sb.WriteString(fmt.Sprintf("  真实闲置物理内存: %s\n", freeMemStr))

	compStr := "n/a"
	if d.Metrics.CompressorMemOK && d.Metrics.CompressorMem != "" {
		compStr = fmt.Sprintf("%s%s%s", compColor(d.Metrics.CompressorMemKB), d.Metrics.CompressorMem, colorReset)
		if d.Metrics.CompressorMemKB >= 3*1024*1024 {
			compStr += fmt.Sprintf(" %s(内存换页压缩活跃)%s", colorYellow, colorReset)
		}
	}
	sb.WriteString(fmt.Sprintf("  内存压缩器占用:   %s\n", compStr))

	tmuxMemStr := "n/a"
	if d.Metrics.TotalMem != "" {
		tmuxMemStr = fmt.Sprintf("%s%s%s", colorCyan, d.Metrics.TotalMem, colorReset)
	}
	sb.WriteString(fmt.Sprintf("  Tmux 整体占用:    %s\n", tmuxMemStr))
	sb.WriteString("\n")

	// Disk Storage
	sb.WriteString(fmt.Sprintf("%s%s[根存储空间 (/)]%s\n", colorBold, colorYellow, colorReset))
	diskStr := "n/a"
	if d.Metrics.DiskOK && d.Metrics.DiskTotalKB > 0 {
		pct := float64(d.Metrics.DiskUsedKB) / float64(d.Metrics.DiskTotalKB) * 100.0
		diskBar := renderBar(pct, 100.0, 18, diskColor(d.Metrics.DiskAvailableKB))
		diskStr = fmt.Sprintf("%s  %s / %s (%.1f%%)", diskBar, d.Metrics.DiskUsed, d.Metrics.DiskTotal, pct)
	}
	sb.WriteString(fmt.Sprintf("  磁盘使用:         %s\n", diskStr))
	sb.WriteString("\n")

	// Agents & Tmux Panes
	sb.WriteString(fmt.Sprintf("%s%s[Agent 任务状态]%s\n", colorBold, colorCyan, colorReset))
	if d.State != nil && len(d.State.Panes) > 0 {
		count := 0
		for pane, rec := range d.State.Panes {
			if rec.Status == "" {
				continue
			}
			count++
			statusIcon := formatAgentStatus(rec.Status)
			loc := fmt.Sprintf("[%s:%s]", rec.Session, rec.Window)
			sb.WriteString(fmt.Sprintf("  • %-4s %-12s %s %s\n", pane, loc, statusIcon, rec.Summary))
		}
		if count == 0 {
			sb.WriteString("  " + colorDim + "无活跃 Agent 记录" + colorReset + "\n")
		}
	} else {
		sb.WriteString("  " + colorDim + "无活跃 Agent 记录" + colorReset + "\n")
	}

	if d.State != nil && d.State.LastCompleted != nil && d.State.LastCompleted.Summary != "" {
		lc := d.State.LastCompleted
		timeAgo := "刚刚"
		if lc.Time > 0 {
			diff := time.Since(time.Unix(lc.Time, 0)).Round(time.Minute)
			if diff >= time.Minute {
				timeAgo = fmt.Sprintf("%d分钟前", int(diff.Minutes()))
			}
		}
		sb.WriteString(fmt.Sprintf("  最近完成: %s [%s:%s] %s (%s)\n",
			lc.Pane, lc.Session, lc.Window, lc.Summary, timeAgo))
	}

	sb.WriteString(colorDim + strings.Repeat("─", 78) + colorReset + "\n")
	sb.WriteString(fmt.Sprintf(" %s[q / Esc]%s 退出看板    %s[r]%s 手动刷新    %s(每 2 秒自动采样更新)%s\n",
		colorBold, colorReset, colorBold, colorReset, colorDim, colorReset))

	return sb.String()
}

func renderBar(val, max float64, width int, color string) string {
	if max <= 0 || width <= 0 {
		return strings.Repeat("░", width)
	}
	pct := val / max
	if pct < 0 {
		pct = 0
	}
	if pct > 1 {
		pct = 1
	}
	filled := int(pct * float64(width))
	return fmt.Sprintf("[%s%s%s%s%s]",
		color,
		strings.Repeat("█", filled),
		colorDim,
		strings.Repeat("░", width-filled),
		colorReset,
	)
}

func cpuColor(pct float64) string {
	if pct >= 80 {
		return colorRed
	}
	if pct >= 50 {
		return colorYellow
	}
	return colorGreen
}

func loadColor(load float64) string {
	if load >= 15.0 {
		return colorRed
	}
	if load >= 5.0 {
		return colorYellow
	}
	return colorGreen
}

func pressureColor(p int) string {
	if p >= 80 {
		return colorRed
	}
	if p >= 60 {
		return colorYellow
	}
	return colorGreen
}

func formatPressureLabel(p int) string {
	if p >= 80 {
		return fmt.Sprintf("%s(Critical / 内存危急)%s", colorRed, colorReset)
	}
	if p >= 60 {
		return fmt.Sprintf("%s(Warning / 内存预警)%s", colorYellow, colorReset)
	}
	return fmt.Sprintf("%s(Normal / 正常)%s", colorGreen, colorReset)
}

func freeMemColor(kb uint64) string {
	mb := kb / 1024
	if mb < 500 {
		return colorRed
	}
	if mb < 1500 {
		return colorYellow
	}
	return colorGreen
}

func compColor(kb uint64) string {
	mb := kb / 1024
	if mb >= 4096 {
		return colorRed
	}
	if mb >= 2048 {
		return colorYellow
	}
	return colorGreen
}

func diskColor(availKB uint64) string {
	// Less than 5 GiB -> Red
	if availKB < 5*1024*1024 {
		return colorRed
	}
	// Less than 20 GiB -> Yellow
	if availKB < 20*1024*1024 {
		return colorYellow
	}
	return colorGreen
}

func formatThermal(level int) string {
	switch level {
	case 0:
		return fmt.Sprintf("%sL0 (Nominal / 温度正常)%s", colorGreen, colorReset)
	case 1:
		return fmt.Sprintf("%sL1 (Fair / 轻度温控降频)%s", colorYellow, colorReset)
	case 2:
		return fmt.Sprintf("%sL2 (Serious / 严重温控限频)%s", colorMagenta, colorReset)
	case 3:
		return fmt.Sprintf("%sL3 (Critical / 极限过热保护)%s", colorRed, colorReset)
	default:
		return fmt.Sprintf("L%d", level)
	}
}

func formatAgentStatus(status string) string {
	switch status {
	case "working":
		return colorCyan + "● working:" + colorReset
	case "waiting":
		return colorYellow + "▲ waiting:" + colorReset
	case "blocked":
		return colorRed + "■ blocked:" + colorReset
	case "completed":
		return colorGreen + "✓ completed:" + colorReset
	default:
		return status + ":"
	}
}
