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

// EncodeCursor returns a base64-encoded, HMAC-signed cursor.
func (s *Store) EncodeCursor(
	endedAt, id string, total ...int,
) string {
	t := 0
	if len(total) > 0 {
		t = total[0]
	}
	c := db.SessionCursor{EndedAt: endedAt, ID: id, Total: t}
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
	pageBuilder := db.NewQueryBuilder(
		db.PostgresQueryDialect(), len(args),
	)
	cursorWhere := where
	if f.Cursor != "" {
		cursorWhere += " AND " +
			pageBuilder.CursorBeforePredicate(cur)
	}

	query := "SELECT " + pgSessionCols +
		" FROM sessions WHERE " + cursorWhere + `
		ORDER BY COALESCE(
			ended_at, started_at, created_at
		) DESC, id DESC
		` + pageBuilder.Limit(f.Limit+1)
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
		ea := last.CreatedAt
		if last.StartedAt != nil && *last.StartedAt != "" {
			ea = *last.StartedAt
		}
		if last.EndedAt != nil && *last.EndedAt != "" {
			ea = *last.EndedAt
		}
		page.NextCursor = s.EncodeCursor(ea, last.ID, total)
	}

	return page, nil
}

// GetSidebarSessionIndex returns the skinny session rows needed by
// the sidebar grouper. It intentionally has no cursor or limit.
func (s *Store) GetSidebarSessionIndex(
	ctx context.Context, f db.SessionFilter,
) (db.SidebarSessionIndex, error) {
	f.IncludeChildren = true
	f.Cursor = ""
	f.Limit = 0

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

	index := db.SidebarSessionIndex{
		Sessions: []db.SidebarSessionIndexRow{},
	}
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
			return db.SidebarSessionIndex{},
				fmt.Errorf(
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
		index.Sessions = append(index.Sessions, row)
	}
	if err := rows.Err(); err != nil {
		return db.SidebarSessionIndex{},
			fmt.Errorf("iterating sidebar session index: %w", err)
	}
	index.Total = len(index.Sessions)

	return index, nil
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
