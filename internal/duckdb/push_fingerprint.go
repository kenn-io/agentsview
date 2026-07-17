package duckdb

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/db"
)

type duckMessagePushKind int

const (
	duckMessagePushReplace duckMessagePushKind = iota
	duckMessagePushSkip
	duckMessagePushAppend
)

type duckMessagePushAction struct {
	kind       duckMessagePushKind
	maxOrdinal int
}

type duckMessageFingerprint struct {
	Count         int
	Sum           int64
	Max           int64
	Min           int64
	MinOrdinal    int
	MaxOrdinal    int
	MessageIDFP   string
	ContentHashFP string
	RoleTimeFP    string
	FlagsFP       string
	SystemFP      string
	TokenFP       string
	ToolCallCount int
	ToolCallSum   int64
	ToolCallFP    string
	ToolResultFP  string
	UsageEventFP  string
}

type duckQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

type syncDuckQueryer struct {
	sync *Sync
}

func (q syncDuckQueryer) QueryContext(
	ctx context.Context, query string, args ...any,
) (*sql.Rows, error) {
	return queryDuckDBContext(
		ctx, q.sync.duck, q.sync.connectionKind, q.sync.quack, query, args...,
	)
}

func (s *Sync) targetQueryer() duckQueryer {
	return syncDuckQueryer{sync: s}
}

func (s *Sync) duckMessagePushAction(
	ctx context.Context, q duckQueryer, sessionID string, msgs []db.Message,
) (duckMessagePushAction, error) {
	localCount := len(msgs)
	targetFP, err := duckMessageFingerprintForSession(ctx, q, sessionID, nil)
	if err != nil {
		return duckMessagePushAction{}, fmt.Errorf(
			"reading duckdb message fingerprint %s: %w", sessionID, err,
		)
	}
	if localCount == 0 {
		return duckMessagePushAction{kind: duckMessagePushReplace}, nil
	}
	if targetFP.Count == localCount && targetFP.Count > 0 {
		localFP, err := duckMessageFingerprintForSession(
			ctx, s.local.Reader(), sessionID, nil,
		)
		if err != nil {
			return duckMessagePushAction{}, fmt.Errorf(
				"reading local message fingerprint %s: %w", sessionID, err,
			)
		}
		if duckMessageFingerprintsMatch(localFP, targetFP, true) {
			return duckMessagePushAction{kind: duckMessagePushSkip}, nil
		}
		return duckMessagePushAction{kind: duckMessagePushReplace}, nil
	}
	if targetFP.Count <= 0 || targetFP.Count >= localCount {
		return duckMessagePushAction{kind: duckMessagePushReplace}, nil
	}

	maxOrdinal := targetFP.MaxOrdinal
	localPrefixFP, err := duckMessageFingerprintForSession(
		ctx, s.local.Reader(), sessionID, &maxOrdinal,
	)
	if err != nil {
		return duckMessagePushAction{}, fmt.Errorf(
			"reading local prefix fingerprint %s: %w", sessionID, err,
		)
	}
	if localPrefixFP.Count != targetFP.Count {
		return duckMessagePushAction{kind: duckMessagePushReplace}, nil
	}
	if duckMessageFingerprintsMatch(localPrefixFP, targetFP, false) {
		return duckMessagePushAction{
			kind:       duckMessagePushAppend,
			maxOrdinal: targetFP.MaxOrdinal,
		}, nil
	}
	return duckMessagePushAction{kind: duckMessagePushReplace}, nil
}

func duckMessageFingerprintsMatch(
	localFP, targetFP duckMessageFingerprint, includeUsage bool,
) bool {
	if localFP.Count != targetFP.Count ||
		localFP.Sum != targetFP.Sum ||
		localFP.Max != targetFP.Max ||
		localFP.Min != targetFP.Min ||
		localFP.MinOrdinal != targetFP.MinOrdinal ||
		localFP.MaxOrdinal != targetFP.MaxOrdinal ||
		localFP.MessageIDFP != targetFP.MessageIDFP ||
		localFP.ContentHashFP != targetFP.ContentHashFP ||
		localFP.RoleTimeFP != targetFP.RoleTimeFP ||
		localFP.FlagsFP != targetFP.FlagsFP ||
		localFP.SystemFP != targetFP.SystemFP ||
		localFP.TokenFP != targetFP.TokenFP ||
		localFP.ToolCallCount != targetFP.ToolCallCount ||
		localFP.ToolCallSum != targetFP.ToolCallSum ||
		localFP.ToolCallFP != targetFP.ToolCallFP ||
		localFP.ToolResultFP != targetFP.ToolResultFP {
		return false
	}
	return !includeUsage || localFP.UsageEventFP == targetFP.UsageEventFP
}

func duckMessageFingerprintForSession(
	ctx context.Context,
	q duckQueryer,
	sessionID string,
	maxOrdinal *int,
) (duckMessageFingerprint, error) {
	fp, err := duckStoredMessageFingerprint(ctx, q, sessionID, maxOrdinal)
	if err != nil {
		return duckMessageFingerprint{}, err
	}
	toolCount, toolSum, toolFP, err := duckStoredToolCallFingerprint(
		ctx, q, sessionID, maxOrdinal,
	)
	if err != nil {
		return duckMessageFingerprint{}, err
	}
	fp.ToolCallCount = toolCount
	fp.ToolCallSum = toolSum
	fp.ToolCallFP = toolFP
	resultFP, err := duckStoredToolResultEventFingerprint(
		ctx, q, sessionID, maxOrdinal,
	)
	if err != nil {
		return duckMessageFingerprint{}, err
	}
	fp.ToolResultFP = resultFP
	if maxOrdinal == nil {
		usageFP, err := duckStoredUsageEventFingerprint(ctx, q, sessionID)
		if err != nil {
			return duckMessageFingerprint{}, err
		}
		fp.UsageEventFP = usageFP
	}
	return fp, nil
}

func duckStoredMessageFingerprint(
	ctx context.Context, q duckQueryer, sessionID string, maxOrdinal *int,
) (duckMessageFingerprint, error) {
	query := `SELECT id, ordinal, role, content, thinking_text, timestamp,
			has_thinking, has_tool_use, content_length, is_system,
			model, token_usage, context_tokens, output_tokens,
			has_context_tokens, has_output_tokens, claude_message_id,
			claude_request_id, source_type, source_subtype, source_uuid,
			source_parent_uuid, is_sidechain, is_compact_boundary
		FROM messages
		WHERE session_id = ?`
	args := []any{sessionID}
	if maxOrdinal != nil {
		query += ` AND ordinal <= ?`
		args = append(args, *maxOrdinal)
	}
	query += ` ORDER BY ordinal ASC`

	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return duckMessageFingerprint{}, err
	}
	defer rows.Close()

	fp := duckMessageFingerprint{MinOrdinal: -1, MaxOrdinal: -1}
	var messageIDs strings.Builder
	var systemOrdinals strings.Builder
	var contentHash strings.Builder
	var roleTime strings.Builder
	var flags strings.Builder
	var token strings.Builder
	for rows.Next() {
		var ordinal, contentLength, contextTokens, outputTokens int
		var id sql.NullInt64
		var role, content, thinkingText, model, tokenUsage string
		var claudeMsgID, claudeReqID string
		var srcType, srcSubtype, srcUUID, srcParentUUID string
		var timestamp any
		var hasThinking, hasToolUse, isSystem bool
		var hasContextTokens, hasOutputTokens bool
		var isSidechain, isCompactBoundary bool
		if err := rows.Scan(
			&id, &ordinal, &role, &content, &thinkingText, &timestamp,
			&hasThinking, &hasToolUse, &contentLength, &isSystem,
			&model, &tokenUsage, &contextTokens, &outputTokens,
			&hasContextTokens, &hasOutputTokens, &claudeMsgID,
			&claudeReqID, &srcType, &srcSubtype, &srcUUID,
			&srcParentUUID, &isSidechain, &isCompactBoundary,
		); err != nil {
			return duckMessageFingerprint{}, err
		}
		if fp.Count == 0 {
			fp.Min = int64(contentLength)
			fp.Max = int64(contentLength)
			fp.MinOrdinal = ordinal
			fp.MaxOrdinal = ordinal
		} else {
			fp.Min = min(fp.Min, int64(contentLength))
			fp.Max = max(fp.Max, int64(contentLength))
			fp.MinOrdinal = min(fp.MinOrdinal, ordinal)
			fp.MaxOrdinal = max(fp.MaxOrdinal, ordinal)
		}
		fp.Count++
		fp.Sum += int64(contentLength)
		fmt.Fprintf(&messageIDs, "%d|%t|%d;", ordinal, id.Valid, id.Int64)
		if isSystem {
			if systemOrdinals.Len() > 0 {
				systemOrdinals.WriteByte(',')
			}
			fmt.Fprintf(&systemOrdinals, "%d", ordinal)
		}

		content = db.SanitizeUTF8(content)
		contentSum := sha256.Sum256([]byte(content))
		fmt.Fprintf(
			&contentHash, "%d|%d|%x;",
			ordinal, contentLength, contentSum,
		)

		role = db.SanitizeUTF8(role)
		timestampText := duckFingerprintTime(timestamp)
		fmt.Fprintf(
			&roleTime, "%d|%d:%s|%d:%s;",
			ordinal, len(role), role, len(timestampText), timestampText,
		)

		thinkingText = db.SanitizeUTF8(thinkingText)
		thinkingSum := sha256.Sum256([]byte(thinkingText))
		fmt.Fprintf(
			&flags, "%d|%t|%t|%t|%x;",
			ordinal, isSystem, hasThinking, hasToolUse, thinkingSum,
		)

		model = db.SanitizeUTF8(model)
		tokenUsage = db.SanitizeUTF8(tokenUsage)
		claudeMsgID = db.SanitizeUTF8(claudeMsgID)
		claudeReqID = db.SanitizeUTF8(claudeReqID)
		srcType = db.SanitizeUTF8(srcType)
		srcSubtype = db.SanitizeUTF8(srcSubtype)
		srcUUID = db.SanitizeUTF8(srcUUID)
		srcParentUUID = db.SanitizeUTF8(srcParentUUID)
		fmt.Fprintf(
			&token,
			"%d|%d:%s|%d:%s|%d|%d|%t|%t|%s|%s|"+
				"%d:%s|%d:%s|%d:%s|%d:%s|%t|%t;",
			ordinal,
			len(model), model,
			len(tokenUsage), tokenUsage,
			contextTokens, outputTokens,
			hasContextTokens, hasOutputTokens,
			claudeMsgID, claudeReqID,
			len(srcType), srcType,
			len(srcSubtype), srcSubtype,
			len(srcUUID), srcUUID,
			len(srcParentUUID), srcParentUUID,
			isSidechain, isCompactBoundary,
		)
	}
	if err := rows.Err(); err != nil {
		return duckMessageFingerprint{}, err
	}
	fp.MessageIDFP = messageIDs.String()
	fp.SystemFP = systemOrdinals.String()
	fp.ContentHashFP = contentHash.String()
	fp.RoleTimeFP = roleTime.String()
	fp.FlagsFP = flags.String()
	fp.TokenFP = token.String()
	return fp, nil
}

func duckStoredToolCallFingerprint(
	ctx context.Context, q duckQueryer, sessionID string, maxOrdinal *int,
) (int, int64, string, error) {
	query := `SELECT m.ordinal, tc.call_index, tc.tool_name, tc.category,
			COALESCE(tc.tool_use_id, ''), COALESCE(tc.input_json, ''),
			COALESCE(tc.skill_name, ''), COALESCE(tc.subagent_session_id, ''),
			COALESCE(tc.result_content_length, 0),
			COALESCE(tc.result_content, ''), COALESCE(tc.file_path, '')
		FROM tool_calls tc
		JOIN messages m ON m.session_id = tc.session_id
			AND m.id = tc.message_id
		WHERE tc.session_id = ?`
	args := []any{sessionID}
	if maxOrdinal != nil {
		query += ` AND m.ordinal <= ?`
		args = append(args, *maxOrdinal)
	}
	query += ` ORDER BY m.ordinal ASC, tc.call_index ASC`

	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return 0, 0, "", err
	}
	defer rows.Close()

	count := 0
	var sum int64
	var b strings.Builder
	for rows.Next() {
		var messageOrdinal, callIndex, resultContentLength int
		var toolName, category, toolUseID, inputJSON string
		var skillName, subagentSessionID, resultContent, filePath string
		if err := rows.Scan(
			&messageOrdinal, &callIndex, &toolName, &category,
			&toolUseID, &inputJSON, &skillName, &subagentSessionID,
			&resultContentLength, &resultContent, &filePath,
		); err != nil {
			return 0, 0, "", err
		}
		count++
		sum += int64(resultContentLength)
		toolName = db.SanitizeUTF8(toolName)
		category = db.SanitizeUTF8(category)
		toolUseID = db.SanitizeUTF8(toolUseID)
		inputJSON = db.SanitizeUTF8(inputJSON)
		skillName = db.SanitizeUTF8(skillName)
		subagentSessionID = db.SanitizeUTF8(subagentSessionID)
		resultContent = db.SanitizeUTF8(resultContent)
		filePath = db.SanitizeUTF8(filePath)
		fmt.Fprintf(
			&b,
			"%d|%d|%d:%s|%d:%s|%d:%s|%d:%s|%d:%s|%d:%s|%d|%d:%s|%d:%s;",
			messageOrdinal, callIndex,
			len(toolName), toolName,
			len(category), category,
			len(toolUseID), toolUseID,
			len(inputJSON), inputJSON,
			len(skillName), skillName,
			len(subagentSessionID), subagentSessionID,
			resultContentLength,
			len(resultContent), resultContent,
			len(filePath), filePath,
		)
	}
	if err := rows.Err(); err != nil {
		return 0, 0, "", err
	}
	return count, sum, b.String(), nil
}

func duckStoredToolResultEventFingerprint(
	ctx context.Context, q duckQueryer, sessionID string, maxOrdinal *int,
) (string, error) {
	query := `SELECT tool_call_message_ordinal, call_index, event_index,
			COALESCE(tool_use_id, ''), COALESCE(agent_id, ''),
			COALESCE(subagent_session_id, ''), source, status,
			content, content_length, timestamp
		FROM tool_result_events
		WHERE session_id = ?`
	args := []any{sessionID}
	if maxOrdinal != nil {
		query += ` AND tool_call_message_ordinal <= ?`
		args = append(args, *maxOrdinal)
	}
	query += ` ORDER BY tool_call_message_ordinal ASC, call_index ASC, event_index ASC`

	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return "", err
	}
	defer rows.Close()

	var b strings.Builder
	for rows.Next() {
		var messageOrdinal, callIndex, eventIndex, contentLength int
		var toolUseID, agentID, subagentSessionID string
		var source, status, content string
		var timestamp any
		if err := rows.Scan(
			&messageOrdinal, &callIndex, &eventIndex,
			&toolUseID, &agentID, &subagentSessionID,
			&source, &status, &content, &contentLength, &timestamp,
		); err != nil {
			return "", err
		}
		toolUseID = db.SanitizeUTF8(toolUseID)
		agentID = db.SanitizeUTF8(agentID)
		subagentSessionID = db.SanitizeUTF8(subagentSessionID)
		source = db.SanitizeUTF8(source)
		status = db.SanitizeUTF8(status)
		content = db.SanitizeUTF8(content)
		contentSum := sha256.Sum256([]byte(content))
		timestampText := duckFingerprintTime(timestamp)
		fmt.Fprintf(
			&b,
			"%d|%d|%d|%d:%s|%d:%s|%d:%s|%d:%s|%d:%s|%d|%x|%d:%s;",
			messageOrdinal, callIndex, eventIndex,
			len(toolUseID), toolUseID,
			len(agentID), agentID,
			len(subagentSessionID), subagentSessionID,
			len(source), source,
			len(status), status,
			contentLength,
			contentSum,
			len(timestampText), timestampText,
		)
	}
	return b.String(), rows.Err()
}

func duckStoredUsageEventFingerprint(
	ctx context.Context, q duckQueryer, sessionID string,
) (string, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT message_ordinal, source, model,
			input_tokens, output_tokens,
			cache_creation_input_tokens, cache_read_input_tokens,
			reasoning_tokens, cost_usd, ai_credits, cost_status, cost_source,
			occurred_at, dedup_key
		FROM usage_events
		WHERE session_id = ?
		ORDER BY COALESCE(CAST(occurred_at AS TEXT), ''), id`,
		sessionID,
	)
	if err != nil {
		return "", err
	}
	defer rows.Close()

	var b strings.Builder
	for rows.Next() {
		var ordinal sql.NullInt64
		var source, model, costStatus, costSource string
		var inputTokens, outputTokens int
		var cacheCreationInputTokens, cacheReadInputTokens int
		var reasoningTokens int
		var cost, aiCredits sql.NullFloat64
		var occurredAt any
		var dedupKey sql.NullString
		if err := rows.Scan(
			&ordinal, &source, &model,
			&inputTokens, &outputTokens,
			&cacheCreationInputTokens, &cacheReadInputTokens,
			&reasoningTokens, &cost, &aiCredits, &costStatus, &costSource,
			&occurredAt, &dedupKey,
		); err != nil {
			return "", err
		}
		occurred := duckFingerprintTime(occurredAt)
		source = db.SanitizeUTF8(source)
		model = db.SanitizeUTF8(model)
		costStatus = db.SanitizeUTF8(costStatus)
		costSource = db.SanitizeUTF8(costSource)
		dedupKey.String = db.SanitizeUTF8(dedupKey.String)
		fmt.Fprintf(
			&b,
			"%t|%d|%d:%s|%d:%s|%d|%d|%d|%d|%d|%t|%g|%t|%g|%d:%s|%d:%s|%d:%s|%d:%s;",
			ordinal.Valid,
			ordinal.Int64,
			len(source), source,
			len(model), model,
			inputTokens,
			outputTokens,
			cacheCreationInputTokens,
			cacheReadInputTokens,
			reasoningTokens,
			cost.Valid,
			cost.Float64,
			aiCredits.Valid,
			aiCredits.Float64,
			len(costStatus), costStatus,
			len(costSource), costSource,
			len(occurred), occurred,
			len(dedupKey.String), dedupKey.String,
		)
	}
	return b.String(), rows.Err()
}

func duckFingerprintTime(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case time.Time:
		return v.UTC().Format(time.RFC3339Nano)
	case string:
		if t, ok := parseTimestamp(v); ok {
			return t.UTC().Format(time.RFC3339Nano)
		}
		return v
	case []byte:
		return duckFingerprintTime(string(v))
	default:
		return fmt.Sprint(v)
	}
}

func messagesAfterDuckOrdinal(msgs []db.Message, maxOrdinal int) []db.Message {
	for i, msg := range msgs {
		if msg.Ordinal > maxOrdinal {
			return msgs[i:]
		}
	}
	return nil
}
