package db

import "sync"

// FrustrationMarkerMemo reuses counts while a session's transcript is unchanged.
// Backends whose handle swaps can change prompt text use it to reject stale
// counts, even when a replacement preserves session revisions.
type FrustrationMarkerMemo struct {
	mu         sync.Mutex
	generation uint64
	counts     map[string]frustrationMarkerEntry
}

type frustrationMarkerEntry struct {
	version string
	count   int
}

func (m *FrustrationMarkerMemo) Generation() uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.generation
}

func (m *FrustrationMarkerMemo) Lookup(generation uint64, id, version string) (int, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, ok := m.counts[id]
	if !ok || generation != m.generation || entry.version != version {
		return 0, false
	}
	return entry.count, true
}

func (m *FrustrationMarkerMemo) Store(generation uint64, id, version string, count int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if generation != m.generation {
		return
	}
	if m.counts == nil {
		m.counts = make(map[string]frustrationMarkerEntry)
	}
	m.counts[id] = frustrationMarkerEntry{version: version, count: count}
}

func (m *FrustrationMarkerMemo) Swap(swap func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	swap()
	m.generation++
	m.counts = nil
}
