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
	rows      []dailyUsageScanRow
	snapshots [][]dailyUsageSnapshot
	order     []int
}

type dailyUsageSnapshot struct {
	projection bunDailyUsageProjection
	selection  activity.ClaudeSnapshotSelection
	position   int
}

// Fixed-size blocks avoid copying large projections when the distinct request
// count grows. Unlike reserving the candidate count, duplicate snapshots do not
// enlarge this storage.
const usageSnapshotBlockSize = 1024

func (a *usageReadArena) snapshot(index int) *dailyUsageSnapshot {
	block := index / usageSnapshotBlockSize
	if block == len(a.snapshots) {
		a.snapshots = append(a.snapshots, make([]dailyUsageSnapshot, usageSnapshotBlockSize))
	}
	return &a.snapshots[block][index%usageSnapshotBlockSize]
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
		uintptr(len(a.snapshots)*usageSnapshotBlockSize)*reflect.TypeFor[dailyUsageSnapshot]().Size() +
		uintptr(cap(a.order))*reflect.TypeFor[int]().Size()
	if bytes > retainedBytes {
		a.rows = nil
		a.snapshots = nil
		a.order = nil
	} else {
		clear(a.rows[:cap(a.rows)])
		a.rows = a.rows[:0]
		for _, block := range a.snapshots {
			clear(block)
		}
		a.order = a.order[:0]
	}
	usageReadArenaPool.Put(a)
}
