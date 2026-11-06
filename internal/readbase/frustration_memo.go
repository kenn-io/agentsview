package readbase

import "sync"

// FrustrationMarkerMemo remembers each session's marker count at a content version.
type FrustrationMarkerMemo[V comparable] struct {
	mu     sync.Mutex
	counts map[string]frustrationMarkerEntry[V]
}

type frustrationMarkerEntry[V comparable] struct {
	version V
	count   int
}

func (m *FrustrationMarkerMemo[V]) Lookup(id string, version V) (int, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, ok := m.counts[id]
	if !ok || entry.version != version {
		return 0, false
	}
	return entry.count, true
}

func (m *FrustrationMarkerMemo[V]) Store(id string, version V, count int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.counts == nil {
		m.counts = make(map[string]frustrationMarkerEntry[V])
	}
	m.counts[id] = frustrationMarkerEntry[V]{version: version, count: count}
}
