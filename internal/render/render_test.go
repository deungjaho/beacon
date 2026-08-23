package render

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/deungjaho/beacon/internal/collector"
)

func TestIconDiskBytes(t *testing.T) {
	// iconDisk is U+F0A0 (BMP) → 3 UTF-8 bytes: 0xEF 0x82 0xA0.
	const wantRune = '\uF0A0'
	wantBytes := []byte{0xEF, 0x82, 0xA0}

	if utf8.RuneCountInString(iconDisk) != 1 {
		t.Fatalf("iconDisk rune count: got %d want 1 (bytes=% x)", utf8.RuneCountInString(iconDisk), []byte(iconDisk))
	}
	r, _ := utf8.DecodeRuneInString(iconDisk)
	if r != wantRune {
		t.Fatalf("iconDisk rune: got U+%04X want U+%04X", r, wantRune)
	}

	got := []byte(iconDisk)
	if len(got) != len(wantBytes) {
		t.Fatalf("iconDisk byte length: got %d want %d (bytes=% x)", len(got), len(wantBytes), got)
	}
	for i := range wantBytes {
		if got[i] != wantBytes[i] {
			t.Fatalf("iconDisk byte %d: got 0x%02X want 0x%02X (bytes=% x)", i, got[i], wantBytes[i], got)
		}
	}
}

func TestRenderEmptyNoMetrics(t *testing.T) {
	args := Args{Width: 160, StatusBG: "black", PaneID: "%1", WindowID: "@1", SessionName: "s", WindowIndex: "1"}
	out := Render(args, collector.Metrics{})
	if out != "" {
		t.Fatalf("expected empty output, got %q", out)
	}
}

func TestRenderNoAgentStatus(t *testing.T) {
	// status-right must never contain agent status text, symbols, or colors.
	// Agent notifications are shown as 🔔 in session/window/pane names instead.
	m := collector.Metrics{
		CPUPercent:    45,
		CPUOK:         true,
		MemPressure:   40,
		MemPressureOK: true,
		ProcCount:     230,
		ProcCountOK:   true,
		PaneMem:       map[string]string{"%1": "120M"},
		WindowMem:     map[string]string{"s:1": "340M"},
		SessionMem:    map[string]string{"s": "1G"},
		TotalMem:      "3G",
	}
	args := Args{Width: 200, StatusBG: "black", PaneID: "%1", WindowID: "@1", SessionName: "s", WindowIndex: "1"}
	out := Render(args, m)
	for _, banned := range []string{"●", "✓", "⚠", "✗", "working", "completed", "waiting", "blocked", "#CC3333"} {
		if strings.Contains(out, banned) {
			t.Fatalf("status-right must not contain agent element %q: %q", banned, out)
		}
	}
}

func TestRenderStrictResourceOrder(t *testing.T) {
	// Strict order: CPU, pressure, proc count, pane mem, window mem, session mem,
	// total mem, disk.
	m := collector.Metrics{
		CPUPercent:      45,
		CPUOK:           true,
		MemPressure:     40,
		MemPressureOK:   true,
		ProcCount:       230,
		ProcCountOK:     true,
		PaneMem:         map[string]string{"%1": "120M"},
		WindowMem:       map[string]string{"s:1": "340M"},
		SessionMem:      map[string]string{"s": "1G"},
		TotalMem:        "3G",
		DiskOK:          true,
		DiskUsed:        "12G",
		DiskTotal:       "228G",
		DiskAvailableKB: 8 * 1024 * 1024,
	}
	args := Args{Width: 400, StatusBG: "black", PaneID: "%1", WindowID: "@1", SessionName: "s", WindowIndex: "1"}
	out := Render(args, m)

	// Find byte positions of each icon and verify order.
	icons := []struct {
		name string
		cp   rune
	}{
		{"CPU", '\uF4BC'},
		{"pressure", '\uF080'},
		{"proc-count", '\uF46C'},
		{"pane-mem", '\uE266'},
		{"window-mem", '\U000F05B2'},
		{"session-mem", '\uEBC8'},
		{"total-mem", '\U000F035B'},
		{"disk", '\uF0A0'},
	}
	prevPos := -1
	for _, ic := range icons {
		ch := string(ic.cp)
		pos := strings.Index(out, ch)
		if pos < 0 {
			t.Fatalf("strict order: %s icon missing in %q", ic.name, out)
		}
		if pos <= prevPos {
			t.Fatalf("strict order: %s (pos %d) must come after previous (pos %d) in %q", ic.name, pos, prevPos, out)
		}
		prevPos = pos
	}
}

func TestRenderCPU(t *testing.T) {
	m := collector.Metrics{CPUPercent: 45, CPUOK: true}
	args := Args{Width: 160, StatusBG: "black", PaneID: "%1", WindowID: "@1", SessionName: "s", WindowIndex: "1"}
	out := Render(args, m)
	if !strings.Contains(out, "\uf4bc") {
		t.Fatalf("missing CPU icon: %q", out)
	}
	if !strings.Contains(out, "45%") {
		t.Fatalf("missing CPU value: %q", out)
	}
	if !strings.Contains(out, "#F0DFAF") {
		t.Fatalf("missing CPU color for 45%%: %q", out)
	}
}

func TestRenderMemPressure(t *testing.T) {
	m := collector.Metrics{MemPressure: 70, MemPressureOK: true}
	args := Args{Width: 160, StatusBG: "black", PaneID: "%1", WindowID: "@1", SessionName: "s", WindowIndex: "1"}
	out := Render(args, m)
	if !strings.Contains(out, "\uf080") {
		t.Fatalf("missing pressure icon: %q", out)
	}
	if !strings.Contains(out, "70%") {
		t.Fatalf("missing pressure value: %q", out)
	}
	if !strings.Contains(out, "#CC9393") {
		t.Fatalf("missing pressure color for 70%%: %q", out)
	}
}

func TestRenderProcCount(t *testing.T) {
	m := collector.Metrics{ProcCount: 230, ProcCountOK: true}
	args := Args{Width: 160, StatusBG: "black", PaneID: "%1", WindowID: "@1", SessionName: "s", WindowIndex: "1"}
	out := Render(args, m)
	if !strings.Contains(out, "\uf46c") {
		t.Fatalf("missing proc icon: %q", out)
	}
	if !strings.Contains(out, "230") {
		t.Fatalf("missing proc count: %q", out)
	}
}

func TestRenderPaneMemory(t *testing.T) {
	m := collector.Metrics{
		PaneMem:   map[string]string{"%1": "120M"},
		WindowMem: map[string]string{"s:1": "340M"},
	}
	args := Args{Width: 160, StatusBG: "black", PaneID: "%1", WindowID: "@1", SessionName: "s", WindowIndex: "1"}
	out := Render(args, m)
	if !strings.Contains(out, "\ue266") {
		t.Fatalf("missing pane mem icon: %q", out)
	}
	if !strings.Contains(out, "120M") {
		t.Fatalf("missing pane mem value: %q", out)
	}
}

func TestRenderWindowSessionTotalMemory(t *testing.T) {
	m := collector.Metrics{
		WindowMem:  map[string]string{"s:1": "340M"},
		SessionMem: map[string]string{"s": "1G"},
		TotalMem:   "3G",
	}
	args := Args{Width: 160, StatusBG: "black", PaneID: "%1", WindowID: "@1", SessionName: "s", WindowIndex: "1"}
	out := Render(args, m)
	if !strings.Contains(out, "\U000F05B2") {
		t.Fatalf("missing window mem icon: %q", out)
	}
	if !strings.Contains(out, "340M") {
		t.Fatalf("missing window mem value: %q", out)
	}
	if !strings.Contains(out, "\uebc8") {
		t.Fatalf("missing session mem icon: %q", out)
	}
	if !strings.Contains(out, "1G") {
		t.Fatalf("missing session mem value: %q", out)
	}
	if !strings.Contains(out, "\U000F035B") {
		t.Fatalf("missing total mem icon: %q", out)
	}
	if !strings.Contains(out, "3G") {
		t.Fatalf("missing total mem value: %q", out)
	}
}

// TestRenderWidthInvariant verifies that the renderer does NOT hide segments
// based on width. All available metrics must always be present in the output
// regardless of width — tmux handles truncation natively.
func TestRenderWidthInvariant(t *testing.T) {
	m := collector.Metrics{
		CPUPercent:      50,
		CPUOK:           true,
		MemPressure:     40,
		MemPressureOK:   true,
		ProcCount:       230,
		ProcCountOK:     true,
		PaneMem:         map[string]string{"%1": "120M"},
		WindowMem:       map[string]string{"s:1": "340M"},
		SessionMem:      map[string]string{"s": "1G"},
		TotalMem:        "3G",
		DiskOK:          true,
		DiskUsed:        "12G",
		DiskTotal:       "228G",
		DiskAvailableKB: 8 * 1024 * 1024,
	}
	// Run at several widths including very narrow ones.
	widths := []int{20, 40, 60, 80, 120, 160, 200, 0}
	for _, w := range widths {
		args := Args{Width: w, StatusBG: "black", PaneID: "%1", WindowID: "@1", SessionName: "s", WindowIndex: "1"}
		out := Render(args, m)
		// All 8 segments must be present at every width.
		checks := []struct{ name, needle string }{
			{"CPU", "\uf4bc"},
			{"pressure", "\uf080"},
			{"proc count", "\uf46c"},
			{"pane mem", "\ue266"},
			{"window mem", "\U000F05B2"},
			{"session mem", "\uebc8"},
			{"total mem", "\U000F035B"},
			{"disk", "\uf0a0"},
		}
		for _, c := range checks {
			if !strings.Contains(out, c.needle) {
				t.Fatalf("width=%d: missing %s segment: %q", w, c.name, out)
			}
		}
	}
}

func TestRenderStatusBGDefault(t *testing.T) {
	m := collector.Metrics{CPUPercent: 45, CPUOK: true}
	args := Args{Width: 160, StatusBG: "default", PaneID: "%1", WindowID: "@1", SessionName: "s", WindowIndex: "1"}
	out := Render(args, m)
	if !strings.Contains(out, "bg=black") {
		t.Fatalf("expected bg=black for default: %q", out)
	}
}

// ---- Exact byte-level icon and Powerline structure tests ----

func exactBytes(r rune) []byte { return []byte(string(r)) }

func assertBytesIn(t *testing.T, name string, haystack []byte, needle []byte) {
	t.Helper()
	if !bytesContains(haystack, needle) {
		t.Errorf("%s: expected bytes %x (%q) in output, not found", name, needle, needle)
	}
}

func bytesContains(haystack, needle []byte) bool {
	if len(needle) == 0 {
		return true
	}
	for i := 0; i <= len(haystack)-len(needle); i++ {
		if bytesEqual(haystack[i:i+len(needle)], needle) {
			return true
		}
	}
	return false
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func countBytesOccurrences(haystack, needle []byte) int {
	if len(needle) == 0 {
		return 0
	}
	count := 0
	i := 0
	for i <= len(haystack)-len(needle) {
		if bytesEqual(haystack[i:i+len(needle)], needle) {
			count++
			i += len(needle)
		} else {
			i++
		}
	}
	return count
}

func bytesHasSuffix(s, suffix []byte) bool {
	if len(suffix) > len(s) {
		return false
	}
	return bytesEqual(s[len(s)-len(suffix):], suffix)
}

func TestRenderExactIconBytes(t *testing.T) {
	m := collector.Metrics{
		CPUPercent:    45,
		CPUOK:         true,
		MemPressure:   40,
		MemPressureOK: true,
		ProcCount:     230,
		ProcCountOK:   true,
		PaneMem:       map[string]string{"%1": "120M"},
		WindowMem:     map[string]string{"s:1": "340M"},
		SessionMem:    map[string]string{"s": "1G"},
		TotalMem:      "3G",
	}
	args := Args{Width: 300, StatusBG: "black", PaneID: "%1", WindowID: "@1", SessionName: "s", WindowIndex: "1"}
	out := []byte(Render(args, m))

	assertBytesIn(t, "cpu-icon", out, exactBytes('\uF4BC'))
	assertBytesIn(t, "pressure-icon", out, exactBytes('\uF080'))
	assertBytesIn(t, "proc-count-icon", out, exactBytes('\uF46C'))
	assertBytesIn(t, "pane-mem-icon", out, exactBytes('\uE266'))
	assertBytesIn(t, "window-mem-icon", out, exactBytes('\U000F05B2'))
	assertBytesIn(t, "session-mem-icon", out, exactBytes('\uEBC8'))
	assertBytesIn(t, "total-mem-icon", out, exactBytes('\U000F035B'))
}

func TestRenderExactPowerlineSeparatorBytes(t *testing.T) {
	m := collector.Metrics{
		CPUPercent:    45,
		CPUOK:         true,
		MemPressure:   40,
		MemPressureOK: true,
		ProcCount:     230,
		ProcCountOK:   true,
		PaneMem:       map[string]string{"%1": "120M"},
		WindowMem:     map[string]string{"s:1": "340M"},
		SessionMem:    map[string]string{"s": "1G"},
		TotalMem:      "3G",
	}
	args := Args{Width: 300, StatusBG: "black", PaneID: "%1", WindowID: "@1", SessionName: "s", WindowIndex: "1"}
	out := []byte(Render(args, m))

	sep := exactBytes('\uE0B2')
	// 7 resource segments → at least 7 separators.
	count := countBytesOccurrences(out, sep)
	if count < 7 {
		t.Errorf("expected at least 7 Powerline separators, got %d in %q", count, out)
	}
}

func TestRenderPowerlineSeparatorBetweenCPUAndPressure(t *testing.T) {
	// CPU at 45% → bg #F0DFAF. Pressure at 20% → bg #7F9F7F.
	m := collector.Metrics{
		CPUPercent:    45,
		CPUOK:         true,
		MemPressure:   20,
		MemPressureOK: true,
	}
	args := Args{Width: 200, StatusBG: "black", PaneID: "%1", WindowID: "@1", SessionName: "s", WindowIndex: "1"}
	out := []byte(Render(args, m))

	sep := exactBytes('\uE0B2')
	// #[fg=#7F9F7F,bg=#F0DFAF]<sep>#[fg=#1d1f21,bg=#7F9F7F
	boundary := []byte("#7F9F7F,bg=#F0DFAF]")
	boundary = append(boundary, sep...)
	boundary = append(boundary, []byte("#[fg=#1d1f21,bg=#7F9F7F")...)
	assertBytesIn(t, "separator-cpu-to-pressure", out, boundary)

	// First separator: black → CPU #F0DFAF.
	firstBoundary := []byte("#[fg=#F0DFAF,bg=black]")
	firstBoundary = append(firstBoundary, sep...)
	firstBoundary = append(firstBoundary, []byte("#[fg=#1d1f21,bg=#F0DFAF")...)
	assertBytesIn(t, "separator-statusbg-to-cpu", out, firstBoundary)
}

func TestRenderPowerlineSeparatorBetweenPressureAndProcCount(t *testing.T) {
	// Pressure at 20% → bg #7F9F7F. Proc count → bg #B48EAD.
	m := collector.Metrics{
		MemPressure:   20,
		MemPressureOK: true,
		ProcCount:     230,
		ProcCountOK:   true,
	}
	args := Args{Width: 200, StatusBG: "black", PaneID: "%1", WindowID: "@1", SessionName: "s", WindowIndex: "1"}
	out := []byte(Render(args, m))

	sep := exactBytes('\uE0B2')
	boundary := []byte("#B48EAD,bg=#7F9F7F]")
	boundary = append(boundary, sep...)
	boundary = append(boundary, []byte("#[fg=#1d1f21,bg=#B48EAD")...)
	assertBytesIn(t, "separator-pressure-to-proccount", out, boundary)
}

func TestRenderPowerlineSeparatorBetweenProcCountAndPaneMem(t *testing.T) {
	// Proc count → bg #B48EAD. Pane mem → bg #7CB8BB.
	m := collector.Metrics{
		ProcCount:   230,
		ProcCountOK: true,
		PaneMem:     map[string]string{"%1": "120M"},
	}
	args := Args{Width: 200, StatusBG: "black", PaneID: "%1", WindowID: "@1", SessionName: "s", WindowIndex: "1"}
	out := []byte(Render(args, m))

	sep := exactBytes('\uE0B2')
	boundary := []byte("#7CB8BB,bg=#B48EAD]")
	boundary = append(boundary, sep...)
	boundary = append(boundary, []byte("#[fg=#1d1f21,bg=#7CB8BB")...)
	assertBytesIn(t, "separator-proccount-to-panemem", out, boundary)
}

func TestRenderPowerlineSeparatorBetweenPaneMemAndWindowMem(t *testing.T) {
	m := collector.Metrics{
		PaneMem:   map[string]string{"%1": "120M"},
		WindowMem: map[string]string{"s:1": "340M"},
	}
	args := Args{Width: 200, StatusBG: "black", PaneID: "%1", WindowID: "@1", SessionName: "s", WindowIndex: "1"}
	out := []byte(Render(args, m))

	sep := exactBytes('\uE0B2')
	boundary := []byte("#5A8A8A,bg=#7CB8BB]")
	boundary = append(boundary, sep...)
	boundary = append(boundary, []byte("#[fg=#F4F4E6,bg=#5A8A8A")...)
	assertBytesIn(t, "separator-panemem-to-windowmem", out, boundary)
}

func TestRenderPowerlineSeparatorBetweenWindowMemAndSessionMem(t *testing.T) {
	m := collector.Metrics{
		WindowMem:  map[string]string{"s:1": "340M"},
		SessionMem: map[string]string{"s": "1G"},
	}
	args := Args{Width: 200, StatusBG: "black", PaneID: "%1", WindowID: "@1", SessionName: "s", WindowIndex: "1"}
	out := []byte(Render(args, m))

	sep := exactBytes('\uE0B2')
	boundary := []byte("#4A7A7A,bg=#5A8A8A]")
	boundary = append(boundary, sep...)
	boundary = append(boundary, []byte("#[fg=#F4F4E6,bg=#4A7A7A")...)
	assertBytesIn(t, "separator-windowmem-to-sessionmem", out, boundary)
}

func TestRenderPowerlineSeparatorBetweenSessionMemAndTotalMem(t *testing.T) {
	m := collector.Metrics{
		SessionMem: map[string]string{"s": "1G"},
		TotalMem:   "3G",
	}
	args := Args{Width: 200, StatusBG: "black", PaneID: "%1", WindowID: "@1", SessionName: "s", WindowIndex: "1"}
	out := []byte(Render(args, m))

	sep := exactBytes('\uE0B2')
	boundary := []byte("#3A6A6A,bg=#4A7A7A]")
	boundary = append(boundary, sep...)
	boundary = append(boundary, []byte("#[fg=#F4F4E6,bg=#3A6A6A")...)
	assertBytesIn(t, "separator-sessionmem-to-totalmem", out, boundary)
}

func TestRenderExactRightCapBytes(t *testing.T) {
	m := collector.Metrics{CPUPercent: 45, CPUOK: true}
	args := Args{Width: 160, StatusBG: "black", PaneID: "%1", WindowID: "@1", SessionName: "s", WindowIndex: "1"}
	out := []byte(Render(args, m))

	cap := exactBytes('\u2588')
	if !bytesHasSuffix(out, cap) {
		t.Errorf("expected output to end with right cap bytes %x, got suffix %x", cap, out[len(out)-len(cap):])
	}
}

func TestRenderRightCapFromLastSegmentToStatusBG(t *testing.T) {
	// With only CPU (bg #F0DFAF) and status-bg black:
	//   #[fg=#F0DFAF,bg=black]<cap>
	m := collector.Metrics{CPUPercent: 45, CPUOK: true}
	args := Args{Width: 160, StatusBG: "black", PaneID: "%1", WindowID: "@1", SessionName: "s", WindowIndex: "1"}
	out := []byte(Render(args, m))

	cap := exactBytes('\u2588')
	suffix := []byte("#[fg=#F0DFAF,bg=black]")
	suffix = append(suffix, cap...)
	if !bytesHasSuffix(out, suffix) {
		t.Errorf("expected output to end with %q, got suffix %q", suffix, out[len(out)-len(suffix):])
	}
}

func TestRenderNoSeparatorWhenSingleSegment(t *testing.T) {
	m := collector.Metrics{CPUPercent: 45, CPUOK: true}
	args := Args{Width: 160, StatusBG: "black", PaneID: "%1", WindowID: "@1", SessionName: "s", WindowIndex: "1"}
	out := []byte(Render(args, m))

	sep := exactBytes('\uE0B2')
	count := countBytesOccurrences(out, sep)
	if count != 1 {
		t.Errorf("single segment: expected exactly 1 separator, got %d in %q", count, out)
	}
	cap := exactBytes('\u2588')
	capCount := countBytesOccurrences(out, cap)
	if capCount != 1 {
		t.Errorf("single segment: expected exactly 1 right cap, got %d in %q", capCount, out)
	}
}

// TestRenderSegmentTrailingSpace verifies that each segment text is
// followed by exactly one space, and the last segment has exactly one
// space before the right cap (no double space).
func TestRenderSegmentTrailingSpace(t *testing.T) {
	m := collector.Metrics{
		CPUPercent:      45,
		CPUOK:           true,
		MemPressure:     40,
		MemPressureOK:   true,
		ProcCount:       230,
		ProcCountOK:     true,
		PaneMem:         map[string]string{"%1": "120M"},
		WindowMem:       map[string]string{"s:1": "340M"},
		SessionMem:      map[string]string{"s": "1G"},
		TotalMem:        "3G",
		DiskOK:          true,
		DiskUsed:        "12G",
		DiskTotal:       "228G",
		DiskAvailableKB: 8 * 1024 * 1024,
	}
	args := Args{Width: 400, StatusBG: "black", PaneID: "%1", WindowID: "@1", SessionName: "s", WindowIndex: "1"}
	out := Render(args, m)

	// Each segment value must be followed by exactly one space.
	// Verify "45% " (CPU), "40% " (pressure), "230 " (proc), etc.
	for _, want := range []string{"45% ", "40% ", "230 ", "120M ", "340M ", "1G ", "3G ", "12G "} {
		if !strings.Contains(out, want) {
			t.Errorf("expected segment text followed by one space %q in %q", want, out)
		}
	}

	// No double space anywhere.
	if strings.Contains(out, "  ") {
		t.Errorf("output must not contain double space: %q", out)
	}

	// The right cap must be immediately preceded by the last segment's
	// single trailing space + the cap color sequence — not a double space.
	cap := string('\u2588')
	if strings.Contains(out, "  "+cap) {
		t.Errorf("double space before right cap: %q", out)
	}
}

func TestRenderAllSegmentsTogether(t *testing.T) {
	m := collector.Metrics{
		CPUPercent:      45,
		CPUOK:           true,
		MemPressure:     40,
		MemPressureOK:   true,
		ProcCount:       230,
		ProcCountOK:     true,
		PaneMem:         map[string]string{"%1": "120M"},
		WindowMem:       map[string]string{"s:1": "340M"},
		SessionMem:      map[string]string{"s": "1G"},
		TotalMem:        "3G",
		DiskOK:          true,
		DiskUsed:        "12G",
		DiskTotal:       "228G",
		DiskAvailableKB: 8 * 1024 * 1024,
	}
	args := Args{Width: 400, StatusBG: "black", PaneID: "%1", WindowID: "@1", SessionName: "s", WindowIndex: "1"}
	out := Render(args, m)
	// status-right shows only used for disk (not /total).
	for _, want := range []string{"45%", "\uf4bc", "\uf080", "120M", "\U000F05B2", "340M", "\uebc8", "1G", "\U000F035B", "3G", "\uf46c", "230", "12G"} {
		if !strings.Contains(out, want) {
			t.Fatalf("wide render missing %q: %q", want, out)
		}
	}
	// Total values must NOT appear in status-right.
	for _, unwanted := range []string{"228G", "12G/228G"} {
		if strings.Contains(out, unwanted) {
			t.Fatalf("wide render must not contain %q: %q", unwanted, out)
		}
	}
}

func TestRenderDiskSegment(t *testing.T) {
	m := collector.Metrics{DiskOK: true, DiskUsed: "12G", DiskTotal: "228G", DiskAvailableKB: 8 * 1024 * 1024}
	args := Args{Width: 200, StatusBG: "black", PaneID: "%1", WindowID: "@1", SessionName: "s", WindowIndex: "1"}
	out := Render(args, m)
	// status-right shows only used, not /total.
	if !strings.Contains(out, "12G") {
		t.Fatalf("missing disk used: %q", out)
	}
	if strings.Contains(out, "12G/228G") {
		t.Fatalf("disk must not show /total in status-right: %q", out)
	}
	if strings.Contains(out, "228G") {
		t.Fatalf("disk must not contain total value: %q", out)
	}
	// Default warm-brown background, dark foreground.
	if !strings.Contains(out, "bg=#DFAF8F") {
		t.Fatalf("missing disk bg #DFAF8F: %q", out)
	}
	if !strings.Contains(out, "fg=#1d1f21,bg=#DFAF8F") {
		t.Fatalf("missing disk fg #1d1f21: %q", out)
	}
}

func TestDiskBGColor(t *testing.T) {
	// 5 GiB = 5 * 1024 * 1024 KB
	const giB5 = uint64(5 * 1024 * 1024)
	tests := []struct {
		name        string
		availableKB uint64
		want        string
	}{
		{"5GiB-1 red", giB5 - 1, "#CC9393"},
		{"exactly 5GiB brown", giB5, "#DFAF8F"},
		{"above 5GiB brown", giB5 + 1, "#DFAF8F"},
		{"0 red", 0, "#CC9393"},
		{"8.5GiB brown (typical)", 8*1024*1024 + 512*1024, "#DFAF8F"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := diskBGColor(tc.availableKB)
			if got != tc.want {
				t.Fatalf("diskBGColor(%d): got %q want %q", tc.availableKB, got, tc.want)
			}
		})
	}
}

func TestRenderDiskSegmentColorByAvailable(t *testing.T) {
	// Verify render uses DiskAvailableKB for color, not total-used.
	tests := []struct {
		name    string
		availKB uint64
		wantBG  string
	}{
		{"low available → red", 4 * 1024 * 1024, "#CC9393"},
		{"sufficient available → brown", 8 * 1024 * 1024, "#DFAF8F"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := collector.Metrics{
				DiskOK:          true,
				DiskUsed:        "220G",
				DiskTotal:       "228G",
				DiskAvailableKB: tc.availKB,
			}
			args := Args{Width: 200, StatusBG: "black", PaneID: "%1", WindowID: "@1", SessionName: "s", WindowIndex: "1"}
			out := Render(args, m)
			if !strings.Contains(out, "bg="+tc.wantBG) {
				t.Fatalf("disk bg: got %q want %s in %q", out, tc.wantBG, out)
			}
			// Text still shows only used, not available.
			if !strings.Contains(out, "220G") {
				t.Fatalf("missing disk used value: %q", out)
			}
		})
	}
}

func TestRenderPowerlineSeparatorBetweenTotalMemAndDisk(t *testing.T) {
	// Total mem → bg #3A6A6A. Disk → bg #DFAF8F (default brown).
	m := collector.Metrics{
		TotalMem:        "3G",
		DiskOK:          true,
		DiskUsed:        "12G",
		DiskAvailableKB: 8 * 1024 * 1024,
	}
	args := Args{Width: 200, StatusBG: "black", PaneID: "%1", WindowID: "@1", SessionName: "s", WindowIndex: "1"}
	out := []byte(Render(args, m))

	sep := exactBytes('\uE0B2')
	boundary := []byte("#DFAF8F,bg=#3A6A6A]")
	boundary = append(boundary, sep...)
	boundary = append(boundary, []byte("#[fg=#1d1f21,bg=#DFAF8F")...)
	assertBytesIn(t, "separator-totalmem-to-disk", out, boundary)
}
