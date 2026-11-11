package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"slices"
	"strings"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/usagefacts"
)

// ConfigureUsageCache enables the persistent reader for a native PostgreSQL
// owner. Unsupported journals and borrowed or hosted views retain direct reads.
func (s *Store) ConfigureUsageCache(ctx context.Context, directory, targetURL, schema string) error {
	if s.readTx != nil || s.hostedRelations {
		return nil
	}
	column, err := sessionVersionColumnPG(ctx, s.pg)
	if err != nil || column != "xmin" {
		return err
	}
	fingerprint, err := pgTargetFingerprint(targetURL, schema)
	if err != nil {
		return err
	}
	reader, err := db.NewUsageCacheReader(filepath.Join(directory, "usage-postgres", strings.ReplaceAll(fingerprint, ":", "-")), &pgUsageCacheSource{Store: s, target: fingerprint})
	if err != nil {
		return err
	}
	// A read that would fill or rebuild much of the cache serves its direct
	// read instead, so a cold or stale cache never makes a report slower.
	reader.DeferWork()
	s.usageCache = reader
	return nil
}

// WaitUsageCacheWarm waits for background usage cache preparation queued by
// earlier reads. Tests and operators can use it to observe a warm pass.
// Without a cache it returns at once.
func (s *Store) WaitUsageCacheWarm(ctx context.Context) error {
	if s.usageCache == nil {
		return nil
	}
	return s.usageCache.WaitWarm(ctx)
}

type pgUsageCacheSource struct {
	*Store
	target string
}

// SummarizeUsageChanges counts journal entries committed after a revision.
// Revision 0 marks entries a writer has not committed yet.
func (source *pgUsageCacheSource) SummarizeUsageChanges(ctx context.Context, afterRevision int64) (db.UsageChangeSummary, error) {
	var summary db.UsageChangeSummary
	err := source.pg.QueryRowContext(ctx, `SELECT
 COUNT(*) FILTER (WHERE session_id <> ''),
 COALESCE(bool_or(session_id = ''), false)
 FROM usage_session_changes WHERE revision > $1`, afterRevision).Scan(
		&summary.Sessions, &summary.Global)
	if isUndefinedTable(err) || isInsufficientPrivilege(err) {
		return summary, db.ErrUsageSourceUnavailable
	}
	return summary, err
}

func usageJournalOID(ctx context.Context, q storeExecutor) (uint32, error) {
	var oid uint32
	err := q.QueryRowContext(ctx, `SELECT 'usage_session_changes'::regclass::oid`).Scan(&oid)
	return oid, err
}

func (source *pgUsageCacheSource) snapshot(ctx context.Context, view *Store) (db.UsageSourceSnapshot, error) {
	snapshot := db.UsageSourceSnapshot{MissingTimestampsLast: true}
	var err error
	snapshot.ChangeRevision, err = view.UsageChangeRevision(ctx)
	if err != nil {
		return snapshot, err
	}
	if snapshot.ChangeRevision == 0 {
		return snapshot, db.ErrUsageSourceUnavailable
	}
	oid, err := usageJournalOID(ctx, view.pg)
	if isUndefinedTable(err) || isInsufficientPrivilege(err) {
		return snapshot, db.ErrUsageSourceUnavailable
	}
	if err != nil {
		return snapshot, err
	}
	var systemID, timeline string
	err = view.pg.QueryRowContext(ctx, `SELECT (pg_control_system()).system_identifier::text,
 substring(pg_walfile_name(pg_current_wal_lsn()), 1, 8)
 WHERE NOT pg_is_in_recovery()`).Scan(&systemID, &timeline)
	if err != nil {
		if ctx.Err() != nil {
			return snapshot, ctx.Err()
		}
		return snapshot, db.ErrUsageSourceUnavailable
	}
	snapshot.DatabaseID = fmt.Sprintf("%s|%d|%s|%s", source.target, oid, systemID, timeline)
	err = view.pg.QueryRowContext(ctx, `SELECT COALESCE((SELECT value::bigint FROM sync_metadata WHERE key='usage_account_revision'), 0)`).Scan(&snapshot.CursorHighWater)
	if isUndefinedTable(err) || isInsufficientPrivilege(err) {
		return snapshot, db.ErrUsageSourceUnavailable
	}
	if err != nil {
		return snapshot, err
	}
	if snapshot.CursorHighWater == 0 {
		return snapshot, db.ErrUsageSourceUnavailable
	}
	snapshot.PricingRows, err = view.loadPricingMap(ctx)
	return snapshot, err
}

func (source *pgUsageCacheSource) CaptureUsage(ctx context.Context, request db.UsageSourceRequest) (db.UsageSourceSnapshot, error) {
	tx, err := source.beginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return db.UsageSourceSnapshot{}, err
	}
	defer func() { _ = tx.Rollback() }()
	view := source.readView(tx)
	snapshot, err := source.snapshot(ctx, view)
	if err != nil {
		return snapshot, err
	}
	ids := request.IDs
	if request.AfterRevision != nil {
		_, changes, err := view.loadUsageChanges(ctx, *request.AfterRevision)
		if err != nil {
			return snapshot, err
		}
		snapshot.ChangeIDs = changes
		selected := slices.Clone(changes)
		if ids != nil {
			selected = append(selected, (*ids)...)
		}
		slices.Sort(selected)
		selected = slices.Compact(selected)
		ids = &selected
	}
	if ids == nil {
		selected, err := pgUsageCacheCandidates(ctx, view, request.Filter)
		if err != nil {
			return snapshot, err
		}
		ids = &selected
	}
	if err := pgCaptureUsageMetadata(ctx, view, *ids, request.Filter, &snapshot); err != nil {
		return snapshot, err
	}
	return snapshot, tx.Commit()
}

func pgUsageCacheCandidates(ctx context.Context, view *Store, filter db.UsageFilter) ([]string, error) {
	pb := &paramBuilder{}
	bounds := pgUsageBoundsForFilter(pb, filter)
	var query string
	if !bounds.bounded() {
		query = `SELECT id FROM sessions s WHERE s.deleted_at IS NULL ORDER BY id`
	} else {
		// Include session starts for facts whose timestamp falls back to them.
		// Eligibility and exact local-day bounds stay in the shared fact reader.
		query = `SELECT DISTINCT session_id FROM (
SELECT session_id FROM messages WHERE ` + appendPGUsageColumnBounds("TRUE", "timestamp", bounds) + `
UNION ALL SELECT session_id FROM usage_events WHERE ` + appendPGUsageColumnBounds("TRUE", "occurred_at", bounds) + `
UNION ALL SELECT id AS session_id FROM sessions WHERE ` + appendPGUsageColumnBounds("TRUE", "started_at", bounds) + `) candidates ORDER BY session_id`
	}
	rows, err := view.pg.QueryContext(ctx, query, pb.args...)
	if err != nil {
		return nil, fmt.Errorf("discovering PostgreSQL usage candidates: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func pgCaptureUsageMetadata(ctx context.Context, view *Store, ids []string, filter db.UsageFilter, snapshot *db.UsageSourceSnapshot) error {
	snapshot.Sessions = slices.Grow(snapshot.Sessions, len(ids))
	snapshot.Versions = slices.Grow(snapshot.Versions, len(ids))
	for batch := range slices.Chunk(ids, 512) {
		if err := func() error {
			pb := &paramBuilder{}
			passes := appendPGUsageSessionFilterClauses("TRUE", pb, filter, view.sessionDialect())
			params := make([]string, len(batch))
			for i, id := range batch {
				params[i] = pb.add(id)
			}
			query := `SELECT s.id, s.project, s.machine, s.agent, COALESCE(s.group_key,''), COALESCE(s.session_name,''), COALESCE(s.git_branch,''), s.created_at, s.started_at, s.ended_at,
   COALESCE(NULLIF(COALESCE(s.display_name,s.session_name),''),NULLIF(s.first_message,''),NULLIF(s.project,''),s.id), s.user_message_count, s.total_output_tokens, s.peak_context_tokens, COALESCE(s.has_total_output_tokens,false), COALESCE(s.has_peak_context_tokens,false), COALESCE(s.is_automated,false), COALESCE(s.termination_status,''), COALESCE((` + passes + `), false), s.xmin::text, COALESCE(c.revision,0)::text
   FROM sessions s LEFT JOIN usage_session_changes c ON c.session_id=s.id WHERE s.deleted_at IS NULL AND s.id IN (` + strings.Join(params, ",") + `) ORDER BY s.id`
			rows, err := view.pg.QueryContext(ctx, query, pb.args...)
			if err != nil {
				return fmt.Errorf("capturing PostgreSQL usage metadata: %w", err)
			}
			defer rows.Close()
			for rows.Next() {
				var session db.UsageSourceSession
				var version db.UsageSourceVersion
				var created, started, ended sql.NullTime
				if err := rows.Scan(&session.ID, &session.Project, &session.Machine, &session.Agent, &session.GroupKey, &session.SessionName, &session.GitBranch, &created, &started, &ended, &session.DisplayName, &session.UserMessageCount, &session.TotalOutputTokens, &session.PeakContextTokens, &session.HasTotalOutput, &session.HasPeakContext, &session.IsAutomated, &session.TerminationStatus, &session.PassesFilter, &version.SyncMarker, &version.UsageEventFingerprint); err != nil {
					_ = rows.Close()
					return err
				}
				session.CreatedAt, session.StartedAt, session.EndedAt = pgUsageCacheTime(created), pgUsageCacheTime(started), pgUsageCacheTime(ended)
				session.StartedAtMillis, _, _ = usagefacts.ParseTimestamp(session.StartedAt)
				session.StartedAtNanos = usagefacts.ParseTimestampNanos(session.StartedAt)
				version.SessionID = session.ID
				snapshot.Sessions = append(snapshot.Sessions, session)
				snapshot.Versions = append(snapshot.Versions, version)
			}
			if err := errors.Join(rows.Err(), rows.Close()); err != nil {
				return err
			}
			return nil
		}(); err != nil {
			return err
		}
	}
	return nil
}

func pgUsageCacheTime(value sql.NullTime) string {
	if !value.Valid {
		return ""
	}
	return value.Time.UTC().Format("2006-01-02T15:04:05.000000000Z")
}

func (source *pgUsageCacheSource) StreamUsage(ctx context.Context, ids []string, emit func(string, usagefacts.Fact) error) (db.UsageSourceSnapshot, error) {
	tx, err := source.beginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return db.UsageSourceSnapshot{}, err
	}
	defer func() { _ = tx.Rollback() }()
	view := source.readView(tx)
	snapshot, err := source.snapshot(ctx, view)
	if err != nil {
		return snapshot, err
	}
	if err := pgCaptureUsageMetadata(ctx, view, ids, db.UsageFilter{}, &snapshot); err != nil {
		return snapshot, err
	}
	for batch := range slices.Chunk(ids, 256) {
		if err := func() error {
			pb := &paramBuilder{}
			params := make([]string, len(batch))
			for i, id := range batch {
				params[i] = pb.add(id)
			}
			selected := strings.Join(params, ",")
			rowsSQL := pgDailyUsageRowsSQLWithColumns(pgUsageMessageEligibility+` AND m.session_id IN (`+selected+`)`, pgUsageEventEligibility+` AND ue.session_id IN (`+selected+`)`, ", m.role = 'assistant' AS activity_eligible", ", true AS activity_eligible")
			extraColumns := ", (" + pgSnapshotOutputTokensSQL() + ")::bigint, u.activity_eligible"
			query := pgDailyUsageRowSelectFromRowsWithSession(rowsSQL, true, "u.session_id", false, false, extraColumns) + ` ORDER BY u.session_id, u.ts ASC NULLS FIRST, COALESCE(u.message_ordinal,-1), u.usage_dedup_key`
			rows, err := view.pg.QueryContext(ctx, query, pb.args...)
			if err != nil {
				return fmt.Errorf("extracting PostgreSQL usage facts: %w", err)
			}
			defer rows.Close()
			for rows.Next() {
				var rankingOutput int64
				var activityEligible bool
				// Keep this conversion aligned with readDailyUsage's scanner.
				row, err := scanPGDailyUsageRowWithMachine(rows, true, &rankingOutput, &activityEligible)
				if err != nil {
					_ = rows.Close()
					return err
				}
				fact := pgDailyUsageRowFact(row)
				fact.Source, fact.Model, fact.ProviderID = row.usageSource, row.model, row.providerID
				fact.RawTimestamp, fact.UsesSessionStart = pgUsageCacheTime(row.pricingTS), !row.pricingTS.Valid
				fact.RankingOutputTokens, fact.CostSource = &rankingOutput, row.costSource
				fact.ClaudeMessageID, fact.ClaudeRequestID, fact.SourceUUID, fact.UsageDedupKey = row.claudeMessageID, row.claudeRequestID, row.sourceUUID, row.usageDedupKey
				fact.TokenEligible, fact.ActivityEligible = true, activityEligible
				fact.TimestampMillis, _, _ = usagefacts.ParseTimestamp(fact.RawTimestamp)
				fact.TimestampNanos = usagefacts.ParseTimestampNanos(fact.RawTimestamp)
				if row.messageOrdinal.Valid {
					ordinal := int(row.messageOrdinal.Int64)
					fact.MessageOrdinal = &ordinal
				}
				if row.cost.Valid {
					cost := row.cost.Int64
					fact.ReportedCostMicrodollars = &cost
				}
				if err := emit(row.sessionID, fact); err != nil {
					_ = rows.Close()
					return err
				}
			}
			if err := errors.Join(rows.Err(), rows.Close()); err != nil {
				return err
			}
			activityRows, err := view.pg.QueryContext(ctx, `SELECT m.session_id,m.ordinal,m.timestamp,m.model,m.provider_id FROM messages m WHERE m.session_id IN (`+selected+`) AND `+pgUsageMatchingMessageSourceEligibility+` AND NOT (`+pgUsageMessageSourceEligibility+`) ORDER BY m.session_id,m.ordinal`, pb.args...)
			if err != nil {
				return err
			}
			defer activityRows.Close()
			for activityRows.Next() {
				var id string
				var input usagefacts.MessageInput
				var timestamp sql.NullTime
				input.Role = "assistant"
				if err := activityRows.Scan(&id, &input.Ordinal, &timestamp, &input.Model, &input.ProviderID); err != nil {
					_ = activityRows.Close()
					return err
				}
				input.Timestamp = pgUsageCacheTime(timestamp)
				fact, _ := usagefacts.FromMessage(input)
				if err := emit(id, fact); err != nil {
					_ = activityRows.Close()
					return err
				}
			}
			if err := errors.Join(activityRows.Err(), activityRows.Close()); err != nil {
				return err
			}
			return nil
		}(); err != nil {
			return snapshot, err
		}
	}
	return snapshot, tx.Commit()
}

func (source *pgUsageCacheSource) StreamUsageAccounts(ctx context.Context, emit func(db.UsageSourceAccount) error) (string, int64, error) {
	tx, err := source.beginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = tx.Rollback() }()
	snapshot, err := source.snapshot(ctx, source.readView(tx))
	if err != nil {
		return "", 0, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,agent,occurred_at,model,input_tokens,output_tokens,cache_write_tokens,cache_read_tokens,charged_microdollars,is_headless,dedup_key FROM cursor_usage_events ORDER BY id`)
	if err != nil {
		return "", 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var account db.UsageSourceAccount
		var timestamp sql.NullTime
		if err := rows.Scan(&account.ID, &account.Agent, &timestamp, &account.Model, &account.InputTokens, &account.OutputTokens, &account.CacheWriteTokens, &account.CacheReadTokens, &account.ChargedMicrodollars, &account.Headless, &account.DedupKey); err != nil {
			return "", 0, err
		}
		account.Timestamp = pgUsageCacheTime(timestamp)
		if account.Model == "" {
			account.Model = "<unknown>"
		}
		if err := emit(account); err != nil {
			return "", 0, err
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return "", 0, err
	}
	return snapshot.DatabaseID, snapshot.CursorHighWater, tx.Commit()
}

func (source *pgUsageCacheSource) UsageSessionNames(ctx context.Context, ids []string) (map[string][2]string, error) {
	metadata, err := source.loadPGTopSessionMetadata(ctx, ids)
	names := make(map[string][2]string, len(metadata))
	for id, meta := range metadata {
		names[id] = [2]string{meta.displayName, meta.sessionName}
	}
	return names, err
}

func directUsageRequired(ctx context.Context, err error) bool {
	return err != nil && ctx.Err() == nil && logUsageFallback(err)
}

func cachedUsage[T any](ctx context.Context, cache *db.UsageCacheReader, read func(*db.UsageCacheReader) (T, error)) (result T, release func(), handled bool, err error) {
	if cache == nil {
		return result, func() {}, false, nil
	}
	result, err = read(cache)
	if !directUsageRequired(ctx, err) {
		return result, func() {}, true, err
	}
	return result, cache.HoldWarm(), false, nil
}

func logUsageFallback(err error) bool {
	if !errors.Is(err, db.ErrUsageCacheWarming) && !errors.Is(err, db.ErrUsageSourceUnavailable) {
		log.Printf("postgres usage cache: falling back to direct read: %v", err)
	}
	return true
}
