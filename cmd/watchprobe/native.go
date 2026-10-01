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
	Unit     int
	Name     string
	Received time.Time
}
type nativeReport struct {
	Allocated, Unavailable, BudgetExcluded, AllocationFailures int
	Received, Dropped, Errors, Observed                        int64
	MaxQueue                                                   int
}
type nativeSource struct {
	watcher                                                    *fsnotify.Watcher
	queue                                                      chan notice
	loss                                                       []atomic.Bool
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
	n := &nativeSource{watcher: w, queue: make(chan notice, queueSize), loss: make([]atomic.Bool, len(dirs)), watched: make([]bool, len(dirs))}
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
				n.received.Add(1)
				item := notice{Unit: id, Name: filepath.Base(e.Name), Received: time.Now()}
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
					n.loss[id].Store(true)
				}
			case _, ok := <-w.Errors:
				if !ok {
					return
				}
				n.errors.Add(1)
				for i := range n.loss {
					if n.watched[i] {
						n.loss[i].Store(true)
					}
				}
			}
		}
	})
	return n, nil
}
func (n *nativeSource) close() error { err := n.watcher.Close(); n.wg.Wait(); return err }
