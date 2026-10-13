package collector

// Severity is the unified three-level health classification shared by
// the tmux status renderer and the dashboard. Thresholds live here so
// identical metric values always yield identical severity judgments;
// each surface only maps Severity onto its own palette.
type Severity int

const (
	SeverityOK Severity = iota
	SeverityWarn
	SeverityCrit
)

// CPUSeverity: <30 ok, 30–60 warn, >=60 crit.
func CPUSeverity(pct float64) Severity {
	switch {
	case pct >= 60:
		return SeverityCrit
	case pct >= 30:
		return SeverityWarn
	default:
		return SeverityOK
	}
}

// PressureSeverity: <30 ok, 30–60 warn, >=60 crit.
func PressureSeverity(p int) Severity {
	switch {
	case p >= 60:
		return SeverityCrit
	case p >= 30:
		return SeverityWarn
	default:
		return SeverityOK
	}
}

// LoadSeverity: <4 ok, 4–10 warn, >=10 crit.
func LoadSeverity(avg float64) Severity {
	switch {
	case avg >= 10.0:
		return SeverityCrit
	case avg >= 4.0:
		return SeverityWarn
	default:
		return SeverityOK
	}
}

// RealFreeSeverity: >=1GiB ok, >=512MiB warn, <512MiB crit.
func RealFreeSeverity(freeKB uint64) Severity {
	const giB = uint64(1024 * 1024)
	switch {
	case freeKB >= giB:
		return SeverityOK
	case freeKB >= giB/2:
		return SeverityWarn
	default:
		return SeverityCrit
	}
}

// CompressorSeverity: <2GiB ok, 2–4GiB warn, >=4GiB crit.
func CompressorSeverity(compKB uint64) Severity {
	const giB = uint64(1024 * 1024)
	switch {
	case compKB >= 4*giB:
		return SeverityCrit
	case compKB >= 2*giB:
		return SeverityWarn
	default:
		return SeverityOK
	}
}

// DiskSeverity: >=20GiB ok, 5–20GiB warn, <5GiB crit.
func DiskSeverity(availableKB uint64) Severity {
	const giB = uint64(1024 * 1024)
	switch {
	case availableKB < 5*giB:
		return SeverityCrit
	case availableKB < 20*giB:
		return SeverityWarn
	default:
		return SeverityOK
	}
}

// ThermalSeverity: 0 ok, 1 warn, >=2 crit.
func ThermalSeverity(level int) Severity {
	switch {
	case level <= 0:
		return SeverityOK
	case level == 1:
		return SeverityWarn
	default:
		return SeverityCrit
	}
}

// SwapSeverity classifies swap usage by used/total ratio:
// <30% ok, 30–75% warn, >=75% crit (or total=0 → ok).
func SwapSeverity(usedKB, totalKB uint64) Severity {
	if totalKB == 0 || usedKB == 0 {
		return SeverityOK
	}
	ratio := float64(usedKB) / float64(totalKB)
	switch {
	case ratio >= 0.75:
		return SeverityCrit
	case ratio >= 0.30:
		return SeverityWarn
	default:
		return SeverityOK
	}
}
