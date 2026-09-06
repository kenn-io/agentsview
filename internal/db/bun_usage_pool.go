package db

import (
	"reflect"
	"sync"

	"go.kenn.io/agentsview/internal/activity"
)

// One usage request owns its projections, snapshot-selection input and rows
// through reduction, then returns storage for opportunistic reuse.
// Its arrays never escape into returned usage results.
type usageReadArena struct {
	rows        []dailyUsageScanRow
	projections []bunDailyUsageProjection
	snapshots   []activity.UsageRow
}

var usageReadArenaPool = sync.Pool{New: func() any {
	return new(usageReadArena)
}}

func (a *usageReadArena) release() {
	// Keep no transcript, JSON or session references between reads. Large
	// all-history queries may use more space, but do not retain that backing
	// in the pool. The cap is per arena; GC may also discard idle pool entries.
	const retainedBytes = 128 << 20
	bytes := uintptr(cap(a.rows))*reflect.TypeFor[dailyUsageScanRow]().Size() +
		uintptr(cap(a.projections))*reflect.TypeFor[bunDailyUsageProjection]().Size() +
		uintptr(cap(a.snapshots))*reflect.TypeFor[activity.UsageRow]().Size()
	if bytes > retainedBytes {
		a.rows = nil
		a.projections = nil
		a.snapshots = nil
	} else {
		clear(a.rows[:cap(a.rows)])
		a.rows = a.rows[:0]
		clear(a.projections[:cap(a.projections)])
		clear(a.snapshots[:cap(a.snapshots)])
		a.projections = a.projections[:0]
		a.snapshots = a.snapshots[:0]
	}
	usageReadArenaPool.Put(a)
}
