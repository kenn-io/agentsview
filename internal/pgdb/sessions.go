package pgdb

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

	"github.com/wesm/agentsview/internal/db"
)

// PGDB wraps a PostgreSQL connection for read-only session queries.
type PGDB struct {
	pg           *sql.DB
	cursorMu     sync.RWMutex
	cursorSecret []byte
}

// pgSessionCols is the column list for standard PG session queries.
// PG has no file_path, file_size, file_mtime, file_hash,
// or local_modified_at columns.
const pgSessionCols = `id, project, machine, agent,
	first_message, display_name, created_at, started_at, ended_at,
	message_count, user_message_count,
	parent_session_id, relationship_type, deleted_at`

// paramBuilder generates numbered PostgreSQL placeholders ($1, $2, ...).
type paramBuilder struct {
	n    int
	args []any
}

func (pb *paramBuilder) add(v any) string {
	pb.n++
	pb.args = append(pb.args, v)
	return fmt.Sprintf("$%d", pb.n)
}

// scanPGSession scans a row with pgSessionCols into a db.Session.
func scanPGSession(rs interface{ Scan(...any) error }) (db.Session, error) {
	var s db.Session
	err := rs.Scan(
		&s.ID, &s.Project, &s.Machine, &s.Agent,
		&s.FirstMessage, &s.DisplayName, &s.CreatedAt, &s.StartedAt, &s.EndedAt,
		&s.MessageCount, &s.UserMessageCount,
		&s.ParentSessionID, &s.RelationshipType,
		&s.DeletedAt,
	)
	return s, err
}

// scanPGSessionRows iterates rows and scans each using scanPGSession.
func scanPGSessionRows(rows *sql.Rows) ([]db.Session, error) {
	sessions := []db.Session{}
	for rows.Next() {
		s, err := scanPGSession(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning session: %w", err)
		}
		sessions = append(sessions, s)
	}
	return sessions, rows.Err()
}

// pgRootSessionFilter is the base WHERE clause for root sessions.
const pgRootSessionFilter = `message_count > 0
	AND relationship_type NOT IN ('subagent', 'fork')
	AND deleted_at IS NULL`

// buildPGSessionFilter returns a WHERE clause with $N placeholders
// and the corresponding args for the non-cursor predicates in
// db.SessionFilter. When IncludeChildren is true, filter predicates
// apply only to root sessions; children are included via a subquery
// on their parent, matching the SQLite implementation.
func buildPGSessionFilter(f db.SessionFilter) (string, []any) {
	pb := &paramBuilder{}
	basePreds := []string{
		"message_count > 0",
		"deleted_at IS NULL",
	}
	if !f.IncludeChildren {
		basePreds = append(basePreds,
			"relationship_type NOT IN ('subagent', 'fork')")
	}

	// Filter predicates narrow results. When IncludeChildren is
	// true these only apply to root sessions; children are
	// included via a subquery on their parent.
	var filterPreds []string

	if f.Project != "" {
		filterPreds = append(filterPreds, "project = "+pb.add(f.Project))
	}
	if f.ExcludeProject != "" {
		filterPreds = append(filterPreds, "project != "+pb.add(f.ExcludeProject))
	}
	if f.Machine != "" {
		filterPreds = append(filterPreds, "machine = "+pb.add(f.Machine))
	}
	if f.Agent != "" {
		agents := strings.Split(f.Agent, ",")
		if len(agents) == 1 {
			filterPreds = append(filterPreds, "agent = "+pb.add(agents[0]))
		} else {
			placeholders := make([]string, len(agents))
			for i, a := range agents {
				placeholders[i] = pb.add(a)
			}
			filterPreds = append(filterPreds,
				"agent IN ("+strings.Join(placeholders, ",")+")",
			)
		}
	}
	if f.Date != "" {
		filterPreds = append(filterPreds,
			"SUBSTRING(COALESCE(NULLIF(started_at, ''), created_at) FROM 1 FOR 10) = "+pb.add(f.Date))
	}
	if f.DateFrom != "" {
		filterPreds = append(filterPreds,
			"SUBSTRING(COALESCE(NULLIF(started_at, ''), created_at) FROM 1 FOR 10) >= "+pb.add(f.DateFrom))
	}
	if f.DateTo != "" {
		filterPreds = append(filterPreds,
			"SUBSTRING(COALESCE(NULLIF(started_at, ''), created_at) FROM 1 FOR 10) <= "+pb.add(f.DateTo))
	}
	if f.ActiveSince != "" {
		filterPreds = append(filterPreds,
			"COALESCE(NULLIF(ended_at, ''), NULLIF(started_at, ''), created_at) >= "+pb.add(f.ActiveSince))
	}
	if f.MinMessages > 0 {
		filterPreds = append(filterPreds, "message_count >= "+pb.add(f.MinMessages))
	}
	if f.MaxMessages > 0 {
		filterPreds = append(filterPreds, "message_count <= "+pb.add(f.MaxMessages))
	}
	if f.MinUserMessages > 0 {
		filterPreds = append(filterPreds, "user_message_count >= "+pb.add(f.MinUserMessages))
	}

	// ExcludeOneShot is handled separately when IncludeChildren
	// is true — children are almost always one-shot by nature
	// and must not be excluded.
	oneShotPred := ""
	if f.ExcludeOneShot {
		if f.IncludeChildren {
			oneShotPred = "user_message_count > 1"
		} else {
			filterPreds = append(filterPreds,
				"user_message_count > 1")
		}
	}

	// Simple case: no IncludeChildren or no user filters.
	hasFilters := len(filterPreds) > 0 || oneShotPred != ""
	if !f.IncludeChildren || !hasFilters {
		allPreds := append(basePreds, filterPreds...)
		return strings.Join(allPreds, " AND "), pb.args
	}

	// IncludeChildren + filters: match the filter directly,
	// or be a child of a session that matches the filter.
	baseWhere := strings.Join(basePreds, " AND ")

	rootMatchParts := append([]string{}, filterPreds...)
	if oneShotPred != "" {
		rootMatchParts = append(rootMatchParts, oneShotPred)
	}
	rootMatch := strings.Join(rootMatchParts, " AND ")

	// PG uses numbered placeholders ($1, $2, ...) so the
	// subquery can reuse the same $N references without
	// duplicating args (unlike SQLite's positional ?).
	subqWhere := "message_count > 0 AND deleted_at IS NULL"
	if rootMatch != "" {
		subqWhere += " AND " + rootMatch
	}

	where := baseWhere + " AND (" + rootMatch +
		" OR parent_session_id IN" +
		" (SELECT id FROM agentsview.sessions WHERE " + subqWhere + "))"

	return where, pb.args
}

// EncodeCursor returns a base64-encoded, HMAC-signed cursor string.
func (p *PGDB) EncodeCursor(endedAt, id string, total ...int) string {
	t := 0
	if len(total) > 0 {
		t = total[0]
	}
	c := db.SessionCursor{EndedAt: endedAt, ID: id, Total: t}
	data, _ := json.Marshal(c)

	p.cursorMu.RLock()
	secret := make([]byte, len(p.cursorSecret))
	copy(secret, p.cursorSecret)
	p.cursorMu.RUnlock()

	mac := hmac.New(sha256.New, secret)
	mac.Write(data)
	sig := mac.Sum(nil)

	return base64.RawURLEncoding.EncodeToString(data) + "." +
		base64.RawURLEncoding.EncodeToString(sig)
}

// DecodeCursor parses a base64-encoded cursor string.
func (p *PGDB) DecodeCursor(s string) (db.SessionCursor, error) {
	parts := strings.Split(s, ".")
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
	} else if len(parts) != 2 {
		return db.SessionCursor{}, fmt.Errorf("%w: invalid format", db.ErrInvalidCursor)
	}

	payload := parts[0]
	sigStr := parts[1]

	data, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return db.SessionCursor{}, fmt.Errorf("%w: invalid payload: %v", db.ErrInvalidCursor, err)
	}

	sig, err := base64.RawURLEncoding.DecodeString(sigStr)
	if err != nil {
		return db.SessionCursor{}, fmt.Errorf("%w: invalid signature encoding: %v", db.ErrInvalidCursor, err)
	}

	p.cursorMu.RLock()
	secret := make([]byte, len(p.cursorSecret))
	copy(secret, p.cursorSecret)
	p.cursorMu.RUnlock()

	mac := hmac.New(sha256.New, secret)
	mac.Write(data)
	expectedSig := mac.Sum(nil)

	if !hmac.Equal(sig, expectedSig) {
		return db.SessionCursor{}, fmt.Errorf("%w: signature mismatch", db.ErrInvalidCursor)
	}

	var c db.SessionCursor
	if err := json.Unmarshal(data, &c); err != nil {
		return db.SessionCursor{}, fmt.Errorf("%w: invalid json: %v", db.ErrInvalidCursor, err)
	}
	return c, nil
}

// ListSessions returns a cursor-paginated list of sessions from PG.
func (p *PGDB) ListSessions(
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
		cur, err = p.DecodeCursor(f.Cursor)
		if err != nil {
			return db.SessionPage{}, err
		}
		total = cur.Total
	}

	// Count total matching sessions (without cursor pagination).
	if total <= 0 {
		countQuery := "SELECT COUNT(*) FROM agentsview.sessions WHERE " + where
		if err := p.pg.QueryRowContext(
			ctx, countQuery, args...,
		).Scan(&total); err != nil {
			return db.SessionPage{},
				fmt.Errorf("counting sessions: %w", err)
		}
	}

	// Build the paginated query with cursor.
	cursorPB := &paramBuilder{n: len(args), args: append([]any{}, args...)}
	cursorWhere := where
	if f.Cursor != "" {
		endedAtParam := cursorPB.add(cur.EndedAt)
		idParam := cursorPB.add(cur.ID)
		cursorWhere += ` AND (
			COALESCE(NULLIF(ended_at, ''), NULLIF(started_at, ''), created_at), id
		) < (` + endedAtParam + `, ` + idParam + `)`
	}

	limitParam := cursorPB.add(f.Limit + 1)
	query := "SELECT " + pgSessionCols +
		" FROM agentsview.sessions WHERE " + cursorWhere + `
		ORDER BY COALESCE(
			NULLIF(ended_at, ''),
			NULLIF(started_at, ''),
			created_at
		) DESC, id DESC
		LIMIT ` + limitParam

	rows, err := p.pg.QueryContext(ctx, query, cursorPB.args...)
	if err != nil {
		return db.SessionPage{},
			fmt.Errorf("querying sessions: %w", err)
	}
	defer rows.Close()

	sessions, err := scanPGSessionRows(rows)
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
		page.NextCursor = p.EncodeCursor(ea, last.ID, total)
	}

	return page, nil
}

// GetSession returns a single session by ID, excluding soft-deleted sessions.
func (p *PGDB) GetSession(
	ctx context.Context, id string,
) (*db.Session, error) {
	row := p.pg.QueryRowContext(
		ctx,
		"SELECT "+pgSessionCols+
			" FROM agentsview.sessions WHERE id = $1 AND deleted_at IS NULL",
		id,
	)

	s, err := scanPGSession(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("getting session %s: %w", id, err)
	}
	return &s, nil
}

// GetSessionFull returns a single session by ID including soft-deleted sessions.
// File metadata fields (FilePath, FileSize, etc.) remain nil since PG
// does not store them.
func (p *PGDB) GetSessionFull(
	ctx context.Context, id string,
) (*db.Session, error) {
	row := p.pg.QueryRowContext(
		ctx,
		"SELECT "+pgSessionCols+
			" FROM agentsview.sessions WHERE id = $1",
		id,
	)

	s, err := scanPGSession(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("getting session full %s: %w", id, err)
	}
	return &s, nil
}

// GetChildSessions returns sessions whose parent_session_id matches
// the given parentID, ordered by started_at ascending.
func (p *PGDB) GetChildSessions(
	ctx context.Context, parentID string,
) ([]db.Session, error) {
	query := "SELECT " + pgSessionCols +
		" FROM agentsview.sessions WHERE parent_session_id = $1 AND deleted_at IS NULL" +
		" ORDER BY COALESCE(NULLIF(started_at, ''), created_at) ASC"
	rows, err := p.pg.QueryContext(ctx, query, parentID)
	if err != nil {
		return nil, fmt.Errorf(
			"querying child sessions for %s: %w", parentID, err,
		)
	}
	defer rows.Close()

	return scanPGSessionRows(rows)
}

// GetStats returns database statistics, counting only root sessions
// with messages.
func (p *PGDB) GetStats(
	ctx context.Context, excludeOneShot bool,
) (db.Stats, error) {
	filter := pgRootSessionFilter
	if excludeOneShot {
		filter += " AND user_message_count > 1"
	}
	query := fmt.Sprintf(`
		SELECT
			(SELECT COUNT(*) FROM agentsview.sessions
			 WHERE %s),
			(SELECT COALESCE(SUM(message_count), 0)
			 FROM agentsview.sessions WHERE %s),
			(SELECT COUNT(DISTINCT project) FROM agentsview.sessions
			 WHERE %s),
			(SELECT COUNT(DISTINCT machine) FROM agentsview.sessions
			 WHERE %s),
			(SELECT MIN(COALESCE(NULLIF(started_at, ''), created_at))
			 FROM agentsview.sessions
			 WHERE %s)`,
		filter, filter, filter, filter, filter)

	var s db.Stats
	err := p.pg.QueryRowContext(ctx, query).Scan(
		&s.SessionCount,
		&s.MessageCount,
		&s.ProjectCount,
		&s.MachineCount,
		&s.EarliestSession,
	)
	if err != nil {
		return db.Stats{}, fmt.Errorf("fetching stats: %w", err)
	}
	return s, nil
}

// GetProjects returns project names with session counts.
func (p *PGDB) GetProjects(
	ctx context.Context, excludeOneShot bool,
) ([]db.ProjectInfo, error) {
	q := `SELECT project, COUNT(*) as session_count
		FROM agentsview.sessions
		WHERE message_count > 0
		  AND relationship_type NOT IN ('subagent', 'fork')
		  AND deleted_at IS NULL`
	if excludeOneShot {
		q += " AND user_message_count > 1"
	}
	q += " GROUP BY project ORDER BY project"
	rows, err := p.pg.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("querying projects: %w", err)
	}
	defer rows.Close()

	projects := []db.ProjectInfo{}
	for rows.Next() {
		var pi db.ProjectInfo
		if err := rows.Scan(&pi.Name, &pi.SessionCount); err != nil {
			return nil, fmt.Errorf("scanning project: %w", err)
		}
		projects = append(projects, pi)
	}
	return projects, rows.Err()
}

// GetAgents returns distinct agent names with session counts.
func (p *PGDB) GetAgents(
	ctx context.Context, excludeOneShot bool,
) ([]db.AgentInfo, error) {
	q := `SELECT agent, COUNT(*) as session_count
		FROM agentsview.sessions
		WHERE message_count > 0 AND agent <> ''
		  AND deleted_at IS NULL
		  AND relationship_type NOT IN ('subagent', 'fork')`
	if excludeOneShot {
		q += " AND user_message_count > 1"
	}
	q += " GROUP BY agent ORDER BY agent"
	rows, err := p.pg.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("querying agents: %w", err)
	}
	defer rows.Close()

	agents := []db.AgentInfo{}
	for rows.Next() {
		var a db.AgentInfo
		if err := rows.Scan(&a.Name, &a.SessionCount); err != nil {
			return nil, fmt.Errorf("scanning agent: %w", err)
		}
		agents = append(agents, a)
	}
	return agents, rows.Err()
}

// GetMachines returns distinct machine names.
func (p *PGDB) GetMachines(
	ctx context.Context, excludeOneShot bool,
) ([]string, error) {
	q := "SELECT DISTINCT machine FROM agentsview.sessions WHERE deleted_at IS NULL"
	if excludeOneShot {
		q += " AND user_message_count > 1"
	}
	q += " ORDER BY machine"
	rows, err := p.pg.QueryContext(ctx, q)
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
