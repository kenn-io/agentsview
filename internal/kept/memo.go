package kept

import (
	"cmp"
	"slices"
	"sync"
)

// Memo keeps the results of recent reads. A slot names the read
// and holds only the rows of its latest version, which names the rows the
// read sees. A push replaces a slot's rows instead of adding a
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
	// zero meaning DefaultMaxBytes. Put drops rows over the cap; PutWithFallback
	// can keep a smaller equivalent form. Without Size only slots are capped.
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
	// DefaultMaxBytes caps the bytes kept when MaxBytes is zero.
	DefaultMaxBytes = 64 << 20
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
	m.put(slot, version, rows, nil)
}

// PutWithFallback keeps a smaller equivalent form when rows or the combined
// entries exceed the byte cap. fallback receives a copy of one slot's rows
// and its estimated size, then returns the replacement rows and their size.
// It runs under the memo lock and should be cheap. Existing slots are reduced
// oldest first; any slots that still do not fit are evicted oldest first.
func (m *Memo[T]) PutWithFallback(
	slot, version string, rows []T,
	fallback func(rows []T, size int64) ([]T, int64),
) {
	m.put(slot, version, rows, fallback)
}

func (m *Memo[T]) put(
	slot, version string, rows []T,
	fallback func(rows []T, size int64) ([]T, int64),
) {
	if slot == "" {
		return
	}
	size := m.rowsSize(rows)
	maxBytes := cmp.Or(m.MaxBytes, DefaultMaxBytes)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.removeLocked(slot)
	reduced := false
	if m.Size != nil && size > maxBytes {
		if fallback == nil {
			return
		}
		rows, size = fallback(slices.Clone(rows), size)
		reduced = true
		if size > maxBytes {
			return
		}
	}
	if m.entries == nil {
		m.entries = map[string]memoEntry[T]{}
	}
	m.entries[slot] = memoEntry[T]{version: version, rows: rows, bytes: size}
	m.order = append(m.order, slot)
	m.bytes += size
	for len(m.order) > cmp.Or(m.Limit, memoLimit) {
		m.removeLocked(m.order[0])
	}
	if m.Size != nil && fallback != nil && m.bytes > maxBytes {
		for _, retained := range slices.Clone(m.order) {
			if m.bytes <= maxBytes {
				break
			}
			if retained == slot && reduced {
				continue
			}
			entry := m.entries[retained]
			oldSize := entry.bytes
			entry.rows, entry.bytes = fallback(slices.Clone(entry.rows), entry.bytes)
			m.bytes += entry.bytes - oldSize
			m.entries[retained] = entry
		}
	}
	for m.bytes > maxBytes {
		m.removeLocked(m.order[0])
	}
}

func (m *Memo[T]) rowsSize(rows []T) int64 {
	var size int64
	if m.Size != nil {
		for _, row := range rows {
			size += m.Size(row)
		}
	}
	return size
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
