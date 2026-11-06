package kept

import (
	"cmp"
	"slices"
	"sync"
)

// Memo keeps the results of recent reads. A slot names the read
// and holds only the rows of its latest version, which names the parts the
// rows were read from. A push replaces a slot's rows instead of adding a
// second copy beside them, and a stale version is never returned. The
// empty slot keeps nothing. The oldest written slots go first when the
// memo is over its slot or byte limit.
type Memo[T any] struct {
	mu      sync.Mutex
	entries map[string]memoEntry[T]
	order   []string
	// Limit caps the slots; zero means memoLimit.
	Limit int
	// Size estimates a row's bytes. With it, MaxBytes caps the bytes kept,
	// zero meaning memoBytes, and rows over that cap are not kept.
	// Without it only the slots are capped.
	Size     func(T) int64
	MaxBytes int64
	bytes    int64
}

type memoEntry[T any] struct {
	version string
	rows    []T
	bytes   int64
}

const (
	memoLimit = 64
	memoBytes = 64 << 20
)

func (m *Memo[T]) Get(slot, version string) ([]T, bool) {
	if slot == "" {
		return nil, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, ok := m.entries[slot]
	if !ok || entry.version != version {
		return nil, false
	}
	return entry.rows, true
}

func (m *Memo[T]) Put(slot, version string, rows []T) {
	if slot == "" {
		return
	}
	var size int64
	if m.Size != nil {
		for _, row := range rows {
			size += m.Size(row)
		}
	}
	maxBytes := cmp.Or(m.MaxBytes, memoBytes)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.removeLocked(slot)
	if m.Size != nil && size > maxBytes {
		return
	}
	if m.entries == nil {
		m.entries = map[string]memoEntry[T]{}
	}
	m.entries[slot] = memoEntry[T]{version: version, rows: rows, bytes: size}
	m.order = append(m.order, slot)
	m.bytes += size
	for len(m.order) > cmp.Or(m.Limit, memoLimit) || m.bytes > maxBytes {
		m.removeLocked(m.order[0])
	}
}

// DeleteSlots removes the slots drop reports.
func (m *Memo[T]) DeleteSlots(drop func(slot string) bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, slot := range slices.Clone(m.order) {
		if drop(slot) {
			m.removeLocked(slot)
		}
	}
}

func (m *Memo[T]) removeLocked(slot string) {
	entry, ok := m.entries[slot]
	if !ok {
		return
	}
	delete(m.entries, slot)
	m.order = slices.DeleteFunc(m.order, func(s string) bool { return s == slot })
	m.bytes -= entry.bytes
}
