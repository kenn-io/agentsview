package db

import "sync"

// frustrationMarkerMemo reuses counts while a session's transcript is unchanged.
// Reader swaps clear it because archive replacement can remove prompt text while
// preserving session revisions.
type frustrationMarkerMemo struct {
	mu         sync.Mutex
	generation uint64
	counts     map[string]frustrationMarkerEntry
}

type frustrationMarkerEntry struct {
	version string
	count   int
}

func (m *frustrationMarkerMemo) lookup(generation uint64, id, version string) (int, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, ok := m.counts[id]
	if !ok || generation != m.generation || entry.version != version {
		return 0, false
	}
	return entry.count, true
}

func (m *frustrationMarkerMemo) store(generation uint64, id, version string, count int) {
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

func (m *frustrationMarkerMemo) swap(swap func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	swap()
	m.generation++
	m.counts = nil
}
