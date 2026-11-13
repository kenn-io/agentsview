package db

import (
	"context"
	"runtime"
	"sync"
	"time"
)

type trendWork struct {
	sessionID string
	content   string
	local     time.Time
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
					if acc.parts != nil {
						acc.addPart(row)
					} else {
						acc.Add(row.content, row.local)
					}
				}
			}
		})
	}
	return w
}

// newTrendPartWorkers counts messages by session and local day.
func newTrendPartWorkers(ctx context.Context, terms []TrendTermInput) *TrendWorkers {
	w := NewTrendWorkers(ctx, "", "", "", terms)
	for _, acc := range w.workers {
		acc.parts = map[string]map[string]*trendDay{}
	}
	return w
}

func (w *TrendWorkers) Add(content string, local time.Time) {
	w.addSession("", content, local)
}

func (w *TrendWorkers) addSession(id, content string, local time.Time) {
	w.batch = append(w.batch, trendWork{id, content, local})
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

func (a *TrendAccumulator) bucket(local time.Time) (int, bool) {
	if !InDateRange(local.Format("2006-01-02"), a.from, a.to) {
		return 0, false
	}
	bucket, ok := a.index[TrendBucketDate(local, local.Location(), a.granularity)]
	return bucket, ok
}

// addDay adds a stored local day's counts to its bucket.
func (a *TrendAccumulator) addDay(local time.Time, day *trendDay) {
	bucket, ok := a.bucket(local)
	if !ok {
		return
	}
	a.messageCounts[bucket] += day.Messages
	for i, count := range day.Counts {
		if i < len(a.counts) {
			a.counts[i][bucket] += count
		}
	}
}

func (a *TrendAccumulator) addPart(row trendWork) {
	days := a.parts[row.sessionID]
	if days == nil {
		days = map[string]*trendDay{}
		a.parts[row.sessionID] = days
	}
	date := row.local.Format("2006-01-02")
	day := days[date]
	if day == nil {
		day = &trendDay{Counts: make([]int, len(a.matchers))}
		days[date] = day
	}
	day.Messages++
	a.lower = appendTrendLower(a.lower, row.content)
	for i, matchers := range a.matchers {
		day.Counts[i] += countTrendOccurrences(a.lower, matchers)
	}
}

// finishParts waits for workers and merges their session days.
func (w *TrendWorkers) finishParts() (map[string]map[string]*trendDay, error) {
	w.flush()
	close(w.jobs)
	w.wait.Wait()
	if err := w.ctx.Err(); err != nil {
		return nil, err
	}
	result := w.workers[0].parts
	for _, other := range w.workers[1:] {
		for id, days := range other.parts {
			target := result[id]
			if target == nil {
				result[id] = days
				continue
			}
			for date, day := range days {
				dst := target[date]
				if dst == nil {
					target[date] = day
					continue
				}
				dst.Messages += day.Messages
				for i, count := range day.Counts {
					dst.Counts[i] += count
				}
			}
		}
	}
	return result, nil
}
