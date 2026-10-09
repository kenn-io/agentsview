package readbase

import (
	"context"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/db"
)

func AnalyticsLocalTime(ts string, loc *time.Location) (time.Time, bool) {
	t, ok := ParseAnalyticsTime(ts)
	if !ok {
		return time.Time{}, false
	}
	return t.In(loc), true
}

func AnalyticsWindowBounds(f db.AnalyticsFilter) (string, string) {
	var from, to string
	if f.From != "" {
		from = PaddedUTCBound(f.From+"T00:00:00Z", -14)
	}
	if f.To != "" {
		to = PaddedUTCBound(f.To+"T23:59:59Z", 14)
		if t, err := time.Parse(time.RFC3339, to); err == nil {
			to = t.Add(time.Second).Format(time.RFC3339)
		}
	}
	return from, to
}

// PaddedUTCBound adds hours to a UTC boundary, preserving invalid input.
func PaddedUTCBound(ts string, hours int) string {
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return ts
	}
	return t.Add(time.Duration(hours) * time.Hour).Format(time.RFC3339)
}

func (s *Analytics) models(ctx context.Context, ids []string) ([]string, error) {
	if len(ids) == 0 {
		return []string{}, nil
	}
	models := map[string]bool{}
	if err := s.backend.VisitModels(ctx, ids, func(model string) { models[model] = true }); err != nil {
		return nil, err
	}
	return db.SortedKeys(models), nil
}

func (s *Analytics) filteredModels(ctx context.Context, ids []string, f db.AnalyticsFilter) ([]string, error) {
	if len(ids) == 0 {
		return []string{}, nil
	}
	seen := make(map[string]struct{}, len(ids))
	unique := make([]string, 0, len(ids))
	for _, id := range ids {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		unique = append(unique, id)
	}
	filter := f.MessageScopeFilter()
	loc := AnalyticsLocation(f.Timezone)
	models := map[string]bool{}
	err := s.backend.VisitModelTimes(ctx, unique, func(model, timestamp string) {
		if len(filter.Models) > 0 {
			if _, ok := filter.Models[model]; !ok {
				return
			}
		}
		if analyticsMessageTimeMatches(timestamp, filter, loc) {
			models[model] = true
		}
	})
	if err != nil {
		return nil, err
	}
	return db.SortedKeys(models), nil
}

func (s *Analytics) filteredToolCounts(ctx context.Context, ids []string, f db.AnalyticsFilter) (map[string]int, error) {
	counts := make(map[string]int, len(ids))
	if len(ids) == 0 || strings.TrimSpace(f.Model) == "" {
		return counts, nil
	}
	filter := f.MessageScopeFilter()
	loc := AnalyticsLocation(f.Timezone)
	err := s.backend.VisitToolCounts(ctx, ids, func(id, model, timestamp string, count int) {
		if _, ok := filter.Models[model]; !ok {
			return
		}
		if analyticsMessageTimeMatches(timestamp, filter, loc) {
			counts[id] += count
		}
	})
	if err != nil {
		return nil, err
	}
	return counts, nil
}

func analyticsMessageTimeMatches(timestamp string, filter db.ScopeFilter, loc *time.Location) bool {
	if filter.DayOfWeek == nil && filter.Hour == nil {
		return true
	}
	t, ok := ParseAnalyticsTime(timestamp)
	return filter.MatchesDayHour(t.In(loc), ok)
}

func (s *Analytics) signalMessages(ctx context.Context, rows []db.SignalRow, f db.AnalyticsFilter) (map[string][]db.SignalMessage, error) {
	out := make(map[string][]db.SignalMessage, len(rows))
	if len(rows) == 0 {
		return out, nil
	}
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	if strings.TrimSpace(f.Model) != "" {
		scope, err := s.backend.MessageScope(ctx, ids, f, true)
		if err != nil {
			return nil, err
		}
		for id, scopedRows := range scope {
			for _, row := range scopedRows {
				out[id] = append(out[id], db.SignalMessage{
					SessionID:     row.SessionID,
					Ordinal:       row.Ordinal,
					Role:          row.Role,
					SourceSubtype: row.SourceSubtype,
					Content:       row.Content,
					Timestamp:     row.Timestamp,
					IsSystem:      row.IsSystem,
					HasToolUse:    row.HasToolUse,
				})
			}
		}
		return out, nil
	}
	err := s.backend.VisitSignalMessages(ctx, ids, func(row db.SignalMessage) {
		out[row.SessionID] = append(out[row.SessionID], row)
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
