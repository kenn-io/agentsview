package readbase

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/db"
)

// AnalyticsMaxSQLVars bounds mirror ID batches below driver parameter limits.
const AnalyticsMaxSQLVars = 900

func AnalyticsLocalTime(ts string, loc *time.Location) (time.Time, bool) {
	t, ok := ParseAnalyticsTime(ts)
	if !ok {
		return time.Time{}, false
	}
	return t.In(loc), true
}

func AnalyticsWindowBounds(f db.AnalyticsFilter) (string, string) {
	from, to := PaddedDateBounds(f.From, f.To)
	if f.To != "" {
		if t, err := time.Parse(time.RFC3339, to); err == nil {
			to = t.Add(time.Second).Format(time.RFC3339)
		}
	}
	return from, to
}

// PaddedDateBounds expands inclusive UTC dates for timezone filtering.
func PaddedDateBounds(from, to string) (string, string) {
	if from != "" {
		from = PaddedUTCBound(from+"T00:00:00Z", -14)
	}
	if to != "" {
		to = PaddedUTCBound(to+"T23:59:59Z", 14)
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
	err := db.QueryChunkedSize(ids, AnalyticsMaxSQLVars, func(chunk []string) error {
		query, args := s.backend.ModelsSQL(chunk)
		rows, err := s.backend.QueryContext(ctx, query, args...)
		if err != nil {
			return fmt.Errorf("querying %s analytics models: %w", s.name, err)
		}
		defer rows.Close()
		for rows.Next() {
			var model string
			if err := rows.Scan(&model); err != nil {
				return fmt.Errorf("scanning %s analytics model: %w", s.name, err)
			}
			models[model] = true
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return db.SortedKeys(models), nil
}

func (s *Analytics) filteredModels(ctx context.Context, ids []string, f db.AnalyticsFilter) ([]string, error) {
	if len(ids) == 0 {
		return []string{}, nil
	}
	unique := uniqueAnalyticsIDs(ids)
	filter := f.MessageScopeFilter()
	loc := AnalyticsLocation(f.Timezone)
	models := map[string]bool{}
	emit := func(model, timestamp string) {
		if len(filter.Models) > 0 {
			if _, ok := filter.Models[model]; !ok {
				return
			}
		}
		if analyticsMessageTimeMatches(timestamp, filter, loc) {
			models[model] = true
		}
	}
	err := db.QueryChunkedSize(unique, AnalyticsMaxSQLVars, func(chunk []string) error {
		query, args := s.backend.ModelTimesSQL(chunk)
		rows, err := s.backend.QueryContext(ctx, query, args...)
		if err != nil {
			return fmt.Errorf("querying %s filtered analytics models: %w", s.name, err)
		}
		defer rows.Close()
		return ScanAnalyticsModelTimes(rows, s.name, s.backend.FormatTime, emit)
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
	emit := func(id, model, timestamp string, count int) {
		if _, ok := filter.Models[model]; !ok {
			return
		}
		if analyticsMessageTimeMatches(timestamp, filter, loc) {
			counts[id] += count
		}
	}
	err := db.QueryChunkedSize(ids, AnalyticsMaxSQLVars, func(chunk []string) error {
		query, args := s.backend.ToolCountsSQL(chunk)
		rows, err := s.backend.QueryContext(ctx, query, args...)
		if err != nil {
			return fmt.Errorf("querying %s filtered analytics tool calls: %w", s.name, err)
		}
		defer rows.Close()
		return ScanAnalyticsToolCounts(rows, s.name, s.backend.FormatTime, emit)
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
	err := s.VisitSignalMessages(ctx, ids, func(row db.SignalMessage) {
		out[row.SessionID] = append(out[row.SessionID], row)
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Analytics) VisitSignalMessages(ctx context.Context, ids []string, emit func(db.SignalMessage)) error {
	query, args := s.backend.SignalMessagesSQL(ids)
	rows, err := s.backend.QueryContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("querying %s signal messages: %w", s.name, err)
	}
	defer rows.Close()
	return ScanAnalyticsSignalMessages(rows, s.name, s.backend.FormatTime, emit)
}

func uniqueAnalyticsIDs(ids []string) []string {
	seen := make(map[string]struct{}, len(ids))
	unique := make([]string, 0, len(ids))
	for _, id := range ids {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		unique = append(unique, id)
	}
	return unique
}
