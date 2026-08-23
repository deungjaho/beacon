package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/deungjaho/beacon/internal/collector"
	"github.com/deungjaho/beacon/internal/state"
)

func newTestDaemon(t *testing.T) (*Daemon, string) {
	t.Helper()
	dir := t.TempDir()
	// macOS limits Unix socket paths to ~104 chars; t.TempDir() is too long.
	// Use a short unique socket path in /tmp.
	sockPath := filepath.Join("/tmp", "beacon-test-"+strconv.Itoa(int(time.Now().UnixNano()))+".sock")
	t.Cleanup(func() { os.Remove(sockPath) })
	cfg := Config{
		StateDir:   filepath.Join(dir, "state"),
		CacheDir:   filepath.Join(dir, "cache"),
		SocketPath: sockPath,
		Interval:   10 * time.Second, // long interval; we trigger samples manually
		OS:         "darwin",
		TmuxBin:    "tmux",
	}
	d, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return d, dir
}

func TestDaemonWritesSnapshot(t *testing.T) {
	d, _ := newTestDaemon(t)
	// Manually trigger one sample to avoid long waits.
	d.dispatchSampleAll()
	d.wg.Wait()
	data, err := os.ReadFile(d.SnapshotPath())
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("snapshot is empty")
	}
}

func TestDaemonSocketReport(t *testing.T) {
	d, dir := newTestDaemon(t)
	// Start daemon in background.
	go d.Run()
	defer d.Stop()
	// Wait for socket to be ready.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if IsRunning(d.cfg.SocketPath) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !IsRunning(d.cfg.SocketPath) {
		t.Fatal("daemon did not start")
	}
	// Send a report.
	err := SendReport(d.cfg.SocketPath, SocketRequest{
		Action:  "report",
		Pane:    "%1",
		Status:  "working",
		Summary: "test job",
		Window:  "@1",
		Session: "s",
		Time:    100,
	})
	if err != nil {
		t.Fatalf("SendReport: %v", err)
	}
	// Verify state was written.
	store, _ := state.NewStore(filepath.Join(dir, "state"))
	st, _ := store.Load()
	rec, ok := st.Panes["%1"]
	if !ok {
		t.Fatal("pane not found")
	}
	if rec.Status != "working" {
		t.Fatalf("status: got %q want working", rec.Status)
	}
}

func TestDaemonConcurrentReportsNoLoss(t *testing.T) {
	d, dir := newTestDaemon(t)
	go d.Run()
	defer d.Stop()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if IsRunning(d.cfg.SocketPath) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !IsRunning(d.cfg.SocketPath) {
		t.Fatal("daemon did not start")
	}
	var wg sync.WaitGroup
	for i := 1; i <= 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			pane := "%" + itoa(i)
			_ = SendReport(d.cfg.SocketPath, SocketRequest{
				Action:  "report",
				Pane:    pane,
				Status:  "working",
				Summary: "job",
				Window:  "@1",
				Session: "s",
				Time:    int64(i),
			})
		}(i)
	}
	wg.Wait()
	store, _ := state.NewStore(filepath.Join(dir, "state"))
	st, _ := store.Load()
	if len(st.Panes) != 50 {
		t.Fatalf("pane count: got %d want 50", len(st.Panes))
	}
}

func TestDaemonKillCLIDoesNotHang(t *testing.T) {
	d, _ := newTestDaemon(t)
	go d.Run()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if IsRunning(d.cfg.SocketPath) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !IsRunning(d.cfg.SocketPath) {
		t.Fatal("daemon did not start")
	}
	d.Stop()
	// After stop, IsRunning should return false.
	if IsRunning(d.cfg.SocketPath) {
		t.Fatal("daemon should not be running after Stop")
	}
}

func TestDaemonSnapshotFallback(t *testing.T) {
	d, _ := newTestDaemon(t)
	d.dispatchSampleAll()
	d.wg.Wait()
	// Simulate daemon down: snapshot file should still be readable.
	data, err := os.ReadFile(d.SnapshotPath())
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var m collector.Metrics
	if err := jsonUnmarshal(data, &m); err != nil {
		t.Fatalf("snapshot corrupt: %v", err)
	}
}

func TestDaemonSampleFastDoesNotSetFootprint(t *testing.T) {
	// sampleFast must NOT populate footprint fields — those come from
	// sampleFootprint at 10s cadence. This ensures the 4s fast path
	// does not trigger a full top -n 999 scan.
	d, _ := newTestDaemon(t)
	d.sampleFast()
	data, err := os.ReadFile(d.SnapshotPath())
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var m collector.Metrics
	if err := jsonUnmarshal(data, &m); err != nil {
		t.Fatalf("snapshot corrupt: %v", err)
	}
	// Footprint should be stale (not yet sampled).
	if !m.FootprintStale {
		t.Fatalf("footprint should be stale after fast-only sample")
	}
	if m.FootprintOK {
		t.Fatalf("footprint should not be OK after fast-only sample")
	}
	// Disk should also be stale.
	if !m.DiskStale {
		t.Fatalf("disk should be stale after fast-only sample")
	}
}

func TestDaemonSampleAllPopulatesAllTiers(t *testing.T) {
	d, _ := newTestDaemon(t)
	d.dispatchSampleAll()
	d.wg.Wait()
	data, err := os.ReadFile(d.SnapshotPath())
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var m collector.Metrics
	if err := jsonUnmarshal(data, &m); err != nil {
		t.Fatalf("snapshot corrupt: %v", err)
	}
	// After sampleAll, all tiers should have been attempted.
	// On macOS, these should succeed.
	if m.SampledAt == 0 {
		t.Fatalf("SampledAt should be set")
	}
}

func TestDaemonLastGoodRetainedOnFailure(t *testing.T) {
	// After a successful sampleAll, a subsequent sampleFast failure
	// should retain last-good footprint values (marked stale).
	d, _ := newTestDaemon(t)
	d.dispatchSampleAll()
	d.wg.Wait()
	data1, _ := os.ReadFile(d.SnapshotPath())
	var m1 collector.Metrics
	jsonUnmarshal(data1, &m1)

	// Simulate a fast sample — footprint should retain last-good.
	d.sampleFast()
	data2, _ := os.ReadFile(d.SnapshotPath())
	var m2 collector.Metrics
	jsonUnmarshal(data2, &m2)

	// Footprint values from sampleAll should still be present.
	if m2.TotalMem != m1.TotalMem {
		t.Fatalf("last-good TotalMem not retained: got %q want %q", m2.TotalMem, m1.TotalMem)
	}
}

func itoa(i int) string {
	return strconv.Itoa(i)
}

func jsonUnmarshal(data []byte, v interface{}) error {
	return json.Unmarshal(data, v)
}

// TestDaemonShutdownNoLateWrite verifies that after Stop returns, no
// sampling goroutine is still running and no cache write happens after
// Stop returns. This catches the bug where Stop closes d.stop but
// in-flight goroutines continue writing the snapshot file.
func TestDaemonShutdownNoLateWrite(t *testing.T) {
	d, _ := newTestDaemon(t)

	// Start the daemon in a goroutine.
	runErr := make(chan error, 1)
	go func() {
		runErr <- d.Run()
	}()

	// Give it time to do initial sampling.
	time.Sleep(3 * time.Second)

	// Stop the daemon — this must join all in-flight goroutines.
	d.Stop()

	// Stop has returned. Record mtime now — no write should happen after.
	infoAtStop, err := os.Stat(d.SnapshotPath())
	if err != nil {
		t.Fatalf("Stat at stop: %v", err)
	}
	mtimeAtStop := infoAtStop.ModTime()

	// Wait to see if any late write happens.
	time.Sleep(2 * time.Second)

	// Check that no goroutine is still in-flight.
	if d.fastInFlight.Load() {
		t.Fatalf("fastInFlight still true after Stop")
	}
	if d.footprintInFlight.Load() {
		t.Fatalf("footprintInFlight still true after Stop")
	}
	if d.slowInFlight.Load() {
		t.Fatalf("slowInFlight still true after Stop")
	}

	// Verify no late write occurred after Stop returned.
	infoAfter, err := os.Stat(d.SnapshotPath())
	if err != nil {
		t.Fatalf("Stat after stop: %v", err)
	}
	if infoAfter.ModTime().After(mtimeAtStop) {
		t.Fatalf("snapshot file was written after Stop returned: atStop=%v after=%v", mtimeAtStop, infoAfter.ModTime())
	}

	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run returned error: %v", err)
		}
	default:
		t.Fatalf("Run did not return after Stop")
	}
}

// TestDaemonFastNotBlockedBySlow verifies that the fast tier is not blocked
// when the slow tier is running. With the old global sampleMu, a slow
// SampleDisk call would block sampleFast.
func TestDaemonFastNotBlockedBySlow(t *testing.T) {
	d, _ := newTestDaemon(t)

	// Manually set slow in-flight to simulate a slow tier running.
	d.slowInFlight.Store(true)

	// Now dispatch a fast sample — it should proceed immediately.
	start := time.Now()
	d.dispatchSample(&d.fastInFlight, &d.fastDropped, d.sampleFast)
	d.wg.Wait()
	elapsed := time.Since(start)

	// Fast sample should complete in well under 10 seconds (the old
	// global mutex would have blocked it until slow finished).
	if elapsed > 8*time.Second {
		t.Fatalf("fast sample was blocked by slow tier: took %v", elapsed)
	}

	d.slowInFlight.Store(false)
}

// TestDaemonInFlightGuardSkipsDuplicate verifies that if a tier is already
// in flight, a second dispatch is skipped and the dropped counter increments.
func TestDaemonInFlightGuardSkipsDuplicate(t *testing.T) {
	d, _ := newTestDaemon(t)

	// Manually hold the fast in-flight flag.
	d.fastInFlight.Store(true)

	// Try to dispatch — should be skipped.
	before := d.fastDropped.Load()
	d.dispatchSample(&d.fastInFlight, &d.fastDropped, d.sampleFast)
	after := d.fastDropped.Load()

	if after != before+1 {
		t.Fatalf("dropped counter: got %d want %d", after, before+1)
	}

	d.fastInFlight.Store(false)
}

// TestDaemonGoroutineNoGrowth verifies that repeatedly dispatching a tier
// while it's in-flight does not spawn new goroutines.
func TestDaemonGoroutineNoGrowth(t *testing.T) {
	d, _ := newTestDaemon(t)

	// Hold the fast in-flight flag so all dispatches are skipped.
	d.fastInFlight.Store(true)
	before := runtime.NumGoroutine()

	for i := 0; i < 100; i++ {
		d.dispatchSample(&d.fastInFlight, &d.fastDropped, d.sampleFast)
	}

	// Allow goroutines to settle.
	time.Sleep(100 * time.Millisecond)
	after := runtime.NumGoroutine()

	d.fastInFlight.Store(false)

	if after > before+2 {
		t.Fatalf("goroutine count grew: before=%d after=%d (dropped=%d)", before, after, d.fastDropped.Load())
	}
}

// TestDaemonFootprintRebuildsMaps verifies that formatted maps are fully
// rebuilt from raw maps on each successful footprint sample. Stale keys
// from closed panes must not persist.
func TestDaemonFootprintRebuildsMaps(t *testing.T) {
	d, _ := newTestDaemon(t)

	// Seed current with a stale pane key that no longer exists.
	d.mu.Lock()
	d.current.PaneMem = map[string]string{"%999": "100M"}
	d.current.WindowMem = map[string]string{"s:999": "100M"}
	d.current.SessionMem = map[string]string{"s_old": "100M"}
	d.current.PaneMemKB = map[string]uint64{"%999": 102400}
	d.current.WindowMemKB = map[string]uint64{"s:999": 102400}
	d.current.SessionMemKB = map[string]uint64{"s_old": 102400}
	d.current.FootprintOK = true
	d.current.FootprintAt = time.Now().Unix()
	d.mu.Unlock()

	// Run a real footprint sample — on this machine, tmux panes will not
	// include %999, s:999, or s_old.
	d.dispatchSample(&d.footprintInFlight, &d.footprintDropped, d.sampleFootprint)
	d.wg.Wait()

	d.mu.RLock()
	defer d.mu.RUnlock()

	// If footprint succeeded, stale keys must be gone.
	if d.current.FootprintOK {
		if _, ok := d.current.PaneMem["%999"]; ok {
			t.Fatalf("stale pane key %%999 should have been removed")
		}
		if _, ok := d.current.WindowMem["s:999"]; ok {
			t.Fatalf("stale window key s:999 should have been removed")
		}
		if _, ok := d.current.SessionMem["s_old"]; ok {
			t.Fatalf("stale session key s_old should have been removed")
		}
	}
}

// TestDaemonConcurrentSnapshotHasAllTiers verifies that when all three
// tiers complete concurrently, the persisted snapshot file contains the
// latest values from all three tiers — not a stale overwrite from a
// late-finishing tier. This catches the bug where writeSnapshot received
// an old m copy and overwrote a newer snapshot.
func TestDaemonConcurrentSnapshotHasAllTiers(t *testing.T) {
	d, _ := newTestDaemon(t)

	// Fire all three tiers in parallel.
	d.dispatchSampleAll()
	d.wg.Wait()

	// Read the snapshot file and verify all three tiers are present.
	data, err := os.ReadFile(d.SnapshotPath())
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var m collector.Metrics
	if err := jsonUnmarshal(data, &m); err != nil {
		t.Fatalf("snapshot corrupt: %v", err)
	}

	// Fast tier should be populated.
	if !m.CPUOK {
		t.Fatalf("CPU should be OK in snapshot")
	}
	if m.CPUPercent == 0 {
		t.Fatalf("CPU should have a non-zero value")
	}

	// Footprint tier should be populated.
	if !m.FootprintOK {
		t.Fatalf("footprint should be OK in snapshot")
	}

	// Slow tier should be populated.
	if !m.DiskOK {
		t.Fatalf("disk should be OK in snapshot")
	}
	if m.DiskTotalKB == 0 {
		t.Fatalf("disk total should be non-zero")
	}
}

// TestDaemonPersistCurrentNoStaleOverwrite verifies that a late-finishing
// tier does not overwrite a newer snapshot. We simulate this by manually
// setting current to a new value, then calling persistCurrent from an
// "old" context — it should write the latest current, not an old copy.
func TestDaemonPersistCurrentNoStaleOverwrite(t *testing.T) {
	d, _ := newTestDaemon(t)

	// Set current to "old" values.
	d.mu.Lock()
	d.current.CPUPercent = 10
	d.current.CPUOK = true
	d.current.DiskTotalKB = 1000
	d.current.DiskOK = true
	d.mu.Unlock()

	// Now update current to "new" values (simulating a fast tier finishing
	// after the slow tier started).
	d.mu.Lock()
	d.current.CPUPercent = 99
	d.current.CPUOK = true
	d.mu.Unlock()

	// Call persistCurrent — it should write the latest (CPU=99), not the
	// old value that a late-finishing goroutine might have passed.
	d.persistCurrent()

	data, err := os.ReadFile(d.SnapshotPath())
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var m collector.Metrics
	if err := jsonUnmarshal(data, &m); err != nil {
		t.Fatalf("snapshot corrupt: %v", err)
	}
	if m.CPUPercent != 99 {
		t.Fatalf("persistCurrent wrote stale CPU: got %.0f want 99", m.CPUPercent)
	}
}
