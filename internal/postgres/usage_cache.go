package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

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
	revision, err := s.UsageChangeRevision(ctx)
	if err != nil || revision == 0 {
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
	s.usageCache = reader
	return nil
}

func (s *Store) cachedUsage() *db.UsageCacheReader {
	if s.readTx != nil || s.hostedRelations {
		return nil
	}
	return s.usageCache
}

var errPGUsageCacheUnavailable = errors.New("PostgreSQL usage journal unavailable")

type pgUsageCacheSource struct {
	*Store
	target string
}

func usageJournalOID(ctx context.Context, q storeExecutor) (uint32, error) {
	var oid uint32
	err := q.QueryRowContext(ctx, `SELECT 'usage_session_changes'::regclass::oid`).Scan(&oid)
	return oid, err
}

func (source *pgUsageCacheSource) snapshot(ctx context.Context, view *Store) (db.UsageSourceSnapshot, error) {
	var snapshot db.UsageSourceSnapshot
	oid, err := usageJournalOID(ctx, view.pg)
	if isUndefinedTable(err) || isInsufficientPrivilege(err) {
		return snapshot, errPGUsageCacheUnavailable
	}
	if err != nil {
		return snapshot, err
	}
	snapshot.DatabaseID = fmt.Sprintf("%s|%d", source.target, oid)
	snapshot.ChangeRevision, err = view.UsageChangeRevision(ctx)
	if err != nil {
		return snapshot, err
	}
	if snapshot.ChangeRevision == 0 {
		return snapshot, errPGUsageCacheUnavailable
	}
	err = view.pg.QueryRowContext(ctx, `SELECT COALESCE((SELECT revision FROM usage_session_changes WHERE session_id=''), 1)`).Scan(&snapshot.CursorHighWater)
	if err != nil {
		return snapshot, err
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
		input := pgUsageSnapshotInputFilter(filter)
		rowsSQL := pgDailyUsageRowsSQLForBounds(pb, input, bounds, view.sessionDialect())
		activitySQL := pgMatchingUsageRowsSQLForBounds(pb, input, bounds, view.sessionDialect())
		query = `SELECT DISTINCT session_id FROM ((` + rowsSQL + `) UNION ALL (` + activitySQL + `)) candidates ORDER BY session_id`
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
	for batch := range slices.Chunk(ids, 512) {
		pb := &paramBuilder{}
		passes := appendPGUsageSessionFilterClauses("TRUE", pb, filter, view.sessionDialect())
		params := make([]string, len(batch))
		for i, id := range batch {
			params[i] = pb.add(id)
		}
		query := `SELECT s.id, s.project, s.machine, s.agent, COALESCE(s.group_key,''), COALESCE(s.session_name,''), COALESCE(s.git_branch,''), s.created_at, s.started_at, s.ended_at,
   COALESCE(NULLIF(COALESCE(s.display_name,s.session_name),''),NULLIF(s.first_message,''),NULLIF(s.project,''),s.id), s.user_message_count, s.total_output_tokens, s.peak_context_tokens, COALESCE(s.has_total_output_tokens,false), COALESCE(s.has_peak_context_tokens,false), COALESCE(s.is_automated,false), COALESCE(s.termination_status,''), (` + passes + `), s.xmin::text, COALESCE(c.revision,0)::text
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
	}
	return nil
}

func pgUsageCacheTime(value sql.NullTime) string {
	if !value.Valid {
		return ""
	}
	return value.Time.UTC().Format(time.RFC3339Nano)
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
		pb := &paramBuilder{}
		params := make([]string, len(batch))
		for i, id := range batch {
			params[i] = pb.add(id)
		}
		selected := strings.Join(params, ",")
		rowsSQL := pgDailyUsageRowsSQLWithWhere(pgUsageMessageEligibility+` AND m.session_id IN (`+selected+`)`, pgUsageEventEligibility+` AND ue.session_id IN (`+selected+`)`)
		rowsSQL = strings.Replace(rowsSQL, "m.ordinal AS message_ordinal,", "m.ordinal AS message_ordinal, m.role AS message_role,", 1)
		rowsSQL = strings.Replace(rowsSQL, "ue.message_ordinal,", "ue.message_ordinal, ''::text AS message_role,", 1)
		query := pgDailyUsageRowSelectFromRowsWithMachine(rowsSQL, true)
		ranking := fmt.Sprintf(`CASE WHEN u.usage_source='message' THEN LEAST(GREATEST(COALESCE(agentsview_json_integer(u.token_usage,ARRAY['output_tokens'],'output_tokens')::numeric,0),0),%d) WHEN u.usage_source='session' THEN GREATEST(u.output_tokens,0) ELSE LEAST(GREATEST(u.output_tokens,0),%d) END::bigint`, db.MaxPlausibleTokens, db.MaxPlausibleTokens)
		query = strings.Replace(query, "\nFROM (", ", "+ranking+", u.message_role\nFROM (", 1) + ` ORDER BY u.session_id, u.ts ASC NULLS FIRST, COALESCE(u.message_ordinal,-1), u.usage_dedup_key`
		rows, err := view.pg.QueryContext(ctx, query, pb.args...)
		if err != nil {
			return snapshot, fmt.Errorf("extracting PostgreSQL usage facts: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var rankingOutput int64
			var role string
			row, err := scanPGDailyUsageRowWithMachine(rows, true, &rankingOutput, &role)
			if err != nil {
				_ = rows.Close()
				return snapshot, err
			}
			input, output, write, read, reasoning := pgDailyUsageRowTokens(row)
			fact := usagefacts.Fact{Source: row.usageSource, Model: row.model, ProviderID: row.providerID, RawTimestamp: pgUsageCacheTime(row.pricingTS), UsesSessionStart: !row.pricingTS.Valid, InputTokens: int64(input), OutputTokens: int64(output), RankingOutputTokens: &rankingOutput, ReasoningTokens: int64(reasoning), CacheCreationTokens: int64(write), CacheReadTokens: int64(read), CacheCreation1hTokens: int64(pgUsageRowCacheCreation1hTokens(row.usageSource, row.tokenJSON, write)), WebSearchRequests: int64(pgDailyUsageRowWebSearchRequests(row)), CostSource: row.costSource, ClaudeMessageID: row.claudeMessageID, ClaudeRequestID: row.claudeRequestID, SourceUUID: row.sourceUUID, UsageDedupKey: row.usageDedupKey, TokenEligible: true, ActivityEligible: row.usageSource != "message" || role == "assistant"}
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
			fact.RequestScoped = row.usageSource == "message" || row.messageOrdinal.Valid || usagefacts.SourceIsRequestScoped(row.usageSource)
			if err := emit(row.sessionID, fact); err != nil {
				_ = rows.Close()
				return snapshot, err
			}
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return snapshot, err
		}
		activityRows, err := view.pg.QueryContext(ctx, `SELECT session_id,ordinal,timestamp,model,provider_id FROM messages WHERE session_id IN (`+selected+`) AND role='assistant' AND model!='<synthetic>' AND NOT (token_usage!='' AND model!='') ORDER BY session_id,ordinal`, pb.args...)
		if err != nil {
			return snapshot, err
		}
		defer activityRows.Close()
		for activityRows.Next() {
			var id string
			var input usagefacts.MessageInput
			var timestamp sql.NullTime
			input.Role = "assistant"
			if err := activityRows.Scan(&id, &input.Ordinal, &timestamp, &input.Model, &input.ProviderID); err != nil {
				_ = activityRows.Close()
				return snapshot, err
			}
			input.Timestamp = pgUsageCacheTime(timestamp)
			fact, _ := usagefacts.FromMessage(input)
			if err := emit(id, fact); err != nil {
				_ = activityRows.Close()
				return snapshot, err
			}
		}
		if err := errors.Join(activityRows.Err(), activityRows.Close()); err != nil {
			return snapshot, err
		}
	}
	return snapshot, tx.Commit()
}

func (source *pgUsageCacheSource) ReadUsageAccounts(ctx context.Context) (string, int64, []db.UsageSourceAccount, error) {
	tx, err := source.beginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		return "", 0, nil, err
	}
	defer func() { _ = tx.Rollback() }()
	snapshot, err := source.snapshot(ctx, source.readView(tx))
	if err != nil {
		return "", 0, nil, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,agent,occurred_at,model,input_tokens,output_tokens,cache_write_tokens,cache_read_tokens,charged_microdollars,is_headless,dedup_key FROM cursor_usage_events ORDER BY id`)
	if err != nil {
		return "", 0, nil, err
	}
	defer rows.Close()
	var accounts []db.UsageSourceAccount
	defer rows.Close()
	for rows.Next() {
		var account db.UsageSourceAccount
		var timestamp sql.NullTime
		if err := rows.Scan(&account.ID, &account.Agent, &timestamp, &account.Model, &account.InputTokens, &account.OutputTokens, &account.CacheWriteTokens, &account.CacheReadTokens, &account.ChargedMicrodollars, &account.Headless, &account.DedupKey); err != nil {
			return "", 0, nil, err
		}
		account.Timestamp = pgUsageCacheTime(timestamp)
		if account.Model == "" {
			account.Model = "<unknown>"
		}
		accounts = append(accounts, account)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return "", 0, nil, err
	}
	return snapshot.DatabaseID, snapshot.CursorHighWater, accounts, tx.Commit()
}

func (source *pgUsageCacheSource) UsageSessionNames(ctx context.Context, ids []string) (map[string][2]string, error) {
	metadata, err := source.loadPGTopSessionMetadata(ctx, ids)
	names := make(map[string][2]string, len(metadata))
	for id, meta := range metadata {
		names[id] = [2]string{meta.displayName, meta.sessionName}
	}
	return names, err
}
