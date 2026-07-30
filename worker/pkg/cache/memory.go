package cache

import (
	"context"
	"sync"
	"time"
)

// NewMemory creates a cache client backed by an in-process map instead of Valkey,
// implementing the same read-through/invalidate contract as New.
//
// It exists for tests in other packages, which cannot build a Client themselves because
// its commander is unexported. Production code uses New: nothing here bounds the map, and
// an expired entry is only evicted when that same key is read again, so it is not a
// substitute for a real cache in a long-running process.
func NewMemory(timeout time.Duration) *Client {
	return &Client{commands: &memoryCommander{entries: map[string]memoryEntry{}}, timeout: timeout}
}

type memoryEntry struct {
	value   []byte
	expires time.Time
}

// memoryCommander is an in-process commander with no Valkey dependency.
type memoryCommander struct {
	mu      sync.Mutex
	entries map[string]memoryEntry
}

func (m *memoryCommander) get(_ context.Context, key string) ([]byte, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, found := m.entries[key]
	if !found {
		return nil, false, nil
	}
	if !entry.expires.IsZero() && time.Now().After(entry.expires) {
		delete(m.entries, key)
		return nil, false, nil
	}
	return entry.value, true, nil
}

func (m *memoryCommander) set(_ context.Context, key string, value []byte, ttl time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry := memoryEntry{value: value}
	if ttl > 0 {
		entry.expires = time.Now().Add(ttl)
	}
	m.entries[key] = entry
	return nil
}

func (m *memoryCommander) del(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.entries, key)
	return nil
}
