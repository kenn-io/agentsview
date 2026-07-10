package db

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
)

// This file holds batched (session_id IN ...) twins of the per-session
// fingerprint queries in messages.go, secret_findings.go, and pins.go.
// Push backends fingerprint every candidate session on every push; issuing
// the dozen per-session queries once per session dominated full-push
// preparation time, so the push loop prefetches per-chunk maps instead.
//
// Byte-identity contract: for any session ID, each batched method's map
// value must equal the corresponding per-session method's return value
// (including nil-vs-empty slice semantics), because push fingerprints are
// hashed and compared against fingerprints stored by earlier pushes. The
// per-row formatting is shared with the per-session methods; the parity
// tests in fingerprints_batch_test.go pin the query side.

// sessionFingerprintBatchSize bounds how many session IDs a single batched
// fingerprint query binds, staying under SQLite's default 999-variable limit.
const sessionFingerprintBatchSize = 900

func forEachSessionIDBatch(
	sessionIDs []string, fn func(chunk []string) error,
) error {
	for start := 0; start < len(sessionIDs); start += sessionFingerprintBatchSize {
		end := min(start+sessionFingerprintBatchSize, len(sessionIDs))
		if err := fn(sessionIDs[start:end]); err != nil {
			return err
		}
	}
	return nil
}

func sessionIDArgs(sessionIDs []string) (string, []any) {
	placeholders := make([]string, len(sessionIDs))
	args := make([]any, len(sessionIDs))
	for i, id := range sessionIDs {
		placeholders[i] = "?"
		args[i] = id
	}
	return strings.Join(placeholders, ","), args
}

type fingerprintBuilders map[string]*strings.Builder

func (m fingerprintBuilders) get(sessionID string) *strings.Builder {
	b := m[sessionID]
	if b == nil {
		b = &strings.Builder{}
		m[sessionID] = b
	}
	return b
}

func (m fingerprintBuilders) mergeInto(out map[string]string) {
	for id, b := range m {
		out[id] += b.String()
	}
}

// MessageContentAggregate mirrors MessageContentFingerprint's
// sum/max/min triple for the batched lookup.
type MessageContentAggregate struct {
	Sum int64
	Max int64
	Min int64
}

// MessageContentFingerprints is the batched twin of
// MessageContentFingerprint. Sessions without messages are absent from the
// map; the zero MessageContentAggregate matches the per-session result.
func (db *DB) MessageContentFingerprints(
	sessionIDs []string,
) (map[string]MessageContentAggregate, error) {
	out := make(map[string]MessageContentAggregate, len(sessionIDs))
	err := forEachSessionIDBatch(sessionIDs, func(chunk []string) error {
		ph, args := sessionIDArgs(chunk)
		rows, err := db.getReader().Query(`
			SELECT session_id, COALESCE(SUM(content_length), 0),
				COALESCE(MAX(content_length), 0),
				COALESCE(MIN(content_length), 0)
			FROM messages
			WHERE session_id IN (`+ph+`)
			GROUP BY session_id`,
			args...,
		)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var sessionID string
			var agg MessageContentAggregate
			if err := rows.Scan(
				&sessionID, &agg.Sum, &agg.Max, &agg.Min,
			); err != nil {
				return err
			}
			out[sessionID] = agg
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// MessageTokenFingerprints is the batched twin of MessageTokenFingerprint.
func (db *DB) MessageTokenFingerprints(
	sessionIDs []string,
) (map[string]string, error) {
	out := make(map[string]string, len(sessionIDs))
	err := forEachSessionIDBatch(sessionIDs, func(chunk []string) error {
		ph, args := sessionIDArgs(chunk)
		rows, err := db.getReader().Query(`
			SELECT session_id, ordinal, model, token_usage, context_tokens,
				output_tokens, has_context_tokens, has_output_tokens,
				claude_message_id, claude_request_id,
				source_type, source_subtype, source_uuid,
				source_parent_uuid, is_sidechain, is_compact_boundary
			 FROM messages
			 WHERE session_id IN (`+ph+`)
			 ORDER BY session_id, ordinal ASC`,
			args...,
		)
		if err != nil {
			return err
		}
		defer rows.Close()
		builders := fingerprintBuilders{}
		for rows.Next() {
			var sessionID string
			var r tokenFingerprintRow
			if err := rows.Scan(
				&sessionID, &r.ordinal, &r.model, &r.tokenUsage,
				&r.contextTokens, &r.outputTokens,
				&r.hasContextTokens, &r.hasOutputTokens,
				&r.claudeMessageID, &r.claudeRequestID,
				&r.sourceType, &r.sourceSubtype, &r.sourceUUID,
				&r.sourceParentUUID, &r.isSidechain, &r.isCompactBoundary,
			); err != nil {
				return err
			}
			r.appendTo(builders.get(sessionID))
		}
		if err := rows.Err(); err != nil {
			return err
		}
		builders.mergeInto(out)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// MessageContentHashFingerprints is the batched twin of
// MessageContentHashFingerprint.
func (db *DB) MessageContentHashFingerprints(
	sessionIDs []string,
) (map[string]string, error) {
	out := make(map[string]string, len(sessionIDs))
	err := forEachSessionIDBatch(sessionIDs, func(chunk []string) error {
		ph, args := sessionIDArgs(chunk)
		rows, err := db.getReader().Query(`
			SELECT session_id, ordinal, content, content_length
			 FROM messages
			 WHERE session_id IN (`+ph+`)
			 ORDER BY session_id, ordinal ASC`,
			args...,
		)
		if err != nil {
			return err
		}
		defer rows.Close()
		builders := fingerprintBuilders{}
		for rows.Next() {
			var sessionID, content string
			var ordinal, contentLength int
			if err := rows.Scan(
				&sessionID, &ordinal, &content, &contentLength,
			); err != nil {
				return err
			}
			appendContentHashFingerprintRow(
				builders.get(sessionID), ordinal, contentLength, content,
			)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		builders.mergeInto(out)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// MessageRoleTimeFingerprintsWithTimestampNormalizer is the batched twin of
// MessageRoleTimeFingerprintWithTimestampNormalizer.
func (db *DB) MessageRoleTimeFingerprintsWithTimestampNormalizer(
	sessionIDs []string,
	normalizeTimestamp func(string) string,
) (map[string]string, error) {
	out := make(map[string]string, len(sessionIDs))
	err := forEachSessionIDBatch(sessionIDs, func(chunk []string) error {
		ph, args := sessionIDArgs(chunk)
		rows, err := db.getReader().Query(`
			SELECT session_id, ordinal, role, COALESCE(timestamp, '')
			 FROM messages
			 WHERE session_id IN (`+ph+`)
			 ORDER BY session_id, ordinal ASC`,
			args...,
		)
		if err != nil {
			return err
		}
		defer rows.Close()
		builders := fingerprintBuilders{}
		for rows.Next() {
			var sessionID, role, timestamp string
			var ordinal int
			if err := rows.Scan(
				&sessionID, &ordinal, &role, &timestamp,
			); err != nil {
				return err
			}
			appendRoleTimeFingerprintRow(
				builders.get(sessionID), ordinal, role, timestamp,
				normalizeTimestamp,
			)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		builders.mergeInto(out)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// MessageFlagsFingerprints is the batched twin of MessageFlagsFingerprint.
func (db *DB) MessageFlagsFingerprints(
	sessionIDs []string,
) (map[string]string, error) {
	out := make(map[string]string, len(sessionIDs))
	err := forEachSessionIDBatch(sessionIDs, func(chunk []string) error {
		ph, args := sessionIDArgs(chunk)
		rows, err := db.getReader().Query(`
			SELECT session_id, ordinal, is_system, has_thinking,
				has_tool_use, thinking_text
			 FROM messages
			 WHERE session_id IN (`+ph+`)
			 ORDER BY session_id, ordinal ASC`,
			args...,
		)
		if err != nil {
			return err
		}
		defer rows.Close()
		builders := fingerprintBuilders{}
		for rows.Next() {
			var sessionID string
			var r flagsFingerprintRow
			if err := rows.Scan(
				&sessionID, &r.ordinal, &r.isSystem, &r.hasThinking,
				&r.hasToolUse, &r.thinkingText,
			); err != nil {
				return err
			}
			r.appendTo(builders.get(sessionID))
		}
		if err := rows.Err(); err != nil {
			return err
		}
		builders.mergeInto(out)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// SystemMessageFingerprints is the batched twin of SystemMessageFingerprint.
// The ordinal list is joined in Go rather than with GROUP_CONCAT so the
// per-session ordering never depends on SQLite's aggregate scan order.
func (db *DB) SystemMessageFingerprints(
	sessionIDs []string,
) (map[string]string, error) {
	out := make(map[string]string, len(sessionIDs))
	err := forEachSessionIDBatch(sessionIDs, func(chunk []string) error {
		ph, args := sessionIDArgs(chunk)
		rows, err := db.getReader().Query(`
			SELECT session_id, ordinal FROM messages
			WHERE session_id IN (`+ph+`) AND is_system = 1
			ORDER BY session_id, ordinal`,
			args...,
		)
		if err != nil {
			return err
		}
		defer rows.Close()
		builders := fingerprintBuilders{}
		for rows.Next() {
			var sessionID string
			var ordinal int
			if err := rows.Scan(&sessionID, &ordinal); err != nil {
				return err
			}
			b := builders.get(sessionID)
			if b.Len() > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(b, "%d", ordinal)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		builders.mergeInto(out)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ToolCallCounts is the batched twin of ToolCallCount. Sessions without
// tool calls are absent from the map; zero matches the per-session result.
func (db *DB) ToolCallCounts(sessionIDs []string) (map[string]int, error) {
	out := make(map[string]int, len(sessionIDs))
	err := forEachSessionIDBatch(sessionIDs, func(chunk []string) error {
		ph, args := sessionIDArgs(chunk)
		rows, err := db.getReader().Query(`
			SELECT session_id, COUNT(*) FROM tool_calls
			WHERE session_id IN (`+ph+`)
			GROUP BY session_id`,
			args...,
		)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var sessionID string
			var n int
			if err := rows.Scan(&sessionID, &n); err != nil {
				return err
			}
			out[sessionID] = n
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ToolCallContentFingerprints is the batched twin of
// ToolCallContentFingerprint.
func (db *DB) ToolCallContentFingerprints(
	sessionIDs []string,
) (map[string]int64, error) {
	out := make(map[string]int64, len(sessionIDs))
	err := forEachSessionIDBatch(sessionIDs, func(chunk []string) error {
		ph, args := sessionIDArgs(chunk)
		rows, err := db.getReader().Query(`
			SELECT session_id, COALESCE(SUM(result_content_length), 0)
			FROM tool_calls
			WHERE session_id IN (`+ph+`)
			GROUP BY session_id`,
			args...,
		)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var sessionID string
			var sum int64
			if err := rows.Scan(&sessionID, &sum); err != nil {
				return err
			}
			out[sessionID] = sum
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ToolCallFingerprints is the batched twin of ToolCallFingerprint.
func (db *DB) ToolCallFingerprints(
	sessionIDs []string,
) (map[string]string, error) {
	out := make(map[string]string, len(sessionIDs))
	err := forEachSessionIDBatch(sessionIDs, func(chunk []string) error {
		ph, args := sessionIDArgs(chunk)
		rows, err := db.getReader().Query(`
			SELECT tc.session_id, m.ordinal, tc.tool_name, tc.category,
				COALESCE(tc.tool_use_id, ''), COALESCE(tc.input_json, ''),
				COALESCE(tc.skill_name, ''),
				COALESCE(tc.subagent_session_id, ''),
				COALESCE(tc.result_content_length, 0),
				COALESCE(tc.result_content, ''),
				COALESCE(tc.file_path, '')
			 FROM tool_calls tc
			 JOIN messages m ON m.id = tc.message_id
			 WHERE tc.session_id IN (`+ph+`)
			 ORDER BY tc.session_id, m.ordinal ASC, tc.id ASC`,
			args...,
		)
		if err != nil {
			return err
		}
		defer rows.Close()
		builders := fingerprintBuilders{}
		var indexer toolCallIndexer
		for rows.Next() {
			var sessionID string
			var r toolCallFingerprintRow
			if err := rows.Scan(
				&sessionID, &r.messageOrdinal, &r.toolName, &r.category,
				&r.toolUseID, &r.inputJSON, &r.skillName,
				&r.subagentSessionID, &r.resultContentLength,
				&r.resultContent, &r.filePath,
			); err != nil {
				return err
			}
			r.callIndex = indexer.next(sessionID, r.messageOrdinal)
			r.appendTo(builders.get(sessionID))
		}
		if err := rows.Err(); err != nil {
			return err
		}
		builders.mergeInto(out)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ToolResultEventFingerprintsWithTimestampNormalizer is the batched twin of
// ToolResultEventFingerprintWithTimestampNormalizer.
func (db *DB) ToolResultEventFingerprintsWithTimestampNormalizer(
	sessionIDs []string,
	normalizeTimestamp func(string) string,
) (map[string]string, error) {
	out := make(map[string]string, len(sessionIDs))
	err := forEachSessionIDBatch(sessionIDs, func(chunk []string) error {
		ph, args := sessionIDArgs(chunk)
		rows, err := db.getReader().Query(`
			SELECT session_id, tool_call_message_ordinal, call_index,
				event_index, COALESCE(tool_use_id, ''),
				COALESCE(agent_id, ''),
				COALESCE(subagent_session_id, ''), source, status,
				content, content_length, COALESCE(timestamp, '')
			 FROM tool_result_events
			 WHERE session_id IN (`+ph+`)
			 ORDER BY session_id, tool_call_message_ordinal ASC,
				call_index ASC, event_index ASC`,
			args...,
		)
		if err != nil {
			return err
		}
		defer rows.Close()
		builders := fingerprintBuilders{}
		for rows.Next() {
			var sessionID string
			var r toolResultEventFingerprintRow
			if err := rows.Scan(
				&sessionID, &r.messageOrdinal, &r.callIndex, &r.eventIndex,
				&r.toolUseID, &r.agentID, &r.subagentSessionID,
				&r.source, &r.status, &r.content, &r.contentLength,
				&r.timestamp,
			); err != nil {
				return err
			}
			r.appendTo(builders.get(sessionID), normalizeTimestamp)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		builders.mergeInto(out)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// SessionSecretFindingsBySession is the batched twin of
// SessionSecretFindings. Every requested session ID gets an entry; sessions
// without findings map to an empty non-nil slice, matching the per-session
// method (the push fingerprint JSON-encodes the slice, so nil and empty
// must not be conflated).
func (db *DB) SessionSecretFindingsBySession(
	ctx context.Context, sessionIDs []string,
) (map[string][]SecretFinding, error) {
	out := make(map[string][]SecretFinding, len(sessionIDs))
	for _, id := range sessionIDs {
		out[id] = []SecretFinding{}
	}
	err := forEachSessionIDBatch(sessionIDs, func(chunk []string) error {
		ph, args := sessionIDArgs(chunk)
		rows, err := db.getReader().QueryContext(ctx, `
			SELECT session_id, rule_name, confidence,
			       location_kind, message_ordinal, call_index, event_index,
			       match_start, match_end, match_index,
			       redacted_match, rules_version
			FROM secret_findings
			WHERE session_id IN (`+ph+`)
			ORDER BY session_id, message_ordinal, match_start, match_index`,
			args...,
		)
		if err != nil {
			return fmt.Errorf("querying secret findings batch: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var f SecretFinding
			if err := rows.Scan(
				&f.SessionID, &f.RuleName, &f.Confidence,
				&f.LocationKind, &f.MessageOrdinal, &f.CallIndex,
				&f.EventIndex, &f.MatchStart, &f.MatchEnd, &f.MatchIndex,
				&f.RedactedMatch, &f.RulesVersion,
			); err != nil {
				return fmt.Errorf("scanning secret finding: %w", err)
			}
			out[f.SessionID] = append(out[f.SessionID], f)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// PinnedMessagesBySession is the batched twin of the per-session
// ListPinnedMessages call. Sessions without pins are absent from the map;
// the nil slice matches the per-session method (the push fingerprint
// JSON-encodes the slice, so nil and empty must not be conflated).
func (db *DB) PinnedMessagesBySession(
	ctx context.Context, sessionIDs []string,
) (map[string][]PinnedMessage, error) {
	out := make(map[string][]PinnedMessage, len(sessionIDs))
	err := forEachSessionIDBatch(sessionIDs, func(chunk []string) error {
		ph, args := sessionIDArgs(chunk)
		rows, err := db.getReader().QueryContext(ctx,
			"SELECT "+pinnedBaseCols+
				" FROM pinned_messages WHERE session_id IN ("+ph+")"+
				" ORDER BY session_id, created_at DESC",
			args...,
		)
		if err != nil {
			return fmt.Errorf("listing pinned messages batch: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			p, err := scanPinnedRow(rows)
			if err != nil {
				return fmt.Errorf("scanning pinned message: %w", err)
			}
			out[p.SessionID] = append(out[p.SessionID], p)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Row formatters shared by the per-session fingerprint methods and their
// batched twins above. Keeping the sanitize-and-format step in one place is
// what guarantees the two paths cannot drift.

type tokenFingerprintRow struct {
	ordinal           int
	model             string
	tokenUsage        string
	contextTokens     int
	outputTokens      int
	hasContextTokens  bool
	hasOutputTokens   bool
	claudeMessageID   string
	claudeRequestID   string
	sourceType        string
	sourceSubtype     string
	sourceUUID        string
	sourceParentUUID  string
	isSidechain       bool
	isCompactBoundary bool
}

// appendTo sanitizes before measuring: the PG-readback fingerprint sees
// values sanitized at insert time, so raw values (e.g. NUL bytes from a
// corrupt parse) would never match and the fast path would rewrite the
// session on every push.
func (r tokenFingerprintRow) appendTo(b *strings.Builder) {
	model := SanitizeUTF8(r.model)
	tokenUsage := SanitizeUTF8(r.tokenUsage)
	claudeMsgID := SanitizeUTF8(r.claudeMessageID)
	claudeReqID := SanitizeUTF8(r.claudeRequestID)
	srcType := SanitizeUTF8(r.sourceType)
	srcSubtype := SanitizeUTF8(r.sourceSubtype)
	srcUUID := SanitizeUTF8(r.sourceUUID)
	srcParentUUID := SanitizeUTF8(r.sourceParentUUID)
	fmt.Fprintf(b,
		"%d|%d:%s|%d:%s|%d|%d|%t|%t|%s|%s|"+
			"%d:%s|%d:%s|%d:%s|%d:%s|%t|%t;",
		r.ordinal,
		len(model), model,
		len(tokenUsage), tokenUsage,
		r.contextTokens, r.outputTokens,
		r.hasContextTokens, r.hasOutputTokens,
		claudeMsgID, claudeReqID,
		len(srcType), srcType,
		len(srcSubtype), srcSubtype,
		len(srcUUID), srcUUID,
		len(srcParentUUID), srcParentUUID,
		r.isSidechain, r.isCompactBoundary,
	)
}

func appendContentHashFingerprintRow(
	b *strings.Builder, ordinal, contentLength int, content string,
) {
	sum := sha256.Sum256([]byte(SanitizeUTF8(content)))
	fmt.Fprintf(b, "%d|%d|%x;", ordinal, contentLength, sum)
}

func appendRoleTimeFingerprintRow(
	b *strings.Builder, ordinal int, role, timestamp string,
	normalizeTimestamp func(string) string,
) {
	role = SanitizeUTF8(role)
	if normalizeTimestamp != nil {
		timestamp = normalizeTimestamp(timestamp)
	}
	fmt.Fprintf(b, "%d|%d:%s|%d:%s;",
		ordinal, len(role), role, len(timestamp), timestamp)
}

type flagsFingerprintRow struct {
	ordinal      int
	isSystem     bool
	hasThinking  bool
	hasToolUse   bool
	thinkingText string
}

func (r flagsFingerprintRow) appendTo(b *strings.Builder) {
	sum := sha256.Sum256([]byte(SanitizeUTF8(r.thinkingText)))
	fmt.Fprintf(b, "%d|%t|%t|%t|%x;",
		r.ordinal, r.isSystem, r.hasThinking, r.hasToolUse, sum)
}

type toolCallFingerprintRow struct {
	messageOrdinal      int
	callIndex           int
	toolName            string
	category            string
	toolUseID           string
	inputJSON           string
	skillName           string
	subagentSessionID   string
	resultContentLength int
	resultContent       string
	filePath            string
}

func (r toolCallFingerprintRow) appendTo(b *strings.Builder) {
	toolName := SanitizeUTF8(r.toolName)
	category := SanitizeUTF8(r.category)
	toolUseID := SanitizeUTF8(r.toolUseID)
	inputJSON := SanitizeUTF8(r.inputJSON)
	skillName := SanitizeUTF8(r.skillName)
	subagentSessionID := SanitizeUTF8(r.subagentSessionID)
	resultContent := SanitizeUTF8(r.resultContent)
	filePath := SanitizeUTF8(r.filePath)
	fmt.Fprintf(b,
		"%d|%d|%d:%s|%d:%s|%d:%s|%d:%s|%d:%s|%d:%s|%d|%d:%s|%d:%s;",
		r.messageOrdinal, r.callIndex,
		len(toolName), toolName,
		len(category), category,
		len(toolUseID), toolUseID,
		len(inputJSON), inputJSON,
		len(skillName), skillName,
		len(subagentSessionID), subagentSessionID,
		r.resultContentLength,
		len(resultContent), resultContent,
		len(filePath), filePath,
	)
}

// toolCallIndexer derives the per-message call index from rows ordered by
// (session_id,) message ordinal, insertion id — the same derivation the
// per-session ToolCallFingerprint used inline.
type toolCallIndexer struct {
	lastSessionID      string
	lastMessageOrdinal int
	callIndex          int
	started            bool
}

func (ix *toolCallIndexer) next(sessionID string, messageOrdinal int) int {
	if ix.started &&
		sessionID == ix.lastSessionID &&
		messageOrdinal == ix.lastMessageOrdinal {
		ix.callIndex++
		return ix.callIndex
	}
	ix.started = true
	ix.lastSessionID = sessionID
	ix.lastMessageOrdinal = messageOrdinal
	ix.callIndex = 0
	return 0
}

type toolResultEventFingerprintRow struct {
	messageOrdinal    int
	callIndex         int
	eventIndex        int
	toolUseID         string
	agentID           string
	subagentSessionID string
	source            string
	status            string
	content           string
	contentLength     int
	timestamp         string
}

func (r toolResultEventFingerprintRow) appendTo(
	b *strings.Builder, normalizeTimestamp func(string) string,
) {
	timestamp := r.timestamp
	if normalizeTimestamp != nil {
		timestamp = normalizeTimestamp(timestamp)
	}
	toolUseID := SanitizeUTF8(r.toolUseID)
	agentID := SanitizeUTF8(r.agentID)
	subagentSessionID := SanitizeUTF8(r.subagentSessionID)
	source := SanitizeUTF8(r.source)
	status := SanitizeUTF8(r.status)
	content := SanitizeUTF8(r.content)
	contentSum := sha256.Sum256([]byte(content))
	fmt.Fprintf(b,
		"%d|%d|%d|%d:%s|%d:%s|%d:%s|%d:%s|%d:%s|%d|%x|%d:%s;",
		r.messageOrdinal, r.callIndex, r.eventIndex,
		len(toolUseID), toolUseID,
		len(agentID), agentID,
		len(subagentSessionID), subagentSessionID,
		len(source), source,
		len(status), status,
		r.contentLength,
		contentSum,
		len(timestamp), timestamp,
	)
}
