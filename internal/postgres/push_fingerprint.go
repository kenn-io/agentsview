package postgres

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"strings"

	"go.kenn.io/agentsview/internal/db"
)

const pushComparisonBatchSize = 900

type pushMessageAggregate struct {
	Count int
	Sum   int64
	Max   int64
	Min   int64
	SysFP string
}

type pushToolCallAggregate struct {
	Count int
	Sum   int64
}

type pushMessageComparison struct {
	MessageAggregates       map[string]pushMessageAggregate
	MessageContentHash      map[string]string
	MessageRoleTime         map[string]string
	MessageFlags            map[string]string
	MessageSystemOrdinals   map[string]string
	MessageTokenFingerprint map[string]string
	ToolCallAggregates      map[string]pushToolCallAggregate
	ToolCallFingerprint     map[string]string
	UsageEventFingerprint   map[string]string
}

type pushLocalMessageFingerprint struct {
	Sum           int64
	Max           int64
	Min           int64
	ContentHashFP string
	RoleTimeFP    string
	FlagsFP       string
	SystemFP      string
	ToolCallCount int
	ToolCallSum   int64
	ToolCallFP    string
	TokenFP       string
	UsageEventFP  string
}

func comparisonAggregates(
	sessionID string,
	comparisons *pushMessageComparison,
) (pushMessageAggregate, pushToolCallAggregate, bool) {
	if comparisons == nil {
		return pushMessageAggregate{}, pushToolCallAggregate{}, false
	}
	return comparisons.MessageAggregates[sessionID],
		comparisons.ToolCallAggregates[sessionID],
		true
}

func readPushSessionMessageComparisons(
	ctx context.Context,
	tx *sql.Tx,
	sessionIDs []string,
) (*pushMessageComparison, error) {
	comparisons := &pushMessageComparison{
		MessageAggregates:       make(map[string]pushMessageAggregate, len(sessionIDs)),
		MessageContentHash:      make(map[string]string, len(sessionIDs)),
		MessageRoleTime:         make(map[string]string, len(sessionIDs)),
		MessageFlags:            make(map[string]string, len(sessionIDs)),
		MessageSystemOrdinals:   make(map[string]string, len(sessionIDs)),
		MessageTokenFingerprint: make(map[string]string, len(sessionIDs)),
		ToolCallAggregates:      make(map[string]pushToolCallAggregate, len(sessionIDs)),
		ToolCallFingerprint:     make(map[string]string, len(sessionIDs)),
		UsageEventFingerprint:   make(map[string]string, len(sessionIDs)),
	}

	for i := 0; i < len(sessionIDs); i += pushComparisonBatchSize {
		end := min(i+pushComparisonBatchSize, len(sessionIDs))
		chunk := sessionIDs[i:end]

		if err := loadPushMessageAggregates(ctx, tx, chunk, comparisons.MessageAggregates); err != nil {
			return nil, err
		}
		if err := loadPushMessageContentHashFingerprints(
			ctx, tx, chunk, comparisons.MessageContentHash,
		); err != nil {
			return nil, err
		}
		if err := loadPushMessageRoleTimeFingerprints(
			ctx, tx, chunk, comparisons.MessageRoleTime,
		); err != nil {
			return nil, err
		}
		if err := loadPushMessageFlagFingerprints(
			ctx, tx, chunk, comparisons.MessageFlags,
		); err != nil {
			return nil, err
		}
		if err := loadPushMessageSystemOrdinals(
			ctx, tx, chunk, comparisons.MessageSystemOrdinals,
		); err != nil {
			return nil, err
		}
		if err := loadPushMessageTokenFingerprints(
			ctx, tx, chunk, comparisons.MessageTokenFingerprint,
		); err != nil {
			return nil, err
		}
		if err := loadPushToolCallAggregates(
			ctx, tx, chunk, comparisons.ToolCallAggregates,
		); err != nil {
			return nil, err
		}
		if err := loadPushToolCallFingerprints(
			ctx, tx, chunk, comparisons.ToolCallFingerprint,
		); err != nil {
			return nil, err
		}
		if err := loadPushUsageEventFingerprints(
			ctx, tx, chunk, comparisons.UsageEventFingerprint,
		); err != nil {
			return nil, err
		}
	}

	return comparisons, nil
}

func loadPushMessageAggregates(
	ctx context.Context,
	tx *sql.Tx,
	sessionIDs []string,
	out map[string]pushMessageAggregate,
) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT session_id, COUNT(*), COALESCE(SUM(content_length), 0),
			COALESCE(MAX(content_length), 0), COALESCE(MIN(content_length), 0),
			COALESCE(STRING_AGG(ordinal::text, ',' ORDER BY ordinal)
				FILTER (WHERE is_system), '')
		FROM messages
		WHERE session_id = ANY($1)
		GROUP BY session_id
	`, sessionIDs)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var sessionID string
		var count int64
		var agg pushMessageAggregate
		if err := rows.Scan(
			&sessionID, &count, &agg.Sum, &agg.Max, &agg.Min, &agg.SysFP,
		); err != nil {
			return err
		}
		agg.Count = int(count)
		out[sessionID] = agg
	}
	return rows.Err()
}

func loadPushMessageContentHashFingerprints(
	ctx context.Context,
	tx *sql.Tx,
	sessionIDs []string,
	out map[string]string,
) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT session_id, ordinal, COALESCE(content, ''),
			content_length
		FROM messages
		WHERE session_id = ANY($1)
		ORDER BY session_id, ordinal ASC
	`, sessionIDs)
	if err != nil {
		return err
	}
	defer rows.Close()

	builders := make(map[string]*strings.Builder, len(sessionIDs))
	for rows.Next() {
		var sessionID string
		var ordinal, contentLength int
		var content string
		if err := rows.Scan(&sessionID, &ordinal, &content, &contentLength); err != nil {
			return err
		}
		sum := sha256.Sum256([]byte(db.SanitizeUTF8(content)))
		b := builders[sessionID]
		if b == nil {
			b = &strings.Builder{}
			builders[sessionID] = b
		}
		fmt.Fprintf(b, "%d|%d|%x;", ordinal, contentLength, sum)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for sessionID, b := range builders {
		out[sessionID] = b.String()
	}
	return nil
}

func loadPushMessageRoleTimeFingerprints(
	ctx context.Context,
	tx *sql.Tx,
	sessionIDs []string,
	out map[string]string,
) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT session_id, ordinal, role, timestamp
		 FROM messages
		WHERE session_id = ANY($1)
		ORDER BY session_id, ordinal ASC
	`, sessionIDs)
	if err != nil {
		return err
	}
	defer rows.Close()

	builders := make(map[string]*strings.Builder, len(sessionIDs))
	for rows.Next() {
		var sessionID string
		var ordinal int
		var role string
		var timestamp sql.NullTime
		if err := rows.Scan(&sessionID, &ordinal, &role, &timestamp); err != nil {
			return err
		}
		timestampText := ""
		if timestamp.Valid {
			timestampText = pgPushTimestampFingerprintText(
				FormatISO8601(timestamp.Time),
			)
		}
		b := builders[sessionID]
		if b == nil {
			b = &strings.Builder{}
			builders[sessionID] = b
		}
		fmt.Fprintf(
			b, "%d|%d:%s|%d:%s;",
			ordinal, len(role), role, len(timestampText), timestampText,
		)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for sessionID, b := range builders {
		out[sessionID] = b.String()
	}
	return nil
}

func loadPushMessageFlagFingerprints(
	ctx context.Context,
	tx *sql.Tx,
	sessionIDs []string,
	out map[string]string,
) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT session_id, ordinal, is_system, has_thinking, has_tool_use,
			COALESCE(thinking_text, '')
		 FROM messages
		WHERE session_id = ANY($1)
		ORDER BY session_id, ordinal ASC
	`, sessionIDs)
	if err != nil {
		return err
	}
	defer rows.Close()

	builders := make(map[string]*strings.Builder, len(sessionIDs))
	for rows.Next() {
		var sessionID string
		var ordinal int
		var isSystem, hasThinking, hasToolUse bool
		var thinkingText string
		if err := rows.Scan(
			&sessionID, &ordinal, &isSystem, &hasThinking, &hasToolUse,
			&thinkingText,
		); err != nil {
			return err
		}
		sum := sha256.Sum256([]byte(db.SanitizeUTF8(thinkingText)))
		b := builders[sessionID]
		if b == nil {
			b = &strings.Builder{}
			builders[sessionID] = b
		}
		fmt.Fprintf(
			b, "%d|%t|%t|%t|%x;", ordinal, isSystem, hasThinking,
			hasToolUse, sum,
		)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for sessionID, b := range builders {
		out[sessionID] = b.String()
	}
	return nil
}

func loadPushMessageSystemOrdinals(
	ctx context.Context,
	tx *sql.Tx,
	sessionIDs []string,
	out map[string]string,
) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT session_id,
			COALESCE(
				STRING_AGG(ordinal::text, ',' ORDER BY ordinal)
					FILTER (WHERE is_system),
				''
			)
		FROM messages
		WHERE session_id = ANY($1)
		GROUP BY session_id
	`, sessionIDs)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var sessionID string
		var systemOrdinals string
		if err := rows.Scan(&sessionID, &systemOrdinals); err != nil {
			return err
		}
		out[sessionID] = systemOrdinals
	}
	return rows.Err()
}

func loadPushMessageTokenFingerprints(
	ctx context.Context,
	tx *sql.Tx,
	sessionIDs []string,
	out map[string]string,
) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT session_id, ordinal, model, token_usage, context_tokens,
			output_tokens, has_context_tokens, has_output_tokens,
			claude_message_id, claude_request_id,
			source_type, source_subtype, source_uuid,
			source_parent_uuid, is_sidechain, is_compact_boundary
		 FROM messages
		WHERE session_id = ANY($1)
		ORDER BY session_id, ordinal ASC
	`, sessionIDs)
	if err != nil {
		return err
	}
	defer rows.Close()

	builders := make(map[string]*strings.Builder, len(sessionIDs))
	for rows.Next() {
		var sessionID string
		var ordinal, contextTokens, outputTokens int
		var model, tokenUsage string
		var hasContextTokens, hasOutputTokens bool
		var claudeMsgID, claudeReqID string
		var srcType, srcSubtype, srcUUID, srcParentUUID string
		var isSidechain, isCompactBoundary bool
		if err := rows.Scan(
			&sessionID, &ordinal, &model, &tokenUsage, &contextTokens,
			&outputTokens, &hasContextTokens, &hasOutputTokens,
			&claudeMsgID, &claudeReqID,
			&srcType, &srcSubtype, &srcUUID, &srcParentUUID,
			&isSidechain, &isCompactBoundary,
		); err != nil {
			return err
		}
		b := builders[sessionID]
		if b == nil {
			b = &strings.Builder{}
			builders[sessionID] = b
		}
		fmt.Fprintf(
			b,
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
		return err
	}
	for sessionID, b := range builders {
		out[sessionID] = b.String()
	}
	return nil
}

func loadPushToolCallAggregates(
	ctx context.Context,
	tx *sql.Tx,
	sessionIDs []string,
	out map[string]pushToolCallAggregate,
) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT session_id,
			COUNT(*), COALESCE(SUM(result_content_length), 0)
		FROM tool_calls
		WHERE session_id = ANY($1)
		GROUP BY session_id
	`, sessionIDs)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var sessionID string
		var agg pushToolCallAggregate
		var count int64
		if err := rows.Scan(&sessionID, &count, &agg.Sum); err != nil {
			return err
		}
		agg.Count = int(count)
		out[sessionID] = agg
	}
	return rows.Err()
}

func loadPushToolCallFingerprints(
	ctx context.Context,
	tx *sql.Tx,
	sessionIDs []string,
	out map[string]string,
) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT session_id, message_ordinal, call_index, tool_name, category,
			tool_use_id, COALESCE(input_json, ''),
			COALESCE(skill_name, ''), COALESCE(subagent_session_id, ''),
			COALESCE(result_content_length, 0),
			COALESCE(result_content, '')
		 FROM tool_calls
		WHERE session_id = ANY($1)
		ORDER BY session_id, message_ordinal ASC, call_index ASC
	`, sessionIDs)
	if err != nil {
		return err
	}
	defer rows.Close()

	builders := make(map[string]*strings.Builder, len(sessionIDs))
	for rows.Next() {
		var sessionID string
		var messageOrdinal, callIndex, resultContentLength int
		var toolName, category, toolUseID, inputJSON string
		var skillName, subagentSessionID, resultContent string
		if err := rows.Scan(
			&sessionID, &messageOrdinal, &callIndex, &toolName,
			&category, &toolUseID, &inputJSON,
			&skillName, &subagentSessionID, &resultContentLength,
			&resultContent,
		); err != nil {
			return err
		}
		b := builders[sessionID]
		if b == nil {
			b = &strings.Builder{}
			builders[sessionID] = b
		}
		fmt.Fprintf(
			b,
			"%d|%d|%d:%s|%d:%s|%d:%s|%d:%s|%d:%s|%d:%s|%d|%d:%s;",
			messageOrdinal, callIndex,
			len(toolName), toolName,
			len(category), category,
			len(toolUseID), toolUseID,
			len(inputJSON), inputJSON,
			len(skillName), skillName,
			len(subagentSessionID), subagentSessionID,
			resultContentLength,
			len(resultContent), resultContent,
		)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for sessionID, b := range builders {
		out[sessionID] = b.String()
	}
	return nil
}

func loadPushUsageEventFingerprints(
	ctx context.Context,
	tx *sql.Tx,
	sessionIDs []string,
	out map[string]string,
) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT session_id, message_ordinal, source, model,
			input_tokens, output_tokens,
			cache_creation_input_tokens, cache_read_input_tokens,
			reasoning_tokens, cost_usd, cost_status, cost_source,
			occurred_at, dedup_key
		 FROM usage_events
		WHERE session_id = ANY($1)
		ORDER BY session_id, occurred_at NULLS FIRST, id
`, sessionIDs)
	if err != nil {
		return err
	}
	defer rows.Close()

	builders := make(map[string]*strings.Builder, len(sessionIDs))
	for rows.Next() {
		var sessionID string
		var ordinal sql.NullInt64
		var source, model, costStatus, costSource string
		var inputTokens, outputTokens int
		var cacheCreationInputTokens, cacheReadInputTokens int
		var reasoningTokens int
		var cost sql.NullFloat64
		var occurredAt sql.NullTime
		var dedupKey sql.NullString
		if err := rows.Scan(
			&sessionID, &ordinal, &source, &model,
			&inputTokens, &outputTokens,
			&cacheCreationInputTokens, &cacheReadInputTokens,
			&reasoningTokens, &cost, &costStatus, &costSource,
			&occurredAt, &dedupKey,
		); err != nil {
			return err
		}
		b := builders[sessionID]
		if b == nil {
			b = &strings.Builder{}
			builders[sessionID] = b
		}
		occurred := ""
		if occurredAt.Valid {
			occurred = FormatISO8601(occurredAt.Time)
		}
		fmt.Fprintf(
			b,
			"%t|%d|%d:%s|%d:%s|%d|%d|%d|%d|%d|%t|%g|%d:%s|%d:%s|%d:%s|%d:%s;",
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
			len(costStatus), costStatus,
			len(costSource), costSource,
			len(occurred), occurred,
			len(dedupKey.String), dedupKey.String,
		)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for sessionID, b := range builders {
		out[sessionID] = b.String()
	}
	return nil
}

func shouldSkipSessionMessages(
	sessionID string,
	localCount int,
	localFP pushLocalMessageFingerprint,
	full bool,
	comparisons *pushMessageComparison,
) bool {
	if full || localCount == 0 || comparisons == nil {
		return false
	}
	pgAgg := comparisons.MessageAggregates[sessionID]
	if pgAgg.Count != localCount || pgAgg.Count == 0 {
		return false
	}

	return localFP.Sum == pgAgg.Sum &&
		localFP.Max == pgAgg.Max &&
		localFP.Min == pgAgg.Min &&
		localFP.ContentHashFP == comparisons.MessageContentHash[sessionID] &&
		localFP.RoleTimeFP == comparisons.MessageRoleTime[sessionID] &&
		localFP.FlagsFP == comparisons.MessageFlags[sessionID] &&
		localFP.SystemFP == comparisons.MessageSystemOrdinals[sessionID] &&
		localFP.ToolCallCount == comparisons.ToolCallAggregates[sessionID].Count &&
		localFP.ToolCallSum == comparisons.ToolCallAggregates[sessionID].Sum &&
		localFP.ToolCallFP == comparisons.ToolCallFingerprint[sessionID] &&
		localFP.TokenFP == comparisons.MessageTokenFingerprint[sessionID] &&
		localFP.UsageEventFP == comparisons.UsageEventFingerprint[sessionID]
}
