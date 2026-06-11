package duckdb

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/secrets"
)

// Compile-time check: *Store satisfies db.Store.
var _ db.Store = (*Store)(nil)

// Store wraps a DuckDB connection for read-mostly serve mode.
type Store struct {
	duck          *sql.DB
	cursorMu      sync.RWMutex
	cursorSecret  []byte
	customPricing map[string]config.CustomModelRate
}

// NewStore opens a local DuckDB mirror file as a db.Store.
func NewStore(path string) (*Store, error) {
	conn, err := Open(path)
	if err != nil {
		return nil, err
	}
	return &Store{duck: conn}, nil
}

// NewStoreFromDB wraps an existing DuckDB connection.
func NewStoreFromDB(conn *sql.DB) *Store { return &Store{duck: conn} }

func (s *Store) DB() *sql.DB { return s.duck }

func (s *Store) Close() error { return s.duck.Close() }

func (s *Store) SetCustomPricing(p map[string]config.CustomModelRate) {
	s.customPricing = p
}

func (s *Store) SetCursorSecret(secret []byte) {
	s.cursorMu.Lock()
	defer s.cursorMu.Unlock()
	s.cursorSecret = append([]byte(nil), secret...)
}

func (s *Store) ReadOnly() bool { return true }

const duckSessionCols = `id, project, machine, agent,
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

func scanSession(rs interface{ Scan(...any) error }) (db.Session, error) {
	var s db.Session
	var createdAt any
	var startedAt, endedAt, deletedAt any
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
	sessions := []db.Session{}
	for rows.Next() {
		s, err := scanSession(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning duckdb session: %w", err)
		}
		sessions = append(sessions, s)
	}
	return sessions, rows.Err()
}

func formatDBTime(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case time.Time:
		return t.UTC().Format(time.RFC3339Nano)
	case string:
		return t
	case []byte:
		return string(t)
	default:
		return fmt.Sprint(t)
	}
}

func (s *Store) EncodeCursor(endedAt, id string, total ...int) string {
	t := 0
	if len(total) > 0 {
		t = total[0]
	}
	c := db.SessionCursor{EndedAt: endedAt, ID: id, Total: t}
	data, _ := json.Marshal(c)
	s.cursorMu.RLock()
	secret := append([]byte(nil), s.cursorSecret...)
	s.cursorMu.RUnlock()
	mac := hmac.New(sha256.New, secret)
	mac.Write(data)
	sig := mac.Sum(nil)
	return base64.RawURLEncoding.EncodeToString(data) + "." +
		base64.RawURLEncoding.EncodeToString(sig)
}

func (s *Store) DecodeCursor(raw string) (db.SessionCursor, error) {
	parts := strings.Split(raw, ".")
	if len(parts) == 1 {
		data, err := base64.RawURLEncoding.DecodeString(parts[0])
		if err != nil {
			return db.SessionCursor{}, fmt.Errorf("%w: %v", db.ErrInvalidCursor, err)
		}
		var c db.SessionCursor
		if err := json.Unmarshal(data, &c); err != nil {
			return db.SessionCursor{}, fmt.Errorf("%w: %v", db.ErrInvalidCursor, err)
		}
		c.Total = 0
		return c, nil
	}
	if len(parts) != 2 {
		return db.SessionCursor{}, fmt.Errorf("%w: invalid format", db.ErrInvalidCursor)
	}
	data, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return db.SessionCursor{}, fmt.Errorf("%w: invalid payload: %v", db.ErrInvalidCursor, err)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return db.SessionCursor{}, fmt.Errorf("%w: invalid signature: %v", db.ErrInvalidCursor, err)
	}
	s.cursorMu.RLock()
	secret := append([]byte(nil), s.cursorSecret...)
	s.cursorMu.RUnlock()
	mac := hmac.New(sha256.New, secret)
	mac.Write(data)
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return db.SessionCursor{}, fmt.Errorf("%w: signature mismatch", db.ErrInvalidCursor)
	}
	var c db.SessionCursor
	if err := json.Unmarshal(data, &c); err != nil {
		return db.SessionCursor{}, fmt.Errorf("%w: invalid json: %v", db.ErrInvalidCursor, err)
	}
	return c, nil
}

func (s *Store) ListSessions(ctx context.Context, f db.SessionFilter) (db.SessionPage, error) {
	if f.Limit <= 0 || f.Limit > db.MaxSessionLimit {
		f.Limit = db.DefaultSessionLimit
	}
	where, args := db.BuildSessionFilterSQL(f, db.DuckDBQueryDialect())
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
		if err := s.duck.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM sessions WHERE "+where,
			args...,
		).Scan(&total); err != nil {
			return db.SessionPage{}, fmt.Errorf("counting duckdb sessions: %w", err)
		}
	}
	cursorArgs := append([]any{}, args...)
	pageBuilder := db.NewQueryBuilder(db.DuckDBQueryDialect(), len(args))
	cursorWhere := where
	if f.Cursor != "" {
		cursorWhere += " AND " + pageBuilder.CursorBeforePredicate(cur)
	}
	query := "SELECT " + duckSessionCols +
		" FROM sessions WHERE " + cursorWhere + `
		ORDER BY COALESCE(ended_at, started_at, created_at) DESC, id DESC
		` + pageBuilder.Limit(f.Limit+1)
	cursorArgs = append(cursorArgs, pageBuilder.Args()...)
	rows, err := s.duck.QueryContext(ctx, query, cursorArgs...)
	if err != nil {
		return db.SessionPage{}, fmt.Errorf("querying duckdb sessions: %w", err)
	}
	defer rows.Close()
	sessions, err := scanSessionRows(rows)
	if err != nil {
		return db.SessionPage{}, err
	}
	page := db.SessionPage{Sessions: sessions, Total: total}
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

func (s *Store) GetSidebarSessionIndex(ctx context.Context, f db.SessionFilter) (db.SidebarSessionIndex, error) {
	f.IncludeChildren = true
	f.Cursor = ""
	f.Limit = 0

	where, args := db.BuildSessionFilterSQL(f, db.DuckDBQueryDialect())
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

	rows, err := s.duck.QueryContext(ctx, query, args...)
	if err != nil {
		return db.SidebarSessionIndex{},
			fmt.Errorf("querying duckdb sidebar session index: %w", err)
	}
	defer rows.Close()

	index := db.SidebarSessionIndex{
		Sessions: []db.SidebarSessionIndexRow{},
	}
	for rows.Next() {
		var row db.SidebarSessionIndexRow
		var startedAt, endedAt, createdAt any
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
					"scanning duckdb sidebar session index: %w",
					err,
				)
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
		return db.SidebarSessionIndex{},
			fmt.Errorf("iterating duckdb sidebar session index: %w", err)
	}
	index.Total = len(index.Sessions)

	return index, nil
}

func (s *Store) GetSession(ctx context.Context, id string) (*db.Session, error) {
	row := s.duck.QueryRowContext(ctx,
		"SELECT "+duckSessionCols+" FROM sessions WHERE id = ? AND deleted_at IS NULL",
		id,
	)
	sess, err := scanSession(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("getting duckdb session: %w", err)
	}
	return &sess, nil
}

func (s *Store) GetSessionFull(ctx context.Context, id string) (*db.Session, error) {
	row := s.duck.QueryRowContext(ctx,
		"SELECT "+duckSessionCols+" FROM sessions WHERE id = ?",
		id,
	)
	sess, err := scanSession(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("getting duckdb full session: %w", err)
	}
	return &sess, nil
}

func (s *Store) GetChildSessions(ctx context.Context, parentID string) ([]db.Session, error) {
	rows, err := s.duck.QueryContext(ctx,
		"SELECT "+duckSessionCols+` FROM sessions
		 WHERE parent_session_id = ? AND deleted_at IS NULL
		 ORDER BY COALESCE(started_at, created_at) ASC`,
		parentID,
	)
	if err != nil {
		return nil, fmt.Errorf("querying duckdb child sessions: %w", err)
	}
	defer rows.Close()
	return scanSessionRows(rows)
}

func (s *Store) GetSessionVersion(id string) (int, int64, bool) {
	var count int
	var updated any
	err := s.duck.QueryRow(
		`SELECT message_count, COALESCE(local_modified_at, ended_at, started_at, created_at)
		 FROM sessions WHERE id = ?`,
		id,
	).Scan(&count, &updated)
	if err != nil {
		return 0, 0, false
	}
	formatted := formatDBTime(updated)
	var h int64
	for _, c := range formatted {
		h = h*31 + int64(c)
	}
	return count, h, true
}

func (s *Store) GetStats(ctx context.Context, excludeOneShot, excludeAutomated bool) (db.Stats, error) {
	filter := rootSessionWhere(excludeOneShot, excludeAutomated)
	query := fmt.Sprintf(`
		SELECT
			(SELECT COUNT(*) FROM sessions WHERE %s),
			(SELECT COALESCE(SUM(message_count), 0) FROM sessions WHERE %s),
			(SELECT COUNT(DISTINCT project) FROM sessions WHERE %s),
			(SELECT COUNT(DISTINCT machine) FROM sessions WHERE %s),
			(SELECT MIN(COALESCE(started_at, created_at)) FROM sessions WHERE %s)`,
		filter, filter, filter, filter, filter)
	var stats db.Stats
	var earliest any
	if err := s.duck.QueryRowContext(ctx, query).Scan(
		&stats.SessionCount, &stats.MessageCount,
		&stats.ProjectCount, &stats.MachineCount, &earliest,
	); err != nil {
		return db.Stats{}, fmt.Errorf("fetching duckdb stats: %w", err)
	}
	if v := formatDBTime(earliest); v != "" {
		stats.EarliestSession = &v
	}
	return stats, nil
}

func (s *Store) GetProjects(ctx context.Context, excludeOneShot, excludeAutomated bool) ([]db.ProjectInfo, error) {
	rows, err := s.duck.QueryContext(ctx,
		`SELECT project, COUNT(*) FROM sessions WHERE `+
			rootSessionWhere(excludeOneShot, excludeAutomated)+
			` GROUP BY project ORDER BY project`,
	)
	if err != nil {
		return nil, err
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

func (s *Store) GetAgents(ctx context.Context, excludeOneShot, excludeAutomated bool) ([]db.AgentInfo, error) {
	rows, err := s.duck.QueryContext(ctx,
		`SELECT agent, COUNT(*) FROM sessions WHERE agent <> '' AND `+
			rootSessionWhere(excludeOneShot, excludeAutomated)+
			` GROUP BY agent ORDER BY agent`,
	)
	if err != nil {
		return nil, err
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
	rows, err := s.duck.QueryContext(ctx,
		`SELECT DISTINCT machine FROM sessions WHERE `+
			rootSessionWhere(excludeOneShot, excludeAutomated)+
			` ORDER BY machine`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var machine string
		if err := rows.Scan(&machine); err != nil {
			return nil, err
		}
		out = append(out, machine)
	}
	return out, rows.Err()
}

func rootSessionWhere(excludeOneShot, excludeAutomated bool) string {
	filter := `message_count > 0
		AND relationship_type NOT IN ('subagent', 'fork')
		AND deleted_at IS NULL`
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
	return filter
}

func (s *Store) HasFTS() bool { return true }

func (s *Store) Search(ctx context.Context, f db.SearchFilter) (db.SearchPage, error) {
	if f.Limit <= 0 || f.Limit > db.MaxSearchLimit {
		f.Limit = db.DefaultSearchLimit
	}
	if f.Query == "" {
		return db.SearchPage{}, nil
	}
	searchTerm := stripSearchQuotes(f.Query)
	if searchTerm == "" {
		return db.SearchPage{}, nil
	}
	pattern := "%" + db.EscapeLikePattern(searchTerm) + "%"
	project := ""
	nameProject := ""
	args := []any{searchTerm, searchTerm, pattern}
	if f.Project != "" {
		project = "AND s.project = ?"
		args = append(args, f.Project)
		nameProject = "AND s.project = ?"
	}
	args = append(args, pattern, pattern, pattern, pattern)
	if f.Project != "" {
		args = append(args, f.Project)
	}
	orderBy := "match_priority ASC, match_pos ASC, session_ended_at DESC, session_id ASC"
	if f.Sort == "recency" {
		orderBy = "session_ended_at DESC, session_id ASC"
	}
	args = append(args, f.Limit+1, f.Cursor)
	rows, err := s.duck.QueryContext(ctx, `
		WITH msg_ranked AS (
			SELECT m.session_id, s.project, s.agent,
				COALESCE(s.display_name, s.session_name, s.first_message, '') AS name,
				COALESCE(s.ended_at, s.started_at, s.created_at) AS session_ended_at,
				m.ordinal, SUBSTRING(m.content, 1, 200) AS snippet,
				1.0 AS rank, 1 AS match_priority,
				INSTR(LOWER(m.content), LOWER(?)) AS match_pos,
				ROW_NUMBER() OVER (
					PARTITION BY m.session_id
					ORDER BY INSTR(LOWER(m.content), LOWER(?)) ASC,
						m.ordinal ASC, COALESCE(m.id, 0) ASC
				) AS rn
			FROM messages m
			JOIN sessions s ON s.id = m.session_id
			WHERE m.content ILIKE ? ESCAPE '\'
				AND s.deleted_at IS NULL
				AND m.is_system = FALSE
				AND `+db.SystemPrefixSQL("m.content", "m.role")+`
				`+project+`
		),
		msg_matches AS (
			SELECT session_id, project, agent, name, session_ended_at,
				ordinal, snippet, rank, match_priority, match_pos
			FROM msg_ranked
			WHERE rn = 1
		),
		name_matches AS (
			SELECT s.id AS session_id, s.project, s.agent,
				COALESCE(s.display_name, s.session_name, s.first_message, '') AS name,
				COALESCE(s.ended_at, s.started_at, s.created_at) AS session_ended_at,
				-1 AS ordinal,
				CASE
					WHEN COALESCE(s.display_name, s.session_name) ILIKE ? ESCAPE '\'
						THEN COALESCE(s.display_name, s.session_name, '')
					WHEN s.first_message ILIKE ? ESCAPE '\'
						THEN COALESCE(s.first_message, '')
					ELSE COALESCE(s.display_name, s.session_name, s.first_message, '')
				END AS snippet,
				1.0 AS rank, 2 AS match_priority, 0 AS match_pos
			FROM sessions s
			WHERE (COALESCE(s.display_name, s.session_name) ILIKE ? ESCAPE '\'
				OR s.first_message ILIKE ? ESCAPE '\')
				AND s.deleted_at IS NULL
				AND EXISTS (
					SELECT 1 FROM messages mx
					WHERE mx.session_id = s.id
						AND mx.is_system = FALSE
						AND `+db.SystemPrefixSQL("mx.content", "mx.role")+`
				)
				AND s.id NOT IN (SELECT session_id FROM msg_matches)
				`+nameProject+`
		)
		SELECT session_id, project, agent, name,
			session_ended_at, ordinal, snippet, rank
		FROM (
			SELECT * FROM msg_matches
			UNION ALL
			SELECT * FROM name_matches
		) combined
		ORDER BY `+orderBy+`
		LIMIT ? OFFSET ?`,
		args...,
	)
	if err != nil {
		return db.SearchPage{}, fmt.Errorf("duckdb search: %w", err)
	}
	defer rows.Close()
	var results []db.SearchResult
	for rows.Next() {
		var r db.SearchResult
		var ended any
		if err := rows.Scan(&r.SessionID, &r.Project, &r.Agent, &r.Name,
			&ended, &r.Ordinal, &r.Snippet, &r.Rank); err != nil {
			return db.SearchPage{}, err
		}
		r.SessionEndedAt = formatDBTime(ended)
		results = append(results, r)
	}
	if err := rows.Err(); err != nil {
		return db.SearchPage{}, err
	}
	page := db.SearchPage{Results: results}
	if len(results) > f.Limit {
		page.Results = results[:f.Limit]
		page.NextCursor = f.Cursor + f.Limit
	}
	return page, nil
}

func stripSearchQuotes(q string) string {
	if len(q) >= 2 && q[0] == '"' && q[len(q)-1] == '"' {
		return q[1 : len(q)-1]
	}
	return q
}

func (s *Store) SearchSession(ctx context.Context, sessionID, query string) ([]int, error) {
	if query == "" {
		return nil, nil
	}
	rows, err := s.duck.QueryContext(ctx, `
		SELECT DISTINCT m.ordinal
		FROM messages m
		LEFT JOIN tool_calls tc
			ON tc.session_id = m.session_id
			AND tc.message_id = m.id
		LEFT JOIN tool_result_events tre
			ON tre.session_id = tc.session_id
			AND tre.tool_call_message_ordinal = m.ordinal
			AND tre.call_index = tc.call_index
		WHERE m.session_id = ?
			AND m.is_system = FALSE
			AND `+db.SystemPrefixSQL("m.content", "m.role")+`
			AND (m.content ILIKE ? ESCAPE '\'
				OR tc.result_content ILIKE ? ESCAPE '\'
				OR tre.content ILIKE ? ESCAPE '\')
		ORDER BY m.ordinal ASC`,
		sessionID, "%"+db.EscapeLikePattern(query)+"%",
		"%"+db.EscapeLikePattern(query)+"%",
		"%"+db.EscapeLikePattern(query)+"%",
	)
	if err != nil {
		return nil, fmt.Errorf("duckdb session search: %w", err)
	}
	defer rows.Close()
	var out []int
	for rows.Next() {
		var ordinal int
		if err := rows.Scan(&ordinal); err != nil {
			return nil, err
		}
		out = append(out, ordinal)
	}
	return out, rows.Err()
}

func (s *Store) SearchContent(ctx context.Context, f db.ContentSearchFilter) (db.ContentSearchPage, error) {
	if f.Limit <= 0 || f.Limit > db.MaxContentSearchLimit {
		f.Limit = db.DefaultContentSearchLimit
	}
	if f.Pattern == "" {
		return db.ContentSearchPage{}, nil
	}
	if len(f.Sources) == 0 {
		f.Sources = []string{"messages", "tool_input", "tool_result"}
	}
	for _, source := range f.Sources {
		if source != "messages" && source != "tool_input" && source != "tool_result" {
			return db.ContentSearchPage{},
				&db.SearchInputError{Msg: fmt.Sprintf("search: unknown source %q", source)}
		}
	}
	switch f.Mode {
	case "", "substring", "fts", "regex":
		// fts falls back to DuckDB substring search, matching the PostgreSQL
		// provider's explicit non-FTS behavior.
	default:
		return db.ContentSearchPage{},
			&db.SearchInputError{Msg: fmt.Sprintf("search: invalid mode %q", f.Mode)}
	}
	matches, err := s.collectContentMatches(ctx, f)
	if err != nil {
		return db.ContentSearchPage{}, err
	}
	page := db.ContentSearchPage{Matches: matches}
	if len(matches) > f.Limit {
		page.Matches = matches[:f.Limit]
		page.NextCursor = f.Cursor + f.Limit
	}
	return page, nil
}

func (s *Store) collectContentMatches(ctx context.Context, f db.ContentSearchFilter) ([]db.ContentMatch, error) {
	if f.Mode != "regex" {
		return s.collectContentSubstringMatches(ctx, f)
	}
	scopeWhere, scopeArgs := db.BuildSessionFilterSQL(contentSessionFilter(f), db.DuckDBQueryDialect())
	pattern := ""
	if f.Mode != "regex" {
		pattern = "%" + db.EscapeLikePattern(f.Pattern) + "%"
	}
	var all []duckContentCandidate
	for _, source := range f.Sources {
		matches, err := s.collectContentSource(ctx, source, scopeWhere, scopeArgs, pattern, f)
		if err != nil {
			return nil, err
		}
		all = append(all, matches...)
	}
	if f.Mode == "regex" {
		re, err := regexp.Compile(f.Pattern)
		if err != nil {
			return nil, &db.SearchInputError{Msg: fmt.Sprintf("search: invalid regex: %v", err)}
		}
		filtered := all[:0]
		for _, m := range all {
			loc := re.FindStringIndex(m.body)
			if loc != nil {
				m.match.Snippet = duckContentSnippet(f, m.body, loc[0], loc[1])
				filtered = append(filtered, m)
			}
		}
		all = filtered
	}
	sortContentCandidates(all)
	if f.Cursor > 0 {
		if f.Cursor >= len(all) {
			return nil, nil
		}
		all = all[f.Cursor:]
	}
	if len(all) > f.Limit+1 {
		all = all[:f.Limit+1]
	}
	return contentCandidateMatches(all), nil
}

func (s *Store) collectContentSubstringMatches(
	ctx context.Context, f db.ContentSearchFilter,
) ([]db.ContentMatch, error) {
	scopeWhere, scopeArgs := db.BuildSessionFilterSQL(contentSessionFilter(f), db.DuckDBQueryDialect())
	pattern := "%" + db.EscapeLikePattern(f.Pattern) + "%"
	var branches []string
	var args []any
	addScopedArgs := func() {
		args = append(args, pattern)
		args = append(args, scopeArgs...)
	}
	for _, source := range f.Sources {
		switch source {
		case "messages":
			sysPred := "TRUE"
			if f.ExcludeSystem {
				sysPred = "m.is_system = FALSE AND " + db.SystemPrefixSQL("m.content", "m.role")
			}
			branches = append(branches, `
				SELECT m.session_id, s.project, s.agent, 'message' AS location,
					m.role, '' AS tool_name, m.ordinal,
					COALESCE(CAST(m.timestamp AS TEXT), '') AS ts,
					m.content AS body,
					COALESCE(s.ended_at, s.started_at, s.created_at) AS sort_ts,
					0 AS src, COALESCE(m.id, 0) AS row_id,
					0 AS call_index, 0 AS event_index
				FROM messages m JOIN sessions s ON s.id = m.session_id
				WHERE m.content ILIKE ? ESCAPE '\'
					AND `+sysPred+`
					AND m.session_id IN (SELECT id FROM sessions WHERE `+scopeWhere+`)`)
			addScopedArgs()
		case "tool_input":
			branches = append(branches, `
				SELECT tc.session_id, s.project, s.agent, 'tool_input' AS location,
					'assistant' AS role, tc.tool_name, m.ordinal,
					COALESCE(CAST(m.timestamp AS TEXT), '') AS ts,
					tc.input_json AS body,
					COALESCE(s.ended_at, s.started_at, s.created_at) AS sort_ts,
					1 AS src, COALESCE(tc.id, 0) AS row_id,
					tc.call_index AS call_index, 0 AS event_index
				FROM tool_calls tc JOIN sessions s ON s.id = tc.session_id
				JOIN messages m ON m.session_id = tc.session_id
					AND m.id = tc.message_id
				WHERE tc.input_json ILIKE ? ESCAPE '\'
					AND tc.session_id IN (SELECT id FROM sessions WHERE `+scopeWhere+`)`)
			addScopedArgs()
		case "tool_result":
			branches = append(branches, `
					SELECT tc.session_id, s.project, s.agent, 'tool_result' AS location,
						'assistant' AS role, tc.tool_name, m.ordinal,
						COALESCE(CAST(m.timestamp AS TEXT), '') AS ts,
						tc.result_content AS body,
						COALESCE(s.ended_at, s.started_at, s.created_at) AS sort_ts,
						2 AS src, COALESCE(tc.id, 0) AS row_id,
						tc.call_index AS call_index, 0 AS event_index
					FROM tool_calls tc JOIN sessions s ON s.id = tc.session_id
					JOIN messages m ON m.session_id = tc.session_id
						AND m.id = tc.message_id
					WHERE tc.result_content ILIKE ? ESCAPE '\'
						AND NOT EXISTS (
							SELECT 1 FROM tool_result_events tre
							WHERE tre.session_id = tc.session_id
								AND tre.tool_use_id = tc.tool_use_id
								AND tc.tool_use_id <> ''
						)
						AND tc.session_id IN (SELECT id FROM sessions WHERE `+scopeWhere+`)`)
			addScopedArgs()
			branches = append(branches, `
					SELECT tre.session_id, s.project, s.agent, 'tool_result' AS location,
						'assistant' AS role, '' AS tool_name,
						tre.tool_call_message_ordinal AS ordinal,
						COALESCE(CAST(tre.timestamp AS TEXT), '') AS ts,
						tre.content AS body,
						COALESCE(s.ended_at, s.started_at, s.created_at) AS sort_ts,
						3 AS src, COALESCE(tre.id, 0) AS row_id,
						tre.call_index AS call_index,
						tre.event_index AS event_index
					FROM tool_result_events tre JOIN sessions s ON s.id = tre.session_id
					WHERE tre.content ILIKE ? ESCAPE '\'
						AND tre.session_id IN (SELECT id FROM sessions WHERE `+scopeWhere+`)`)
			addScopedArgs()
		default:
			return nil, &db.SearchInputError{Msg: fmt.Sprintf("search: unknown source %q", source)}
		}
	}
	if len(branches) == 0 {
		return nil, nil
	}
	query := `
		SELECT session_id, project, agent, location, role, tool_name,
			ordinal, ts, body
		FROM (` + strings.Join(branches, " UNION ALL ") + `)
		ORDER BY sort_ts DESC, session_id ASC, ordinal ASC,
			src ASC, row_id ASC, call_index ASC, event_index ASC
		LIMIT ? OFFSET ?`
	args = append(args, f.Limit+1, f.Cursor)
	return s.scanContentMatches(ctx, query, args, func(body string) string {
		off := max(db.CaseInsensitiveIndex(body, f.Pattern), 0)
		return duckContentSnippet(f, body, off, min(off+len(f.Pattern), len(body)))
	})
}

func duckContentSnippet(f db.ContentSearchFilter, body string, start, end int) string {
	lo, hi := duckSnippetBounds(body, start, end, 60)
	if f.RevealSecrets {
		return body[lo:hi]
	}
	return secrets.RedactWindow(body, lo, hi)
}

func duckSnippetBounds(text string, start, end, radius int) (int, int) {
	lo := max(start-radius, 0)
	hi := min(end+radius, len(text))
	for lo < start && !utf8.RuneStart(text[lo]) {
		lo++
	}
	for hi > end && hi < len(text) && !utf8.RuneStart(text[hi]) {
		hi--
	}
	return lo, hi
}

type duckContentCandidate struct {
	match      db.ContentMatch
	body       string
	sortTS     string
	sortTime   time.Time
	hasSort    bool
	sourceRank int
	rowID      int64
	callIndex  int
	eventIndex int
}

func sortContentCandidates(all []duckContentCandidate) {
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].hasSort && all[j].hasSort && !all[i].sortTime.Equal(all[j].sortTime) {
			return all[i].sortTime.After(all[j].sortTime)
		}
		if all[i].hasSort != all[j].hasSort {
			return all[i].hasSort
		}
		if all[i].sortTS != all[j].sortTS {
			return all[i].sortTS > all[j].sortTS
		}
		if all[i].match.SessionID != all[j].match.SessionID {
			return all[i].match.SessionID < all[j].match.SessionID
		}
		if all[i].match.Ordinal != all[j].match.Ordinal {
			return all[i].match.Ordinal < all[j].match.Ordinal
		}
		if all[i].sourceRank != all[j].sourceRank {
			return all[i].sourceRank < all[j].sourceRank
		}
		if all[i].rowID != all[j].rowID {
			return all[i].rowID < all[j].rowID
		}
		if all[i].callIndex != all[j].callIndex {
			return all[i].callIndex < all[j].callIndex
		}
		if all[i].eventIndex != all[j].eventIndex {
			return all[i].eventIndex < all[j].eventIndex
		}
		if all[i].match.Location != all[j].match.Location {
			return all[i].match.Location < all[j].match.Location
		}
		if all[i].match.ToolName != all[j].match.ToolName {
			return all[i].match.ToolName < all[j].match.ToolName
		}
		if all[i].match.Role != all[j].match.Role {
			return all[i].match.Role < all[j].match.Role
		}
		if all[i].match.Timestamp != all[j].match.Timestamp {
			return all[i].match.Timestamp < all[j].match.Timestamp
		}
		if all[i].match.Project != all[j].match.Project {
			return all[i].match.Project < all[j].match.Project
		}
		if all[i].match.Agent != all[j].match.Agent {
			return all[i].match.Agent < all[j].match.Agent
		}
		return all[i].match.Snippet < all[j].match.Snippet
	})
}

func contentCandidateMatches(candidates []duckContentCandidate) []db.ContentMatch {
	out := make([]db.ContentMatch, len(candidates))
	for i, candidate := range candidates {
		out[i] = candidate.match
	}
	return out
}

func contentSessionFilter(f db.ContentSearchFilter) db.SessionFilter {
	return db.SessionFilter{
		Project: f.Project, ExcludeProject: f.ExcludeProject,
		Machine: f.Machine, Agent: f.Agent,
		Date: f.Date, DateFrom: f.DateFrom, DateTo: f.DateTo,
		ActiveSince:      f.ActiveSince,
		ExcludeOneShot:   !f.IncludeOneShot,
		ExcludeAutomated: !f.IncludeAutomated,
		IncludeChildren:  f.IncludeChildren,
	}
}

func (s *Store) collectContentSource(
	ctx context.Context, source, scopeWhere string, scopeArgs []any,
	pattern string, f db.ContentSearchFilter,
) ([]duckContentCandidate, error) {
	var query string
	var orderBy string
	args := append([]any{}, scopeArgs...)
	switch source {
	case "messages":
		query = `SELECT m.session_id, s.project, s.agent, 'message',
			m.role, '', m.ordinal, COALESCE(CAST(m.timestamp AS TEXT), ''),
			m.content,
			COALESCE(s.ended_at, s.started_at, s.created_at) AS sort_ts,
			0 AS src, COALESCE(m.id, 0) AS row_id,
			0 AS call_index, 0 AS event_index
			FROM messages m JOIN sessions s ON s.id = m.session_id
			WHERE m.session_id IN (SELECT id FROM sessions WHERE ` + scopeWhere + `)`
		if f.Mode != "regex" {
			query += ` AND m.content ILIKE ? ESCAPE '\'`
			args = append(args, pattern)
		}
		if f.ExcludeSystem {
			query += " AND m.is_system = FALSE AND " + db.SystemPrefixSQL("m.content", "m.role")
		}
		orderBy = "m.session_id, m.ordinal, COALESCE(m.id, 0)"
	case "tool_input":
		query = `SELECT tc.session_id, s.project, s.agent, 'tool_input',
				'assistant', tc.tool_name, m.ordinal, COALESCE(CAST(m.timestamp AS TEXT), ''),
				COALESCE(tc.input_json, ''),
				COALESCE(s.ended_at, s.started_at, s.created_at) AS sort_ts,
				1 AS src, COALESCE(tc.id, 0) AS row_id,
				tc.call_index AS call_index, 0 AS event_index
				FROM tool_calls tc JOIN sessions s ON s.id = tc.session_id
				JOIN messages m ON m.session_id = tc.session_id
					AND m.id = tc.message_id
				WHERE tc.session_id IN (SELECT id FROM sessions WHERE ` + scopeWhere + `)`
		if f.Mode != "regex" {
			query += ` AND tc.input_json ILIKE ? ESCAPE '\'`
			args = append(args, pattern)
		}
		orderBy = "tc.session_id, m.ordinal, COALESCE(tc.id, 0), tc.call_index"
	case "tool_result":
		args = append([]any{}, scopeArgs...)
		contentPred := "TRUE"
		eventPred := "TRUE"
		if f.Mode != "regex" {
			contentPred = `tc.result_content ILIKE ? ESCAPE '\'`
			eventPred = `tre.content ILIKE ? ESCAPE '\'`
		}
		query = `SELECT session_id, project, agent, location, role,
				tool_name, ordinal, ts, body, sort_ts, src, row_id, call_index, event_index
				FROM (
					SELECT tc.session_id, s.project, s.agent, 'tool_result' AS location,
						'assistant' AS role, tc.tool_name, m.ordinal,
						COALESCE(CAST(m.timestamp AS TEXT), '') AS ts,
						COALESCE(tc.result_content, '') AS body,
						COALESCE(s.ended_at, s.started_at, s.created_at) AS sort_ts,
						2 AS src,
						COALESCE(tc.id, 0) AS row_id,
						tc.call_index AS call_index, 0 AS event_index
					FROM tool_calls tc JOIN sessions s ON s.id = tc.session_id
					JOIN messages m ON m.session_id = tc.session_id
						AND m.id = tc.message_id
					WHERE tc.session_id IN (SELECT id FROM sessions WHERE ` + scopeWhere + `)
						AND ` + contentPred + `
						AND NOT EXISTS (
							SELECT 1 FROM tool_result_events tre
							WHERE tre.session_id = tc.session_id
								AND tre.tool_use_id = tc.tool_use_id
								AND tc.tool_use_id <> ''
						)
					UNION ALL
					SELECT tre.session_id, s.project, s.agent, 'tool_result' AS location,
						'assistant' AS role, '' AS tool_name,
						tre.tool_call_message_ordinal AS ordinal,
						COALESCE(CAST(tre.timestamp AS TEXT), '') AS ts,
						tre.content AS body,
						COALESCE(s.ended_at, s.started_at, s.created_at) AS sort_ts,
						3 AS src,
						COALESCE(tre.id, 0) AS row_id,
						tre.call_index AS call_index, tre.event_index
					FROM tool_result_events tre JOIN sessions s ON s.id = tre.session_id
					WHERE tre.session_id IN (SELECT id FROM sessions WHERE ` + scopeWhere + `)
						AND ` + eventPred + `
				)`
		if f.Mode != "regex" {
			args = append(args, pattern)
		}
		args = append(args, scopeArgs...)
		if f.Mode != "regex" {
			args = append(args, pattern)
		}
		orderBy = "session_id, ordinal, src, row_id, call_index, event_index"
	default:
		return nil, &db.SearchInputError{Msg: fmt.Sprintf("search: unknown source %q", source)}
	}
	query += ` ORDER BY ` + orderBy
	rows, err := s.duck.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("duckdb content search: %w", err)
	}
	return scanDuckContentCandidateRows(rows)
}

func (s *Store) scanContentMatches(
	ctx context.Context, query string, args []any, makeSnippet func(string) string,
) ([]db.ContentMatch, error) {
	rows, err := s.duck.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("duckdb content search: %w", err)
	}
	return scanDuckContentRows(rows, makeSnippet)
}

func scanDuckContentRows(rows *sql.Rows, makeSnippet func(string) string) ([]db.ContentMatch, error) {
	defer rows.Close()
	var out []db.ContentMatch
	for rows.Next() {
		var m db.ContentMatch
		var body string
		if err := rows.Scan(&m.SessionID, &m.Project, &m.Agent,
			&m.Location, &m.Role, &m.ToolName, &m.Ordinal,
			&m.Timestamp, &body); err != nil {
			return nil, err
		}
		m.Snippet = makeSnippet(body)
		out = append(out, m)
	}
	return out, rows.Err()
}

func scanDuckContentCandidateRows(rows *sql.Rows) ([]duckContentCandidate, error) {
	defer rows.Close()
	var out []duckContentCandidate
	for rows.Next() {
		var candidate duckContentCandidate
		var sortTS any
		if err := rows.Scan(
			&candidate.match.SessionID, &candidate.match.Project,
			&candidate.match.Agent, &candidate.match.Location,
			&candidate.match.Role, &candidate.match.ToolName,
			&candidate.match.Ordinal, &candidate.match.Timestamp,
			&candidate.body, &sortTS, &candidate.sourceRank,
			&candidate.rowID, &candidate.callIndex, &candidate.eventIndex,
		); err != nil {
			return nil, err
		}
		candidate.sortTS = formatDBTime(sortTS)
		candidate.sortTime, candidate.hasSort = parseAnalyticsTime(candidate.sortTS)
		candidate.match.Snippet = candidate.body
		out = append(out, candidate)
	}
	return out, rows.Err()
}
