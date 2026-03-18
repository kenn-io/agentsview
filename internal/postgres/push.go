package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"maps"
	"sort"
	"strings"
	"time"

	"github.com/wesm/agentsview/internal/db"
)

const lastPushBoundaryStateKey = "last_push_boundary_state"

// syncStateStore abstracts sync state read/write operations on the
// local database. Used by push boundary state helpers.
type syncStateStore interface {
	GetSyncState(key string) (string, error)
	SetSyncState(key, value string) error
}

type pushBoundaryState struct {
	Cutoff       string            `json:"cutoff"`
	Fingerprints map[string]string `json:"fingerprints"`
}

// PushResult summarizes a push sync operation.
type PushResult struct {
	SessionsPushed int
	MessagesPushed int
	Errors         int
	Duration       time.Duration
}

// Push syncs local sessions and messages to PostgreSQL.
// Only sessions modified since the last push are processed.
// When full is true, the per-message content heuristic is
// bypassed and every candidate session's messages are
// re-pushed unconditionally.
//
// Known limitation: sessions that are permanently deleted
// from SQLite (via prune) are not propagated as deletions
// to PG because the local rows no longer exist at push time.
// Sessions soft-deleted with deleted_at are synced correctly.
// Use a direct PG DELETE to remove permanently pruned
// sessions from PG if needed.
func (s *Sync) Push(
	ctx context.Context, full bool,
) (PushResult, error) {
	start := time.Now()
	var result PushResult

	if err := s.normalizeSyncTimestamps(ctx); err != nil {
		return result, err
	}

	lastPush, err := s.local.GetSyncState("last_push_at")
	if err != nil {
		return result, fmt.Errorf(
			"reading last_push_at: %w", err,
		)
	}
	if full {
		lastPush = ""
		// Caller requested a full push — the PG schema
		// may have been dropped since schemaDone was set.
		// Clear the memo so EnsureSchema re-runs.
		s.schemaMu.Lock()
		s.schemaDone = false
		s.schemaMu.Unlock()
		if err := s.normalizeSyncTimestamps(
			ctx,
		); err != nil {
			return result, err
		}
	}

	// Coherence check: if the local watermark says we've
	// pushed before but PG has zero sessions for this
	// machine, the PG side was reset (schema dropped, DB
	// recreated, etc.). Force a full push so all sessions
	// are re-synced.
	if lastPush != "" {
		pgCount, cErr := s.pgSessionCount(ctx)
		if cErr != nil {
			return result, cErr
		}
		if pgCount == 0 {
			log.Printf(
				"pgsync: local watermark set but PG has "+
					"0 sessions for machine %q; "+
					"forcing full push",
				s.machine,
			)
			lastPush = ""
			full = true
			s.schemaMu.Lock()
			s.schemaDone = false
			s.schemaMu.Unlock()
			if err := s.normalizeSyncTimestamps(
				ctx,
			); err != nil {
				return result, err
			}
		}
	}

	cutoff := time.Now().UTC().Format(LocalSyncTimestampLayout)

	allSessions, err := s.local.ListSessionsModifiedBetween(
		ctx, lastPush, cutoff,
	)
	if err != nil {
		return result, fmt.Errorf(
			"listing modified sessions: %w", err,
		)
	}

	sessionByID := make(
		map[string]db.Session, len(allSessions),
	)
	for _, sess := range allSessions {
		sessionByID[sess.ID] = sess
	}

	var priorFingerprints map[string]string
	var boundaryState map[string]string
	var boundaryOK bool
	if !full {
		var bErr error
		priorFingerprints, boundaryState, boundaryOK, bErr = readBoundaryAndFingerprints(
			s.local, lastPush,
		)
		if bErr != nil {
			return result, bErr
		}
	}

	if lastPush != "" {
		ok := boundaryOK
		windowStart, err := PreviousLocalSyncTimestamp(
			lastPush,
		)
		if err != nil {
			return result, fmt.Errorf(
				"computing push boundary window before %s: %w",
				lastPush, err,
			)
		}
		boundarySessions, err := s.local.ListSessionsModifiedBetween(
			ctx, windowStart, lastPush,
		)
		if err != nil {
			return result, fmt.Errorf(
				"listing push boundary sessions: %w", err,
			)
		}

		for _, sess := range boundarySessions {
			marker := localSessionSyncMarker(sess)
			if marker != lastPush {
				continue
			}
			if ok {
				fp := sessionPushFingerprint(sess)
				if boundaryState[sess.ID] == fp {
					continue
				}
			}
			if _, exists := sessionByID[sess.ID]; exists {
				continue
			}
			sessionByID[sess.ID] = sess
		}
	}

	if len(priorFingerprints) > 0 {
		for id, sess := range sessionByID {
			fp := sessionPushFingerprint(sess)
			if priorFingerprints[id] == fp {
				delete(sessionByID, id)
			}
		}
	}

	var sessions []db.Session
	for _, sess := range sessionByID {
		sessions = append(sessions, sess)
	}
	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].ID < sessions[j].ID
	})

	if len(sessions) == 0 {
		if err := finalizePushState(
			s.local, cutoff, sessions, nil,
		); err != nil {
			return result, err
		}
		result.Duration = time.Since(start)
		return result, nil
	}

	var pushed []db.Session
	const batchSize = 50
	for i := 0; i < len(sessions); i += batchSize {
		end := min(i+batchSize, len(sessions))
		batch := sessions[i:end]

		batchResult, err := s.pushBatch(
			ctx, batch, full, &pushed,
		)
		if err != nil {
			return result, err
		}
		if batchResult.ok {
			result.SessionsPushed += batchResult.sessions
			result.MessagesPushed += batchResult.messages
			continue
		}
		// Batch failed — retry each session individually
		// so one bad session doesn't block the rest.
		for _, sess := range batch {
			sr, retryErr := s.pushBatch(
				ctx, []db.Session{sess},
				full, &pushed,
			)
			if retryErr != nil {
				return result, retryErr
			}
			if sr.ok {
				result.SessionsPushed += sr.sessions
				result.MessagesPushed += sr.messages
			} else {
				result.Errors++
			}
		}
	}

	// When all sessions succeeded, advance the watermark to
	// cutoff. When some failed, keep the watermark at lastPush
	// so the failed sessions (plus any already-pushed ones) are
	// re-evaluated next time. Already-pushed sessions are
	// fingerprint-matched and skipped cheaply.
	finalizeCutoff := cutoff
	var mergedFingerprints map[string]string
	if result.Errors > 0 {
		finalizeCutoff = lastPush
		mergedFingerprints = priorFingerprints
	}
	if err := finalizePushState(
		s.local, finalizeCutoff, pushed,
		mergedFingerprints,
	); err != nil {
		return result, err
	}

	result.Duration = time.Since(start)
	return result, nil
}

// pgSessionCount returns the number of sessions in PG for
// this machine. Used to detect schema resets.
func (s *Sync) pgSessionCount(
	ctx context.Context,
) (int, error) {
	var count int
	err := s.pg.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM sessions WHERE machine = $1",
		s.machine,
	).Scan(&count)
	if err != nil {
		if isUndefinedTable(err) {
			return 0, nil
		}
		return 0, fmt.Errorf(
			"counting pg sessions: %w", err,
		)
	}
	return count, nil
}

type batchResult struct {
	ok       bool
	sessions int
	messages int
}

// pushBatch pushes a slice of sessions within a single
// transaction. On success it appends to pushed and returns
// ok=true with session/message counts. On a session-level
// error it rolls back and returns ok=false so the caller
// can retry individually. Fatal errors (BeginTx failure)
// return a non-nil error.
func (s *Sync) pushBatch(
	ctx context.Context,
	batch []db.Session,
	full bool,
	pushed *[]db.Session,
) (batchResult, error) {
	tx, err := s.pg.BeginTx(ctx, nil)
	if err != nil {
		return batchResult{}, fmt.Errorf(
			"begin pg tx: %w", err,
		)
	}

	n := 0
	msgs := 0
	for _, sess := range batch {
		if err := s.pushSession(
			ctx, tx, sess,
		); err != nil {
			log.Printf(
				"pgsync: session %s: %v",
				sess.ID, err,
			)
			_ = tx.Rollback()
			*pushed = (*pushed)[:len(*pushed)-n]
			return batchResult{}, nil
		}

		msgCount, err := s.pushMessages(
			ctx, tx, sess.ID, full,
		)
		if err != nil {
			log.Printf(
				"pgsync: session %s: %v",
				sess.ID, err,
			)
			_ = tx.Rollback()
			*pushed = (*pushed)[:len(*pushed)-n]
			return batchResult{}, nil
		}

		// Bump updated_at when messages were rewritten
		// but pushSession was a metadata no-op (its
		// WHERE clause skips unchanged rows).
		if msgCount > 0 {
			if _, err := tx.ExecContext(ctx, `
				UPDATE sessions
				SET updated_at = NOW()
				WHERE id = $1`,
				sess.ID,
			); err != nil {
				log.Printf(
					"pgsync: bumping updated_at %s: %v",
					sess.ID, err,
				)
				_ = tx.Rollback()
				*pushed = (*pushed)[:len(*pushed)-n]
				return batchResult{}, nil
			}
		}

		*pushed = append(*pushed, sess)
		n++
		msgs += msgCount
	}

	if err := tx.Commit(); err != nil {
		log.Printf(
			"pgsync: batch commit failed: %v", err,
		)
		*pushed = (*pushed)[:len(*pushed)-n]
		return batchResult{}, nil
	}
	return batchResult{ok: true, sessions: n, messages: msgs}, nil
}

func finalizePushState(
	local syncStateStore,
	cutoff string,
	sessions []db.Session,
	priorFingerprints map[string]string,
) error {
	if err := local.SetSyncState(
		"last_push_at", cutoff,
	); err != nil {
		return fmt.Errorf("updating last_push_at: %w", err)
	}
	return writePushBoundaryState(
		local, cutoff, sessions, priorFingerprints,
	)
}

func readBoundaryAndFingerprints(
	local syncStateStore,
	cutoff string,
) (
	fingerprints map[string]string,
	boundary map[string]string,
	boundaryOK bool,
	err error,
) {
	raw, err := local.GetSyncState(
		lastPushBoundaryStateKey,
	)
	if err != nil {
		return nil, nil, false, fmt.Errorf(
			"reading %s: %w",
			lastPushBoundaryStateKey, err,
		)
	}
	if raw == "" {
		return nil, nil, false, nil
	}
	var state pushBoundaryState
	if err := json.Unmarshal(
		[]byte(raw), &state,
	); err != nil {
		return nil, nil, false, nil
	}
	fingerprints = state.Fingerprints
	if cutoff != "" &&
		state.Cutoff == cutoff &&
		state.Fingerprints != nil {
		boundary = state.Fingerprints
		boundaryOK = true
	}
	return fingerprints, boundary, boundaryOK, nil
}

func writePushBoundaryState(
	local syncStateStore,
	cutoff string,
	sessions []db.Session,
	priorFingerprints map[string]string,
) error {
	state := pushBoundaryState{
		Cutoff: cutoff,
		Fingerprints: make(
			map[string]string,
			len(priorFingerprints)+len(sessions),
		),
	}
	maps.Copy(state.Fingerprints, priorFingerprints)
	for _, sess := range sessions {
		state.Fingerprints[sess.ID] = sessionPushFingerprint(sess)
	}
	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf(
			"encoding %s: %w",
			lastPushBoundaryStateKey, err,
		)
	}
	if err := local.SetSyncState(
		lastPushBoundaryStateKey, string(data),
	); err != nil {
		return fmt.Errorf(
			"writing %s: %w",
			lastPushBoundaryStateKey, err,
		)
	}
	return nil
}

func localSessionSyncMarker(sess db.Session) string {
	marker, err := NormalizeLocalSyncTimestamp(sess.CreatedAt)
	if err != nil || marker == "" {
		if err != nil {
			log.Printf(
				"pgsync: normalizing CreatedAt %q for "+
					"session %s: %v (skipping non-RFC3339 "+
					"value)",
				sess.CreatedAt, sess.ID, err,
			)
		}
		marker = ""
	}
	for _, value := range []*string{
		sess.LocalModifiedAt,
		sess.EndedAt,
		sess.StartedAt,
	} {
		if value == nil {
			continue
		}
		normalized, err := NormalizeLocalSyncTimestamp(*value)
		if err != nil {
			continue
		}
		if normalized > marker {
			marker = normalized
		}
	}
	if sess.FileMtime != nil {
		fileMtime := time.Unix(
			0, *sess.FileMtime,
		).UTC().Format(LocalSyncTimestampLayout)
		if fileMtime > marker {
			marker = fileMtime
		}
	}
	if marker == "" {
		log.Printf(
			"pgsync: session %s: all timestamps failed "+
				"normalization, falling back to raw "+
				"CreatedAt %q",
			sess.ID, sess.CreatedAt,
		)
		marker = sess.CreatedAt
	}
	return marker
}

func sessionPushFingerprint(sess db.Session) string {
	fields := []string{
		sess.ID,
		sess.Project,
		sess.Machine,
		sess.Agent,
		stringValue(sess.FirstMessage),
		stringValue(sess.DisplayName),
		stringValue(sess.StartedAt),
		stringValue(sess.EndedAt),
		stringValue(sess.DeletedAt),
		fmt.Sprintf("%d", sess.MessageCount),
		fmt.Sprintf("%d", sess.UserMessageCount),
		stringValue(sess.ParentSessionID),
		sess.RelationshipType,
		stringValue(sess.FileHash),
		int64Value(sess.FileMtime),
		stringValue(sess.LocalModifiedAt),
		sess.CreatedAt,
	}
	var b strings.Builder
	for _, f := range fields {
		fmt.Fprintf(&b, "%d:%s", len(f), f)
	}
	return b.String()
}

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func int64Value(value *int64) string {
	if value == nil {
		return ""
	}
	return fmt.Sprintf("%d", *value)
}

// nilStr converts a nil or empty *string to SQL NULL.
// Sanitizes before checking emptiness so strings like "\x00"
// that reduce to "" are correctly returned as NULL.
func nilStr(s *string) any {
	if s == nil {
		return nil
	}
	v := sanitizePG(*s)
	if v == "" {
		return nil
	}
	return v
}

// nilStrTS converts a nil or empty *string timestamp to a
// *time.Time for PG TIMESTAMPTZ columns.
func nilStrTS(s *string) any {
	if s == nil || *s == "" {
		return nil
	}
	t, ok := ParseSQLiteTimestamp(*s)
	if !ok {
		return nil
	}
	return t
}

// pushSession upserts a single session into PG.
// File-level metadata (file_hash, file_path, file_size,
// file_mtime) is intentionally not synced to PG -- it is
// local-only and used solely by the sync engine to detect
// re-parsed sessions.
func (s *Sync) pushSession(
	ctx context.Context, tx *sql.Tx, sess db.Session,
) error {
	createdAt, _ := ParseSQLiteTimestamp(sess.CreatedAt)
	_, err := tx.ExecContext(ctx, `
		INSERT INTO sessions (
			id, machine, project, agent,
			first_message, display_name,
			created_at, started_at, ended_at, deleted_at,
			message_count, user_message_count,
			parent_session_id, relationship_type,
			updated_at
		) VALUES (
			$1, $2, $3, $4, $5, $6,
			$7, $8, $9, $10,
			$11, $12, $13, $14, NOW()
		)
		ON CONFLICT (id) DO UPDATE SET
			machine = EXCLUDED.machine,
			project = EXCLUDED.project,
			agent = EXCLUDED.agent,
			first_message = EXCLUDED.first_message,
			display_name = EXCLUDED.display_name,
			created_at = EXCLUDED.created_at,
			started_at = EXCLUDED.started_at,
			ended_at = EXCLUDED.ended_at,
			deleted_at = EXCLUDED.deleted_at,
			message_count = EXCLUDED.message_count,
			user_message_count = EXCLUDED.user_message_count,
			parent_session_id = EXCLUDED.parent_session_id,
			relationship_type = EXCLUDED.relationship_type,
			updated_at = NOW()
		WHERE sessions.machine IS DISTINCT FROM EXCLUDED.machine
			OR sessions.project IS DISTINCT FROM EXCLUDED.project
			OR sessions.agent IS DISTINCT FROM EXCLUDED.agent
			OR sessions.first_message IS DISTINCT FROM EXCLUDED.first_message
			OR sessions.display_name IS DISTINCT FROM EXCLUDED.display_name
			OR sessions.created_at IS DISTINCT FROM EXCLUDED.created_at
			OR sessions.started_at IS DISTINCT FROM EXCLUDED.started_at
			OR sessions.ended_at IS DISTINCT FROM EXCLUDED.ended_at
			OR sessions.deleted_at IS DISTINCT FROM EXCLUDED.deleted_at
			OR sessions.message_count IS DISTINCT FROM EXCLUDED.message_count
			OR sessions.user_message_count IS DISTINCT FROM EXCLUDED.user_message_count
			OR sessions.parent_session_id IS DISTINCT FROM EXCLUDED.parent_session_id
			OR sessions.relationship_type IS DISTINCT FROM EXCLUDED.relationship_type`,
		sess.ID, s.machine,
		sanitizePG(sess.Project),
		sess.Agent,
		nilStr(sess.FirstMessage),
		nilStr(sess.DisplayName),
		createdAt,
		nilStrTS(sess.StartedAt),
		nilStrTS(sess.EndedAt),
		nilStrTS(sess.DeletedAt),
		sess.MessageCount, sess.UserMessageCount,
		nilStr(sess.ParentSessionID),
		sess.RelationshipType,
	)
	return err
}

// pushMessages replaces a session's messages and tool calls
// in PG. It skips the replacement when the PG message count
// already matches the local count, avoiding redundant work
// for metadata-only changes.
func (s *Sync) pushMessages(
	ctx context.Context,
	tx *sql.Tx,
	sessionID string,
	full bool,
) (int, error) {
	localCount, err := s.local.MessageCount(sessionID)
	if err != nil {
		return 0, fmt.Errorf(
			"counting local messages: %w", err,
		)
	}
	if localCount == 0 {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM tool_calls WHERE session_id = $1`,
			sessionID,
		); err != nil {
			return 0, fmt.Errorf(
				"deleting stale pg tool_calls: %w", err,
			)
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM messages WHERE session_id = $1`,
			sessionID,
		); err != nil {
			return 0, fmt.Errorf(
				"deleting stale pg messages: %w", err,
			)
		}
		return 0, nil
	}

	var pgCount int
	var pgContentSum, pgContentMax, pgContentMin int64
	var pgToolCallCount int
	var pgTCContentSum int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*),
			COALESCE(SUM(content_length), 0),
			COALESCE(MAX(content_length), 0),
			COALESCE(MIN(content_length), 0)
		 FROM messages
		 WHERE session_id = $1`,
		sessionID,
	).Scan(
		&pgCount, &pgContentSum,
		&pgContentMax, &pgContentMin,
	); err != nil {
		return 0, fmt.Errorf(
			"counting pg messages: %w", err,
		)
	}
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*),
			COALESCE(SUM(result_content_length), 0)
		 FROM tool_calls
		 WHERE session_id = $1`,
		sessionID,
	).Scan(&pgToolCallCount, &pgTCContentSum); err != nil {
		return 0, fmt.Errorf(
			"counting pg tool_calls: %w", err,
		)
	}

	if !full && pgCount == localCount && pgCount > 0 {
		localSum, localMax, localMin, err := s.local.MessageContentFingerprint(sessionID)
		if err != nil {
			return 0, fmt.Errorf(
				"computing local content fingerprint: %w",
				err,
			)
		}
		localTCCount, err := s.local.ToolCallCount(sessionID)
		if err != nil {
			return 0, fmt.Errorf(
				"counting local tool_calls: %w", err,
			)
		}
		localTCSum, err := s.local.ToolCallContentFingerprint(sessionID)
		if err != nil {
			return 0, fmt.Errorf(
				"computing local tool_call content "+
					"fingerprint: %w", err,
			)
		}
		if localSum == pgContentSum &&
			localMax == pgContentMax &&
			localMin == pgContentMin &&
			localTCCount == pgToolCallCount &&
			localTCSum == pgTCContentSum {
			return 0, nil
		}
	}

	if _, err := tx.ExecContext(ctx, `
		DELETE FROM tool_calls
		WHERE session_id = $1
	`, sessionID); err != nil {
		return 0, fmt.Errorf(
			"deleting pg tool_calls: %w", err,
		)
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM messages
		WHERE session_id = $1
	`, sessionID); err != nil {
		return 0, fmt.Errorf(
			"deleting pg messages: %w", err,
		)
	}

	count := 0
	startOrdinal := 0
	for {
		msgs, err := s.local.GetMessages(
			ctx, sessionID, startOrdinal,
			db.MaxMessageLimit, true,
		)
		if err != nil {
			return count, fmt.Errorf(
				"reading local messages: %w", err,
			)
		}
		if len(msgs) == 0 {
			break
		}

		nextOrdinal := msgs[len(msgs)-1].Ordinal + 1
		if nextOrdinal <= startOrdinal {
			return count, fmt.Errorf(
				"pushMessages %s: ordinal did not "+
					"advance (start=%d, last=%d)",
				sessionID, startOrdinal,
				msgs[len(msgs)-1].Ordinal,
			)
		}

		if err := bulkInsertMessages(
			ctx, tx, sessionID, msgs,
		); err != nil {
			return count, err
		}
		if err := bulkInsertToolCalls(
			ctx, tx, sessionID, msgs,
		); err != nil {
			return count, err
		}
		count += len(msgs)
		startOrdinal = nextOrdinal
	}

	return count, nil
}

const msgInsertBatch = 100

// bulkInsertMessages inserts messages using multi-row VALUES.
func bulkInsertMessages(
	ctx context.Context, tx *sql.Tx,
	sessionID string, msgs []db.Message,
) error {
	for i := 0; i < len(msgs); i += msgInsertBatch {
		end := min(i+msgInsertBatch, len(msgs))
		batch := msgs[i:end]

		var b strings.Builder
		b.WriteString(`INSERT INTO messages (
			session_id, ordinal, role, content,
			timestamp, has_thinking, has_tool_use,
			content_length) VALUES `)
		args := make([]any, 0, len(batch)*8)
		for j, m := range batch {
			if j > 0 {
				b.WriteByte(',')
			}
			p := j*8 + 1
			fmt.Fprintf(&b,
				"($%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d)",
				p, p+1, p+2, p+3,
				p+4, p+5, p+6, p+7,
			)
			var ts any
			if m.Timestamp != "" {
				if t, ok := ParseSQLiteTimestamp(
					m.Timestamp,
				); ok {
					ts = t
				}
			}
			args = append(args,
				sessionID, m.Ordinal, m.Role,
				sanitizePG(m.Content), ts,
				m.HasThinking,
				m.HasToolUse, m.ContentLength,
			)
		}
		if _, err := tx.ExecContext(
			ctx, b.String(), args...,
		); err != nil {
			return fmt.Errorf(
				"bulk inserting messages: %w", err,
			)
		}
	}
	return nil
}

// bulkInsertToolCalls inserts tool calls using multi-row VALUES.
func bulkInsertToolCalls(
	ctx context.Context, tx *sql.Tx,
	sessionID string, msgs []db.Message,
) error {
	// Collect all tool calls from messages.
	type tcRow struct {
		ordinal int
		index   int
		tc      db.ToolCall
	}
	var rows []tcRow
	for _, m := range msgs {
		for i, tc := range m.ToolCalls {
			rows = append(rows, tcRow{m.Ordinal, i, tc})
		}
	}
	if len(rows) == 0 {
		return nil
	}

	const tcBatch = 50
	for i := 0; i < len(rows); i += tcBatch {
		end := min(i+tcBatch, len(rows))
		batch := rows[i:end]

		var b strings.Builder
		b.WriteString(`INSERT INTO tool_calls (
			session_id, tool_name, category,
			call_index, tool_use_id, input_json,
			skill_name, result_content_length,
			result_content, subagent_session_id,
			message_ordinal) VALUES `)
		args := make([]any, 0, len(batch)*11)
		for j, r := range batch {
			if j > 0 {
				b.WriteByte(',')
			}
			p := j*11 + 1
			fmt.Fprintf(&b,
				"($%d,$%d,$%d,$%d,$%d,$%d,"+
					"$%d,$%d,$%d,$%d,$%d)",
				p, p+1, p+2, p+3, p+4, p+5,
				p+6, p+7, p+8, p+9, p+10,
			)
			args = append(args,
				sessionID,
				sanitizePG(r.tc.ToolName),
				sanitizePG(r.tc.Category),
				r.index,
				sanitizePG(r.tc.ToolUseID),
				nilIfEmpty(r.tc.InputJSON),
				nilIfEmpty(r.tc.SkillName),
				nilIfZero(r.tc.ResultContentLength),
				nilIfEmpty(r.tc.ResultContent),
				nilIfEmpty(r.tc.SubagentSessionID),
				r.ordinal,
			)
		}
		if _, err := tx.ExecContext(
			ctx, b.String(), args...,
		); err != nil {
			return fmt.Errorf(
				"bulk inserting tool_calls: %w", err,
			)
		}
	}
	return nil
}

// normalizeSyncTimestamps ensures schema exists and normalizes
// local sync state timestamps.
func (s *Sync) normalizeSyncTimestamps(
	ctx context.Context,
) error {
	s.schemaMu.Lock()
	defer s.schemaMu.Unlock()
	if !s.schemaDone {
		if err := EnsureSchema(
			ctx, s.pg, s.schema,
		); err != nil {
			return err
		}
		s.schemaDone = true
	}
	return NormalizeLocalSyncStateTimestamps(s.local)
}

// sanitizePG strips null bytes and replaces invalid UTF-8
// sequences so text can be safely inserted into PostgreSQL,
// which enforces strict UTF-8 encoding.
func sanitizePG(s string) string {
	s = strings.ReplaceAll(s, "\x00", "")
	s = strings.ToValidUTF8(s, "")
	return s
}

func nilIfEmpty(s string) any {
	s = sanitizePG(s)
	if s == "" {
		return nil
	}
	return s
}

func nilIfZero(n int) any {
	if n == 0 {
		return nil
	}
	return n
}
