package db

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json/v2"
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

// TrendPartsSource reads session versions before streaming their complete messages.
type TrendPartsSource interface {
	TrendSessionVersions(context.Context, AnalyticsFilter) (map[string]string, error)
	TrendWindowSessions(context.Context, AnalyticsFilter, []string) ([]string, error)
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
	if disk == nil || !disk.Enabled() || identity == "" || f.Hour != nil || len(f.MessageScopeFilter().Models) != 0 || f.Timezone == "Local" || f.ActiveSince != "" || TerminationUsesTime(f.Termination) {
		return TrendsTermsResponse{}, false, nil
	}
	if granularity == "" {
		granularity = "week"
	}
	versions, err := src.TrendSessionVersions(ctx, f)
	if err != nil {
		return TrendsTermsResponse{}, true, err
	}
	selection := f
	selection.From, selection.To, selection.Timezone = "", "", ""
	selection.DayOfWeek, selection.Hour = nil, nil
	selection.Model = ""
	key, err := json.Marshal(selection)
	if err != nil {
		return TrendsTermsResponse{}, true, err
	}
	slot := "trends-parts|" + usageTimezoneIdentityFor(f.location(), nil).Key + "|" + TrendsKeptVariant(terms, "") + "|" + string(key)
	version := fmt.Sprintf("%s|%d|%d", identity, trendsMatchingVersion, trendsPartsVersion)
	parts := kept.LoadParts[map[string]*trendDay](disk, slot, version)
	changed := kept.StaleParts(parts, versions)
	ids := make([]string, 0, len(changed))
	for id := range changed {
		if _, ok := versions[id]; ok {
			ids = append(ids, id)
		}
		delete(parts, id)
	}
	sort.Strings(ids)
	if len(ids) > 0 {
		ids, err = src.TrendWindowSessions(ctx, f, ids)
		if err != nil {
			return TrendsTermsResponse{}, true, err
		}
	}
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
			parts[id] = kept.Part[map[string]*trendDay]{ID: id, Fingerprint: versions[id], Present: true, Value: days[id]}
		}
	}
	if err := ctx.Err(); err != nil {
		return TrendsTermsResponse{}, true, err
	}
	if len(changed) > 0 {
		rows := make([]kept.Part[map[string]*trendDay], 0, len(parts))
		for _, part := range parts {
			rows = append(rows, part)
		}
		kept.SaveParts(disk, slot, version, rows, func(days map[string]*trendDay) int64 {
			var size int64
			for date, day := range days {
				size += int64(len(date) + 8 + 8*len(day.Counts))
			}
			return size
		})
	}

	acc := NewTrendAccumulator(f.From, f.To, granularity, terms)
	flt := f.MessageScopeFilter()
	for id := range versions {
		for date, day := range parts[id].Value {
			if local, err := time.Parse("2006-01-02", date); err == nil && flt.MatchesDayHour(local, true) {
				acc.addDay(local, day)
			}
		}
	}
	return acc.Response(), true, nil
}
