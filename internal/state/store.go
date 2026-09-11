// Package state persists which issues have been processed, so the agent does
// not redo work across restarts. It is a single JSON file written atomically.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Status of an issue run.
type Status string

const (
	StatusDone   Status = "done"
	StatusFailed Status = "failed"
)

// Entry records the outcome of one issue run.
type Entry struct {
	UpdatedAt   string    `json:"updated_at"`
	Status      Status    `json:"status"`
	ProcessedAt time.Time `json:"processed_at"`
	Error       string    `json:"error,omitempty"`
	Attempts    int       `json:"attempts"`
}

// Store is a concurrency-safe file-backed map.
type Store struct {
	path string
	mu   sync.Mutex
	data map[string]Entry
}

// Key builds the canonical key for an issue.
func Key(repo string, number int) string { return fmt.Sprintf("%s#%d", repo, number) }

// Open loads path, or starts empty when it does not exist. An empty path gives
// an in-memory store.
func Open(path string) (*Store, error) {
	s := &Store{path: path, data: map[string]Entry{}}
	if path == "" {
		return s, nil
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 {
		return s, nil
	}
	if err := json.Unmarshal(raw, &s.data); err != nil {
		return nil, fmt.Errorf("decode state %s: %w", path, err)
	}
	return s, nil
}

// Get returns the entry for key.
func (s *Store) Get(key string) (Entry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.data[key]
	return e, ok
}

// Set records e under key and flushes to disk.
func (s *Store) Set(key string, e Entry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	prev := s.data[key]
	e.Attempts = prev.Attempts + 1
	if e.ProcessedAt.IsZero() {
		e.ProcessedAt = time.Now().UTC()
	}
	s.data[key] = e
	return s.flushLocked()
}

// Len returns the number of entries.
func (s *Store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.data)
}

func (s *Store) flushLocked() error {
	if s.path == "" {
		return nil
	}
	raw, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
