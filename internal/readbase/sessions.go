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

// Row is a row from either a local driver or the Quack transport.
type Row interface{ Scan(...any) error }

// SessionsBackend supplies every SQL operation and typed loader for session reads.
type SessionsBackend interface {
	MemoryBackendName() string
	StarredSessionIDsSQL() (string, []any)
	PinnedMessagesSQL(string, string) (string, []any)
	TrashedSessionsSQL(string) string
	ChildSessionsSQL(string) string
	AllMessagesSQL() string
	SessionVersionSQL() string
	GetMachineMetadataSQL(prefix string) (string, []any)
	ListSecretFindingsSQL(f db.SecretFindingFilter) (string, []any)
	MessageWindowSQL(string, db.MessageWindow) (string, []any, bool)
	// ScanMessages reports terminal rows.Err().
	ScanMessages(db.MessageRows) ([]db.Message, error)
	ToolCallsSQL() string
	ToolResultEventsSQL() string
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
	if total <= 0 {
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
	return db.BuildSessionPage(sessions, total, f, rs, s.backend.EncodeCursor), nil
}

func (s Sessions) GetSidebarSessionIndex(ctx context.Context, f db.SessionFilter) (db.SidebarSessionIndex, error) {
	f.IncludeChildren = true
	f.IncludeOrphans = true
	f.Cursor = ""
	f.Limit = 0

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
			return db.SidebarSessionIndex{}, fmt.Errorf("scanning %s sidebar session index: %w", s.backend.MemoryBackendName(), err)
		}
		if v := s.backend.FormatTime(startedAt); v != "" {
			row.StartedAt = &v
		}
		if v := s.backend.FormatTime(endedAt); v != "" {
			row.EndedAt = &v
		}
		row.CreatedAt = s.backend.FormatTime(createdAt)
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

func (s Sessions) GetSessionVersion(ctx context.Context, id string) (int, int64, bool) {
	var count int
	var fileMtime sql.NullInt64
	var fileHash sql.NullString
	var updated any
	var fingerprint string
	err := s.backend.QueryRowContext(ctx, s.backend.SessionVersionSQL(), id).Scan(&count, &fileMtime, &fileHash, &updated, &fingerprint)
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
	return count, db.SessionVersionMarker(fileMtimePart, fileHashPart, s.backend.FormatTime(updated), fingerprint), true
}
