package db

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/export"
	"go.kenn.io/agentsview/internal/usagefacts"
)

type usageQueryKind int

const (
	usageQueryKindToken usageQueryKind = iota
	usageQueryKindActivity
)

type usageSourceVersion struct {
	SessionID             string
	SyncMarker            string
	TranscriptRevision    string
	UsageEventFingerprint string
}

func (v usageSourceVersion) Equal(other usageSourceVersion) bool {
	return v.SessionID == other.SessionID &&
		v.SyncMarker == other.SyncMarker &&
		v.TranscriptRevision == other.TranscriptRevision &&
		v.UsageEventFingerprint == other.UsageEventFingerprint
}

type usageQuerySession struct {
	ID                string
	Project           string
	Machine           string
	Agent             string
	GitBranch         string
	CreatedAt         string
	StartedAt         string
	EndedAt           string
	DisplayName       string
	StartedAtMillis   *int64
	StartedAtNanos    *int64
	UserMessageCount  int
	TotalOutputTokens int
	PeakContextTokens int
	HasTotalOutput    bool
	HasPeakContext    bool
	IsAutomated       bool
	TerminationStatus string
	PassesFilter      bool
}

type usageQueryInterval struct {
	FromMillis    int64
	ToMillis      int64
	FromLocalDate string
	ToLocalDate   string
}

type usageQuerySnapshot struct {
	DatabaseID      string
	Sessions        []usageQuerySession
	Versions        []usageSourceVersion
	Intervals       []usageQueryInterval
	CursorHighWater int64
	PricingRows     []export.EffectivePricingRow
	location        *time.Location
}

// Close exists to make the snapshot boundary explicit to callers. Capture
// copies every value and closes its archive transaction before returning.
func (usageQuerySnapshot) Close() error { return nil }

type usageQuerySessionRecord struct {
	session usageQuerySession
	version usageSourceVersion
}

func (db *DB) captureUsageQuery(
	ctx context.Context, filter UsageFilter, kind usageQueryKind,
) (usageQuerySnapshot, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	conn, err := db.getReader().Conn(ctx)
	if err != nil {
		return usageQuerySnapshot{}, fmt.Errorf("pinning usage archive connection: %w", err)
	}
	defer conn.Close()
	tx, err := conn.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return usageQuerySnapshot{}, fmt.Errorf("starting usage archive snapshot: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var snapshot usageQuerySnapshot
	snapshot.location = filter.location()
	if err := tx.QueryRowContext(ctx,
		`SELECT value FROM archive_metadata WHERE key = ?`,
		archiveMetadataDatabaseIDKey,
	).Scan(&snapshot.DatabaseID); err != nil {
		return usageQuerySnapshot{}, fmt.Errorf("reading usage snapshot database id: %w", err)
	}
	pricingRows, err := db.loadPricingMapFrom(ctx, tx)
	if err != nil {
		return usageQuerySnapshot{}, fmt.Errorf("reading usage snapshot pricing: %w", err)
	}
	snapshot.PricingRows = pricingRows

	bounds := usageCandidateBoundsForFilter(filter)
	query, args := usageCandidateDiscoverySQL(bounds, kind)
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return usageQuerySnapshot{}, fmt.Errorf("discovering usage candidates: %w", err)
	}
	var candidateIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return usageQuerySnapshot{}, fmt.Errorf("scanning usage candidate: %w", err)
		}
		candidateIDs = append(candidateIDs, id)
	}
	if err := rows.Close(); err != nil {
		return usageQuerySnapshot{}, fmt.Errorf("closing usage candidate rows: %w", err)
	}
	if err := rows.Err(); err != nil {
		return usageQuerySnapshot{}, fmt.Errorf("iterating usage candidates: %w", err)
	}

	records, err := loadUsageQuerySessionRecords(ctx, tx, candidateIDs, filter)
	if err != nil {
		return usageQuerySnapshot{}, err
	}
	fingerprints, err := usageEventFingerprintsWithQuerier(ctx, tx, candidateIDs)
	if err != nil {
		return usageQuerySnapshot{}, fmt.Errorf("fingerprinting usage events: %w", err)
	}
	for i := range records {
		records[i].version.UsageEventFingerprint =
			fingerprints[records[i].session.ID]
		snapshot.Sessions = append(snapshot.Sessions, records[i].session)
		snapshot.Versions = append(snapshot.Versions, records[i].version)
	}
	snapshot.Intervals = usageQueryIntervals(filter)
	var hasCursorTable bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM sqlite_master
		WHERE type = 'table' AND name = 'cursor_usage_events'
	)`).Scan(&hasCursorTable); err != nil {
		return usageQuerySnapshot{}, fmt.Errorf("checking Cursor usage table: %w", err)
	}
	if hasCursorTable {
		if err := tx.QueryRowContext(ctx,
			`SELECT COALESCE(MAX(id), 0) FROM cursor_usage_events`,
		).Scan(&snapshot.CursorHighWater); err != nil {
			return usageQuerySnapshot{}, fmt.Errorf("reading Cursor usage high-water mark: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return usageQuerySnapshot{}, fmt.Errorf("closing usage archive snapshot: %w", err)
	}
	return snapshot, nil
}

// usageCandidateBoundsForFilter converts local calendar-day filters to their
// exact UTC instants before padding for the full range of RFC3339 offsets that
// may appear in stored timestamp text. The result is deliberately a superset;
// exact membership is applied against cached parsed timestamps later.
func usageCandidateBoundsForFilter(filter UsageFilter) usageBounds {
	location := filter.location()
	var bounds usageBounds
	if filter.From != "" {
		if day, err := time.ParseInLocation("2006-01-02", filter.From, location); err == nil {
			bounds.from = day.UTC().Add(-14 * time.Hour).Format(time.RFC3339)
		}
	}
	if filter.To != "" {
		if day, err := time.ParseInLocation("2006-01-02", filter.To, location); err == nil {
			bounds.to = day.AddDate(0, 0, 1).UTC().Add(14*time.Hour - time.Nanosecond).
				Format(time.RFC3339Nano)
		}
	}
	return bounds
}

func usageCandidateDiscoverySQL(
	bounds usageBounds, kind usageQueryKind,
) (string, []any) {
	if !bounds.bounded() {
		return `SELECT s.id FROM sessions s
			WHERE s.deleted_at IS NULL ORDER BY s.id`, nil
	}

	var branches []string
	var args []any
	appendBranch := func(prefix, column string) {
		where, branchArgs := appendUsageColumnBounds("1=1", column, bounds, nil)
		branches = append(branches, prefix+"\n AND "+where)
		args = append(args, branchArgs...)
	}
	appendBranch(`SELECT m.session_id
		FROM messages AS m INDEXED BY idx_messages_usage_covering
		JOIN sessions s ON s.id = m.session_id
		WHERE `+usageMessageSourceEligibility+`
		  AND s.deleted_at IS NULL
		  AND m.timestamp IS NOT NULL AND m.timestamp != ''`, "m.timestamp")
	appendBranch(`SELECT m.session_id
		FROM messages AS m INDEXED BY idx_messages_usage_covering
		JOIN sessions s ON s.id = m.session_id
		WHERE `+usageMessageSourceEligibility+`
		  AND s.deleted_at IS NULL AND m.timestamp IS NULL`, "s.started_at")
	appendBranch(`SELECT m.session_id
		FROM messages AS m INDEXED BY idx_messages_usage_covering
		JOIN sessions s ON s.id = m.session_id
		WHERE `+usageMessageSourceEligibility+`
		  AND s.deleted_at IS NULL AND m.timestamp = ''`, "s.started_at")
	appendBranch(`SELECT ue.session_id
		FROM usage_events AS ue INDEXED BY idx_usage_events_occurred
		JOIN sessions s ON s.id = ue.session_id
		WHERE `+usageEventSourceEligibility+`
		  AND s.deleted_at IS NULL
		  AND ue.occurred_at IS NOT NULL AND ue.occurred_at != ''`, "ue.occurred_at")
	appendBranch(`SELECT ue.session_id
		FROM usage_events ue
		JOIN sessions s ON s.id = ue.session_id
		WHERE `+usageEventSourceEligibility+`
		  AND s.deleted_at IS NULL
		  AND (ue.occurred_at IS NULL OR ue.occurred_at = '')`, "s.started_at")

	if kind == usageQueryKindActivity {
		appendBranch(`SELECT m.session_id
			FROM messages AS m INDEXED BY idx_messages_activity_timestamp
			JOIN sessions s ON s.id = m.session_id
			WHERE m.role = 'assistant' AND m.model != '<synthetic>'
			  AND s.deleted_at IS NULL
			  AND m.timestamp IS NOT NULL AND m.timestamp != ''`, "m.timestamp")
		appendBranch(`SELECT m.session_id
			FROM messages AS m INDEXED BY idx_messages_activity_timestamp
			JOIN sessions s ON s.id = m.session_id
			WHERE m.role = 'assistant' AND m.model != '<synthetic>'
			  AND s.deleted_at IS NULL AND m.timestamp IS NULL`, "s.started_at")
		appendBranch(`SELECT m.session_id
			FROM messages AS m INDEXED BY idx_messages_activity_timestamp
			JOIN sessions s ON s.id = m.session_id
			WHERE m.role = 'assistant' AND m.model != '<synthetic>'
			  AND s.deleted_at IS NULL AND m.timestamp = ''`, "s.started_at")
	}

	return `SELECT DISTINCT candidate.session_id
		FROM (` + strings.Join(branches, "\nUNION ALL\n") + `) candidate
		ORDER BY candidate.session_id`, args
}

func loadUsageQuerySessionRecords(
	ctx context.Context, tx *sql.Tx, candidateIDs []string, filter UsageFilter,
) ([]usageQuerySessionRecord, error) {
	if len(candidateIDs) == 0 {
		return nil, nil
	}
	const batchSize = 900
	records := make([]usageQuerySessionRecord, 0, len(candidateIDs))
	filterWhere, filterArgs := filter.appendUsageSessionFilterClauses("1=1", nil)
	for start := 0; start < len(candidateIDs); start += batchSize {
		end := min(start+batchSize, len(candidateIDs))
		ids := candidateIDs[start:end]
		placeholders := make([]string, len(ids))
		idArgs := make([]any, len(ids))
		for i, id := range ids {
			placeholders[i] = "?"
			idArgs[i] = id
		}
		query := `SELECT
			s.id, s.project, s.machine, s.agent, COALESCE(s.git_branch, ''),
			COALESCE(s.created_at, ''), COALESCE(s.started_at, ''),
			COALESCE(s.ended_at, ''),
			COALESCE(NULLIF(COALESCE(s.display_name, s.session_name), ''),
			         NULLIF(s.first_message, ''), NULLIF(s.project, ''), s.id),
			s.user_message_count, s.total_output_tokens, s.peak_context_tokens,
			COALESCE(s.has_total_output_tokens, 0),
			COALESCE(s.has_peak_context_tokens, 0),
			COALESCE(s.is_automated, 0),
			COALESCE(s.termination_status, ''),
			CASE WHEN (` + filterWhere + `) THEN 1 ELSE 0 END,
			COALESCE(s.sync_marker, ''), COALESCE(s.transcript_revision, '0')
		FROM sessions s
		WHERE s.deleted_at IS NULL AND s.id IN (` +
			strings.Join(placeholders, ",") + `)
		ORDER BY s.id`
		args := append(append([]any(nil), filterArgs...), idArgs...)
		rows, err := tx.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, fmt.Errorf("loading usage candidate metadata: %w", err)
		}
		for rows.Next() {
			var record usageQuerySessionRecord
			var passes, automated, hasTotalOutput, hasPeakContext int
			if err := rows.Scan(
				&record.session.ID, &record.session.Project,
				&record.session.Machine, &record.session.Agent,
				&record.session.GitBranch, &record.session.CreatedAt,
				&record.session.StartedAt, &record.session.EndedAt,
				&record.session.DisplayName, &record.session.UserMessageCount,
				&record.session.TotalOutputTokens,
				&record.session.PeakContextTokens,
				&hasTotalOutput, &hasPeakContext,
				&automated, &record.session.TerminationStatus, &passes,
				&record.version.SyncMarker, &record.version.TranscriptRevision,
			); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("scanning usage candidate metadata: %w", err)
			}
			record.session.IsAutomated = automated != 0
			record.session.HasTotalOutput = hasTotalOutput != 0
			record.session.HasPeakContext = hasPeakContext != 0
			record.session.PassesFilter = passes != 0
			if millis, _, _ := usagefacts.ParseTimestamp(record.session.StartedAt); millis != nil {
				record.session.StartedAtMillis = millis
				record.session.StartedAtNanos = usagefacts.ParseTimestampNanos(
					record.session.StartedAt)
			}
			record.version.SessionID = record.session.ID
			records = append(records, record)
		}
		if err := rows.Close(); err != nil {
			return nil, fmt.Errorf("closing usage candidate metadata: %w", err)
		}
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("iterating usage candidate metadata: %w", err)
		}
	}
	sort.Slice(records, func(i, j int) bool {
		return records[i].session.ID < records[j].session.ID
	})
	return records, nil
}

func usageQueryIntervals(filter UsageFilter) []usageQueryInterval {
	if filter.From == "" && filter.To == "" {
		return nil
	}
	location := filter.location()
	parseDate := func(value string) (time.Time, bool) {
		parsed, err := time.ParseInLocation("2006-01-02", value, location)
		return parsed, err == nil
	}
	from, hasFrom := parseDate(filter.From)
	to, hasTo := parseDate(filter.To)
	if filter.From != "" && !hasFrom || filter.To != "" && !hasTo {
		return nil
	}
	if hasFrom && hasTo {
		if from.After(to) {
			return nil
		}
		var intervals []usageQueryInterval
		for day := from; !day.After(to); day = day.AddDate(0, 0, 1) {
			next := day.AddDate(0, 0, 1)
			date := day.Format("2006-01-02")
			intervals = append(intervals, usageQueryInterval{
				FromMillis: day.UTC().UnixMilli(), ToMillis: next.UTC().UnixMilli(),
				FromLocalDate: date, ToLocalDate: date,
			})
		}
		return intervals
	}
	if hasFrom {
		return []usageQueryInterval{{
			FromMillis: from.UTC().UnixMilli(), ToMillis: math.MaxInt64,
			FromLocalDate: filter.From,
		}}
	}
	next := to.AddDate(0, 0, 1)
	return []usageQueryInterval{{
		FromMillis: math.MinInt64, ToMillis: next.UTC().UnixMilli(),
		ToLocalDate: filter.To,
	}}
}
