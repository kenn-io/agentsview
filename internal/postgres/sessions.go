package postgres

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
)

// Store wraps a PostgreSQL connection for read-only session
// queries.
type Store struct {
	pg           *sql.DB
	cursorMu     sync.RWMutex
	cursorSecret []byte

	pricingMu     sync.Mutex
	pricingLoadMu sync.Mutex
	pricingLoad   *pricingLoad
	customPricing map[string]config.CustomModelRate
}

// pgSessionCols is the column list for standard PG session
// queries. PG has no file_path, file_size, file_mtime,
// file_hash, or local_modified_at columns.
const pgSessionCols = `id, project, machine, agent,
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
	quality_signal_version,
	short_prompt_count, unstructured_start,
	missing_success_criteria_count,
	missing_verification_count, duplicate_prompt_count,
	no_code_context_count, runaway_tool_loop_count,
	data_version,
	cwd, git_branch, source_session_id, source_version,
	parser_malformed_lines, is_truncated,
	secret_leak_count, secrets_rules_version,
	deleted_at, termination_status`

// paramBuilder generates numbered PostgreSQL placeholders.
type paramBuilder struct {
	n    int
	args []any
}

func (pb *paramBuilder) add(v any) string {
	pb.n++
	pb.args = append(pb.args, v)
	return fmt.Sprintf("$%d", pb.n)
}

func normalizePGAutomatedScope(
	scope string,
	excludeAutomated bool,
) string {
	switch strings.TrimSpace(scope) {
	case "human", "all", "automated":
		return strings.TrimSpace(scope)
	}
	if excludeAutomated {
		return "human"
	}
	return "all"
}

func pgAutomatedScopePredicate(scope, col string) string {
	switch scope {
	case "human":
		return col + " = FALSE"
	case "automated":
		return col + " = TRUE"
	default:
		return ""
	}
}

// pgActivityWindows holds the cutoff durations used by
// pgTerminationPred. Kept in sync with the SQLite-side constants
// in internal/db/sessions.go so both stores classify a session
// the same way at the same wall-clock time.
const (
	pgActiveWindow = 10 * time.Minute
	pgStaleWindow  = 60 * time.Minute
)

// pgActivityExpr returns the COALESCEd activity timestamp
// expression used to compute a session's effective recency.
const pgActivityExpr = "COALESCE(ended_at, started_at, created_at)"

const pgSidebarActivityExprS = "COALESCE(s.ended_at, s.started_at, s.created_at)"

func pgSidebarStarredRootCTE(enabled bool) string {
	if !enabled {
		return ""
	}
	return `,
		eligible_roots(id) AS (
			SELECT DISTINCT t.root_id
			FROM tree t
			JOIN starred_sessions ss ON ss.session_id = t.id
		)`
}

func pgSidebarStarredRootJoin(enabled bool) string {
	if !enabled {
		return ""
	}
	return "JOIN eligible_roots e ON e.id = t.root_id"
}

// pgTerminationPred returns a WHERE fragment for the multi-state
// termination filter (active / stale / unclean). The status value
// may be comma-separated to OR multiple states. Returns "" when
// status is empty or "all".
//
// Stale and unclean both require a parser red flag — sessions with
// termination_status NULL or 'clean' never appear under those
// filters, so a short-lived agent that completes normally never
// generates a yellow false-positive once it ages past 10 minutes.
func pgTerminationPred(status string, pb *paramBuilder) string {
	if status == "" || status == "all" {
		return ""
	}
	now := time.Now().UTC()
	activeCutoff := now.Add(-pgActiveWindow)
	staleCutoff := now.Add(-pgStaleWindow)
	const flagged = "termination_status IN ('tool_call_pending', 'truncated')"

	parts := strings.Split(status, ",")
	preds := make([]string, 0, len(parts))
	for _, p := range parts {
		switch strings.TrimSpace(p) {
		case "active":
			preds = append(preds,
				pgActivityExpr+" > "+pb.add(activeCutoff))
		case "stale":
			preds = append(preds, "("+
				pgActivityExpr+" > "+pb.add(staleCutoff)+
				" AND "+pgActivityExpr+" <= "+pb.add(activeCutoff)+
				" AND "+flagged+")")
		case "unclean":
			preds = append(preds, "("+
				pgActivityExpr+" <= "+pb.add(staleCutoff)+
				" AND "+flagged+")")
		case "clean":
			preds = append(preds, "termination_status = 'clean'")
		case "awaiting_user":
			preds = append(preds,
				"termination_status = 'awaiting_user'")
		}
	}
	if len(preds) == 0 {
		return ""
	}
	if len(preds) == 1 {
		return preds[0]
	}
	return "(" + strings.Join(preds, " OR ") + ")"
}

// scanPGSession scans a row with pgSessionCols into a
// db.Session, converting TIMESTAMPTZ columns to string.
func scanPGSession(
	rs interface{ Scan(...any) error },
) (db.Session, error) {
	var s db.Session
	var createdAt *time.Time
	var startedAt, endedAt, deletedAt *time.Time
	err := rs.Scan(
		&s.ID, &s.Project, &s.Machine, &s.Agent,
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
		&s.QualitySignalVersion,
		&s.ShortPromptCount, &s.UnstructuredStart,
		&s.MissingSuccessCriteriaCount,
		&s.MissingVerificationCount, &s.DuplicatePromptCount,
		&s.NoCodeContextCount, &s.RunawayToolLoopCount,
		&s.DataVersion,
		&s.Cwd, &s.GitBranch,
		&s.SourceSessionID, &s.SourceVersion,
		&s.ParserMalformedLines, &s.IsTruncated,
		&s.SecretLeakCount, &s.SecretsRulesVersion,
		&deletedAt, &s.TerminationStatus,
	)
	if err != nil {
		return s, err
	}
	if createdAt != nil {
		s.CreatedAt = FormatISO8601(*createdAt)
	}
	if startedAt != nil {
		str := FormatISO8601(*startedAt)
		s.StartedAt = &str
	}
	if endedAt != nil {
		str := FormatISO8601(*endedAt)
		s.EndedAt = &str
	}
	if deletedAt != nil {
		str := FormatISO8601(*deletedAt)
		s.DeletedAt = &str
	}
	return s, nil
}

// scanPGSessionRows iterates rows and scans each.
func scanPGSessionRows(
	rows *sql.Rows,
) ([]db.Session, error) {
	sessions := []db.Session{}
	for rows.Next() {
		s, err := scanPGSession(rows)
		if err != nil {
			return nil, fmt.Errorf(
				"scanning session: %w", err,
			)
		}
		sessions = append(sessions, s)
	}
	return sessions, rows.Err()
}

// pgRootSessionFilter is the base WHERE clause for root
// sessions.
const pgRootSessionFilter = `message_count > 0
	AND relationship_type NOT IN ('subagent', 'fork')
	AND deleted_at IS NULL`

// buildPGSessionFilter returns a WHERE clause with $N
// placeholders and the corresponding args.
func buildPGSessionFilter(
	f db.SessionFilter,
) (string, []any) {
	return db.BuildSessionFilterSQL(f, db.PostgresQueryDialect())
}

func buildPGSessionBaseFilter(
	f db.SessionFilter,
) (string, []any) {
	return db.BuildSessionBaseFilterSQL(f, db.PostgresQueryDialect())
}

// EncodeCursor returns a base64-encoded, HMAC-signed cursor.
func (s *Store) EncodeCursor(c db.SessionCursor) string {
	data, _ := json.Marshal(c)

	s.cursorMu.RLock()
	secret := make([]byte, len(s.cursorSecret))
	copy(secret, s.cursorSecret)
	s.cursorMu.RUnlock()

	mac := hmac.New(sha256.New, secret)
	mac.Write(data)
	sig := mac.Sum(nil)

	return base64.RawURLEncoding.EncodeToString(data) + "." +
		base64.RawURLEncoding.EncodeToString(sig)
}

// DecodeCursor parses a base64-encoded cursor string.
func (s *Store) DecodeCursor(
	raw string,
) (db.SessionCursor, error) {
	parts := strings.Split(raw, ".")
	if len(parts) == 1 {
		data, err := base64.RawURLEncoding.DecodeString(
			parts[0],
		)
		if err != nil {
			return db.SessionCursor{},
				fmt.Errorf("%w: %v",
					db.ErrInvalidCursor, err)
		}
		var c db.SessionCursor
		if err := json.Unmarshal(data, &c); err != nil {
			return db.SessionCursor{},
				fmt.Errorf("%w: %v",
					db.ErrInvalidCursor, err)
		}
		c.Total = 0
		return c, nil
	} else if len(parts) != 2 {
		return db.SessionCursor{},
			fmt.Errorf("%w: invalid format",
				db.ErrInvalidCursor)
	}

	payload := parts[0]
	sigStr := parts[1]

	data, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return db.SessionCursor{},
			fmt.Errorf("%w: invalid payload: %v",
				db.ErrInvalidCursor, err)
	}

	sig, err := base64.RawURLEncoding.DecodeString(sigStr)
	if err != nil {
		return db.SessionCursor{},
			fmt.Errorf(
				"%w: invalid signature encoding: %v",
				db.ErrInvalidCursor, err)
	}

	s.cursorMu.RLock()
	secret := make([]byte, len(s.cursorSecret))
	copy(secret, s.cursorSecret)
	s.cursorMu.RUnlock()

	mac := hmac.New(sha256.New, secret)
	mac.Write(data)
	expectedSig := mac.Sum(nil)

	if !hmac.Equal(sig, expectedSig) {
		return db.SessionCursor{},
			fmt.Errorf("%w: signature mismatch",
				db.ErrInvalidCursor)
	}

	var c db.SessionCursor
	if err := json.Unmarshal(data, &c); err != nil {
		return db.SessionCursor{},
			fmt.Errorf("%w: invalid json: %v",
				db.ErrInvalidCursor, err)
	}
	return c, nil
}

// ListSessions returns a cursor-paginated list of sessions.
func (s *Store) ListSessions(
	ctx context.Context, f db.SessionFilter,
) (db.SessionPage, error) {
	if f.Limit <= 0 || f.Limit > db.MaxSessionLimit {
		f.Limit = db.DefaultSessionLimit
	}

	where, args := buildPGSessionFilter(f)

	dialect := db.PostgresQueryDialect()
	rs := db.ResolveSort(f)

	var total int
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
		countQ := "SELECT COUNT(*) FROM sessions WHERE " +
			where
		if err := s.pg.QueryRowContext(
			ctx, countQ, args...,
		).Scan(&total); err != nil {
			return db.SessionPage{},
				fmt.Errorf("counting sessions: %w", err)
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
		cursorWhere += " AND " + pageBuilder.CursorPredicate(
			rs, f, vals, cur.ID,
		)
	}

	query := "SELECT " + pgSessionCols +
		" FROM sessions WHERE " + cursorWhere + " " +
		pageBuilder.OrderByClause(rs, f) + " " +
		pageBuilder.Limit(f.Limit+1)
	cursorArgs = append(cursorArgs, pageBuilder.Args()...)

	rows, err := s.pg.QueryContext(
		ctx, query, cursorArgs...,
	)
	if err != nil {
		return db.SessionPage{},
			fmt.Errorf("querying sessions: %w", err)
	}
	defer rows.Close()

	sessions, err := scanPGSessionRows(rows)
	if err != nil {
		return db.SessionPage{}, err
	}

	page := db.SessionPage{
		Sessions: sessions, Total: total,
	}
	if len(sessions) > f.Limit {
		page.Sessions = sessions[:f.Limit]
		last := page.Sessions[f.Limit-1]
		page.NextCursor = s.EncodeCursor(
			db.NextSessionCursor(&last, rs, total, f),
		)
	}

	return page, nil
}

// GetSidebarSessionIndex returns the skinny session rows needed by
// the sidebar grouper. Paginated calls page root sessions and include
// each root's descendants so grouped sidebar trees stay complete.
func (s *Store) GetSidebarSessionIndex(
	ctx context.Context, f db.SessionFilter,
) (db.SidebarSessionIndex, error) {
	f.IncludeChildren = true
	f.IncludeOrphans = true

	if f.Limit > 0 || f.Cursor != "" || f.Starred {
		return s.getSidebarSessionIndexPage(ctx, f)
	}

	f.Cursor = ""

	where, args := buildPGSessionFilter(f)
	query := `
		SELECT
			id,
			parent_session_id,
			relationship_type,
			project,
			machine,
			agent,
			COALESCE(display_name, session_name) AS display_name,
			started_at,
			ended_at,
			created_at,
			termination_status,
			message_count,
			user_message_count,
			is_automated,
			position('<teammate-message' in COALESCE(first_message, '')) > 0
		FROM sessions
		WHERE ` + where + `
		ORDER BY COALESCE(
			ended_at, started_at, created_at
		) DESC, id DESC`

	rows, err := s.pg.QueryContext(ctx, query, args...)
	if err != nil {
		return db.SidebarSessionIndex{},
			fmt.Errorf("querying sidebar session index: %w", err)
	}
	defer rows.Close()

	sessions, err := scanPGSidebarSessionIndexRows(rows)
	if err != nil {
		return db.SidebarSessionIndex{}, err
	}
	index := db.SidebarSessionIndex{
		Sessions: sessions,
		Total:    len(sessions),
	}

	return index, nil
}

func (s *Store) getSidebarSessionIndexPage(
	ctx context.Context, f db.SessionFilter,
) (db.SidebarSessionIndex, error) {
	if f.Limit <= 0 || f.Limit > db.MaxSessionLimit {
		f.Limit = db.DefaultSessionLimit
	}

	rootFilter := f
	rootFilter.IncludeChildren = false
	rootFilter.Cursor = ""
	rootFilter.Starred = false
	rootWhere, rootArgs := buildPGSessionBaseFilter(rootFilter)
	canonicalRootWhere := db.BuildCanonicalRootWhere(db.PostgresQueryDialect(), "sessions", f.IncludeOrphans)

	var total int
	var cur db.SessionCursor
	if f.Cursor != "" {
		var err error
		cur, err = s.DecodeCursor(f.Cursor)
		if err != nil {
			return db.SidebarSessionIndex{}, err
		}
		total = cur.Total
	}
	if total <= 0 {
		if f.Starred {
			countQuery := `
				WITH RECURSIVE root_candidates(id) AS (
					SELECT id
					FROM sessions
					WHERE ` + rootWhere + `
					  AND ` + canonicalRootWhere + `
				),
				tree(root_id, id) AS (
					SELECT id, id FROM root_candidates
					UNION
					SELECT t.root_id, s.id
					FROM sessions s
					JOIN tree t ON s.parent_session_id = t.id
					WHERE s.message_count > 0
					  AND s.deleted_at IS NULL
				),
				eligible_roots(id) AS (
					SELECT DISTINCT t.root_id
					FROM tree t
					JOIN starred_sessions ss ON ss.session_id = t.id
				)
				SELECT COUNT(*) FROM eligible_roots`
			if err := s.pg.QueryRowContext(
				ctx, countQuery, rootArgs...,
			).Scan(&total); err != nil {
				return db.SidebarSessionIndex{},
					fmt.Errorf("counting sidebar roots: %w", err)
			}
		} else {
			countQuery := "SELECT COUNT(*) FROM sessions WHERE " +
				rootWhere + " AND " + canonicalRootWhere
			if err := s.pg.QueryRowContext(
				ctx, countQuery, rootArgs...,
			).Scan(&total); err != nil {
				return db.SidebarSessionIndex{},
					fmt.Errorf("counting sidebar roots: %w", err)
			}
		}
	}

	pageBuilder := db.NewQueryBuilder(
		db.PostgresQueryDialect(), len(rootArgs),
	)
	cursorWhere := ""
	if f.Cursor != "" {
		cursorWhere = "WHERE (activity, id) < (" +
			pageBuilder.Add(cur.EndedAt) + "::timestamptz, " +
			pageBuilder.Add(cur.ID) + ")"
	}
	rootQuery := `
		WITH RECURSIVE root_candidates(id) AS (
			SELECT id
			FROM sessions
			WHERE ` + rootWhere + `
			  AND ` + canonicalRootWhere + `
		),
		tree(root_id, id) AS (
			SELECT id, id FROM root_candidates
			UNION
			SELECT t.root_id, s.id
			FROM sessions s
			JOIN tree t ON s.parent_session_id = t.id
			WHERE s.message_count > 0
			  AND s.deleted_at IS NULL
		)
		` + pgSidebarStarredRootCTE(f.Starred) + `,
		root_activity(id, activity) AS (
			SELECT t.root_id AS id, MAX(` + pgSidebarActivityExprS + `) AS activity
			FROM tree t
			` + pgSidebarStarredRootJoin(f.Starred) + `
			JOIN sessions s ON s.id = t.id
			GROUP BY t.root_id
		)
		SELECT id, activity
		FROM root_activity
		` + cursorWhere + `
		ORDER BY activity DESC, id DESC
		` + pageBuilder.Limit(f.Limit+1)
	rootQueryArgs := append([]any{}, rootArgs...)
	rootQueryArgs = append(rootQueryArgs, pageBuilder.Args()...)

	rows, err := s.pg.QueryContext(ctx, rootQuery, rootQueryArgs...)
	if err != nil {
		return db.SidebarSessionIndex{},
			fmt.Errorf("querying sidebar root page: %w", err)
	}
	defer rows.Close()

	type rootRow struct {
		id       string
		activity time.Time
	}
	roots := []rootRow{}
	for rows.Next() {
		var row rootRow
		if err := rows.Scan(&row.id, &row.activity); err != nil {
			return db.SidebarSessionIndex{},
				fmt.Errorf("scanning sidebar root page: %w", err)
		}
		roots = append(roots, row)
	}
	if err := rows.Err(); err != nil {
		return db.SidebarSessionIndex{},
			fmt.Errorf("iterating sidebar root page: %w", err)
	}

	index := db.SidebarSessionIndex{
		Sessions: []db.SidebarSessionIndexRow{},
		Total:    total,
	}
	if len(roots) == 0 {
		return index, nil
	}
	selected := roots
	if len(roots) > f.Limit {
		selected = roots[:f.Limit]
		last := selected[f.Limit-1]
		index.NextCursor = s.EncodeCursor(db.SessionCursor{
			EndedAt: FormatISO8601(last.activity),
			ID:      last.id,
			Total:   total,
		})
	}

	page := db.NewQueryBuilder(db.PostgresQueryDialect(), 0)
	cteParts := make([]string, 0, len(selected))
	treeArgs := make([]any, 0, len(selected)*2)
	for i, root := range selected {
		id := page.Add(root.id)
		ord := page.Add(i)
		if i == 0 {
			cteParts = append(cteParts,
				"SELECT "+id+"::text AS id, "+ord+"::integer AS ord")
		} else {
			cteParts = append(cteParts,
				"UNION ALL SELECT "+id+"::text, "+ord+"::integer")
		}
	}
	treeArgs = append(treeArgs, page.Args()...)

	treeQuery := `
		WITH RECURSIVE root_page(id, ord) AS (
			` + strings.Join(cteParts, "\n") + `
		),
		tree(id, ord) AS (
			SELECT id, ord FROM root_page
			UNION
			SELECT s.id, t.ord
			FROM sessions s
			JOIN tree t ON s.parent_session_id = t.id
			WHERE s.message_count > 0
			  AND s.deleted_at IS NULL
		),
		ranked_tree(id, ord) AS (
			SELECT id, MIN(ord) AS ord
			FROM tree
			GROUP BY id
		)
		SELECT
			s.id,
			s.parent_session_id,
			s.relationship_type,
			s.project,
			s.machine,
			s.agent,
			COALESCE(s.display_name, s.session_name) AS display_name,
			s.started_at,
			s.ended_at,
			s.created_at,
			s.termination_status,
			s.message_count,
			s.user_message_count,
			s.is_automated,
			position('<teammate-message' in COALESCE(s.first_message, '')) > 0
		FROM sessions s
		JOIN ranked_tree t ON s.id = t.id
		ORDER BY
			t.ord ASC,
			` + pgSidebarActivityExprS + ` DESC,
			s.id DESC`

	rows, err = s.pg.QueryContext(ctx, treeQuery, treeArgs...)
	if err != nil {
		return db.SidebarSessionIndex{},
			fmt.Errorf("querying sidebar tree page: %w", err)
	}
	defer rows.Close()

	index.Sessions, err = scanPGSidebarSessionIndexRows(rows)
	if err != nil {
		return db.SidebarSessionIndex{}, err
	}
	return index, nil
}

func scanPGSidebarSessionIndexRows(
	rows *sql.Rows,
) ([]db.SidebarSessionIndexRow, error) {
	sessions := []db.SidebarSessionIndexRow{}
	for rows.Next() {
		var row db.SidebarSessionIndexRow
		var startedAt, endedAt, createdAt *time.Time
		if err := rows.Scan(
			&row.ID,
			&row.ParentSessionID,
			&row.RelationshipType,
			&row.Project,
			&row.Machine,
			&row.Agent,
			&row.DisplayName,
			&startedAt,
			&endedAt,
			&createdAt,
			&row.TerminationStatus,
			&row.MessageCount,
			&row.UserMessageCount,
			&row.IsAutomated,
			&row.IsTeammate,
		); err != nil {
			return nil, fmt.Errorf(
				"scanning sidebar session index: %w",
				err,
			)
		}
		if startedAt != nil {
			str := FormatISO8601(*startedAt)
			row.StartedAt = &str
		}
		if endedAt != nil {
			str := FormatISO8601(*endedAt)
			row.EndedAt = &str
		}
		if createdAt != nil {
			row.CreatedAt = FormatISO8601(*createdAt)
		}
		sessions = append(sessions, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating sidebar session index: %w", err)
	}
	return sessions, nil
}

// GetSession returns a single session by ID, excluding
// soft-deleted sessions.
func (s *Store) GetSession(
	ctx context.Context, id string,
) (*db.Session, error) {
	row := s.pg.QueryRowContext(
		ctx,
		"SELECT "+pgSessionCols+
			" FROM sessions WHERE id = $1"+
			" AND deleted_at IS NULL",
		id,
	)
	sess, err := scanPGSession(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf(
			"getting session %s: %w", id, err,
		)
	}
	return &sess, nil
}

// FindSessionIDsByRawSuffix returns up to limit session IDs whose
// stored id is either the exact raw input or the raw input preceded
// by an agent prefix. The suffix comparison is literal and results
// match SQLite ordering: exact match first, then most recent session.
func (s *Store) FindSessionIDsByRawSuffix(
	ctx context.Context, raw string, limit int,
) ([]string, error) {
	if raw == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 5
	}
	rows, err := s.pg.QueryContext(ctx,
		`SELECT id FROM sessions
		 WHERE (id = $1
		        OR RIGHT(id, LENGTH($1) + 1) = ':' || $1)
		   AND deleted_at IS NULL
		 ORDER BY (id = $1) DESC,
		          COALESCE(ended_at, started_at, created_at) DESC
		 LIMIT $2`,
		raw, limit,
	)
	if err != nil {
		return nil, fmt.Errorf(
			"finding pg sessions by raw suffix %q: %w",
			raw, err,
		)
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf(
				"scanning pg session id: %w", err,
			)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf(
			"iterating pg raw suffix session ids: %w", err,
		)
	}
	return ids, nil
}

// GetSessionFull returns a single session by ID including
// soft-deleted sessions.
func (s *Store) GetSessionFull(
	ctx context.Context, id string,
) (*db.Session, error) {
	row := s.pg.QueryRowContext(
		ctx,
		"SELECT "+pgSessionCols+
			" FROM sessions WHERE id = $1",
		id,
	)
	sess, err := scanPGSession(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf(
			"getting session full %s: %w", id, err,
		)
	}
	return &sess, nil
}

// GetChildSessions returns sessions whose
// parent_session_id matches the given parentID.
func (s *Store) GetChildSessions(
	ctx context.Context, parentID string,
) ([]db.Session, error) {
	query := "SELECT " + pgSessionCols +
		" FROM sessions" +
		" WHERE parent_session_id = $1" +
		" AND deleted_at IS NULL" +
		" ORDER BY COALESCE(started_at, created_at) ASC"
	rows, err := s.pg.QueryContext(ctx, query, parentID)
	if err != nil {
		return nil, fmt.Errorf(
			"querying child sessions for %s: %w",
			parentID, err,
		)
	}
	defer rows.Close()

	return scanPGSessionRows(rows)
}

// GetStats returns database statistics, counting only root
// sessions with messages.
func (s *Store) GetStats(
	ctx context.Context,
	excludeOneShot, excludeAutomated bool,
) (db.Stats, error) {
	filter := pgRootSessionFilter
	if excludeOneShot {
		if !excludeAutomated {
			filter += " AND (user_message_count > 1 OR is_automated = TRUE)"
		} else {
			filter += " AND user_message_count > 1"
		}
	}
	if excludeAutomated {
		filter += " AND is_automated = FALSE"
	}
	query := fmt.Sprintf(`
		SELECT
			(SELECT COUNT(*) FROM sessions
			 WHERE %s),
			(SELECT COALESCE(SUM(message_count), 0)
			 FROM sessions WHERE %s),
			(SELECT COUNT(DISTINCT project) FROM sessions
			 WHERE %s),
			(SELECT COUNT(DISTINCT machine) FROM sessions
			 WHERE %s),
			(SELECT MIN(COALESCE(started_at, created_at))
			 FROM sessions
			 WHERE %s)`,
		filter, filter, filter, filter, filter)

	var st db.Stats
	var earliest *time.Time
	err := s.pg.QueryRowContext(ctx, query).Scan(
		&st.SessionCount,
		&st.MessageCount,
		&st.ProjectCount,
		&st.MachineCount,
		&earliest,
	)
	if err != nil {
		return db.Stats{},
			fmt.Errorf("fetching stats: %w", err)
	}
	if earliest != nil {
		str := FormatISO8601(*earliest)
		st.EarliestSession = &str
	}
	return st, nil
}

// GetProjects returns project names with session counts.
func (s *Store) GetProjects(
	ctx context.Context,
	excludeOneShot, excludeAutomated bool,
) ([]db.ProjectInfo, error) {
	q := `SELECT project, COUNT(*) as session_count
		FROM sessions
		WHERE message_count > 0
		  AND relationship_type NOT IN ('subagent', 'fork')
		  AND deleted_at IS NULL`
	if excludeOneShot {
		if !excludeAutomated {
			q += " AND (user_message_count > 1 OR is_automated = TRUE)"
		} else {
			q += " AND user_message_count > 1"
		}
	}
	if excludeAutomated {
		q += " AND is_automated = FALSE"
	}
	q += " GROUP BY project ORDER BY project"
	rows, err := s.pg.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf(
			"querying projects: %w", err,
		)
	}
	defer rows.Close()

	projects := []db.ProjectInfo{}
	for rows.Next() {
		var pi db.ProjectInfo
		if err := rows.Scan(
			&pi.Name, &pi.SessionCount,
		); err != nil {
			return nil, fmt.Errorf(
				"scanning project: %w", err,
			)
		}
		projects = append(projects, pi)
	}
	return projects, rows.Err()
}

// GetAgents returns distinct agent names with session counts.
func (s *Store) GetAgents(
	ctx context.Context,
	excludeOneShot, excludeAutomated bool,
) ([]db.AgentInfo, error) {
	q := `SELECT agent, COUNT(*) as session_count
		FROM sessions
		WHERE message_count > 0 AND agent <> ''
		  AND deleted_at IS NULL
		  AND relationship_type NOT IN ('subagent', 'fork')`
	if excludeOneShot {
		if !excludeAutomated {
			q += " AND (user_message_count > 1 OR is_automated = TRUE)"
		} else {
			q += " AND user_message_count > 1"
		}
	}
	if excludeAutomated {
		q += " AND is_automated = FALSE"
	}
	q += " GROUP BY agent ORDER BY agent"
	rows, err := s.pg.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf(
			"querying agents: %w", err,
		)
	}
	defer rows.Close()

	agents := []db.AgentInfo{}
	for rows.Next() {
		var a db.AgentInfo
		if err := rows.Scan(
			&a.Name, &a.SessionCount,
		); err != nil {
			return nil, fmt.Errorf(
				"scanning agent: %w", err,
			)
		}
		agents = append(agents, a)
	}
	return agents, rows.Err()
}

// GetMachines returns distinct machine names.
func (s *Store) GetMachines(
	ctx context.Context,
	excludeOneShot, excludeAutomated bool,
) ([]string, error) {
	q := `SELECT DISTINCT machine FROM sessions
		WHERE deleted_at IS NULL`
	if excludeOneShot {
		if !excludeAutomated {
			q += " AND (user_message_count > 1 OR is_automated = TRUE)"
		} else {
			q += " AND user_message_count > 1"
		}
	}
	if excludeAutomated {
		q += " AND is_automated = FALSE"
	}
	q += " ORDER BY machine"
	rows, err := s.pg.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	machines := []string{}
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			return nil, err
		}
		machines = append(machines, m)
	}
	return machines, rows.Err()
}
