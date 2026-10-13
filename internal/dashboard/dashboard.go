package dashboard

import (
	"fmt"
	"os"
	"runtime"
	"sort"
	"strconv"
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
	Width         int // terminal columns; 0 → default 84
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

	sb.WriteString(fmt.Sprintf("  %s%sBeacon System Monitor%s  %s•%s  %s (%s/%s)  %s•%s  Daemon: %s  %s(%s)%s\n",
		colorBold, colorCyan, colorReset,
		colorDim, colorReset,
		host, runtime.GOOS, runtime.GOARCH,
		colorDim, colorReset,
		daemonStatus,
		colorDim, snapTime, colorReset,
	))
	sb.WriteString("  " + colorDim + strings.Repeat("─", d.sepWidth()) + colorReset + "\n")

	// [系统状态] — label line (name + bar) then a value line: the main value
	// leads at the left margin, attributes follow in fixed 18-wide cells.
	childRow := func(cells ...string) {
		if len(cells) == 0 {
			return
		}
		sb.WriteString("    ")
		for i, c := range cells {
			if i < len(cells)-1 {
				sb.WriteString(padRight(c, 18))
			} else {
				sb.WriteString(c)
			}
		}
		sb.WriteString("\n")
	}
	sb.WriteString(fmt.Sprintf("  %s%s[系统状态]%s\n", colorBold, colorBlue, colorReset))

	cpuBar := renderBar(d.Metrics.CPUPercent, 100.0, 16, sevColor(collector.CPUSeverity(d.Metrics.CPUPercent)))
	cpuStr := "n/a"
	if d.Metrics.CPUOK {
		cpuStr = fmt.Sprintf("%.1f%%", d.Metrics.CPUPercent)
	}
	sb.WriteString(fmt.Sprintf("    %sCPU%s    %s%s\n", colorBold, colorReset, cpuBar, staleMark(d.Metrics.CPUStale)))
	cpuCells := []string{cpuStr}
	if d.Metrics.ThermalOK {
		cpuCells = append(cpuCells, "温控  "+formatThermal(d.Metrics.ThermalLevel))
	}
	if d.Metrics.LoadAvgOK {
		loadStr := fmt.Sprintf("%s%.2f%s", sevColor(collector.LoadSeverity(d.Metrics.LoadAvg1m)), d.Metrics.LoadAvg1m, colorReset)
		if d.Metrics.LoadAvg5m > 0 || d.Metrics.LoadAvg15m > 0 {
			loadStr += fmt.Sprintf("/%.2f/%.2f", d.Metrics.LoadAvg5m, d.Metrics.LoadAvg15m)
		}
		cpuCells = append(cpuCells, "负载  "+loadStr)
	}
	childRow(cpuCells...)
	if d.Metrics.CPUSplitOK {
		childRow(
			fmt.Sprintf("用户  %.0f%%", d.Metrics.CPUUsr),
			fmt.Sprintf("系统  %.0f%%", d.Metrics.CPUSys),
			fmt.Sprintf("空闲  %.0f%%", d.Metrics.CPUIdle),
		)
	}

	// 内存: main value = physical used/total; pressure/free/compressor/swap
	// follow as attribute cells (pressure skipped when it is the fallback).
	physShown := d.Metrics.PhysMemOK && d.Metrics.PhysMemTotalKB > 0
	memMain := "n/a"
	memPct := float64(d.Metrics.MemPressure)
	if physShown {
		memMain = fmt.Sprintf("%s/%s",
			collector.FormatMemoryCompact(d.Metrics.PhysMemUsedKB),
			collector.FormatMemoryCompact(d.Metrics.PhysMemTotalKB))
		memPct = float64(d.Metrics.PhysMemUsedKB) / float64(d.Metrics.PhysMemTotalKB) * 100.0
	} else if d.Metrics.MemPressureOK {
		memMain = sevColor(collector.PressureSeverity(d.Metrics.MemPressure)) +
			fmt.Sprintf("%d%%", d.Metrics.MemPressure) + colorReset
	}
	sb.WriteString("\n")
	memBar := renderBar(memPct, 100.0, 16, sevColor(collector.PressureSeverity(d.Metrics.MemPressure)))
	sb.WriteString(fmt.Sprintf("    %s内存%s   %s\n", colorBold, colorReset, memBar))
	memCells := []string{memMain}
	if physShown && d.Metrics.MemPressureOK {
		memCells = append(memCells, "压力  "+sevColor(collector.PressureSeverity(d.Metrics.MemPressure))+
			fmt.Sprintf("%d%%", d.Metrics.MemPressure)+colorReset)
	}
	if d.Metrics.RealFreeMemOK && d.Metrics.RealFreeMem != "" {
		memCells = append(memCells, "空闲  "+sevColor(collector.RealFreeSeverity(d.Metrics.RealFreeMemKB))+d.Metrics.RealFreeMem+colorReset)
	}
	if d.Metrics.CompressorMemOK && d.Metrics.CompressorMem != "" {
		memCells = append(memCells, "压缩  "+sevColor(collector.CompressorSeverity(d.Metrics.CompressorMemKB))+d.Metrics.CompressorMem+colorReset)
	}
	if d.Metrics.SwapOK {
		memCells = append(memCells, "swap  "+sevColor(collector.SwapSeverity(d.Metrics.SwapUsedKB, d.Metrics.SwapTotalKB))+fmt.Sprintf("%s/%s", d.Metrics.SwapUsed, d.Metrics.SwapTotal)+colorReset)
	}
	childRow(memCells...)
	// Second value line: app/wired breakdown and paging rates.
	var memCells2 []string
	if d.Metrics.PhysAppKB > 0 {
		memCells2 = append(memCells2, "应用  "+collector.FormatMemoryCompact(d.Metrics.PhysAppKB))
	}
	if d.Metrics.PhysWiredKB > 0 {
		memCells2 = append(memCells2, "wired  "+collector.FormatMemoryCompact(d.Metrics.PhysWiredKB))
	}
	if d.Metrics.PageIOOK {
		memCells2 = append(memCells2,
			fmt.Sprintf("换入  %.0f/s", d.Metrics.PageInRate),
			fmt.Sprintf("换出  %.0f/s", d.Metrics.PageOutRate))
	}
	childRow(memCells2...)

	sb.WriteString("\n")
	if d.Metrics.DiskOK && d.Metrics.DiskTotalKB > 0 {
		pct := float64(d.Metrics.DiskUsedKB) / float64(d.Metrics.DiskTotalKB) * 100.0
		sb.WriteString(fmt.Sprintf("    %s磁盘%s   %s%s\n", colorBold, colorReset,
			renderBar(pct, 100.0, 16, sevColor(collector.DiskSeverity(d.Metrics.DiskAvailableKB))),
			staleMark(d.Metrics.DiskStale)))
		diskCells := []string{fmt.Sprintf("%s/%s", d.Metrics.DiskUsed, d.Metrics.DiskTotal)}
		if d.Metrics.DiskAvailableKB > 0 {
			diskCells = append(diskCells, "可用  "+sevColor(collector.DiskSeverity(d.Metrics.DiskAvailableKB))+collector.FormatMemoryMB(d.Metrics.DiskAvailableKB)+colorReset)
		}
		if d.Metrics.DiskIOOK {
			diskCells = append(diskCells, fmt.Sprintf("IO    %.0ftps · %.1fM/s", d.Metrics.DiskTps, d.Metrics.DiskMBs))
		}
		childRow(diskCells...)
	} else {
		sb.WriteString(fmt.Sprintf("    %s磁盘%s   n/a%s\n", colorBold, colorReset, staleMark(d.Metrics.DiskStale)))
	}

	sb.WriteString("\n")
	var procCells []string
	if d.Metrics.ProcCountOK {
		procCells = append(procCells, fmt.Sprintf("%d", d.Metrics.ProcCount))
	}
	if d.Metrics.ThreadCountOK && d.Metrics.ThreadCount > 0 {
		procCells = append(procCells, fmt.Sprintf("线程  %d", d.Metrics.ThreadCount))
	}
	if len(procCells) > 0 {
		sb.WriteString("    " + colorBold + "进程" + colorReset + "\n")
		childRow(procCells...)
	}
	if d.Metrics.NetOK {
		sb.WriteString("\n    " + colorBold + "网络" + colorReset + "\n")
		childRow("↓ "+fmtNetRate(d.Metrics.NetRxKBs), "↑ "+fmtNetRate(d.Metrics.NetTxKBs))
	}
	sb.WriteString("\n")

	// [Tmux 内存分布] — top panes with identity, per-session rollup
	if len(d.Metrics.PaneMemKB) > 0 || len(d.Metrics.SessionMemKB) > 0 {
		sb.WriteString(fmt.Sprintf("  %s%s[Tmux 内存分布]%s", colorBold, colorCyan, colorReset))
		if d.Metrics.TotalMem != "" {
			sb.WriteString(fmt.Sprintf("  %s总 %s%s", colorDim, d.Metrics.TotalMem, colorReset))
		}
		sb.WriteString(fmt.Sprintf("  %sΔ: 5min 变化%s", colorDim, colorReset))
		sb.WriteString(staleMark(d.Metrics.FootprintStale) + "\n")

		// Hierarchical: session (bold + total) → window row with all of
		// that window's panes inline. Fixed ordering: session by name,
		// window by index, pane by id — positions don't jump with usage.
		type gPane struct {
			id, cmd, mem, delta string
			cpu                 float64
		}
		type gWin struct {
			name, idx string
			panes     []gPane
		}
		type gSess struct {
			name  string
			total uint64
			wins  []*gWin
		}
		sessMap := map[string]*gSess{}
		var sessOrder []*gSess
		for id, kb := range d.Metrics.PaneMemKB {
			meta := d.Metrics.PaneMeta[id]
			sname := meta.Session
			if sname == "" {
				sname = "?"
			}
			wname := meta.WindowName
			if wname == "" {
				wname = meta.WindowIndex
			}
			wkey := meta.WindowIndex + "|" + wname
			s := sessMap[sname]
			if s == nil {
				s = &gSess{name: sname}
				sessMap[sname] = s
				sessOrder = append(sessOrder, s)
			}
			var w *gWin
			for _, x := range s.wins {
				if x.idx+"|"+x.name == wkey {
					w = x
					break
				}
			}
			if w == nil {
				w = &gWin{name: wname, idx: meta.WindowIndex}
				s.wins = append(s.wins, w)
			}
			mem := d.Metrics.PaneMem[id]
			if mem == "" {
				mem = collector.FormatMemoryMB(kb)
			}
			w.panes = append(w.panes, gPane{id: id, cmd: clip(meta.Cmd, 14), mem: mem,
				delta: paneDelta(d.Metrics, id), cpu: d.Metrics.PaneCPU[id]})
			s.total += kb
		}
		for _, s := range sessOrder {
			if t := d.Metrics.SessionMemKB[s.name]; t > 0 {
				s.total = t
			}
		}
		sort.Slice(sessOrder, func(i, j int) bool { return sessOrder[i].name < sessOrder[j].name })
		for _, s := range sessOrder {
			sort.Slice(s.wins, func(i, j int) bool { return natLess(s.wins[i].idx, s.wins[j].idx) })
			for _, w := range s.wins {
				sort.Slice(w.panes, func(i, j int) bool { return natLess(w.panes[i].id, w.panes[j].id) })
			}
		}
		for _, s := range sessOrder {
			sb.WriteString(fmt.Sprintf("    %s%s%s  %s%s%s\n",
				colorBold, s.name, colorReset, colorDim,
				collector.FormatMemoryCompact(s.total), colorReset))
			for _, w := range s.wins {
				sb.WriteString("      " + padRight(w.name, 12))
				for i, p := range w.panes {
					if i > 0 && i%2 == 0 {
						sb.WriteString("\n" + strings.Repeat(" ", 18))
					}
					cell := fmt.Sprintf("%s %s %.0f%% %s %s%s%s",
						p.id, p.cmd, p.cpu, p.mem, colorDim, p.delta, colorReset)
					if i == len(w.panes)-1 {
						sb.WriteString(cell)
					} else {
						sb.WriteString(padRight(cell, 38))
					}
				}
				sb.WriteString("\n")
			}
			sb.WriteString("\n")
		}
	}

	// Agents & Tmux Panes — opt-in via BEACON_SHOW_AGENTS=1
	if os.Getenv("BEACON_SHOW_AGENTS") == "1" {
		sb.WriteString(fmt.Sprintf("  %s%s[Agent 任务状态]%s\n", colorBold, colorCyan, colorReset))
		if d.State != nil && len(d.State.Panes) > 0 {
			count := 0
			for pane, rec := range d.State.Panes {
				if rec.Status == "" {
					continue
				}
				count++
				statusIcon := formatAgentStatus(rec.Status)
				loc := fmt.Sprintf("[%s:%s]", rec.Session, rec.Window)
				sb.WriteString(fmt.Sprintf("    • %-4s %-12s %s %s\n", pane, loc, statusIcon, rec.Summary))
			}
			if count == 0 {
				sb.WriteString("    " + colorDim + "无活跃 Agent 记录" + colorReset + "\n")
			}
		} else {
			sb.WriteString("    " + colorDim + "无活跃 Agent 记录" + colorReset + "\n")
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
			sb.WriteString(fmt.Sprintf("    最近完成: %s [%s:%s] %s (%s)\n",
				lc.Pane, lc.Session, lc.Window, lc.Summary, timeAgo))
		}
	}

	sb.WriteString("  " + colorDim + strings.Repeat("─", d.sepWidth()) + colorReset + "\n")
	sb.WriteString(fmt.Sprintf("  %s[q / Esc]%s 退出看板    %s[r]%s 手动刷新    %s(每 2 秒自动采样更新 · BEACON_SHOW_AGENTS=1 显示 Agent)%s\n",
		colorBold, colorReset, colorBold, colorReset, colorDim, colorReset))

	return sb.String()
}

// sepWidth picks the separator width: 2-cell margins on both sides.
func (d Data) sepWidth() int {
	if d.Width >= 56 && d.Width <= 210 {
		return d.Width - 6
	}
	return 80
}

// staleMark renders a dim "数据过期" marker when a sampling tier is stale.
func staleMark(stale bool) string {
	if stale {
		return colorDim + " (数据过期)" + colorReset
	}
	return ""
}

// natLess orders ids numerically when both are numeric (a leading "%" is
// ignored, e.g. %2 < %13), otherwise lexically.
func natLess(a, b string) bool {
	na, ea := strconv.Atoi(strings.TrimPrefix(a, "%"))
	nb, eb := strconv.Atoi(strings.TrimPrefix(b, "%"))
	if ea == nil && eb == nil {
		return na < nb
	}
	return a < b
}

// displayWidth approximates terminal cell width (CJK counts as 2).
func displayWidth(s string) int {
	w := 0
	esc := false
	for _, r := range s {
		switch {
		case esc:
			if r == 'm' {
				esc = false
			}
		case r == '\033':
			esc = true
		case r >= 0x2E80:
			w += 2
		default:
			w++
		}
	}
	return w
}

// padRight pads s with spaces up to the given display width.
func padRight(s string, w int) string {
	if d := w - displayWidth(s); d > 0 {
		return s + strings.Repeat(" ", d)
	}
	return s
}

// clip truncates s to max display cells with a trailing ellipsis.
func clip(s string, max int) string {
	if displayWidth(s) <= max {
		return s
	}
	out := make([]rune, 0, len(s))
	w := 0
	for _, r := range s {
		rw := 1
		if r >= 0x2E80 {
			rw = 2
		}
		if w+rw > max-1 {
			break
		}
		out = append(out, r)
		w += rw
	}
	return string(out) + "…"
}

// paneDelta formats a pane's memory change vs the daemon's ~5min baseline.
func paneDelta(m collector.Metrics, id string) string {
	base, ok := m.PaneMemBaselineKB[id]
	if !ok {
		return "Δ—"
	}
	delta := int64(m.PaneMemKB[id]) - int64(base)
	if delta < 0 {
		return fmt.Sprintf("Δ-%s", collector.FormatMemoryMB(uint64(-delta)))
	}
	return fmt.Sprintf("Δ+%s", collector.FormatMemoryMB(uint64(delta)))
}

// fmtNetRate renders a KB/s rate as "123K/s" or "1.2M/s".
func fmtNetRate(kbs float64) string {
	if kbs >= 1024 {
		return fmt.Sprintf("%.1fM/s", kbs/1024)
	}
	return fmt.Sprintf("%.0fK/s", kbs)
}

// sevColor maps the shared Severity onto the dashboard ANSI palette.
func sevColor(s collector.Severity) string {
	switch s {
	case collector.SeverityCrit:
		return colorRed
	case collector.SeverityWarn:
		return colorYellow
	default:
		return colorGreen
	}
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

// formatThermal renders a compact thermal tag: "L0" when nominal,
// "L1降频" etc. colored by shared thermal severity.
func formatThermal(level int) string {
	label := fmt.Sprintf("L%d", level)
	if level > 0 {
		label += "降频"
	}
	return sevColor(collector.ThermalSeverity(level)) + label + colorReset
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
