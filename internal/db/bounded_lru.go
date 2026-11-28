package db

import "github.com/hashicorp/golang-lru/v2/simplelru"

// boundedLRU requires its caller to hold a lock.
type boundedLRU[K comparable, V any] struct {
	entries *simplelru.LRU[K, lruEntry[V]]
	bytes   int64
}

type lruEntry[V any] struct {
	value V
	bytes int64
}

func (l *boundedLRU[K, V]) reset() {
	l.entries = nil
	l.bytes = 0
}

func (l *boundedLRU[K, V]) Len() int {
	if l.entries == nil {
		return 0
	}
	return l.entries.Len()
}

func (l *boundedLRU[K, V]) get(key K) (V, bool) {
	var zero V
	if l.entries == nil {
		return zero, false
	}
	entry, ok := l.entries.Get(key)
	return entry.value, ok
}

func (l *boundedLRU[K, V]) peek(key K) (V, bool) {
	var zero V
	if l.entries == nil {
		return zero, false
	}
	entry, ok := l.entries.Peek(key)
	return entry.value, ok
}

func (l *boundedLRU[K, V]) keys() []K {
	if l.entries == nil {
		return nil
	}
	return l.entries.Keys()
}

func (l *boundedLRU[K, V]) remove(key K) {
	if l.entries == nil {
		return
	}
	if entry, ok := l.entries.Peek(key); ok {
		l.entries.Remove(key)
		l.bytes -= entry.bytes
	}
}

func (l *boundedLRU[K, V]) add(key K, value V, size int64) {
	if l.entries == nil {
		entries, err := simplelru.NewLRU[K, lruEntry[V]](int(^uint(0)>>1), nil)
		if err != nil {
			panic(err)
		}
		l.entries = entries
	}
	if old, ok := l.entries.Peek(key); ok {
		l.bytes -= old.bytes
	}
	l.entries.Add(key, lruEntry[V]{value: value, bytes: size})
	l.bytes += size
}

func (l *boundedLRU[K, V]) trim(limit int64) {
	for l.bytes > limit {
		_, entry, ok := l.entries.RemoveOldest()
		if !ok {
			return
		}
		l.bytes -= entry.bytes
	}
}
