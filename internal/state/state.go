// Package state provides concurrent-safe, atomic local state management for
// Beacon pane records. It is the Go equivalent of lib/state.sh.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"sync"
	"time"
)

const (
	CompletedTTLSeconds = 300
	LockTimeout         = 1500 * time.Millisecond
	LockStaleAge        = 30 * time.Second
)

// PaneRecord is the per-pane agent state written by hooks and report.
type PaneRecord struct {
	Session      string `json:"session"`
	Window       string `json:"window"`
	Status       string `json:"status"`
	Summary      string `json:"summary"`
	Time         int64  `json:"time"`
	Cwd          string `json:"cwd"`
	Acknowledged bool   `json:"acknowledged"`
}

// LastCompleted tracks the most recently completed pane for jump.
type LastCompleted struct {
	Pane    string `json:"pane"`
	Session string `json:"session"`
	Window  string `json:"window"`
	Summary string `json:"summary"`
	Time    int64  `json:"time"`
}

// State is the full on-disk state document.
type State struct {
	Panes         map[string]PaneRecord `json:"panes"`
	LastCompleted *LastCompleted        `json:"last_completed"`
}

func defaultState() *State {
	return &State{Panes: map[string]PaneRecord{}, LastCompleted: nil}
}

// NewState returns a fresh empty State.
func NewState() *State {
	return defaultState()
}

func validStatus(s string) bool {
	switch s {
	case "working", "waiting", "blocked", "completed":
		return true
	}
	return false
}

// IsNotificationStatus returns true for statuses that require user attention
// and should trigger a bell. "working" is a lifecycle state, not a notification.
func IsNotificationStatus(s string) bool {
	switch s {
	case "waiting", "blocked", "completed":
		return true
	}
	return false
}

// Store manages the local panes.json file with a mkdir-based mutex lock.
type Store struct {
	dir     string
	file    string
	lockDir string
	mu      sync.Mutex // serializes in-process callers; cross-process uses mkdir lock
	now     func() int64
}

// NewStore creates a Store rooted at dir. The directory is created if needed.
func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("state: mkdir: %w", err)
	}
	return &Store{
		dir:     dir,
		file:    filepath.Join(dir, "panes.json"),
		lockDir: filepath.Join(dir, ".state.lock"),
		now:     func() int64 { return time.Now().Unix() },
	}, nil
}

// SetNow overrides the clock (for testing).
func (s *Store) SetNow(fn func() int64) { s.now = fn }

// Load reads and validates panes.json, falling back to default on missing or
// corrupt files.
func (s *Store) Load() (*State, error) {
	data, err := os.ReadFile(s.file)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return defaultState(), nil
		}
		return nil, fmt.Errorf("state: read: %w", err)
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return defaultState(), nil
	}
	if st.Panes == nil {
		st.Panes = map[string]PaneRecord{}
	}
	return &st, nil
}

func (s *Store) acquireLock() error {
	s.mu.Lock()
	deadline := time.Now().Add(LockTimeout)
	for {
		if err := os.Mkdir(s.lockDir, 0o700); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("state: lock timeout")
		}
		// Check for stale lock.
		if info, err := os.Stat(s.lockDir); err == nil {
			if time.Since(info.ModTime()) > LockStaleAge {
				_ = os.RemoveAll(s.lockDir)
				continue
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (s *Store) releaseLock() {
	_ = os.RemoveAll(s.lockDir)
	s.mu.Unlock()
}

func (s *Store) writeAtomic(data []byte) error {
	tmp := filepath.Join(s.dir, ".panes.json.tmp."+strconv.Itoa(os.Getpid()))
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("state: write tmp: %w", err)
	}
	if err := os.Rename(tmp, s.file); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("state: rename: %w", err)
	}
	return nil
}

func (s *Store) save(st *State) error {
	data, err := json.Marshal(st)
	if err != nil {
		return fmt.Errorf("state: marshal: %w", err)
	}
	return s.writeAtomic(data)
}

// mutate loads, applies fn, and saves atomically under lock.
func (s *Store) mutate(fn func(*State)) error {
	if err := s.acquireLock(); err != nil {
		return err
	}
	defer s.releaseLock()
	st, err := s.Load()
	if err != nil {
		return err
	}
	fn(st)
	return s.save(st)
}

// SetPane writes a pane record. Validates status and pane ID.
// Notification statuses (waiting/blocked/completed) auto-set Acknowledged=false
// to trigger a bell. "working" auto-sets Acknowledged=true (lifecycle, no bell).
func (s *Store) SetPane(pane string, rec PaneRecord) error {
	if pane == "" {
		return fmt.Errorf("state: pane is required")
	}
	if !validStatus(rec.Status) {
		return fmt.Errorf("state: unsupported status: %s", rec.Status)
	}
	// Auto-manage acknowledged flag based on status transition.
	// Notification statuses reset the bell; working clears it.
	if IsNotificationStatus(rec.Status) {
		rec.Acknowledged = false
	} else {
		rec.Acknowledged = true
	}
	return s.mutate(func(st *State) {
		if st.Panes == nil {
			st.Panes = map[string]PaneRecord{}
		}
		st.Panes[pane] = rec
	})
}

// DelPane removes a pane record.
func (s *Store) DelPane(pane string) error {
	return s.mutate(func(st *State) {
		delete(st.Panes, pane)
	})
}

// SetLast records the last completed pane.
func (s *Store) SetLast(rec LastCompleted) error {
	if rec.Pane == "" {
		return fmt.Errorf("state: pane is required")
	}
	return s.mutate(func(st *State) {
		st.LastCompleted = &rec
	})
}

// Cleanup removes expired completed records and panes not in the live set.
// livePanes can be nil to skip the live-pane check.
func (s *Store) Cleanup(now int64, ttl int64, livePanes []string) {
	liveSet := map[string]bool{}
	for _, p := range livePanes {
		liveSet[p] = true
	}
	_ = s.mutate(func(st *State) {
		for key, rec := range st.Panes {
			if rec.Status == "completed" && now-rec.Time > ttl {
				delete(st.Panes, key)
				continue
			}
			if livePanes != nil && !liveSet[key] {
				delete(st.Panes, key)
			}
		}
	})
}

// Acknowledge marks a pane's notification as acknowledged (bell cleared).
func (s *Store) Acknowledge(pane string) error {
	if pane == "" {
		return fmt.Errorf("state: pane is required")
	}
	return s.mutate(func(st *State) {
		if rec, ok := st.Panes[pane]; ok {
			rec.Acknowledged = true
			st.Panes[pane] = rec
		}
	})
}

// PendingNotification represents an unacknowledged notification pane.
type PendingNotification struct {
	Pane    string
	Session string
	Window  string
	Status  string
	Summary string
	Time    int64
}

// PendingNotifications returns all unacknowledged notification panes,
// sorted by time ascending (oldest first).
func (s *Store) PendingNotifications() []PendingNotification {
	st, err := s.Load()
	if err != nil {
		return nil
	}
	var result []PendingNotification
	for pane, rec := range st.Panes {
		if IsNotificationStatus(rec.Status) && !rec.Acknowledged {
			result = append(result, PendingNotification{
				Pane:    pane,
				Session: rec.Session,
				Window:  rec.Window,
				Status:  rec.Status,
				Summary: rec.Summary,
				Time:    rec.Time,
			})
		}
	}
	// Sort by time ascending (oldest first), with pane ID as stable tie-break.
	for i := 0; i < len(result); i++ {
		for j := i + 1; j < len(result); j++ {
			if result[j].Time < result[i].Time ||
				(result[j].Time == result[i].Time && result[j].Pane < result[i].Pane) {
				result[i], result[j] = result[j], result[i]
			}
		}
	}
	return result
}

// Reset clears all state to the default empty document.
func (s *Store) Reset() error {
	if err := s.acquireLock(); err != nil {
		return err
	}
	defer s.releaseLock()
	return s.save(defaultState())
}

var (
	reCodeBlock    = regexp.MustCompile("(?s)```[a-zA-Z0-9_-]*\n?(.*?)```\n?")
	reInlineCode   = regexp.MustCompile("`([^`\n]+)`")
	reLeftoverTick = regexp.MustCompile("`+")
	reImg          = regexp.MustCompile(`!\[([^\]]*)\]\([^)]*\)`)
	reLink         = regexp.MustCompile(`\[([^\]]+)\]\([^)]*\)`)
	reRefLink      = regexp.MustCompile(`\[([^\]]+)\]\[[^\]]*\]`)
	reHTML         = regexp.MustCompile(`<[^>]+>`)
	reHeader       = regexp.MustCompile(`(?m)^[ \t]*#{1,6}[ \t]*`)
	reQuote        = regexp.MustCompile(`(?m)^[ \t]*>[ \t]*`)
	reListBullet   = regexp.MustCompile(`(?m)^[ \t]*[-*+][ \t]+`)
	reListNum      = regexp.MustCompile(`(?m)^[ \t]*\d+\.[ \t]+`)
	reHR           = regexp.MustCompile(`(?m)^[ \t]*[-*_]{3,}[ \t]*$`)
	reBoldStar     = regexp.MustCompile(`\*{2,}(.*?)\*{2,}`)
	reBoldUnder    = regexp.MustCompile(`_{2,}(.*?)_{2,}`)
	reItalicStar   = regexp.MustCompile(`\*(.*?)\*`)
	reStrike       = regexp.MustCompile(`~~(.*?)~~`)
)

// StripMarkdown removes common Markdown formatting syntax from a string.
func StripMarkdown(s string) string {
	s = reCodeBlock.ReplaceAllString(s, "$1")
	s = reImg.ReplaceAllString(s, "$1")
	s = reLink.ReplaceAllString(s, "$1")
	s = reRefLink.ReplaceAllString(s, "$1")
	s = reHTML.ReplaceAllString(s, "")
	s = reHeader.ReplaceAllString(s, "")
	s = reQuote.ReplaceAllString(s, "")
	s = reListBullet.ReplaceAllString(s, "")
	s = reListNum.ReplaceAllString(s, "")
	s = reHR.ReplaceAllString(s, "")
	s = reBoldStar.ReplaceAllString(s, "$1")
	s = reBoldUnder.ReplaceAllString(s, "$1")
	s = reItalicStar.ReplaceAllString(s, "$1")
	s = reStrike.ReplaceAllString(s, "$1")
	s = reInlineCode.ReplaceAllString(s, "$1")
	s = reLeftoverTick.ReplaceAllString(s, "")
	return s
}

// SanitizeSummary collapses whitespace and truncates to 80 characters.
func SanitizeSummary(s string) string {
	s = StripMarkdown(s)
	var out []rune
	prevSpace := false
	for _, r := range s {
		switch r {
		case '\r', '\n', '\t', ' ':
			if !prevSpace && len(out) > 0 {
				out = append(out, ' ')
				prevSpace = true
			}
		default:
			out = append(out, r)
			prevSpace = false
		}
	}
	for len(out) > 0 && out[len(out)-1] == ' ' {
		out = out[:len(out)-1]
	}
	if len(out) > 80 {
		out = out[:80]
	}
	return string(out)
}
