package clickhouse

import (
	"bytes"
	"context"
	"encoding/gob"
	"io"
	"time"
	"unsafe"

	"go.kenn.io/agentsview/internal/db"
)

const usageDiskVersion = "usage-2|"

func encodeUsageGroups(rows []chDailyUsageGroupRow) ([]byte, bool) {
	var buf bytes.Buffer
	enc := gob.NewEncoder(&buf)
	if err := enc.Encode(len(rows)); err != nil {
		return nil, false
	}
	for _, row := range rows {
		if err := enc.Encode(row); err != nil || buf.Len() > dailyUsageRowsBytes {
			return nil, false
		}
	}
	return buf.Bytes(), true
}

func decodeUsageGroups(data []byte) ([]chDailyUsageGroupRow, bool) {
	dec := gob.NewDecoder(bytes.NewReader(data))
	var count int
	if err := dec.Decode(&count); err != nil || count < 0 || count > dailyUsageRowsBytes/int(unsafe.Sizeof(chDailyUsageGroupRow{})) {
		return nil, false
	}
	var rows []chDailyUsageGroupRow
	var size int64
	for range count {
		var row chDailyUsageGroupRow
		if err := dec.Decode(&row); err != nil {
			return nil, false
		}
		size += dailyUsageRowBytes(row)
		if size > dailyUsageRowsBytes {
			return nil, false
		}
		rows = append(rows, row)
	}
	var extra chDailyUsageGroupRow
	return rows, dec.Decode(&extra) == io.EOF
}

// usageDiskVersionAt captures a fresh parts snapshot and the logical rows it names.
func (s *Store) usageDiskVersionAt(ctx context.Context, pricingDigest string) (string, bool) {
	ctx, err := s.withPartsSnapshot(ctx)
	if err != nil {
		return "", false
	}
	state, err := s.preparedUsageState(ctx)
	if err != nil || !state.ready || len(state.raw) != 0 {
		return "", false
	}
	version, err := s.usageReadFingerprint(ctx, state, pricingDigest)
	if err != nil || version == "" {
		return "", false
	}
	return version, true
}

// usageDiskFilter drops the end date only when no source has later usage.
func (s *Store) usageDiskFilter(ctx context.Context, state preparedUsageState, f db.UsageFilter) db.UsageFilter {
	if f.To == "" || f.Timezone == "" {
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
	readCtx, err := withUsageDeltaTables(ctx, state)
	if err != nil {
		return f
	}
	bound := end.UTC().Format(time.RFC3339Nano)
	source, args := chPreparedUsageSourceSQL(state, "ts >= "+chTimestampSQL, []any{bound})
	args = append(args, bound)
	var future bool
	err = s.queryRowContext(readCtx, "SELECT EXISTS(SELECT 1 FROM ("+source+") LIMIT 1) OR EXISTS(SELECT 1 FROM cursor_usage_events WHERE occurred_at >= "+chTimestampSQL+" LIMIT 1)", args...).Scan(&future)
	if err == nil && !future {
		f.To = ""
	}
	return f
}
