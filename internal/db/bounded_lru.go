package db

import "container/list"

// boundedLRU requires its caller to hold a lock.
type boundedLRU[K comparable, V any] struct {
	entries map[K]*list.Element
	order   list.List
	bytes   int64
}

func (l *boundedLRU[K, V]) reset() {
	l.entries = nil
	l.order.Init()
	l.bytes = 0
}

func (l *boundedLRU[K, V]) remove(key K, size func(V) int64) {
	if old := l.entries[key]; old != nil {
		l.bytes -= size(old.Value.(V))
		l.order.Remove(old)
		delete(l.entries, key)
	}
}

func (l *boundedLRU[K, V]) add(key K, value V, size int64) {
	if l.entries == nil {
		l.entries = make(map[K]*list.Element)
	}
	l.entries[key] = l.order.PushBack(value)
	l.bytes += size
}

func (l *boundedLRU[K, V]) trim(limit int64, key func(V) K, size func(V) int64) {
	for l.bytes > limit {
		value := l.order.Front().Value.(V)
		l.remove(key(value), size)
	}
}
