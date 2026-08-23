package state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return store
}

func TestStoreInitCreatesDefaultState(t *testing.T) {
	store := newTestStore(t)
	state, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if state.Panes == nil || len(state.Panes) != 0 {
		t.Fatalf("expected empty panes, got %v", state.Panes)
	}
	if state.LastCompleted != nil {
		t.Fatalf("expected nil last_completed, got %v", state.LastCompleted)
	}
}

func TestStoreSetPaneRecordsContext(t *testing.T) {
	store := newTestStore(t)
	rec := PaneRecord{
		Status:  "working",
		Summary: "build project",
		Window:  "@1",
		Session: "test-session",
		Time:    100,
		Cwd:     "/tmp/project",
	}
	if err := store.SetPane("%1", rec); err != nil {
		t.Fatalf("SetPane: %v", err)
	}
	state, _ := store.Load()
	got := state.Panes["%1"]
	if got.Status != "working" {
		t.Fatalf("status: got %q want working", got.Status)
	}
	if got.Summary != "build project" {
		t.Fatalf("summary: got %q want %q", got.Summary, "build project")
	}
	if got.Window != "@1" {
		t.Fatalf("window: got %q want @1", got.Window)
	}
}

func TestStoreSetPaneRejectsBadStatus(t *testing.T) {
	store := newTestStore(t)
	rec := PaneRecord{Status: "bogus", Summary: "x", Time: 1}
	if err := store.SetPane("%1", rec); err == nil {
		t.Fatal("expected error for bad status")
	}
}

func TestStoreSetLastCompleted(t *testing.T) {
	store := newTestStore(t)
	rec := PaneRecord{Status: "completed", Summary: "done", Window: "@1", Session: "s", Time: 100, Cwd: "/tmp"}
	if err := store.SetPane("%1", rec); err != nil {
		t.Fatalf("SetPane: %v", err)
	}
	if err := store.SetLast(LastCompleted{Pane: "%1", Session: "s", Window: "@1", Summary: "done", Time: 100}); err != nil {
		t.Fatalf("SetLast: %v", err)
	}
	state, _ := store.Load()
	if state.LastCompleted == nil || state.LastCompleted.Pane != "%1" {
		t.Fatalf("last_completed: got %v", state.LastCompleted)
	}
}

func TestStoreConcurrentReportsNoLoss(t *testing.T) {
	store := newTestStore(t)
	var wg sync.WaitGroup
	for i := 1; i <= 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rec := PaneRecord{
				Status:  "working",
				Summary: "job-" + strconv.Itoa(i),
				Window:  "@1",
				Session: "s",
				Time:    int64(i),
			}
			if err := store.SetPane(fmt.Sprintf("%%%d", i), rec); err != nil {
				t.Errorf("SetPane %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	state, _ := store.Load()
	if len(state.Panes) != 50 {
		t.Fatalf("pane count: got %d want 50", len(state.Panes))
	}
}

func TestStoreCleanupExpiresCompletedRetainsActive(t *testing.T) {
	store := newTestStore(t)
	store.SetPane("%1", PaneRecord{Status: "completed", Summary: "old", Time: 100})
	store.SetPane("%2", PaneRecord{Status: "working", Summary: "active", Time: 100})
	store.Cleanup(1000, 300, []string{"%1", "%2"})
	state, _ := store.Load()
	if _, ok := state.Panes["%1"]; ok {
		t.Fatal("completed should have expired")
	}
	if got := state.Panes["%2"]; got.Status != "working" {
		t.Fatalf("active should remain: got %v", got)
	}
}

func TestStoreCleanupRemovesDeadPanes(t *testing.T) {
	store := newTestStore(t)
	store.SetPane("%1", PaneRecord{Status: "working", Summary: "live", Time: 100})
	store.SetPane("%2", PaneRecord{Status: "working", Summary: "dead", Time: 100})
	store.Cleanup(200, 300, []string{"%1"})
	state, _ := store.Load()
	if _, ok := state.Panes["%2"]; ok {
		t.Fatal("dead pane should have been removed")
	}
	if _, ok := state.Panes["%1"]; !ok {
		t.Fatal("live pane should remain")
	}
}

func TestStoreResetClearsState(t *testing.T) {
	store := newTestStore(t)
	store.SetPane("%1", PaneRecord{Status: "working", Summary: "x", Time: 1})
	store.Reset()
	state, _ := store.Load()
	if len(state.Panes) != 0 || state.LastCompleted != nil {
		t.Fatalf("reset did not clear: %v", state)
	}
}

func TestStoreAtomicWriteDoesNotCorrupt(t *testing.T) {
	store := newTestStore(t)
	var wg sync.WaitGroup
	for round := 0; round < 10; round++ {
		wg.Add(1)
		go func(round int) {
			defer wg.Done()
			for i := 1; i <= 10; i++ {
				pane := fmt.Sprintf("%%%d", round*10+i)
				store.SetPane(pane, PaneRecord{Status: "working", Summary: "x", Time: int64(i)})
			}
		}(round)
	}
	wg.Wait()
	// Verify file is valid JSON
	data, err := os.ReadFile(filepath.Join(store.dir, "panes.json"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("corrupt JSON: %v", err)
	}
}

func TestStoreLoadHandlesMissingFile(t *testing.T) {
	dir := t.TempDir()
	store, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	state, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if state.Panes == nil {
		t.Fatal("panes should be initialized")
	}
}

func TestStoreLoadHandlesCorruptFile(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "panes.json"), []byte("{broken"), 0o600)
	store, err := NewStore(dir)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	state, err := store.Load()
	if err != nil {
		t.Fatalf("Load should fall back to default: %v", err)
	}
	if len(state.Panes) != 0 {
		t.Fatalf("corrupt file should yield default: %v", state)
	}
}

func TestStripMarkdown(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "bold and inline code",
			in:   "**简短精炼**：控制在 15 字以内的单一短句（如 `已修复并验证配置同步`），不使用 Markdown 格式。",
			want: "简短精炼：控制在 15 字以内的单一短句（如 已修复并验证配置同步），不使用 Markdown 格式。",
		},
		{
			name: "headers and lists",
			in:   "# Title\n- item 1\n* item 2\n1. item 3",
			want: "Title\nitem 1\nitem 2\nitem 3",
		},
		{
			name: "links and images",
			in:   "See [documentation](https://example.com) and ![logo](img.png)",
			want: "See documentation and logo",
		},
		{
			name: "code blocks and strikethrough",
			in:   "```go\nfmt.Println(1)\n```\n~~deprecated~~",
			want: "fmt.Println(1)\ndeprecated",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := StripMarkdown(tt.in)
			if got != tt.want {
				t.Fatalf("StripMarkdown(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestSanitizeSummaryStripsMarkdownAndTruncates(t *testing.T) {
	in := "**简短精炼**：控制在 15 字以内的单一短句（如 `已修复并验证配置同步`），不使用 Markdown 格式。"
	got := SanitizeSummary(in)
	want := "简短精炼：控制在 15 字以内的单一短句（如 已修复并验证配置同步），不使用 Markdown 格式。"
	if got != want {
		t.Fatalf("SanitizeSummary = %q, want %q", got, want)
	}
}
