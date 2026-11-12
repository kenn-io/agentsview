package clickhouse

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/gob"
	"errors"
	"io"
	"time"

	"go.kenn.io/agentsview/internal/db"
)

const (
	usageDiskVersion    = "usage-3|"
	topUsageDiskVersion = "usage-top-2|"
)

func encodeUsageRows[T any](rows []T, limit int64, size func(T) int64) ([]byte, error) {
	var buf bytes.Buffer
	enc := gob.NewEncoder(&buf)
	if err := enc.Encode(len(rows)); err != nil {
		return nil, err
	}
	var total int64
	for _, row := range rows {
		total += size(row)
		if total > limit {
			return nil, errors.New("usage selection exceeds limit")
		}
		if err := enc.Encode(row); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), nil
}

func decodeUsageRows[T any](data []byte) ([]T, bool) {
	dec := gob.NewDecoder(bytes.NewReader(data))
	var count int
	if err := dec.Decode(&count); err != nil || count < 0 {
		return nil, false
	}
	rows := []T{}
	for range count {
		var row T
		if err := dec.Decode(&row); err != nil {
			return nil, false
		}
		rows = append(rows, row)
	}
	var extra T
	return rows, errors.Is(dec.Decode(&extra), io.EOF)
}

// usageCaptureStable validates a fill against the parts captured for its request.
func (s *Store) usageCaptureStable(ctx context.Context) bool {
	before, ok := ctx.Value(partsSnapshotKey{}).(partsSnapshot)
	if !ok || ctx.Err() != nil {
		return false
	}
	after, err := s.readPartsSnapshot(ctx)
	if err != nil {
		return false
	}
	for _, table := range append(append([]string{}, chUsageReadTables...), "prepared_usage", "usage_price_models") {
		if before[table].digest != after[table].digest {
			return false
		}
	}
	return ctx.Err() == nil
}

// usageDiskFilter shares one source extent per logical version across selections.
func (s *Store) usageDiskFilter(ctx context.Context, state preparedUsageState, f db.UsageFilter, version string) db.UsageFilter {
	if !s.usageDisk.Enabled() || f.To == "" || f.Timezone == "" || db.TerminationUsesTime(f.Termination) || version == "" || !state.ready || len(state.raw) != 0 {
		return f
	}
	loc, err := time.LoadLocation(f.Timezone)
	if err != nil {
		return f
	}
	_, end, err := db.FrictionDayBounds(f.To, loc)
	if err != nil {
		return f
	}
	s.usageExtent.Lock()
	defer s.usageExtent.Unlock()
	if !s.usageExtent.valid || s.usageExtent.version != version {
		readCtx, err := withUsageDeltaTables(ctx, state)
		if err != nil {
			return f
		}
		source, args := chPreparedUsageSourceSQL(state, "1", nil)
		var latest sql.NullTime
		var unknown uint64
		if s.beforeUsageExtent != nil {
			s.beforeUsageExtent()
		}
		err = s.queryRowContext(readCtx, "SELECT maxOrNull(ts), countIf(isNull(ts)) FROM (SELECT ts FROM ("+source+") UNION ALL SELECT occurred_at AS ts FROM cursor_usage_events)", args...).Scan(&latest, &unknown)
		if err != nil || !s.usageCaptureStable(ctx) {
			return f
		}
		s.usageExtent.version, s.usageExtent.latest, s.usageExtent.unknown, s.usageExtent.valid = version, latest.Time, unknown != 0, true
	}
	if !s.usageExtent.unknown && s.usageExtent.latest.Before(end) {
		f.To = ""
	}
	return f
}
