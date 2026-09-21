package clickhouse

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"

	"go.kenn.io/agentsview/internal/db"
)

const sessionCols = `id, project, project_assigned, machine, agent,
	agent_label, entrypoint, session_kind,
	first_message, COALESCE(display_name, session_name) AS display_name, created_at, started_at,
	ended_at, message_count, user_message_count,
	parent_session_id, relationship_type,
	total_output_tokens, peak_context_tokens,
	has_total_output_tokens, has_peak_context_tokens,
	is_automated,
	tool_failure_signal_count, tool_retry_count,
	edit_churn_count, consecutive_failure_max,
	outcome, outcome_confidence,
	ended_with_role, final_failure_streak,
	signals_pending_since,
	compaction_count, mid_task_compaction_count,
	context_pressure_max,
	health_score, health_grade,
	has_tool_calls, has_context_data,
	quality_signal_version, short_prompt_count, unstructured_start,
	missing_success_criteria_count, missing_verification_count,
	duplicate_prompt_count, no_code_context_count, runaway_tool_loop_count,
	data_version,
	cwd, git_branch, source_session_id, source_version, transcript_fidelity,
	parser_malformed_lines, is_truncated,
	secret_leak_count, secrets_rules_version,
	deleted_at, deletion_cause, termination_status, transcript_revision`

// sessionFullCols is the GetSessionFull list. It adds file_path the way
// PostgreSQL serve does, and omits volatile fingerprint columns
// (file_size, file_mtime, file_hash, local_modified_at).
const sessionFullCols = sessionCols + `,
	file_path`

// sessionActivityExpr orders sessions by their most recent activity.
const sessionActivityExpr = "COALESCE(ended_at, started_at, created_at)"

func scanSession(rs interface{ Scan(...any) error }) (db.Session, error) {
	return scanSessionWithSource(rs, false)
}

func scanSessionWithSource(
	rs interface{ Scan(...any) error }, includeSource bool,
) (db.Session, error) {
	var s db.Session
	var createdAt, startedAt, endedAt, deletedAt any
	targets := []any{
		&s.ID, &s.Project, &s.ProjectAssigned, &s.Machine, &s.Agent,
		&s.AgentLabel, &s.Entrypoint, &s.SessionKind,
		&s.FirstMessage, &s.DisplayName,
		&createdAt, &startedAt, &endedAt,
		&s.MessageCount, &s.UserMessageCount,
		&s.ParentSessionID, &s.RelationshipType,
		&s.TotalOutputTokens, &s.PeakContextTokens,
		&s.HasTotalOutputTokens, &s.HasPeakContextTokens,
		&s.IsAutomated,
		&s.ToolFailureSignalCount, &s.ToolRetryCount,
		&s.EditChurnCount, &s.ConsecutiveFailureMax,
		&s.Outcome, &s.OutcomeConfidence,
		&s.EndedWithRole, &s.FinalFailureStreak,
		&s.SignalsPendingSince,
		&s.CompactionCount, &s.MidTaskCompactionCount,
		&s.ContextPressureMax,
		&s.HealthScore, &s.HealthGrade,
		&s.HasToolCalls, &s.HasContextData,
		&s.QualitySignalVersion, &s.ShortPromptCount,
		&s.UnstructuredStart, &s.MissingSuccessCriteriaCount,
		&s.MissingVerificationCount, &s.DuplicatePromptCount,
		&s.NoCodeContextCount, &s.RunawayToolLoopCount,
		&s.DataVersion,
		&s.Cwd, &s.GitBranch,
		&s.SourceSessionID, &s.SourceVersion, &s.TranscriptFidelity,
		&s.ParserMalformedLines, &s.IsTruncated,
		&s.SecretLeakCount, &s.SecretsRulesVersion,
		&deletedAt, &s.DeletionCause, &s.TerminationStatus, &s.TranscriptRevision,
	}
	if includeSource {
		targets = append(targets, &s.FilePath)
	}
	if err := rs.Scan(targets...); err != nil {
		return s, err
	}
	s.CreatedAt = formatDBTime(createdAt)
	if v := formatDBTime(startedAt); v != "" {
		s.StartedAt = &v
	}
	if v := formatDBTime(endedAt); v != "" {
		s.EndedAt = &v
	}
	if v := formatDBTime(deletedAt); v != "" {
		s.DeletedAt = &v
	}
	return s, nil
}

func scanSessionRows(rows *sql.Rows) ([]db.Session, error) {
	return scanSessionRowsWithSource(rows, false)
}

func scanSessionRowsWithSource(rows *sql.Rows, includeSource bool) ([]db.Session, error) {
	sessions := []db.Session{}
	for rows.Next() {
		s, err := scanSessionWithSource(rows, includeSource)
		if err != nil {
			return nil, fmt.Errorf("scanning clickhouse session: %w", err)
		}
		sessions = append(sessions, s)
	}
	return sessions, rows.Err()
}

func (s *Store) ListSessions(ctx context.Context, f db.SessionFilter) (db.SessionPage, error) {
	if f.Limit <= 0 || f.Limit > db.MaxSessionLimit {
		f.Limit = db.DefaultSessionLimit
	}
	dialect := db.ClickHouseQueryDialect()
	where, args := db.BuildSessionFilterSQL(f, dialect)
	rs := db.ResolveSort(f)
	total := 0
	var cur db.SessionCursor
	if f.Cursor != "" {
		var err error
		cur, err = s.DecodeCursor(f.Cursor)
		if err != nil {
			return db.SessionPage{}, err
		}
		total = cur.Total
	}
	if total <= 0 {
		if err := s.queryRowContext(ctx,
			"SELECT toInt64(COUNT(*)) FROM sessions WHERE "+where, args...,
		).Scan(&total); err != nil {
			return db.SessionPage{}, fmt.Errorf("counting clickhouse sessions: %w", err)
		}
	}
	cursorArgs := append([]any{}, args...)
	pageBuilder := db.NewQueryBuilder(dialect, len(args))
	cursorWhere := where
	if f.Cursor != "" {
		vals, err := db.CursorPredicateValues(cur, rs)
		if err != nil {
			return db.SessionPage{}, err
		}
		cursorWhere += " AND " + pageBuilder.CursorPredicate(rs, f, vals, cur.ID)
	}
	columns := sessionCols
	if f.IncludeSource {
		columns += ", file_path"
	}
	query := "SELECT " + columns +
		" FROM sessions WHERE " + cursorWhere + " " +
		pageBuilder.OrderByClause(rs, f) + " " +
		pageBuilder.Limit(f.Limit+1)
	cursorArgs = append(cursorArgs, pageBuilder.Args()...)
	rows, err := s.queryContext(ctx, query, cursorArgs...)
	if err != nil {
		return db.SessionPage{}, fmt.Errorf("querying clickhouse sessions: %w", err)
	}
	defer rows.Close()
	sessions, err := scanSessionRowsWithSource(rows, f.IncludeSource)
	if err != nil {
		return db.SessionPage{}, err
	}
	page := db.SessionPage{Sessions: sessions, Total: total}
	if len(sessions) > f.Limit {
		page.Sessions = sessions[:f.Limit]
		last := page.Sessions[f.Limit-1]
		page.NextCursor = s.EncodeCursor(db.NextSessionCursor(&last, rs, total, f))
	}
	return page, nil
}

func (s *Store) GetSidebarSessionIndex(ctx context.Context, f db.SessionFilter) (db.SidebarSessionIndex, error) {
	f.IncludeChildren = true
	f.IncludeOrphans = true
	f.Cursor = ""
	f.Limit = 0

	dialect := db.ClickHouseQueryDialect()
	where, args := db.BuildSessionFilterSQL(f, dialect)
	rootFilter := f
	rootFilter.IncludeChildren = false
	rootWhere, rootArgs := db.BuildSessionBaseFilterSQL(rootFilter, dialect)
	canonicalRootWhere := db.BuildCanonicalRootWhere(dialect, "sessions", f.IncludeOrphans)
	var total int
	if err := s.queryRowContext(ctx,
		"SELECT toInt64(COUNT(*)) FROM sessions WHERE "+rootWhere+" AND "+canonicalRootWhere,
		rootArgs...,
	).Scan(&total); err != nil {
		return db.SidebarSessionIndex{}, fmt.Errorf("counting clickhouse sidebar roots: %w", err)
	}
	query := `
		SELECT
			id,
			parent_session_id,
			relationship_type,
			project,
			project_assigned,
			machine,
			agent,
			agent_label,
			entrypoint,
			session_kind,
			COALESCE(display_name, session_name) AS display_name,
			started_at,
			ended_at,
			created_at,
			termination_status,
			message_count,
			user_message_count,
			transcript_revision,
			is_automated,
			position(COALESCE(first_message, ''), '<teammate-message') > 0
		FROM sessions
		WHERE ` + where + `
		ORDER BY ` + sessionActivityExpr + ` DESC, id DESC`
	rows, err := s.queryContext(ctx, query, args...)
	if err != nil {
		return db.SidebarSessionIndex{}, fmt.Errorf("querying clickhouse sidebar session index: %w", err)
	}
	defer rows.Close()

	index := db.SidebarSessionIndex{Sessions: []db.SidebarSessionIndexRow{}, Total: total}
	for rows.Next() {
		var row db.SidebarSessionIndexRow
		var startedAt, endedAt, createdAt any
		if err := rows.Scan(
			&row.ID, &row.ParentSessionID, &row.RelationshipType,
			&row.Project, &row.ProjectAssigned, &row.Machine, &row.Agent,
			&row.AgentLabel, &row.Entrypoint, &row.SessionKind, &row.DisplayName,
			&startedAt, &endedAt, &createdAt,
			&row.TerminationStatus, &row.MessageCount, &row.UserMessageCount,
			&row.TranscriptRevision, &row.IsAutomated, &row.IsTeammate,
		); err != nil {
			return db.SidebarSessionIndex{}, fmt.Errorf("scanning clickhouse sidebar session index: %w", err)
		}
		if v := formatDBTime(startedAt); v != "" {
			row.StartedAt = &v
		}
		if v := formatDBTime(endedAt); v != "" {
			row.EndedAt = &v
		}
		row.CreatedAt = formatDBTime(createdAt)
		index.Sessions = append(index.Sessions, row)
	}
	if err := rows.Err(); err != nil {
		return db.SidebarSessionIndex{}, fmt.Errorf("iterating clickhouse sidebar session index: %w", err)
	}
	return index, nil
}

func (s *Store) GetSession(ctx context.Context, id string) (*db.Session, error) {
	row := s.queryRowContext(ctx,
		"SELECT "+sessionCols+" FROM sessions WHERE id = ? AND deleted_at IS NULL", id)
	sess, err := scanSession(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("getting clickhouse session: %w", err)
	}
	return &sess, nil
}

func (s *Store) GetSessionFull(ctx context.Context, id string) (*db.Session, error) {
	row := s.queryRowContext(ctx, "SELECT "+sessionFullCols+" FROM sessions WHERE id = ?", id)
	sess, err := scanSessionWithSource(row, true)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("getting clickhouse full session: %w", err)
	}
	return &sess, nil
}

func (s *Store) ListTrashedSessions(ctx context.Context) ([]db.Session, error) {
	rows, err := s.queryContext(ctx,
		"SELECT "+sessionCols+" FROM sessions WHERE deleted_at IS NOT NULL"+
			" ORDER BY deleted_at DESC LIMIT 500")
	if err != nil {
		return nil, fmt.Errorf("listing clickhouse trash: %w", err)
	}
	defer rows.Close()
	return scanSessionRows(rows)
}

func (s *Store) GetChildSessions(ctx context.Context, parentID string) ([]db.Session, error) {
	rows, err := s.queryContext(ctx,
		"SELECT "+sessionCols+` FROM sessions
		 WHERE parent_session_id = ? AND deleted_at IS NULL
		 ORDER BY COALESCE(started_at, created_at) ASC`, parentID)
	if err != nil {
		return nil, fmt.Errorf("querying clickhouse child sessions: %w", err)
	}
	defer rows.Close()
	return scanSessionRows(rows)
}

func (s *Store) GetSessionVersion(ctx context.Context, id string) (int, int64, bool) {
	var count int
	var fileMtime sql.NullInt64
	var fileHash sql.NullString
	var updated any
	err := s.queryRowContext(ctx,
		`SELECT message_count, file_mtime, file_hash,
		        COALESCE(local_modified_at, ended_at, started_at, created_at)
		 FROM sessions WHERE id = ?`, id,
	).Scan(&count, &fileMtime, &fileHash, &updated)
	if err != nil {
		return 0, 0, false
	}
	fileMtimePart := ""
	if fileMtime.Valid {
		fileMtimePart = strconv.FormatInt(fileMtime.Int64, 10)
	}
	fileHashPart := ""
	if fileHash.Valid {
		fileHashPart = fileHash.String
	}
	return count, db.SessionVersionMarker(fileMtimePart, fileHashPart, formatDBTime(updated)), true
}

func (s *Store) FindSessionIDsByPartial(ctx context.Context, partial string, limit int) ([]string, error) {
	if partial == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 5
	}
	rows, err := s.queryContext(ctx,
		`SELECT id FROM sessions
		 WHERE position(id, ?) > 0 AND deleted_at IS NULL
		 ORDER BY `+sessionActivityExpr+` DESC
		 LIMIT ?`, partial, limit)
	if err != nil {
		return nil, fmt.Errorf("finding clickhouse sessions by partial id %q: %w", partial, err)
	}
	defer rows.Close()
	return scanIDs(rows)
}

// FindSessionIDsByRawSuffix returns IDs that equal raw or end with a literal
// colon/tilde delimiter followed by raw.
func (s *Store) FindSessionIDsByRawSuffix(ctx context.Context, raw string, limit int) ([]string, error) {
	if raw == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 5
	}
	rows, err := s.queryContext(ctx,
		`SELECT id FROM sessions
		 WHERE (id = ? OR endsWith(id, ?) OR endsWith(id, ?))
		   AND deleted_at IS NULL
		 ORDER BY (id = ?) DESC, `+sessionActivityExpr+` DESC
		 LIMIT ?`, raw, ":"+raw, "~"+raw, raw, limit)
	if err != nil {
		return nil, fmt.Errorf("finding clickhouse sessions by raw suffix %q: %w", raw, err)
	}
	defer rows.Close()
	ids, err := scanIDs(rows)
	if err != nil {
		return nil, err
	}
	if ids == nil {
		ids = []string{}
	}
	return ids, nil
}

func scanIDs(rows *sql.Rows) ([]string, error) {
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scanning clickhouse session id: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *Store) GetStats(ctx context.Context, excludeOneShot, excludeAutomated bool) (db.Stats, error) {
	query := `
		SELECT
			toInt64(COUNT(*)),
			toInt64(COALESCE(SUM(message_count), 0)),
			toInt64(COUNT(DISTINCT project)),
			toInt64(COUNT(DISTINCT machine)),
			MIN(COALESCE(started_at, created_at))
		FROM sessions
		WHERE ` + rootSessionWhere(excludeOneShot, excludeAutomated)
	var stats db.Stats
	var earliest any
	if err := s.queryRowContext(ctx, query).Scan(
		&stats.SessionCount, &stats.MessageCount,
		&stats.ProjectCount, &stats.MachineCount, &earliest,
	); err != nil {
		return db.Stats{}, fmt.Errorf("fetching clickhouse stats: %w", err)
	}
	if v := formatDBTime(earliest); v != "" {
		stats.EarliestSession = &v
	}
	return stats, nil
}

func (s *Store) GetProjects(ctx context.Context, excludeOneShot, excludeAutomated bool) ([]db.ProjectInfo, error) {
	rows, err := s.queryContext(ctx,
		`SELECT project, toInt64(COUNT(*)) FROM sessions WHERE `+
			rootSessionWhere(excludeOneShot, excludeAutomated)+
			` GROUP BY project ORDER BY project`)
	if err != nil {
		return nil, fmt.Errorf("querying clickhouse projects: %w", err)
	}
	defer rows.Close()
	var out []db.ProjectInfo
	for rows.Next() {
		var p db.ProjectInfo
		if err := rows.Scan(&p.Name, &p.SessionCount); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) GetActiveProjectLabels(ctx context.Context) ([]string, error) {
	rows, err := s.queryContext(ctx,
		`SELECT DISTINCT project FROM sessions WHERE deleted_at IS NULL ORDER BY project`)
	if err != nil {
		return nil, fmt.Errorf("querying active project labels: %w", err)
	}
	defer rows.Close()
	return scanIDs(rows)
}

func (s *Store) GetAgents(ctx context.Context, excludeOneShot, excludeAutomated bool) ([]db.AgentInfo, error) {
	rows, err := s.queryContext(ctx,
		`SELECT agent, toInt64(COUNT(*)) FROM sessions WHERE agent <> '' AND `+
			rootSessionWhere(excludeOneShot, excludeAutomated)+
			` GROUP BY agent ORDER BY agent`)
	if err != nil {
		return nil, fmt.Errorf("querying clickhouse agents: %w", err)
	}
	defer rows.Close()
	var out []db.AgentInfo
	for rows.Next() {
		var a db.AgentInfo
		if err := rows.Scan(&a.Name, &a.SessionCount); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) GetMachines(ctx context.Context, excludeOneShot, excludeAutomated bool) ([]string, error) {
	rows, err := s.queryContext(ctx,
		`SELECT DISTINCT machine FROM sessions WHERE `+
			rootSessionWhere(excludeOneShot, excludeAutomated)+
			` ORDER BY machine`)
	if err != nil {
		return nil, fmt.Errorf("querying clickhouse machines: %w", err)
	}
	defer rows.Close()
	return scanIDs(rows)
}

func (s *Store) GetBranches(ctx context.Context, excludeOneShot, excludeAutomated bool) ([]db.BranchInfo, error) {
	rows, err := s.queryContext(ctx,
		`SELECT DISTINCT project, git_branch FROM sessions WHERE `+
			rootSessionWhere(excludeOneShot, excludeAutomated)+
			` ORDER BY project, git_branch`)
	if err != nil {
		return nil, fmt.Errorf("querying clickhouse branches: %w", err)
	}
	defer rows.Close()
	out := []db.BranchInfo{}
	for rows.Next() {
		var bi db.BranchInfo
		if err := rows.Scan(&bi.Project, &bi.Branch); err != nil {
			return nil, fmt.Errorf("scanning clickhouse branch: %w", err)
		}
		bi.Token = db.EncodeBranchFilterToken(bi.Project, bi.Branch)
		out = append(out, bi)
	}
	return out, rows.Err()
}

func rootSessionWhere(excludeOneShot, excludeAutomated bool) string {
	filter := `message_count > 0
		AND relationship_type NOT IN ('subagent', 'fork')
		AND deleted_at IS NULL`
	if excludeOneShot {
		if !excludeAutomated {
			filter += " AND (user_message_count > 1 OR is_automated = true)"
		} else {
			filter += " AND user_message_count > 1"
		}
	}
	if excludeAutomated {
		filter += " AND is_automated = false"
	}
	return filter
}
