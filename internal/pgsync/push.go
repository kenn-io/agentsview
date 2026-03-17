package pgsync

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
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
// When full is true, the per-message content heuristic is bypassed
// and every candidate session's messages are re-pushed unconditionally.
//
// Known limitation: sessions that are permanently deleted from
// SQLite (via prune) are not propagated as deletions to PG because
// the local rows no longer exist at push time. Sessions soft-deleted
// with deleted_at are synced correctly. Use a direct PG DELETE to
// remove permanently pruned sessions from PG if needed.
func (p *PGSync) Push(ctx context.Context, full bool) (PushResult, error) {
	start := time.Now()
	var result PushResult

	if err := p.normalizeSyncTimestamps(ctx); err != nil {
		return result, err
	}

	lastPush, err := p.local.GetSyncState("last_push_at")
	if err != nil {
		return result, fmt.Errorf("reading last_push_at: %w", err)
	}
	if full {
		lastPush = ""
	}

	cutoff := time.Now().UTC().Format(localSyncTimestampLayout)

	allSessions, err := p.local.ListSessionsModifiedBetween(
		ctx, lastPush, cutoff,
	)
	if err != nil {
		return result, fmt.Errorf("listing modified sessions: %w", err)
	}

	sessionByID := make(map[string]db.Session, len(allSessions))
	for _, s := range allSessions {
		sessionByID[s.ID] = s
	}
	// Read boundary state once to get both the cutoff-validated
	// map (for boundary replay) and the raw fingerprints (for
	// general dedup). Skip when full=true so -full re-pushes
	// everything.
	var priorFingerprints map[string]string
	var boundaryState map[string]string
	var boundaryOK bool
	if !full {
		var bErr error
		priorFingerprints, boundaryState, boundaryOK, bErr = readBoundaryAndFingerprints(p.local, lastPush)
		if bErr != nil {
			return result, bErr
		}
	}

	// When full=true, lastPush is "" so this block is skipped.
	// boundaryOK stays false, causing all boundary sessions to
	// be conservatively re-pushed — the intended -full behavior.
	if lastPush != "" {
		ok := boundaryOK
		windowStart, err := previousLocalSyncTimestamp(lastPush)
		if err != nil {
			return result, fmt.Errorf(
				"computing push boundary window before %s: %w",
				lastPush, err,
			)
		}
		boundarySessions, err := p.local.ListSessionsModifiedBetween(
			ctx, windowStart, lastPush,
		)
		if err != nil {
			return result, fmt.Errorf(
				"listing push boundary sessions: %w", err,
			)
		}

		for _, s := range boundarySessions {
			marker := localSessionSyncMarker(s)
			if marker != lastPush {
				continue
			}
			if ok {
				fingerprint := sessionPushFingerprint(s)
				if boundaryState[s.ID] == fingerprint {
					continue
				}
			}
			// When ok is false (no prior boundary state, e.g. after
			// process restart), we conservatively re-push all boundary
			// sessions rather than skip them, since we cannot tell
			// whether they were already pushed. The upserts make this
			// redundant work benign.
			if _, exists := sessionByID[s.ID]; exists {
				continue
			}
			sessionByID[s.ID] = s
		}
	}

	// Skip sessions already pushed with unchanged fingerprints.
	// This avoids redundant re-pushes when the watermark is held
	// back due to errors on a prior push cycle.
	if len(priorFingerprints) > 0 {
		for id, s := range sessionByID {
			if priorFingerprints[id] == sessionPushFingerprint(s) {
				delete(sessionByID, id)
			}
		}
	}

	var sessions []db.Session
	for _, s := range sessionByID {
		sessions = append(sessions, s)
	}

	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].ID < sessions[j].ID
	})

	if len(sessions) == 0 {
		if err := finalizePushState(p.local, cutoff, sessions, nil); err != nil {
			return result, err
		}
		result.Duration = time.Since(start)
		return result, nil
	}

	// Each session gets its own PG transaction. If pushMessages
	// fails for a session, we roll back that transaction, log the
	// error, and continue. Only successfully committed sessions
	// are recorded in boundary state so failed ones are retried
	// on the next push cycle.
	var pushed []db.Session
	for _, s := range sessions {
		tx, err := p.pg.BeginTx(ctx, nil)
		if err != nil {
			return result, fmt.Errorf("begin pg tx: %w", err)
		}

		if err := p.pushSession(ctx, tx, s); err != nil {
			_ = tx.Rollback()
			log.Printf("pgsync: skipping session %s: %v", s.ID, err)
			result.Errors++
			continue
		}

		msgCount, err := p.pushMessages(ctx, tx, s.ID, full)
		if err != nil {
			_ = tx.Rollback()
			log.Printf("pgsync: skipping session %s: %v", s.ID, err)
			result.Errors++
			continue
		}

		// Bump updated_at when messages were rewritten so pg-read
		// SSE watchers detect the change even when message_count
		// is unchanged (e.g. content rewrites, -full pushes).
		if msgCount > 0 {
			if _, err := tx.ExecContext(ctx, `
				UPDATE agentsview.sessions
				SET updated_at = `+pgTimestampSQL("NOW() AT TIME ZONE 'UTC'")+`
				WHERE id = $1`,
				s.ID,
			); err != nil {
				_ = tx.Rollback()
				log.Printf("pgsync: skipping session %s: %v", s.ID, err)
				result.Errors++
				continue
			}
		}

		if err := tx.Commit(); err != nil {
			log.Printf("pgsync: skipping session %s: commit: %v", s.ID, err)
			result.Errors++
			continue
		}

		pushed = append(pushed, s)
		result.SessionsPushed++
		result.MessagesPushed += msgCount
	}

	// When any session failed, do not advance last_push_at so
	// failed sessions remain in the next ListSessionsModifiedBetween
	// window. Boundary state still records pushed sessions to avoid
	// redundant re-pushes of successful ones.
	finalizeCutoff := cutoff
	if result.Errors > 0 {
		finalizeCutoff = lastPush
	}
	// When the cutoff is held back (errors kept watermark at
	// lastPush), merge prior fingerprints with newly pushed ones
	// so sessions from earlier successful retries are not
	// re-pushed on subsequent cycles.
	var mergedFingerprints map[string]string
	if finalizeCutoff == lastPush && len(priorFingerprints) > 0 {
		mergedFingerprints = priorFingerprints
	}
	if err := finalizePushState(p.local, finalizeCutoff, pushed, mergedFingerprints); err != nil {
		return result, err
	}

	result.Duration = time.Since(start)
	return result, nil
}

func finalizePushState(local syncStateStore, cutoff string, sessions []db.Session, priorFingerprints map[string]string) error {
	if err := local.SetSyncState("last_push_at", cutoff); err != nil {
		return fmt.Errorf("updating last_push_at: %w", err)
	}
	if err := writePushBoundaryState(local, cutoff, sessions, priorFingerprints); err != nil {
		return err
	}
	return nil
}

// readBoundaryAndFingerprints reads the stored push boundary state
// once and returns both the raw fingerprints (for general dedup,
// regardless of cutoff) and the cutoff-validated boundary state
// (for boundary replay). This avoids redundant reads of the same
// sync state key.
func readBoundaryAndFingerprints(local syncStateStore, cutoff string) (
	fingerprints map[string]string,
	boundary map[string]string,
	boundaryOK bool,
	err error,
) {
	raw, err := local.GetSyncState(lastPushBoundaryStateKey)
	if err != nil {
		return nil, nil, false, fmt.Errorf("reading %s: %w", lastPushBoundaryStateKey, err)
	}
	if raw == "" {
		return nil, nil, false, nil
	}
	var state pushBoundaryState
	if err := json.Unmarshal([]byte(raw), &state); err != nil {
		return nil, nil, false, nil
	}
	fingerprints = state.Fingerprints
	if cutoff != "" && state.Cutoff == cutoff && state.Fingerprints != nil {
		boundary = state.Fingerprints
		boundaryOK = true
	}
	return fingerprints, boundary, boundaryOK, nil
}

// writePushBoundaryState persists boundary fingerprints. When
// priorFingerprints is non-nil, its entries are merged with the
// newly pushed sessions so that earlier successful retries are
// preserved across repeated partial-failure cycles.
func writePushBoundaryState(local syncStateStore, cutoff string, sessions []db.Session, priorFingerprints map[string]string) error {
	state := pushBoundaryState{
		Cutoff:       cutoff,
		Fingerprints: make(map[string]string, len(priorFingerprints)+len(sessions)),
	}
	for id, fp := range priorFingerprints {
		state.Fingerprints[id] = fp
	}
	for _, s := range sessions {
		state.Fingerprints[s.ID] = sessionPushFingerprint(s)
	}
	data, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("encoding %s: %w", lastPushBoundaryStateKey, err)
	}
	if err := local.SetSyncState(lastPushBoundaryStateKey, string(data)); err != nil {
		return fmt.Errorf("writing %s: %w", lastPushBoundaryStateKey, err)
	}
	return nil
}

func localSessionSyncMarker(s db.Session) string {
	marker, err := normalizeLocalSyncTimestamp(s.CreatedAt)
	if err != nil || marker == "" {
		if err != nil {
			log.Printf("pgsync: normalizing CreatedAt %q for session %s: %v (skipping non-RFC3339 value)", s.CreatedAt, s.ID, err)
		}
		marker = ""
	}
	for _, value := range []*string{
		s.LocalModifiedAt,
		s.EndedAt,
		s.StartedAt,
	} {
		if value == nil {
			continue
		}
		normalized, err := normalizeLocalSyncTimestamp(*value)
		if err != nil {
			continue
		}
		if normalized > marker {
			marker = normalized
		}
	}
	if s.FileMtime != nil {
		fileMtime := time.Unix(0, *s.FileMtime).UTC().Format(localSyncTimestampLayout)
		if fileMtime > marker {
			marker = fileMtime
		}
	}
	if marker == "" {
		log.Printf("pgsync: session %s: all timestamps failed normalization, falling back to raw CreatedAt %q", s.ID, s.CreatedAt)
		marker = s.CreatedAt
	}
	return marker
}

func sessionPushFingerprint(s db.Session) string {
	fields := []string{
		s.ID,
		s.Project,
		s.Machine,
		s.Agent,
		stringValue(s.FirstMessage),
		stringValue(s.DisplayName),
		stringValue(s.StartedAt),
		stringValue(s.EndedAt),
		stringValue(s.DeletedAt),
		fmt.Sprintf("%d", s.MessageCount),
		fmt.Sprintf("%d", s.UserMessageCount),
		stringValue(s.ParentSessionID),
		s.RelationshipType,
		stringValue(s.FileHash),
		int64Value(s.FileMtime),
		stringValue(s.LocalModifiedAt),
		s.CreatedAt,
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
func nilStr(s *string) any {
	if s == nil || *s == "" {
		return nil
	}
	return *s
}

// pushSession upserts a single session into PG.
// File-level metadata (file_hash, file_path, file_size, file_mtime)
// is intentionally not synced to PG — it is local-only and used
// solely by the sync engine to detect re-parsed sessions.
func (p *PGSync) pushSession(
	ctx context.Context, tx *sql.Tx, s db.Session,
) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO agentsview.sessions (
			id, machine, project, agent,
			first_message, display_name,
			created_at, started_at, ended_at, deleted_at,
			message_count, user_message_count,
			parent_session_id, relationship_type,
			updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, `+pgTimestampSQL("NOW() AT TIME ZONE 'UTC'")+`)
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
			updated_at = `+pgTimestampSQL("NOW() AT TIME ZONE 'UTC'")+`
		WHERE agentsview.sessions.machine IS DISTINCT FROM EXCLUDED.machine
			OR agentsview.sessions.project IS DISTINCT FROM EXCLUDED.project
			OR agentsview.sessions.agent IS DISTINCT FROM EXCLUDED.agent
			OR agentsview.sessions.first_message IS DISTINCT FROM EXCLUDED.first_message
			OR agentsview.sessions.display_name IS DISTINCT FROM EXCLUDED.display_name
			OR agentsview.sessions.created_at IS DISTINCT FROM EXCLUDED.created_at
			OR agentsview.sessions.started_at IS DISTINCT FROM EXCLUDED.started_at
			OR agentsview.sessions.ended_at IS DISTINCT FROM EXCLUDED.ended_at
			OR agentsview.sessions.deleted_at IS DISTINCT FROM EXCLUDED.deleted_at
			OR agentsview.sessions.message_count IS DISTINCT FROM EXCLUDED.message_count
			OR agentsview.sessions.user_message_count IS DISTINCT FROM EXCLUDED.user_message_count
			OR agentsview.sessions.parent_session_id IS DISTINCT FROM EXCLUDED.parent_session_id
			OR agentsview.sessions.relationship_type IS DISTINCT FROM EXCLUDED.relationship_type`,
		s.ID, p.machine, s.Project, s.Agent,
		nilStr(s.FirstMessage), nilStr(s.DisplayName),
		s.CreatedAt, nilStr(s.StartedAt), nilStr(s.EndedAt), nilStr(s.DeletedAt),
		s.MessageCount, s.UserMessageCount,
		nilStr(s.ParentSessionID), s.RelationshipType,
	)
	return err
}

// pushMessages replaces a session's messages and tool calls in PG.
// It skips the replacement when the PG message count already matches
// the local count, avoiding redundant work for metadata-only changes.
func (p *PGSync) pushMessages(
	ctx context.Context, tx *sql.Tx, sessionID string, full bool,
) (int, error) {
	localCount, err := p.local.MessageCount(sessionID)
	if err != nil {
		return 0, fmt.Errorf("counting local messages: %w", err)
	}
	if localCount == 0 {
		// Clean up any stale PG messages/tool_calls for this session
		// (e.g. local resync re-parsed the file as empty).
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM agentsview.tool_calls WHERE session_id = $1`,
			sessionID,
		); err != nil {
			return 0, fmt.Errorf("deleting stale pg tool_calls: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM agentsview.messages WHERE session_id = $1`,
			sessionID,
		); err != nil {
			return 0, fmt.Errorf("deleting stale pg messages: %w", err)
		}
		return 0, nil
	}

	var pgCount int
	var pgContentSum, pgContentMax, pgContentMin int64
	var pgToolCallCount int
	var pgTCContentSum int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(content_length), 0), COALESCE(MAX(content_length), 0), COALESCE(MIN(content_length), 0)
		 FROM agentsview.messages
		 WHERE session_id = $1`,
		sessionID,
	).Scan(&pgCount, &pgContentSum, &pgContentMax, &pgContentMin); err != nil {
		return 0, fmt.Errorf("counting pg messages: %w", err)
	}
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(result_content_length), 0) FROM agentsview.tool_calls
		 WHERE session_id = $1`,
		sessionID,
	).Scan(&pgToolCallCount, &pgTCContentSum); err != nil {
		return 0, fmt.Errorf("counting pg tool_calls: %w", err)
	}

	// Heuristic skip: if message count, content fingerprint
	// (sum + max + min of content_length), tool_call count, and
	// tool_call content fingerprint (sum of result_content_length)
	// all match we assume the session is unchanged. Skipped when
	// full=true.
	//
	// Known limitation: this heuristic uses aggregate length
	// statistics rather than content hashes, so it can produce
	// false negatives when message content is rewritten to
	// different text of identical byte length. In practice this
	// is extremely rare since agent sessions are append-only.
	// Use -full to force a complete re-push when needed.
	if !full && pgCount == localCount && pgCount > 0 {
		localSum, localMax, localMin, err := p.local.MessageContentFingerprint(sessionID)
		if err != nil {
			return 0, fmt.Errorf("computing local content fingerprint: %w", err)
		}
		localTCCount, err := p.local.ToolCallCount(sessionID)
		if err != nil {
			return 0, fmt.Errorf("counting local tool_calls: %w", err)
		}
		localTCSum, err := p.local.ToolCallContentFingerprint(sessionID)
		if err != nil {
			return 0, fmt.Errorf("computing local tool_call content fingerprint: %w", err)
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
		DELETE FROM agentsview.tool_calls
		WHERE session_id = $1
	`, sessionID); err != nil {
		return 0, fmt.Errorf("deleting pg tool_calls: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM agentsview.messages
		WHERE session_id = $1
	`, sessionID); err != nil {
		return 0, fmt.Errorf("deleting pg messages: %w", err)
	}

	msgStmt, err := tx.PrepareContext(ctx, `
		INSERT INTO agentsview.messages (
			session_id, ordinal, role, content,
			timestamp, has_thinking, has_tool_use, content_length
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`)
	if err != nil {
		return 0, fmt.Errorf("preparing message insert: %w", err)
	}
	defer msgStmt.Close()

	tcStmt, err := tx.PrepareContext(ctx, `
		INSERT INTO agentsview.tool_calls (
			session_id, tool_name, category,
			call_index, tool_use_id, input_json, skill_name,
			result_content_length, result_content,
			subagent_session_id, message_ordinal
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`)
	if err != nil {
		return 0, fmt.Errorf("preparing tool_call insert: %w", err)
	}
	defer tcStmt.Close()

	count := 0
	startOrdinal := 0
	for {
		msgs, err := p.local.GetMessages(
			ctx, sessionID, startOrdinal, db.MaxMessageLimit, true,
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
				"pushMessages %s: ordinal did not advance (start=%d, last=%d)",
				sessionID, startOrdinal, msgs[len(msgs)-1].Ordinal,
			)
		}

		for _, m := range msgs {
			var ts any
			if m.Timestamp != "" {
				ts = m.Timestamp
			}
			_, err := msgStmt.ExecContext(ctx,
				sessionID, m.Ordinal, m.Role,
				m.Content, ts, m.HasThinking, m.HasToolUse,
				m.ContentLength,
			)
			if err != nil {
				return count, fmt.Errorf(
					"inserting message ordinal %d: %w",
					m.Ordinal, err,
				)
			}
			count++

			for i, tc := range m.ToolCalls {
				_, err := tcStmt.ExecContext(ctx,
					sessionID,
					tc.ToolName, tc.Category,
					i,
					tc.ToolUseID,
					nilIfEmpty(tc.InputJSON),
					nilIfEmpty(tc.SkillName),
					nilIfZero(tc.ResultContentLength),
					nilIfEmpty(tc.ResultContent),
					nilIfEmpty(tc.SubagentSessionID),
					m.Ordinal,
				)
				if err != nil {
					return count, fmt.Errorf(
						"inserting tool_call: %w", err,
					)
				}
			}
		}

		startOrdinal = nextOrdinal
	}

	return count, nil
}

func nilIfEmpty(s string) any {
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
