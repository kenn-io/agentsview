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
					acc.Add(row.content, row.local)
				}
			}
		})
	}
	return w
}

func (w *TrendWorkers) Add(content string, local time.Time) {
	w.batch = append(w.batch, trendWork{content: content, local: local})
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

// TrendPartWorkers counts messages by session and local day.
type TrendPartWorkers struct {
	ctx   context.Context
	jobs  chan []trendWork
	batch []trendWork
	parts []map[string]map[string]*trendDay
	wait  sync.WaitGroup
}

func newTrendPartWorkers(ctx context.Context, terms []TrendTermInput) *TrendPartWorkers {
	n := max(1, min(4, runtime.GOMAXPROCS(0)-1))
	w := &TrendPartWorkers{ctx: ctx, jobs: make(chan []trendWork, n), parts: make([]map[string]map[string]*trendDay, n)}
	for i := range w.parts {
		days := map[string]map[string]*trendDay{}
		w.parts[i] = days
		matchers := make([][]trendMatcher, len(terms))
		for j, term := range terms {
			matchers[j] = prepareTrendMatchers(term)
		}
		w.wait.Go(func() {
			var lower []byte
			for batch := range w.jobs {
				for _, row := range batch {
					if ctx.Err() != nil {
						return
					}
					session := days[row.sessionID]
					if session == nil {
						session = map[string]*trendDay{}
						days[row.sessionID] = session
					}
					date := row.local.Format("2006-01-02")
					day := session[date]
					if day == nil {
						day = &trendDay{Counts: make([]int, len(terms))}
						session[date] = day
					}
					day.Messages++
					lower = appendTrendLower(lower, row.content)
					for j, m := range matchers {
						day.Counts[j] += countTrendOccurrences(lower, m)
					}
				}
			}
		})
	}
	return w
}
func (w *TrendPartWorkers) add(id, content string, local time.Time) {
	w.batch = append(w.batch, trendWork{sessionID: id, content: content, local: local})
	if len(w.batch) == 64 {
		w.flush()
	}
}
func (w *TrendPartWorkers) flush() {
	if len(w.batch) == 0 {
		return
	}
	select {
	case w.jobs <- w.batch:
	case <-w.ctx.Done():
	}
	w.batch = nil
}
func (w *TrendPartWorkers) finish() (map[string]map[string]*trendDay, error) {
	w.flush()
	close(w.jobs)
	w.wait.Wait()
	if err := w.ctx.Err(); err != nil {
		return nil, err
	}
	result := w.parts[0]
	for _, sessions := range w.parts[1:] {
		for id, days := range sessions {
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
