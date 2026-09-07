package db

import (
	"reflect"
	"sync"
)

// One request owns surviving rows through reduction. Grouped snapshot reads
// keep only one winner and attribution row outside this reusable storage.
// Public results never borrow its backing array.
type usageReadArena struct {
	rows []dailyUsageScanRow
}

var usageReadArenaPool = sync.Pool{New: func() any {
	return new(usageReadArena)
}}

func (a *usageReadArena) release() {
	// Clear transcript and session references. The cap is per arena; the Go
	// collector may also discard idle entries independently of this limit.
	const retainedBytes = 128 << 20
	if uintptr(cap(a.rows))*reflect.TypeFor[dailyUsageScanRow]().Size() > retainedBytes {
		a.rows = nil
	} else {
		clear(a.rows[:cap(a.rows)])
		a.rows = a.rows[:0]
	}
	usageReadArenaPool.Put(a)
}
