package duckdb

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/readbase"
)

type sessionsSQL struct{ *Store }

func (s sessionsSQL) StarredSessionIDsSQL() (string, []any) {
	return (readbase.SessionQueries{}).StarredSessionIDsSQL()
}

func (s sessionsSQL) PinnedMessagesSQL(sessionID, project string) (string, []any) {
	return (readbase.SessionQueries{}).PinnedMessagesSQL(sessionID, project)
}

func (s sessionsSQL) TrashedSessionsSQL(columns string) string {
	return (readbase.SessionQueries{}).TrashedSessionsSQL(columns)
}

func (s sessionsSQL) ChildSessionsSQL(columns string) string {
	return (readbase.SessionQueries{}).ChildSessionsSQL(columns)
}

func (s sessionsSQL) AllMessagesSQL() string {
	return (readbase.SessionQueries{}).AllMessagesSQL()
}

func (s sessionsSQL) SessionVersionSQL() string {
	return (readbase.SessionQueries{}).SessionVersionSQL()
}

func (s sessionsSQL) SidebarSQL(f db.SessionFilter, dialect db.QueryDialect) (string, []any) {
	return (readbase.SessionQueries{}).SidebarSQL(f, dialect)
}

func (s *Store) sessions() readbase.Sessions {
	return readbase.NewSessions(sessionsSQL{Store: s})
}

func (s *Store) ListStarredSessionIDs(ctx context.Context) ([]string, error) {
	return s.sessions().ListStarredSessionIDs(ctx)
}

func (s *Store) ListPinnedMessages(
	ctx context.Context, sessionID string, project string,
) ([]db.PinnedMessage, error) {
	return s.sessions().ListPinnedMessages(ctx, sessionID, project)
}

func (s sessionsSQL) GetMachineMetadataSQL(prefix string) (string, []any) {
	return `
		SELECT key, value FROM sync_metadata
		WHERE key LIKE ? ESCAPE '\'`, []any{strings.ReplaceAll(prefix, "_", "\\_") + "%"}
}

func (s sessionsSQL) ListSecretFindingsSQL(f db.SecretFindingFilter) (string, []any) {
	return readbase.SecretFindingsSQL(f, "CAST(COALESCE(s.started_at, s.created_at) AS DATE)", "CAST(? AS DATE)", "sf.id")
}

func (s *Store) ListSecretFindings(
	ctx context.Context, f db.SecretFindingFilter,
) (db.SecretFindingPage, error) {
	return s.sessions().ListSecretFindings(ctx, f)
}

func (s *Store) GetMachineLabels(ctx context.Context) (map[string]string, error) {
	return s.sessions().GetMachineLabels(ctx)
}

func (s *Store) GetMachineAliases(ctx context.Context) (map[string]string, error) {
	return s.sessions().GetMachineAliases(ctx)
}

func (s *Store) SecretFindingSource(
	ctx context.Context, f db.SecretFinding,
) (string, bool, error) {
	return s.sessions().SecretFindingSource(ctx, f)
}

func (s *Store) GetMessages(ctx context.Context, sessionID string, from, limit int, asc bool) ([]db.Message, error) {
	return s.sessions().GetMessages(ctx, sessionID, from, limit, asc)
}

func (s *Store) GetMessagesWindow(ctx context.Context, sessionID string, w db.MessageWindow) ([]db.Message, error) {
	return s.sessions().GetMessagesWindow(ctx, sessionID, w)
}

func (s *Store) GetAllMessages(ctx context.Context, sessionID string) ([]db.Message, error) {
	return s.sessions().GetAllMessages(ctx, sessionID)
}

func (s sessionsSQL) ToolCallsSQL() string {
	return `
		SELECT m.ordinal, tc.call_index, tc.tool_name, tc.category,
			COALESCE(tc.tool_use_id, ''), COALESCE(tc.input_json, ''),
			COALESCE(tc.skill_name, ''), COALESCE(tc.result_content_length, 0),
			COALESCE(tc.result_content, ''),
			COALESCE(tc.subagent_session_id, ''),
			COALESCE(tc.file_path, ''), tc.observed_outcome, tc.observed_repeat, tc.sequence_ending
		FROM tool_calls tc
		JOIN messages m ON m.session_id = tc.session_id
			AND m.id = tc.message_id
		WHERE tc.session_id = ?
		ORDER BY m.ordinal, tc.call_index`
}

func (s sessionsSQL) ToolResultEventsSQL() string {
	return `
		SELECT tool_call_message_ordinal, call_index,
			COALESCE(tool_use_id, ''), COALESCE(agent_id, ''),
			COALESCE(subagent_session_id, ''), source, status,
			content, content_length, timestamp, event_index
		FROM tool_result_events
		WHERE session_id = ?
		ORDER BY tool_call_message_ordinal, call_index, event_index`
}

func (s *Store) GetSessionActivity(ctx context.Context, sessionID string) (*db.SessionActivityResponse, error) {
	return s.sessions().GetSessionActivity(ctx, sessionID)
}

func (s *Store) GetSessionTiming(ctx context.Context, sessionID string) (*db.SessionTiming, error) {
	return s.sessions().GetSessionTiming(ctx, sessionID)
}

func (s sessionsSQL) TurnRows(ctx context.Context, session *db.Session) ([]db.TurnRow, error) {
	return s.queryTurnRows(ctx, session)
}

func (s sessionsSQL) CallRows(ctx context.Context, id string) ([]db.CallRow, error) {
	return s.queryCallRows(ctx, id)
}

func (s *Store) ListSessions(ctx context.Context, f db.SessionFilter) (db.SessionPage, error) {
	return s.sessions().ListSessions(ctx, f)
}

func (s sessionsSQL) Dialect() db.QueryDialect { return db.DuckDBQueryDialect() }

func (s sessionsSQL) SessionColumns(source bool) string {
	if source {
		return duckSessionCols + ", file_path, file_size, local_modified_at"
	}
	return duckSessionCols
}

func (s sessionsSQL) ScanSessions(rows *sql.Rows, source bool) ([]db.Session, error) {
	return scanSessionRowsWithSource(rows, source)
}

func (s *Store) GetSidebarSessionIndex(ctx context.Context, f db.SessionFilter) (db.SidebarSessionIndex, error) {
	return s.sessions().GetSidebarSessionIndex(ctx, f)
}

func (s *Store) GetSession(ctx context.Context, id string) (*db.Session, error) {
	return s.sessions().GetSession(ctx, id)
}

func (s sessionsSQL) SessionSQL(full bool) string {
	if full {
		return "SELECT " + duckSessionCols + " FROM sessions WHERE id = ?"
	}
	return "SELECT " + duckSessionCols + " FROM sessions WHERE id = ? AND deleted_at IS NULL"
}

func (s *Store) GetSessionFull(ctx context.Context, id string) (*db.Session, error) {
	return s.sessions().GetSessionFull(ctx, id)
}

func (s sessionsSQL) ScanSession(row readbase.Row, full bool) (db.Session, error) {
	return scanSession(row)
}

func (s *Store) ListTrashedSessions(ctx context.Context) ([]db.Session, error) {
	return s.sessions().ListTrashedSessions(ctx)
}

func (s *Store) GetChildSessions(ctx context.Context, parentID string) ([]db.Session, error) {
	return s.sessions().GetChildSessions(ctx, parentID)
}

func (s *Store) GetSessionVersion(ctx context.Context, id string) (int, int64, bool) {
	return s.sessions().GetSessionVersion(ctx, id)
}

func (s sessionsSQL) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return s.queryContext(ctx, query, args...)
}

func (s sessionsSQL) QueryRowContext(ctx context.Context, query string, args ...any) readbase.Row {
	return s.queryRowContext(ctx, query, args...)
}

func (s sessionsSQL) FormatTime(value any) string { return formatDBTime(value) }

func (s sessionsSQL) ParseTime(value string) (time.Time, bool) { return parseTimestamp(value) }

func (s sessionsSQL) ReadError(operation readbase.ReadOperation, err error) error {
	switch operation {
	case readbase.ScanAllMessages, readbase.ListStarredSessions, readbase.QueryToolCalls, readbase.ScanToolCall, readbase.QueryToolResultEvents, readbase.ScanToolResultEvent:
		return err
	case readbase.ReadMachineMetadata:
		return fmt.Errorf("reading machine metadata: %w", err)
	default:
		return operation.Wrap(s.MemoryBackendName(), err)
	}
}

func (s sessionsSQL) MessageWindowSQL(sessionID string, w db.MessageWindow) (string, []any, bool) {
	if w.Around == nil && w.ObservedRevision == nil && len(w.Roles) == 0 {
		return readbase.MessageWindowSQL(readbase.MessageColumns, "", nil, sessionID, w)
	}
	return readbase.MessageWindowSQL(readbase.MessageColumns, duckRevisionCol, []any{sessionID}, sessionID, w)
}

func (s sessionsSQL) ScanMessages(rows db.MessageRows) ([]db.Message, error) {
	return scanMessages(rows)
}
