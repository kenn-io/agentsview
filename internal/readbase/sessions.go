package readbase

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"

	"go.kenn.io/agentsview/internal/db"
)

// Row is a row from a database driver.
type Row interface{ Scan(...any) error }

// SessionsBackend supplies every SQL operation and typed loader for session reads.
type SessionsBackend interface {
	MemoryBackendName() string
	StarredSessionIDsSQL() (string, []any)
	PinnedMessagesSQL(string, string) (string, []any)
	TrashedSessionsSQL(string) string
	ChildSessionsSQL(string) string
	AllMessagesSQL() string
	SessionVersionColumns() string
	GetMachineMetadataSQL(prefix string) (string, []any)
	ListSecretFindingsSQL(f db.SecretFindingFilter) (string, []any)
	MessageWindowSQL(string, db.MessageWindow) (string, []any, bool)
	// ScanMessages reports terminal rows.Err().
	ScanMessages(db.MessageRows) ([]db.Message, error)
	// A zero ordinal count selects the whole session.
	ToolCallsSQL(ordinals int) string
	ToolResultEventsSQL(ordinals int) string
	TurnRows(context.Context, *db.Session) ([]db.TurnRow, error)
	CallRows(context.Context, string) ([]db.CallRow, error)
	Dialect() db.QueryDialect
	SessionColumns(bool) string
	// ScanSessions reports terminal rows.Err().
	ScanSessions(*sql.Rows, bool) ([]db.Session, error)
	EncodeCursor(db.SessionCursor) string
	DecodeCursor(string) (db.SessionCursor, error)
	SidebarSQL(db.SessionFilter, db.QueryDialect) (string, []any)
	SessionSQL(bool) string
	ScanSession(Row, bool) (db.Session, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) Row
	FormatTime(any) string
	ParseTime(string) (time.Time, bool)
	ReadError(ReadOperation, error) error
}

// Sessions shares session and transcript reads across mirrors.
type Sessions struct {
	backend SessionsBackend
}

func NewSessions(backend SessionsBackend) Sessions {
	return Sessions{backend: backend}
}

func (s Sessions) ListSessions(ctx context.Context, f db.SessionFilter) (db.SessionPage, error) {
	f.Limit = db.NormalizeSessionLimit(f.Limit)
	dialect := s.backend.Dialect()
	where, args := db.BuildSessionFilterSQL(f, dialect)
	rs := db.ResolveSort(f)
	total := 0
	var cur db.SessionCursor
	if f.Cursor != "" {
		var err error
		cur, err = s.backend.DecodeCursor(f.Cursor)
		if err != nil {
			return db.SessionPage{}, err
		}
		total = cur.Total
	}
	var exclusions *db.SessionExclusionCounts
	if f.WantsExclusionCounts() {
		var err error
		total, exclusions, err = db.CountSessionExclusions(ctx, s.backend, f, dialect)
		if err != nil {
			return db.SessionPage{}, fmt.Errorf("counting session exclusions: %w", err)
		}
	} else if total <= 0 {
		if err := s.backend.QueryRowContext(ctx,
			CountSessionsSQL(dialect.SignedAggregate("COUNT(*)"), where), args...,
		).Scan(&total); err != nil {
			return db.SessionPage{}, fmt.Errorf("counting %s sessions: %w", s.backend.MemoryBackendName(), err)
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
	columns := s.backend.SessionColumns(f.IncludeSource)
	query := "SELECT " + columns +
		" FROM sessions WHERE " + cursorWhere + " " +
		pageBuilder.OrderByClause(rs, f) + " " +
		pageBuilder.Limit(f.Limit+1)
	cursorArgs = append(cursorArgs, pageBuilder.Args()...)
	rows, err := s.backend.QueryContext(ctx, query, cursorArgs...) //nolint:kennlint // rowserrcheck: ScanSessions checks rows.Err().
	if err != nil {
		return db.SessionPage{}, fmt.Errorf("querying %s sessions: %w", s.backend.MemoryBackendName(), err)
	}
	defer rows.Close()
	sessions, err := s.backend.ScanSessions(rows, f.IncludeSource)
	if err != nil {
		return db.SessionPage{}, err
	}
	exclusions.LoadChildren(ctx, s.backend, f, dialect, total)
	page := db.BuildSessionPage(sessions, total, f, rs, s.backend.EncodeCursor)
	page.ExclusionCounts = exclusions
	return page, nil
}

func (s Sessions) GetSidebarSessionIndex(ctx context.Context, f db.SessionFilter) (db.SidebarSessionIndex, error) {
	f.IncludeChildren = true
	f.IncludeOrphans = true
	f.SidebarTree = true
	if f.Limit > 0 || f.Cursor != "" || f.Starred {
		return s.getSidebarSessionIndexPage(ctx, f)
	}

	dialect := s.backend.Dialect()
	rootFilter := f
	rootFilter.IncludeChildren = false
	rootWhere, args := db.BuildSessionBaseFilterSQL(rootFilter, dialect)
	rootWhere += " AND " + db.BuildCanonicalRootWhere(dialect, "sessions", f.IncludeOrphans)
	query := CountSessionsSQL(dialect.SignedAggregate("COUNT(*)"), rootWhere)
	var total int
	if err := s.backend.QueryRowContext(ctx, query, args...).Scan(&total); err != nil {
		return db.SidebarSessionIndex{}, fmt.Errorf("counting %s sidebar roots: %w", s.backend.MemoryBackendName(), err)
	}
	query, args = s.backend.SidebarSQL(f, dialect)
	rows, err := s.backend.QueryContext(ctx, query, args...)
	if err != nil {
		return db.SidebarSessionIndex{}, fmt.Errorf("querying %s sidebar session index: %w", s.backend.MemoryBackendName(), err)
	}

	defer rows.Close()

	index := db.SidebarSessionIndex{Sessions: []db.SidebarSessionIndexRow{}, Total: total}
	for rows.Next() {
		row, err := s.scanSidebarRow(rows, nil)
		if err != nil {
			return db.SidebarSessionIndex{}, err
		}
		index.Sessions = append(index.Sessions, row)
	}
	if err := rows.Err(); err != nil {
		return db.SidebarSessionIndex{}, fmt.Errorf("iterating %s sidebar session index: %w", s.backend.MemoryBackendName(), err)
	}
	return index, nil
}

func (s Sessions) GetSession(ctx context.Context, id string) (*db.Session, error) {
	row := s.backend.QueryRowContext(ctx, s.backend.SessionSQL(false), id)
	sess, err := s.backend.ScanSession(row, false)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("getting %s session: %w", s.backend.MemoryBackendName(), err)
	}
	return &sess, nil
}

func (s Sessions) GetSessionFull(ctx context.Context, id string) (*db.Session, error) {
	row := s.backend.QueryRowContext(ctx, s.backend.SessionSQL(true), id)
	sess, err := s.backend.ScanSession(row, true)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("getting %s full session: %w", s.backend.MemoryBackendName(), err)
	}
	return &sess, nil
}

func (s Sessions) ListTrashedSessions(ctx context.Context) ([]db.Session, error) {
	rows, err := s.backend.QueryContext(ctx, s.backend.TrashedSessionsSQL(s.backend.SessionColumns(false))) //nolint:kennlint // rowserrcheck: ScanSessions checks rows.Err().
	if err != nil {
		return nil, fmt.Errorf("listing %s trash: %w", s.backend.MemoryBackendName(), err)
	}
	defer rows.Close()
	return s.backend.ScanSessions(rows, false)
}

func (s Sessions) GetChildSessions(ctx context.Context, parentID string) ([]db.Session, error) {
	rows, err := s.backend.QueryContext(ctx, s.backend.ChildSessionsSQL(s.backend.SessionColumns(false)), parentID) //nolint:kennlint // rowserrcheck: ScanSessions checks rows.Err().
	if err != nil {
		return nil, fmt.Errorf("querying %s child sessions: %w", s.backend.MemoryBackendName(), err)
	}
	defer rows.Close()
	return s.backend.ScanSessions(rows, false)
}

type sessionVersionRow struct {
	count       int
	mtime       sql.NullInt64
	hash        sql.NullString
	updated     any
	fingerprint string
}

func (v *sessionVersionRow) fields() []any {
	return []any{&v.count, &v.mtime, &v.hash, &v.updated, &v.fingerprint}
}

func (v *sessionVersionRow) version(backend SessionsBackend) int64 {
	mtime, hash := "", ""
	if v.mtime.Valid {
		mtime = strconv.FormatInt(v.mtime.Int64, 10)
	}
	if v.hash.Valid {
		hash = v.hash.String
	}
	return db.SessionVersionMarker(mtime, hash, backend.FormatTime(v.updated), v.fingerprint)
}

func (s Sessions) GetSessionVersion(ctx context.Context, id string) (int, int64, bool) {
	var v sessionVersionRow
	if err := s.backend.QueryRowContext(ctx, "SELECT "+s.backend.SessionVersionColumns()+" FROM sessions WHERE id = ?", id).Scan(v.fields()...); err != nil {
		return 0, 0, false
	}
	return v.count, v.version(s.backend), true
}

func (s Sessions) scanSidebarRow(rows *sql.Rows, dest []any) (db.SidebarSessionIndexRow, error) {
	var row db.SidebarSessionIndexRow
	var startedAt, endedAt, createdAt any
	dest = append(dest,
		&row.ID, &row.ParentSessionID, &row.RelationshipType,
		&row.Project, &row.ProjectAssigned, &row.Machine, &row.Agent,
		&row.AgentLabel, &row.Entrypoint, &row.SessionKind, &row.DisplayName,
		&startedAt, &endedAt, &createdAt,
		&row.TerminationStatus, &row.MessageCount, &row.UserMessageCount,
		&row.TranscriptRevision, &row.IsAutomated, &row.IsTeammate,
	)
	if err := rows.Scan(dest...); err != nil {
		return db.SidebarSessionIndexRow{}, fmt.Errorf("scanning %s sidebar session index: %w", s.backend.MemoryBackendName(), err)
	}
	if v := s.backend.FormatTime(startedAt); v != "" {
		row.StartedAt = &v
	}
	if v := s.backend.FormatTime(endedAt); v != "" {
		row.EndedAt = &v
	}
	row.CreatedAt = s.backend.FormatTime(createdAt)
	return row, nil
}

func (s Sessions) getSidebarSessionIndexPage(ctx context.Context, f db.SessionFilter) (db.SidebarSessionIndex, error) {
	f.Limit = db.NormalizeSessionLimit(f.Limit)
	dialect := s.backend.Dialect()
	var cur db.SessionCursor
	if f.Cursor != "" {
		var err error
		cur, err = s.backend.DecodeCursor(f.Cursor)
		if err != nil {
			return db.SidebarSessionIndex{}, err
		}
	}
	index := db.SidebarSessionIndex{Sessions: []db.SidebarSessionIndexRow{}, Total: cur.Total}
	queries := SessionQueries{}
	if index.Total <= 0 {
		query, args := queries.SidebarRootPageSQL(f, dialect, cur, true)
		if err := s.backend.QueryRowContext(ctx, query, args...).Scan(&index.Total); err != nil {
			return db.SidebarSessionIndex{}, fmt.Errorf("counting %s sidebar roots: %w", s.backend.MemoryBackendName(), err)
		}
	}
	query, args := queries.SidebarRootPageSQL(f, dialect, cur, false)
	rows, err := s.backend.QueryContext(ctx, query, args...)
	if err != nil {
		return db.SidebarSessionIndex{}, fmt.Errorf("querying %s sidebar root page: %w", s.backend.MemoryBackendName(), err)
	}
	defer rows.Close()
	ids := []string{}
	activities := []string{}
	for rows.Next() {
		var id string
		var activity any
		if err := rows.Scan(&id, &activity); err != nil {
			return db.SidebarSessionIndex{}, fmt.Errorf("scanning %s sidebar root page: %w", s.backend.MemoryBackendName(), err)
		}
		ids = append(ids, id)
		activities = append(activities, s.backend.FormatTime(activity))
	}
	if err := rows.Err(); err != nil {
		return db.SidebarSessionIndex{}, fmt.Errorf("iterating %s sidebar root page: %w", s.backend.MemoryBackendName(), err)
	}
	if err := rows.Close(); err != nil {
		return db.SidebarSessionIndex{}, err
	}
	if len(ids) == 0 {
		return index, nil
	}
	if len(ids) > f.Limit {
		ids = ids[:f.Limit]
		index.NextCursor = s.backend.EncodeCursor(db.SessionCursor{
			EndedAt: activities[f.Limit-1], ID: ids[f.Limit-1], Total: index.Total,
		})
	}
	query, args = queries.SidebarTreePageSQL(f, dialect, ids)
	treeRows, err := s.backend.QueryContext(ctx, query, args...)
	if err != nil {
		return db.SidebarSessionIndex{}, fmt.Errorf("querying %s sidebar tree page: %w", s.backend.MemoryBackendName(), err)
	}
	defer treeRows.Close()
	// A session reachable from several roots belongs to the earliest root.
	seen := map[string]struct{}{}
	for treeRows.Next() {
		var rootID string
		row, err := s.scanSidebarRow(treeRows, []any{&rootID})
		if err != nil {
			return db.SidebarSessionIndex{}, err
		}
		if _, exists := seen[row.ID]; exists {
			continue
		}
		seen[row.ID] = struct{}{}
		index.Sessions = append(index.Sessions, row)
	}
	if err := treeRows.Err(); err != nil {
		return db.SidebarSessionIndex{}, fmt.Errorf("iterating %s sidebar tree page: %w", s.backend.MemoryBackendName(), err)
	}
	return index, nil
}

// GetSessionTreeMarker reads the parent and its direct children together.
func (s Sessions) GetSessionTreeMarker(ctx context.Context, id string) (db.SessionTreeSnapshot, error) {
	rows, err := s.backend.QueryContext(ctx, "SELECT id, "+s.backend.SessionVersionColumns()+" FROM sessions WHERE id = ? OR (parent_session_id = ? AND deleted_at IS NULL)", id, id)
	if err != nil {
		return db.SessionTreeSnapshot{}, err
	}
	defer rows.Close()
	var parent *db.SessionTreeEntry
	var children []db.SessionTreeEntry
	for rows.Next() {
		var e db.SessionTreeEntry
		var v sessionVersionRow
		if err := rows.Scan(append([]any{&e.ID}, v.fields()...)...); err != nil {
			return db.SessionTreeSnapshot{}, err
		}
		e.Count, e.Version = v.count, v.version(s.backend)
		if e.ID == id {
			parent = &e
		} else {
			children = append(children, e)
		}
	}
	if err := rows.Err(); err != nil {
		return db.SessionTreeSnapshot{}, err
	}
	if parent == nil {
		return db.SessionTreeSnapshot{}, errors.New("session not found")
	}
	return db.NewSessionTreeSnapshot(append([]db.SessionTreeEntry{*parent}, children...))
}
