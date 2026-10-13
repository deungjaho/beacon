package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"
)

// ResourceRecord is a pane-owned resource (browser tab, dev server, pick,
// trace) reported by an external producer (argus, devproxy, ...).
// Beacon stores and lists them; lifecycle actions stay with the producer.
type ResourceRecord struct {
	Type      string         `json:"type"`
	ID        string         `json:"id"`
	OwnerPane string         `json:"owner_pane"`
	Producer  string         `json:"producer"`
	Metadata  map[string]any `json:"metadata,omitempty"`
	CreatedAt int64          `json:"created_at"`
	LastSeen  int64          `json:"last_seen"`
	Stale     bool           `json:"stale,omitempty"`
}

// RegistryStore manages resources.json with the same mkdir-lock + atomic
// write discipline as Store.
type RegistryStore struct {
	dir     string
	file    string
	lockDir string
	mu      sync.Mutex
	now     func() int64
}

func NewRegistryStore(dir string) (*RegistryStore, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("registry: mkdir: %w", err)
	}
	return &RegistryStore{
		dir:     dir,
		file:    filepath.Join(dir, "resources.json"),
		lockDir: filepath.Join(dir, ".resources.lock"),
		now:     func() int64 { return time.Now().Unix() },
	}, nil
}

func (r *RegistryStore) SetNow(fn func() int64) { r.now = fn }

func resourceKey(t, id string) string { return t + "\x00" + id }

func (r *RegistryStore) loadAll() (map[string]ResourceRecord, error) {
	data, err := os.ReadFile(r.file)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]ResourceRecord{}, nil
		}
		return nil, fmt.Errorf("registry: read: %w", err)
	}
	var m map[string]ResourceRecord
	if err := json.Unmarshal(data, &m); err != nil || m == nil {
		return map[string]ResourceRecord{}, nil
	}
	return m, nil
}

func (r *RegistryStore) acquireLock() error {
	r.mu.Lock()
	deadline := time.Now().Add(LockTimeout)
	for {
		if err := os.Mkdir(r.lockDir, 0o700); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("registry: lock timeout")
		}
		if info, err := os.Stat(r.lockDir); err == nil {
			if time.Since(info.ModTime()) > LockStaleAge {
				_ = os.RemoveAll(r.lockDir)
				continue
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (r *RegistryStore) releaseLock() {
	_ = os.RemoveAll(r.lockDir)
	r.mu.Unlock()
}

func (r *RegistryStore) saveAll(m map[string]ResourceRecord) error {
	data, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("registry: marshal: %w", err)
	}
	tmp := filepath.Join(r.dir, ".resources.json.tmp."+strconv.Itoa(os.Getpid()))
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("registry: write tmp: %w", err)
	}
	if err := os.Rename(tmp, r.file); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("registry: rename: %w", err)
	}
	return nil
}

func (r *RegistryStore) mutate(fn func(map[string]ResourceRecord)) error {
	if err := r.acquireLock(); err != nil {
		return err
	}
	defer r.releaseLock()
	m, err := r.loadAll()
	if err != nil {
		return err
	}
	fn(m)
	return r.saveAll(m)
}

// Upsert registers or refreshes a resource. Re-registering an existing
// (type,id) keeps the original CreatedAt and updates LastSeen/metadata.
func (r *RegistryStore) Upsert(rec ResourceRecord) error {
	if rec.Type == "" || rec.ID == "" {
		return fmt.Errorf("registry: type and id are required")
	}
	return r.mutate(func(m map[string]ResourceRecord) {
		key := resourceKey(rec.Type, rec.ID)
		if old, ok := m[key]; ok {
			rec.CreatedAt = old.CreatedAt
		}
		if rec.CreatedAt == 0 {
			rec.CreatedAt = r.now()
		}
		if rec.LastSeen == 0 {
			rec.LastSeen = r.now()
		}
		rec.Stale = false
		m[key] = rec
	})
}

func (r *RegistryStore) Remove(t, id string) error {
	return r.mutate(func(m map[string]ResourceRecord) {
		delete(m, resourceKey(t, id))
	})
}

// List returns resources filtered by type/pane/stale, sorted by CreatedAt.
func (r *RegistryStore) List(t, pane string, staleOnly bool) []ResourceRecord {
	m, err := r.loadAll()
	if err != nil {
		return nil
	}
	var out []ResourceRecord
	for _, rec := range m {
		if t != "" && rec.Type != t {
			continue
		}
		if pane != "" && rec.OwnerPane != pane {
			continue
		}
		if staleOnly && !rec.Stale {
			continue
		}
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt != out[j].CreatedAt {
			return out[i].CreatedAt < out[j].CreatedAt
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// MarkStaleForDeadPanes flags every resource whose owner pane is no longer
// live. Producers reap the underlying artifacts on their next call.
func (r *RegistryStore) MarkStaleForDeadPanes(livePanes []string) {
	if livePanes == nil {
		return
	}
	live := map[string]bool{}
	for _, p := range livePanes {
		live[p] = true
	}
	_ = r.mutate(func(m map[string]ResourceRecord) {
		for k, rec := range m {
			if rec.OwnerPane != "" && !live[rec.OwnerPane] {
				rec.Stale = true
				m[k] = rec
			}
		}
	})
}
