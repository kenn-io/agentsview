package db

import (
	"cmp"
	"context"
	"database/sql"
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

// ScanTrendVersions reads id and version rows.
func ScanTrendVersions(rows *sql.Rows, err error) (map[string]string, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := map[string]string{}
	for rows.Next() {
		var id, version string
		if err := rows.Scan(&id, &version); err != nil {
			return nil, err
		}
		result[id] = version
	}
	return result, rows.Err()
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
		workers := newTrendPartWorkers(ctx, terms)
		err := src.VisitTrendMessages(ctx, f, ids, workers.addSession)
		days, workerErr := workers.finishParts()
		if err := cmp.Or(err, workerErr); err != nil {
			return TrendsTermsResponse{}, true, err
		}
		for _, id := range ids {
			parts[id] = trendPart{ID: id, Version: versions[id], Days: days[id]}
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
	acc := NewTrendAccumulator(f.From, f.To, granularity, terms)
	flt := f.MessageScopeFilter()
	for id := range versions {
		for date, day := range parts[id].Days {
			if local, err := time.Parse("2006-01-02", date); err == nil && flt.MatchesDayHour(local, true) {
				acc.addDay(local, day)
			}
		}
	}
	return acc.Response(), true, nil
}
