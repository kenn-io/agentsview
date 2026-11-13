package db

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"go.kenn.io/agentsview/internal/kept"
)

const trendsPartsVersion = 1

type trendDay struct {
	Messages int
	Counts   []int
}

type trendPart struct {
	ID, Version string
	Days        map[string]*trendDay
}

// TrendPartsSource reads session versions before streaming their complete messages.
type TrendPartsSource interface {
	TrendSessionVersions(context.Context, AnalyticsFilter) (map[string]string, error)
	VisitTrendMessages(context.Context, AnalyticsFilter, []string, func(string, string, time.Time)) error
}

var trendPartsObserver struct {
	sync.Mutex
	read func([]string)
}

// SetTrendPartsReadObserver installs an observer for tests and returns its reset function.
func SetTrendPartsReadObserver(observe func([]string)) func() {
	trendPartsObserver.Lock()
	trendPartsObserver.read = observe
	trendPartsObserver.Unlock()
	return func() { trendPartsObserver.Lock(); trendPartsObserver.read = nil; trendPartsObserver.Unlock() }
}

// TrendsFromParts reuses session contributions when the selection permits it.
func TrendsFromParts(ctx context.Context, disk *kept.Disk, identity string, f AnalyticsFilter, terms []TrendTermInput, granularity string, src TrendPartsSource) (TrendsTermsResponse, bool, error) {
	if disk == nil || !disk.Enabled() || identity == "" || f.Hour != nil || len(f.MessageScopeFilter().Models) != 0 || f.Timezone == "Local" {
		return TrendsTermsResponse{}, false, nil
	}
	if granularity == "" {
		granularity = "week"
	}
	versions, err := src.TrendSessionVersions(ctx, f)
	if err != nil {
		return TrendsTermsResponse{}, true, err
	}
	slot := "trends-parts|" + f.Timezone + "|" + TrendsKeptVariant(terms, "")
	version := fmt.Sprintf("%s|%d|%d", identity, trendsMatchingVersion, trendsPartsVersion)
	parts := make(map[string]trendPart)
	if data, ok := disk.Get(slot, version); ok {
		if rows, ok := kept.DecodeRows[trendPart](data); ok {
			for _, row := range rows {
				parts[row.ID] = row
			}
		}
	}
	ids := make([]string, 0)
	for id, version := range versions {
		part, ok := parts[id]
		if !ok || version == "" || version != part.Version {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	if len(ids) > 0 {
		trendPartsObserver.Lock()
		if trendPartsObserver.read != nil {
			trendPartsObserver.read(ids)
		}
		trendPartsObserver.Unlock()
		for _, id := range ids {
			parts[id] = trendPart{ID: id, Version: versions[id], Days: make(map[string]*trendDay)}
		}
		workers := newTrendPartWorkers(ctx, terms)
		err := src.VisitTrendMessages(ctx, f, ids, workers.add)
		days, workerErr := workers.finish()
		if err != nil {
			return TrendsTermsResponse{}, true, err
		}
		if workerErr != nil {
			return TrendsTermsResponse{}, true, workerErr
		}
		for id, d := range days {
			part, ok := parts[id]
			if ok {
				part.Days = d
				parts[id] = part
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return TrendsTermsResponse{}, true, err
	}
	if len(ids) > 0 {
		rows := make([]trendPart, 0, len(parts))
		for _, part := range parts {
			rows = append(rows, part)
		}
		if data, err := kept.EncodeRows(rows, 128<<20, func(part trendPart) int64 {
			size := int64(len(part.ID) + len(part.Version))
			for date, day := range part.Days {
				size += int64(len(date) + 8 + 8*len(day.Counts))
			}
			return size
		}); err == nil {
			disk.Put(slot, version, data)
		}
	}
	buckets := TrendBucketRange(f.From, f.To, granularity)
	index := trendBucketIndex(buckets)
	counts := make([][]int, len(terms))
	for i := range counts {
		counts[i] = make([]int, len(buckets))
	}
	messages := make([]int, len(buckets))
	for id := range versions {
		for date, day := range parts[id].Days {
			if !InDateRange(date, f.From, f.To) {
				continue
			}
			t, err := time.Parse("2006-01-02", date)
			if err != nil {
				continue
			}
			if f.DayOfWeek != nil && (int(t.Weekday())+6)%7 != *f.DayOfWeek {
				continue
			}
			i, ok := index[TrendBucketDate(t, time.UTC, granularity)]
			if !ok {
				continue
			}
			messages[i] += day.Messages
			for j, count := range day.Counts {
				if j < len(counts) {
					counts[j][i] += count
				}
			}
		}
	}
	return BuildTrendsTermsResponse(f.From, f.To, granularity, buckets, terms, counts, messages), true, nil
}
