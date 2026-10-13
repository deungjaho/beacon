// Package daemon implements the Beacon background daemon. It samples host
// metrics on a bounded interval, writes an atomic snapshot cache, and listens
// on a Unix socket for report/hook updates.
package daemon

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/deungjaho/beacon/internal/collector"
	"github.com/deungjaho/beacon/internal/state"
)

// Config controls daemon behavior.
type Config struct {
	StateDir   string        // panes.json directory
	CacheDir   string        // snapshot.json + socket directory
	SocketPath string        // Unix socket path
	Interval   time.Duration // fast-tier interval (default 4s)
	OS         string
	TmuxBin    string
}

// Daemon is the background sampler and socket server.
// Sampling runs in three independent tiers, each with its own in-flight
// guard so a slow tier never blocks a fast one:
//   - fast (Interval, default 4s): CPU, memory pressure, process count
//   - footprint (10s): per-pane/window/session/total tmux memory
//   - slow (60s): root disk usage
//
// If a tier's tick fires while that tier is still sampling, the tick is
// skipped (not queued) and a dropped counter is incremented. This prevents
// goroutine accumulation when a command times out.
type Daemon struct {
	cfg       Config
	store     *state.Store
	collector *collector.Collector

	// mu protects current (the merged Metrics snapshot) and paneBaseline.
	mu      sync.RWMutex
	current collector.Metrics

	// paneBaseline holds pane memory from ~5min ago (guarded by mu); the
	// dashboard diffs current vs baseline to flag fast-growing panes.
	paneBaseline   map[string]uint64
	paneBaselineAt int64

	// prevNet holds the previous cumulative net counters (guarded by mu)
	// so mergeFast can derive KB/s rates from the delta.
	prevNetRx, prevNetTx uint64
	prevNetAt            int64

	// prevPage holds the previous cumulative paging counters (guarded by
	// mu) so mergeFast can derive pages/s rates from the delta.
	prevPageIns, prevPageOuts uint64
	prevPageAt                int64

	// writeMu serializes snapshot file writes so concurrent tier
	// completions don't collide on the temp file.
	writeMu sync.Mutex

	// Per-tier in-flight guards. Each is held only while that tier is
	// actively sampling; a tick that finds the guard held is skipped.
	fastInFlight      atomic.Bool
	footprintInFlight atomic.Bool
	slowInFlight      atomic.Bool

	// Per-tier dropped counters (ticks skipped because tier was busy).
	fastDropped      atomic.Int64
	footprintDropped atomic.Int64
	slowDropped      atomic.Int64

	// wg tracks all in-flight sampling goroutines so Stop/Run can join
	// them before returning. No sampling goroutine or cache write
	// outlives Run.
	wg sync.WaitGroup

	// acceptWG tracks the acceptLoop goroutine so Stop can join it
	// after closing the listener.
	acceptWG sync.WaitGroup

	stop chan struct{}
	done chan struct{}
}

// New creates a Daemon. The directories are created if needed.
func New(cfg Config) (*Daemon, error) {
	if cfg.Interval <= 0 {
		cfg.Interval = 4 * time.Second
	}
	if cfg.OS == "" {
		cfg.OS = runtime.GOOS
	}
	if cfg.TmuxBin == "" {
		cfg.TmuxBin = "tmux"
	}
	if cfg.SocketPath == "" {
		cfg.SocketPath = filepath.Join(cfg.CacheDir, "beacon.sock")
	}
	store, err := state.NewStore(cfg.StateDir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(cfg.CacheDir, 0o700); err != nil {
		return nil, fmt.Errorf("daemon: mkdir cache: %w", err)
	}
	return &Daemon{
		cfg:       cfg,
		store:     store,
		collector: collector.NewCollector(cfg.OS, cfg.TmuxBin),
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
	}, nil
}

// Run starts the sampling loop and socket server. Blocks until Stop is called.
// On Stop, Run waits for all in-flight tier goroutines and the acceptLoop
// to finish before returning, so no goroutine or cache write outlives Run.
func (d *Daemon) Run() error {
	// Initial samples: fire all three tiers in parallel.
	d.dispatchSampleAll()

	// Start socket listener.
	listener, err := d.listen()
	if err != nil {
		d.wg.Wait()
		close(d.done)
		return err
	}

	// Three independent tickers for three cadence tiers.
	fastTicker := time.NewTicker(d.cfg.Interval)
	defer fastTicker.Stop()
	footprintTicker := time.NewTicker(10 * time.Second)
	defer footprintTicker.Stop()
	slowTicker := time.NewTicker(60 * time.Second)
	defer slowTicker.Stop()

	// Socket accept loop (tracked goroutine).
	d.acceptWG.Add(1)
	go func() {
		defer d.acceptWG.Done()
		d.acceptLoop(listener)
	}()

	// Main loop: dispatch tier-specific samples with in-flight guards.
	for {
		select {
		case <-fastTicker.C:
			d.dispatchSample(&d.fastInFlight, &d.fastDropped, d.sampleFast)
		case <-footprintTicker.C:
			d.dispatchSample(&d.footprintInFlight, &d.footprintDropped, d.sampleFootprint)
		case <-slowTicker.C:
			d.dispatchSample(&d.slowInFlight, &d.slowDropped, d.sampleSlow)
		case <-d.stop:
			// Stop scheduling new samples; join in-flight sampling goroutines.
			d.wg.Wait()
			// Close listener to unblock acceptLoop, then join it.
			listener.Close()
			d.acceptWG.Wait()
			_ = os.Remove(d.cfg.SocketPath)
			close(d.done)
			return nil
		}
	}
}

// dispatchSample fires a tier sample in a goroutine if that tier is not
// already in flight. If the tier is busy, the tick is skipped and the
// tier's dropped counter is incremented. The goroutine is tracked by d.wg
// so Stop can join it.
func (d *Daemon) dispatchSample(inFlight *atomic.Bool, dropped *atomic.Int64, fn func()) {
	select {
	case <-d.stop:
		return
	default:
	}
	if !inFlight.CompareAndSwap(false, true) {
		dropped.Add(1)
		return
	}
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		defer inFlight.Store(false)
		fn()
	}()
}

// dispatchSampleAll fires all three tiers in parallel for the initial sample.
func (d *Daemon) dispatchSampleAll() {
	d.dispatchSample(&d.fastInFlight, &d.fastDropped, d.sampleFast)
	d.dispatchSample(&d.footprintInFlight, &d.footprintDropped, d.sampleFootprint)
	d.dispatchSample(&d.slowInFlight, &d.slowDropped, d.sampleSlow)
}

// Stop signals the daemon to stop and waits for Run to return (which in
// turn waits for all in-flight sampling goroutines).
func (d *Daemon) Stop() {
	select {
	case <-d.stop:
	default:
		close(d.stop)
	}
	<-d.done
}

// sampleFast collects fast-tier metrics (CPU, thermal, load, pressure, free mem, compressor, proc count).
// Does NOT scan the full process list. Retains last-good footprint/slow
// values and marks them stale if they are older than their cadence.
func (d *Daemon) sampleFast() {
	res := d.collector.SampleFast()
	now := time.Now().Unix()

	d.mu.Lock()
	m := d.current
	m.SampledAt = now

	// Fast tier: update or mark stale.
	if res.CPUOK {
		m.CPUPercent = res.CPU
		m.CPUOK = true
		m.CPUSampledAt = now
		m.CPUStale = false
	} else {
		m.CPUStale = true
	}
	if res.PressureOK {
		m.MemPressure = res.Pressure
		m.MemPressureOK = true
	}
	if res.ProcCountOK {
		m.ProcCount = res.ProcCount
		m.ProcCountOK = true
	}
	if res.ThreadCountOK {
		m.ThreadCount = res.ThreadCount
		m.ThreadCountOK = true
	}
	if res.LoadAvgOK {
		m.LoadAvg1m = res.LoadAvg1m
		m.LoadAvg5m = res.LoadAvg5m
		m.LoadAvg15m = res.LoadAvg15m
		m.LoadAvgOK = true
	}
	if res.ThermalOK {
		m.ThermalLevel = res.ThermalLevel
		m.ThermalOK = true
	}
	if res.RealFreeOK {
		m.RealFreeMemKB = res.RealFreeKB
		m.RealFreeMem = collector.FormatMemoryCompact(res.RealFreeKB)
		m.RealFreeMemOK = true
	}
	if res.CompressorOK {
		m.CompressorMemKB = res.CompressorKB
		m.CompressorMem = collector.FormatMemoryCompact(res.CompressorKB)
		m.CompressorMemOK = true
	}
	if res.TopCPU != nil {
		m.TopCPUProcs = res.TopCPU
	}
	if res.TopMem != nil {
		m.TopMemProcs = res.TopMem
	}
	if res.SwapOK {
		m.SwapTotalKB = res.SwapTotalKB
		m.SwapUsedKB = res.SwapUsedKB
		m.SwapTotal = collector.FormatMemoryMB(res.SwapTotalKB)
		m.SwapUsed = collector.FormatMemoryMB(res.SwapUsedKB)
		m.SwapOK = true
	}
	if res.PhysMemOK {
		m.PhysMemUsedKB = res.PhysMemUsedKB
		m.PhysMemTotalKB = res.PhysMemTotalKB
		m.PhysMemOK = true
		m.PhysWiredKB = res.PhysWiredKB
		m.PhysAppKB = res.PhysAppKB
	}
	if res.CPUOK && (res.CPUUsr > 0 || res.CPUSys > 0 || res.CPUIdle > 0) {
		m.CPUUsr, m.CPUSys, m.CPUIdle = res.CPUUsr, res.CPUSys, res.CPUIdle
		m.CPUSplitOK = true
	}
	if res.DiskIOOK {
		m.DiskTps, m.DiskMBs, m.DiskIOOK = res.DiskTps, res.DiskMBs, true
	}
	if res.PageIOOK {
		if d.prevPageAt > 0 {
			if dt := float64(now - d.prevPageAt); dt > 0 && res.PageIns >= d.prevPageIns && res.PageOuts >= d.prevPageOuts {
				m.PageInRate = float64(res.PageIns-d.prevPageIns) / dt
				m.PageOutRate = float64(res.PageOuts-d.prevPageOuts) / dt
				m.PageIOOK = true
			}
		}
		d.prevPageIns, d.prevPageOuts, d.prevPageAt = res.PageIns, res.PageOuts, now
	}
	if res.NetOK {
		if d.prevNetAt > 0 {
			if dt := float64(now - d.prevNetAt); dt > 0 && res.NetRxBytes >= d.prevNetRx && res.NetTxBytes >= d.prevNetTx {
				m.NetRxKBs = float64(res.NetRxBytes-d.prevNetRx) / dt / 1024
				m.NetTxKBs = float64(res.NetTxBytes-d.prevNetTx) / dt / 1024
				m.NetOK = true
			}
		}
		d.prevNetRx, d.prevNetTx, d.prevNetAt = res.NetRxBytes, res.NetTxBytes, now
	}

	// Mark footprint/slow stale if they haven't been sampled yet or are old.
	if !m.FootprintOK || m.FootprintAt == 0 {
		m.FootprintStale = true
	} else if now-m.FootprintAt > 30 {
		m.FootprintStale = true
	}
	if !m.DiskOK || m.DiskSampledAt == 0 {
		m.DiskStale = true
	} else if now-m.DiskSampledAt > 120 {
		m.DiskStale = true
	}

	d.current = m
	d.mu.Unlock()
	d.persistCurrent()
}

// sampleFootprint collects per-pane/window/session/total tmux memory.
// Retains last-good fast/slow values. On success, formatted maps are
// fully rebuilt from the new raw maps — stale keys from closed panes
// are removed, not overlaid.
func (d *Daemon) sampleFootprint() {
	result, ok := d.collector.SampleFootprint()
	now := time.Now().Unix()

	d.mu.Lock()
	m := d.current

	if ok {
		m.PaneMemKB = result.PaneMem
		m.WindowMemKB = result.WindowMem
		m.SessionMemKB = result.SessionMem
		m.TotalMemKB = result.TotalMem
		// Rebuild formatted maps from scratch — no stale keys from closed panes.
		m.PaneMem = make(map[string]string, len(result.PaneMem))
		m.WindowMem = make(map[string]string, len(result.WindowMem))
		m.SessionMem = make(map[string]string, len(result.SessionMem))
		for k, v := range result.PaneMem {
			m.PaneMem[k] = collector.FormatMemoryMB(v)
		}
		for k, v := range result.WindowMem {
			m.WindowMem[k] = collector.FormatMemoryMB(v)
		}
		for k, v := range result.SessionMem {
			m.SessionMem[k] = collector.FormatMemoryMB(v)
		}
		m.TotalMem = collector.FormatMemoryMB(result.TotalMem)
		m.PaneMeta = result.PaneMeta
		m.WindowNames = result.WindowNames
		m.PaneCPU = result.PaneCPU
		// Refresh the ~5min baseline after exposing the old one, so this
		// snapshot still diffs against the previous window.
		m.PaneMemBaselineKB = d.paneBaseline
		if d.paneBaseline == nil || now-d.paneBaselineAt > 300 {
			d.paneBaseline = result.PaneMem
			d.paneBaselineAt = now
		}
		m.FootprintOK = true
		m.FootprintAt = now
		m.FootprintStale = false
	} else {
		m.FootprintStale = true
	}

	d.current = m
	d.mu.Unlock()
	d.persistCurrent()
}

// sampleSlow collects root disk usage.
// External commands run outside the lock so they don't block fast-tier
// merges. Only the short merge phase holds d.mu.
func (d *Daemon) sampleSlow() {
	// Collect outside the lock — this is a slow external command.
	diskUsed, diskTotal, diskAvail, diskOK := d.collector.SampleDisk()
	sysInfo := d.collector.SampleSystemInfo()
	now := time.Now().Unix()

	d.mu.Lock()
	m := d.current

	if diskOK {
		m.DiskUsedKB = diskUsed
		m.DiskTotalKB = diskTotal
		m.DiskAvailableKB = diskAvail
		m.DiskOK = true
		m.DiskSampledAt = now
		m.DiskStale = false
		m.DiskUsed = collector.FormatMemoryMB(diskUsed)
		m.DiskTotal = collector.FormatMemoryMB(diskTotal)
	} else {
		m.DiskStale = true
	}
	if sysInfo.BootTimeSec > 0 {
		m.BootTimeSec = sysInfo.BootTimeSec
	}
	if sysInfo.BatteryOK {
		m.BatteryPct = sysInfo.BatteryPct
		m.BatteryState = sysInfo.BatteryState
		m.BatteryOK = true
	}

	d.current = m
	d.mu.Unlock()
	d.persistCurrent()
}

// SnapshotPath returns the path to the snapshot cache file.
func (d *Daemon) SnapshotPath() string {
	return filepath.Join(d.cfg.CacheDir, "snapshot.json")
}

// persistCurrent writes the latest d.current to the snapshot file.
// It acquires writeMu first (to serialize file writes), then RLocks d.mu
// to copy the current metrics. This ensures that a late-finishing tier
// never overwrites a newer snapshot with its stale copy — the written
// data is always the latest merged state.
func (d *Daemon) persistCurrent() {
	d.writeMu.Lock()
	defer d.writeMu.Unlock()

	d.mu.RLock()
	m := d.current
	d.mu.RUnlock()

	data, err := json.Marshal(m)
	if err != nil {
		return
	}
	tmp := d.SnapshotPath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, d.SnapshotPath())
}

func (d *Daemon) listen() (net.Listener, error) {
	// Remove stale socket.
	if _, err := os.Stat(d.cfg.SocketPath); err == nil {
		// Try connecting; if it fails, the socket is stale.
		conn, err := net.Dial("unix", d.cfg.SocketPath)
		if err != nil {
			_ = os.Remove(d.cfg.SocketPath)
		} else {
			conn.Close()
			return nil, fmt.Errorf("daemon: already running on %s", d.cfg.SocketPath)
		}
	}
	// Ensure parent dir exists.
	if err := os.MkdirAll(filepath.Dir(d.cfg.SocketPath), 0o700); err != nil {
		return nil, err
	}
	listener, err := net.Listen("unix", d.cfg.SocketPath)
	if err != nil {
		return nil, fmt.Errorf("daemon: listen: %w", err)
	}
	// Set socket permissions.
	_ = os.Chmod(d.cfg.SocketPath, 0o600)
	return listener, nil
}

func (d *Daemon) acceptLoop(listener net.Listener) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			select {
			case <-d.stop:
				return
			default:
				continue
			}
		}
		go d.handleConn(conn)
	}
}

// SocketRequest is the JSON protocol for the Unix socket.
type SocketRequest struct {
	Action  string `json:"action"` // "report", "set-last", "cleanup", "acknowledge", "ping"
	Pane    string `json:"pane,omitempty"`
	Status  string `json:"status,omitempty"`
	Summary string `json:"summary,omitempty"`
	Window  string `json:"window,omitempty"`
	Session string `json:"session,omitempty"`
	Cwd     string `json:"cwd,omitempty"`
	Time    int64  `json:"time,omitempty"`
}

type socketResponse struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

func (d *Daemon) handleConn(conn net.Conn) {
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	dec := json.NewDecoder(conn)
	enc := json.NewEncoder(conn)
	var req SocketRequest
	if err := dec.Decode(&req); err != nil {
		_ = enc.Encode(socketResponse{OK: false, Error: err.Error()})
		return
	}
	resp := d.handleRequest(req)
	_ = enc.Encode(resp)
}

func (d *Daemon) handleRequest(req SocketRequest) socketResponse {
	switch req.Action {
	case "ping":
		return socketResponse{OK: true}
	case "report":
		if req.Pane == "" {
			return socketResponse{OK: false, Error: "pane required"}
		}
		rec := state.PaneRecord{
			Status:  req.Status,
			Summary: state.SanitizeSummary(req.Summary),
			Window:  req.Window,
			Session: req.Session,
			Cwd:     req.Cwd,
			Time:    req.Time,
		}
		if rec.Time == 0 {
			rec.Time = time.Now().Unix()
		}
		if err := d.store.SetPane(req.Pane, rec); err != nil {
			return socketResponse{OK: false, Error: err.Error()}
		}
		if req.Status == "completed" {
			_ = d.store.SetLast(state.LastCompleted{
				Pane:    req.Pane,
				Session: req.Session,
				Window:  req.Window,
				Summary: rec.Summary,
				Time:    rec.Time,
			})
		}
		return socketResponse{OK: true}
	case "set-last":
		if req.Pane == "" {
			return socketResponse{OK: false, Error: "pane required"}
		}
		if err := d.store.SetLast(state.LastCompleted{
			Pane:    req.Pane,
			Session: req.Session,
			Window:  req.Window,
			Summary: state.SanitizeSummary(req.Summary),
			Time:    req.Time,
		}); err != nil {
			return socketResponse{OK: false, Error: err.Error()}
		}
		return socketResponse{OK: true}
	case "cleanup":
		livePanes := d.livePanes()
		d.store.Cleanup(time.Now().Unix(), state.CompletedTTLSeconds, livePanes)
		return socketResponse{OK: true}
	case "acknowledge":
		if req.Pane == "" {
			return socketResponse{OK: false, Error: "pane required"}
		}
		if err := d.store.Acknowledge(req.Pane); err != nil {
			return socketResponse{OK: false, Error: err.Error()}
		}
		return socketResponse{OK: true}
	default:
		return socketResponse{OK: false, Error: "unknown action: " + req.Action}
	}
}

func (d *Daemon) livePanes() []string {
	out, err := exec.Command(d.cfg.TmuxBin, "list-panes", "-a", "-F", "#{pane_id}").Output()
	if err != nil {
		return nil
	}
	var panes []string
	line := ""
	for _, b := range out {
		if b == '\n' {
			if line != "" {
				panes = append(panes, line)
			}
			line = ""
		} else {
			line += string(b)
		}
	}
	if line != "" {
		panes = append(panes, line)
	}
	return panes
}

// CurrentMetrics returns the latest sampled metrics (thread-safe).
func (d *Daemon) CurrentMetrics() collector.Metrics {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.current
}

// IsRunning checks if a daemon is listening on the given socket path.
func IsRunning(socketPath string) bool {
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		return false
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(2 * time.Second))
	enc := json.NewEncoder(conn)
	dec := json.NewDecoder(conn)
	if err := enc.Encode(SocketRequest{Action: "ping"}); err != nil {
		return false
	}
	var resp socketResponse
	if err := dec.Decode(&resp); err != nil {
		return false
	}
	return resp.OK
}

// SendReport sends a report request to the daemon via Unix socket.
// Returns an error if the daemon is unreachable.
func SendReport(socketPath string, req SocketRequest) error {
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		return err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	enc := json.NewEncoder(conn)
	dec := json.NewDecoder(conn)
	if err := enc.Encode(req); err != nil {
		return err
	}
	var resp socketResponse
	if err := dec.Decode(&resp); err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf("daemon: %s", resp.Error)
	}
	return nil
}

// SendAcknowledge sends an acknowledge request to the daemon via socket.
// Falls back silently if the daemon is not running (caller should do file-based ack).
func SendAcknowledge(socketPath, pane string) error {
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		return err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	enc := json.NewEncoder(conn)
	dec := json.NewDecoder(conn)
	if err := enc.Encode(SocketRequest{Action: "acknowledge", Pane: pane}); err != nil {
		return err
	}
	var resp socketResponse
	if err := dec.Decode(&resp); err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf("daemon: %s", resp.Error)
	}
	return nil
}

// KillDaemon sends SIGTERM to the daemon process listening on socketPath.
// This is used by `beacon daemon stop`.
func KillDaemon(socketPath string) error {
	// Read the pid from the socket path's sibling pid file.
	pidFile := socketPath + ".pid"
	data, err := os.ReadFile(pidFile)
	if err != nil {
		return fmt.Errorf("daemon: no pid file: %w", err)
	}
	var pid int
	if _, err := fmt.Sscanf(string(data), "%d", &pid); err != nil {
		return fmt.Errorf("daemon: bad pid file: %w", err)
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		return fmt.Errorf("daemon: kill: %w", err)
	}
	_ = os.Remove(pidFile)
	_ = os.Remove(socketPath)
	return nil
}
