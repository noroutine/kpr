package store

import (
	"context"
	"sync"
	"time"

	"nrtn.dev/catalyst/kpr/internal/policy"
)

// MemStore is the in-memory Store: hermetic tests for the sweeper,
// CLI, and console without a live redis. Not for production.
type MemStore struct {
	mu       sync.Mutex
	rows     map[string]policy.Row
	current  Current
	activity []Outcome
	locks    map[string]bool
	unlocked bool
	identity Identity
}

// NewMemStore builds an empty MemStore.
func NewMemStore() *MemStore {
	return &MemStore{rows: map[string]policy.Row{}, locks: map[string]bool{}}
}

func key(repo, tag string) string { return repo + "\x00" + tag }

func (m *MemStore) Ping(context.Context) error { return nil }

func (m *MemStore) Record(_ context.Context, r policy.Row) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := key(r.Repo, r.Tag)
	if old, ok := m.rows[k]; ok && !r.PushedAt.After(old.PushedAt) {
		r.Due, r.Reason = old.Due, old.Reason
	}
	m.rows[k] = r
	return nil
}

func (m *MemStore) All(context.Context) ([]policy.Row, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]policy.Row, 0, len(m.rows))
	for _, r := range m.rows {
		out = append(out, r)
	}
	return out, nil
}

func (m *MemStore) Due(context.Context) ([]policy.Row, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []policy.Row
	for _, r := range m.rows {
		if r.Due {
			out = append(out, r)
		}
	}
	return out, nil
}

func (m *MemStore) MarkDue(_ context.Context, repo, tag, reason string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.rows[key(repo, tag)]
	r.Repo, r.Tag = repo, tag
	r.Due, r.Reason = true, reason
	m.rows[key(repo, tag)] = r
	return nil
}

func (m *MemStore) ClearDue(context.Context) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for k, r := range m.rows {
		if r.Due {
			r.Due, r.Reason = false, ""
			m.rows[k] = r
			n++
		}
	}
	return n, nil
}

func (m *MemStore) UnmarkDue(_ context.Context, repo, tag string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rows[key(repo, tag)]
	if !ok || !r.Due {
		return false, nil
	}
	r.Due, r.Reason = false, ""
	m.rows[key(repo, tag)] = r
	return true, nil
}

func (m *MemStore) Delete(_ context.Context, repo, tag string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.rows, key(repo, tag))
	return nil
}

func (m *MemStore) SetCurrent(_ context.Context, c Current) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.current = c
	return nil
}

func (m *MemStore) GetCurrent(context.Context) (Current, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.current, nil
}

func (m *MemStore) IsUnlocked(context.Context) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.unlocked, nil
}

func (m *MemStore) SetUnlocked(_ context.Context, unlocked bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.unlocked = unlocked
	return nil
}

// GetIdentity returns the paired lineage: zero means unpaired.
func (m *MemStore) GetIdentity(context.Context) (Identity, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.identity, nil
}

// SetIdentity records the pairing outright.
func (m *MemStore) SetIdentity(_ context.Context, id Identity) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.identity = id
	return nil
}

func (m *MemStore) PushActivity(_ context.Context, o Outcome) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.activity = append([]Outcome{o}, m.activity...)
	if len(m.activity) > ActivityCap {
		m.activity = m.activity[:ActivityCap]
	}
	return nil
}

func (m *MemStore) Activity(context.Context) ([]Outcome, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Outcome(nil), m.activity...), nil
}

func (m *MemStore) AcquireLock(_ context.Context, name string, _ time.Duration) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.locks[name] {
		return false, nil
	}
	m.locks[name] = true
	return true, nil
}

func (m *MemStore) ReleaseLock(_ context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.locks, name)
	return nil
}
