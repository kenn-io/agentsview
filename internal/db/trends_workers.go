package db

import (
	"context"
	"runtime"
	"sync"
	"time"
)

type trendWork struct {
	content string
	local   time.Time
}

// TrendWorkers counts independent messages while the caller reads rows.
type TrendWorkers struct {
	ctx     context.Context
	jobs    chan []trendWork
	batch   []trendWork
	workers []*TrendAccumulator
	wait    sync.WaitGroup
}

func NewTrendWorkers(ctx context.Context, from, to, granularity string, terms []TrendTermInput) *TrendWorkers {
	n := max(1, min(4, runtime.GOMAXPROCS(0)-1))
	w := &TrendWorkers{ctx: ctx, jobs: make(chan []trendWork, n), workers: make([]*TrendAccumulator, n)}
	for i := range w.workers {
		acc := NewTrendAccumulator(from, to, granularity, terms)
		w.workers[i] = acc
		w.wait.Go(func() {
			for batch := range w.jobs {
				for _, row := range batch {
					if ctx.Err() != nil {
						return
					}
					acc.Add(row.content, row.local)
				}
			}
		})
	}
	return w
}

func (w *TrendWorkers) Add(content string, local time.Time) {
	w.batch = append(w.batch, trendWork{content, local})
	if len(w.batch) == 64 {
		w.flush()
	}
}

func (w *TrendWorkers) flush() {
	if len(w.batch) == 0 {
		return
	}
	select {
	case w.jobs <- w.batch:
	case <-w.ctx.Done():
	}
	w.batch = nil
}

// Finish waits for workers and combines their independent counts.
func (w *TrendWorkers) Finish() (TrendsTermsResponse, error) {
	w.flush()
	close(w.jobs)
	w.wait.Wait()
	if err := w.ctx.Err(); err != nil {
		return TrendsTermsResponse{}, err
	}
	result := w.workers[0]
	for _, other := range w.workers[1:] {
		for i := range result.messageCounts {
			result.messageCounts[i] += other.messageCounts[i]
		}
		for i := range result.counts {
			for j := range result.counts[i] {
				result.counts[i][j] += other.counts[i][j]
			}
		}
	}
	return result.Response(), nil
}
