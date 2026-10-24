package main

import (
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"
)

type notice struct {
	Unit      int
	Name      string
	Received  time.Time
	Operation fsnotify.Op
}
type nativeReport struct {
	ElapsedS                                                   float64
	QueueDepth                                                 int
	Allocated, Unavailable, BudgetExcluded, AllocationFailures int
	Received, Dropped, Errors, Observed                        int64
	MaxQueue                                                   int
	Operations                                                 nativeOperations
	PendingLossUnits                                           int
	OldestPendingLossSeconds                                   float64
	VerifiedEqualSignature, VerifiedChangedSignature           int64
}
type nativeOperations struct{ Write, Create, Rename, Remove, Chmod int64 }

type nativeSource struct {
	watcher                                                    *fsnotify.Watcher
	queue                                                      chan notice
	loss                                                       []atomic.Int64
	started                                                    time.Time
	operations                                                 [5]atomic.Int64
	received, dropped, errors                                  atomic.Int64
	wg                                                         sync.WaitGroup
	allocated, unavailable, budgetExcluded, allocationFailures int
	peakQueue                                                  atomic.Int64
	watched                                                    []bool
}

func startNative(dirs []string, maxWatches, queueSize int) (*nativeSource, error) {
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	n := &nativeSource{watcher: w, queue: make(chan notice, queueSize), loss: make([]atomic.Int64, len(dirs)), started: time.Now(), watched: make([]bool, len(dirs))}
	owners := make(map[string]int, len(dirs))
	for i, d := range dirs {
		owners[d] = i
		if n.allocated >= maxWatches {
			n.unavailable++
			n.budgetExcluded++
			continue
		}
		if err = w.Add(d); err != nil {
			n.unavailable++
			n.allocationFailures++
		} else {
			n.allocated++
			n.watched[i] = true
		}
	}
	n.wg.Go(func() {
		for {
			select {
			case e, ok := <-w.Events:
				if !ok {
					return
				}
				id, ok := owners[filepath.Dir(e.Name)]
				if !ok || !strings.HasSuffix(e.Name, ".jsonl") {
					continue
				}
				n.offer(notice{Unit: id, Name: filepath.Base(e.Name), Received: time.Now(), Operation: e.Op})
			case _, ok := <-w.Errors:
				if !ok {
					return
				}
				n.errors.Add(1)
				for i := range n.loss {
					if n.watched[i] {
						n.markLoss(i)
					}
				}
			}
		}
	})
	return n, nil
}
func (n *nativeSource) close() error { err := n.watcher.Close(); n.wg.Wait(); return err }

// offer never waits for scanning or telemetry and preserves the first loss time.
func (n *nativeSource) offer(item notice) {
	n.received.Add(1)
	for i, op := range [...]fsnotify.Op{fsnotify.Write, fsnotify.Create, fsnotify.Rename, fsnotify.Remove, fsnotify.Chmod} {
		if item.Operation.Has(op) {
			n.operations[i].Add(1)
		}
	}
	select {
	case n.queue <- item:
		size := int64(len(n.queue))
		for old := n.peakQueue.Load(); size > old; old = n.peakQueue.Load() {
			if n.peakQueue.CompareAndSwap(old, size) {
				break
			}
		}
	default:
		n.dropped.Add(1)
		n.markLoss(item.Unit)
	}
}

func (n *nativeSource) markLoss(unit int) {
	n.loss[unit].CompareAndSwap(0, time.Since(n.started).Nanoseconds()+1)
}

func (n *nativeSource) snapshot() nativeReport {
	r := nativeReport{ElapsedS: time.Since(n.started).Seconds(), QueueDepth: len(n.queue), Allocated: n.allocated, Unavailable: n.unavailable, BudgetExcluded: n.budgetExcluded, AllocationFailures: n.allocationFailures, Received: n.received.Load(), Dropped: n.dropped.Load(), Errors: n.errors.Load(), MaxQueue: int(n.peakQueue.Load())}
	r.Operations = nativeOperations{n.operations[0].Load(), n.operations[1].Load(), n.operations[2].Load(), n.operations[3].Load(), n.operations[4].Load()}
	for i := range n.loss {
		if stamp := n.loss[i].Load(); stamp != 0 {
			r.PendingLossUnits++
			r.OldestPendingLossSeconds = max(r.OldestPendingLossSeconds, (time.Since(n.started) - time.Duration(stamp-1)).Seconds())
		}
	}
	return r
}
