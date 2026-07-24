package postgres

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
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
	ToolResultFingerprint   map[string]string
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
	ToolResultFP  string
	TokenFP       string
	UsageEventFP  string
}

func localSessionDependencyPushFingerprint(
	ctx context.Context,
	local *db.DB,
	sessionID string,
	usageEventFingerprint string,
	usageKnown bool,
) (string, error) {
	msgFP, err := localPushMessageFingerprint(
		local, sessionID, usageEventFingerprint, usageKnown,
	)
	if err != nil {
		return "", err
	}
	findings, err := local.SessionSecretFindings(ctx, sessionID)
	if err != nil {
		return "", fmt.Errorf("reading local secret findings: %w", err)
	}
	pins, err := local.ListPinnedMessages(ctx, sessionID, "")
	if err != nil {
		return "", fmt.Errorf("reading local pinned messages: %w", err)
	}
	return hashLocalDependencyPayload(msgFP, findings, pins)
}

// hashLocalDependencyPayload is the shared final step of the per-session
// and batched dependency fingerprints. The JSON encoding is the persisted
// fingerprint format: any change re-pushes every session on the next push.
func hashLocalDependencyPayload(
	msgFP pushLocalMessageFingerprint,
	findings []db.SecretFinding,
	pins []db.PinnedMessage,
) (string, error) {
	payload := struct {
		Messages       pushLocalMessageFingerprint
		SecretFindings []db.SecretFinding
		Pins           []db.PinnedMessage
	}{
		Messages:       msgFP,
		SecretFindings: findings,
		Pins:           pins,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("encoding local dependency fingerprint: %w", err)
	}
	sum := sha256.Sum256(data)
	return fmt.Sprintf("%x", sum), nil
}

// localPushDependencyState holds one chunk's worth of prefetched local
// fingerprint inputs. The push loop fingerprints every candidate session on
// every push; reading these per session (a dozen point queries each) took
// minutes on a full push, so the loop prefetches them in batched queries.
// Values must match the per-session methods byte for byte — see the
// batched twins in internal/db for the identity contract.
type localPushDependencyState struct {
	contentAgg     map[string]db.MessageContentAggregate
	contentHashFP  map[string]string
	roleTimeFP     map[string]string
	flagsFP        map[string]string
	systemFP       map[string]string
	tokenFP        map[string]string
	toolCallCount  map[string]int
	toolCallSum    map[string]int64
	toolCallFP     map[string]string
	toolResultFP   map[string]string
	secretFindings map[string][]db.SecretFinding
	pins           map[string][]db.PinnedMessage
}

func readLocalPushDependencyState(
	ctx context.Context, local *db.DB, sessionIDs []string,
) (*localPushDependencyState, error) {
	st := &localPushDependencyState{}
	var err error
	if st.contentAgg, err = local.MessageContentFingerprints(sessionIDs); err != nil {
		return nil, fmt.Errorf("computing local content fingerprints: %w", err)
	}
	if st.contentHashFP, err = local.MessageContentHashFingerprints(sessionIDs); err != nil {
		return nil, fmt.Errorf("computing local content hash fingerprints: %w", err)
	}
	if st.roleTimeFP, err = local.MessageRoleTimeFingerprintsWithTimestampNormalizer(
		sessionIDs, pgPushTimestampFingerprintText,
	); err != nil {
		return nil, fmt.Errorf("computing local role/time fingerprints: %w", err)
	}
	if st.flagsFP, err = local.MessageFlagsFingerprints(sessionIDs); err != nil {
		return nil, fmt.Errorf(
			"computing local message flags fingerprints: %w", err,
		)
	}
	if st.systemFP, err = local.SystemMessageFingerprints(sessionIDs); err != nil {
		return nil, fmt.Errorf(
			"computing local system message fingerprints: %w", err,
		)
	}
	if st.tokenFP, err = local.MessageTokenFingerprints(sessionIDs); err != nil {
		return nil, fmt.Errorf("computing local token fingerprints: %w", err)
	}
	if st.toolCallCount, err = local.ToolCallCounts(sessionIDs); err != nil {
		return nil, fmt.Errorf("counting local tool_calls: %w", err)
	}
	if st.toolCallSum, err = local.ToolCallContentFingerprints(sessionIDs); err != nil {
		return nil, fmt.Errorf(
			"computing local tool_call content fingerprints: %w", err,
		)
	}
	if st.toolCallFP, err = local.ToolCallFingerprints(sessionIDs); err != nil {
		return nil, fmt.Errorf("computing local tool_call fingerprints: %w", err)
	}
	if st.toolResultFP, err = local.ToolResultEventFingerprintsWithTimestampNormalizer(
		sessionIDs, pgPushTimestampFingerprintText,
	); err != nil {
		return nil, fmt.Errorf(
			"computing local tool_result_event fingerprints: %w", err,
		)
	}
	if st.secretFindings, err = local.SessionSecretFindingsBySession(
		ctx, sessionIDs,
	); err != nil {
		return nil, fmt.Errorf("reading local secret findings: %w", err)
	}
	if st.pins, err = local.PinnedMessagesBySession(ctx, sessionIDs); err != nil {
		return nil, fmt.Errorf("reading local pinned messages: %w", err)
	}
	return st, nil
}

// dependencyFingerprint assembles the same fingerprint as
// localSessionDependencyPushFingerprint from the prefetched maps. local is
// only queried on the usageKnown=false fallback, which the push loop never
// takes (it prefetches usage fingerprints for every candidate).
func (st *localPushDependencyState) dependencyFingerprint(
	local *db.DB,
	sessionID string,
	usageEventFingerprint string,
	usageKnown bool,
) (string, error) {
	agg := st.contentAgg[sessionID]
	msgFP := pushLocalMessageFingerprint{
		Sum:           agg.Sum,
		Max:           agg.Max,
		Min:           agg.Min,
		ContentHashFP: st.contentHashFP[sessionID],
		RoleTimeFP:    st.roleTimeFP[sessionID],
		FlagsFP:       st.flagsFP[sessionID],
		SystemFP:      st.systemFP[sessionID],
		ToolCallCount: st.toolCallCount[sessionID],
		ToolCallSum:   st.toolCallSum[sessionID],
		ToolCallFP:    st.toolCallFP[sessionID],
		ToolResultFP:  st.toolResultFP[sessionID],
		TokenFP:       st.tokenFP[sessionID],
	}
	if usageKnown {
		msgFP.UsageEventFP = usageEventFingerprint
	} else {
		var err error
		msgFP.UsageEventFP, err = local.UsageEventFingerprint(sessionID)
		if err != nil {
			return "", fmt.Errorf(
				"computing local usage event fingerprint: %w", err,
			)
		}
	}
	return hashLocalDependencyPayload(
		msgFP, st.secretFindings[sessionID], st.pins[sessionID],
	)
}

func localPushMessageFingerprint(
	local *db.DB,
	sessionID string,
	usageEventFingerprint string,
	usageKnown bool,
) (pushLocalMessageFingerprint, error) {
	fp := pushLocalMessageFingerprint{}
	var err error
	fp.Sum, fp.Max, fp.Min, err = local.MessageContentFingerprint(sessionID)
	if err != nil {
		return fp, fmt.Errorf("computing local content fingerprint: %w", err)
	}
	fp.ContentHashFP, err = local.MessageContentHashFingerprint(sessionID)
	if err != nil {
		return fp, fmt.Errorf("computing local content hash fingerprint: %w", err)
	}
	fp.RoleTimeFP, err = localMessageRoleTimePGFingerprint(local, sessionID)
	if err != nil {
		return fp, fmt.Errorf("computing local role/time fingerprint: %w", err)
	}
	fp.FlagsFP, err = local.MessageFlagsFingerprint(sessionID)
	if err != nil {
		return fp, fmt.Errorf("computing local message flags fingerprint: %w", err)
	}
	fp.SystemFP, err = local.SystemMessageFingerprint(sessionID)
	if err != nil {
		return fp, fmt.Errorf("computing local system message fingerprint: %w", err)
	}
	fp.ToolCallCount, err = local.ToolCallCount(sessionID)
	if err != nil {
		return fp, fmt.Errorf("counting local tool_calls: %w", err)
	}
	fp.ToolCallSum, err = local.ToolCallContentFingerprint(sessionID)
	if err != nil {
		return fp, fmt.Errorf("computing local tool_call content fingerprint: %w", err)
	}
	fp.ToolCallFP, err = local.ToolCallFingerprint(sessionID)
	if err != nil {
		return fp, fmt.Errorf("computing local tool_call fingerprint: %w", err)
	}
	fp.ToolResultFP, err = localToolResultEventPGFingerprint(local, sessionID)
	if err != nil {
		return fp, fmt.Errorf("computing local tool_result_event fingerprint: %w", err)
	}
	fp.TokenFP, err = local.MessageTokenFingerprint(sessionID)
	if err != nil {
		return fp, fmt.Errorf("computing local token fingerprint: %w", err)
	}
	if usageKnown {
		fp.UsageEventFP = usageEventFingerprint
		return fp, nil
	}
	fp.UsageEventFP, err = local.UsageEventFingerprint(sessionID)
	if err != nil {
		return fp, fmt.Errorf("computing local usage event fingerprint: %w", err)
	}
	return fp, nil
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
		ToolResultFingerprint:   make(map[string]string, len(sessionIDs)),
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
		if err := loadPushToolResultEventFingerprints(
			ctx, tx, chunk, comparisons.ToolResultFingerprint,
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
			source_type, source_subtype, prompt_source, source_uuid,
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
		var srcType, srcSubtype, promptSource, srcUUID, srcParentUUID string
		var isSidechain, isCompactBoundary bool
		if err := rows.Scan(
			&sessionID, &ordinal, &model, &tokenUsage, &contextTokens,
			&outputTokens, &hasContextTokens, &hasOutputTokens,
			&claudeMsgID, &claudeReqID,
			&srcType, &srcSubtype, &promptSource, &srcUUID, &srcParentUUID,
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
				"%d:%s|%d:%s|%d:%s|%d:%s|%d:%s|%t|%t;",
			ordinal,
			len(model), model,
			len(tokenUsage), tokenUsage,
			contextTokens, outputTokens,
			hasContextTokens, hasOutputTokens,
			claudeMsgID, claudeReqID,
			len(srcType), srcType,
			len(srcSubtype), srcSubtype,
			len(promptSource), promptSource,
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
			COALESCE(result_content, ''),
			COALESCE(file_path, '')
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
		var skillName, subagentSessionID, resultContent, filePath string
		if err := rows.Scan(
			&sessionID, &messageOrdinal, &callIndex, &toolName,
			&category, &toolUseID, &inputJSON,
			&skillName, &subagentSessionID, &resultContentLength,
			&resultContent, &filePath,
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

func loadPushToolResultEventFingerprints(
	ctx context.Context,
	tx *sql.Tx,
	sessionIDs []string,
	out map[string]string,
) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT session_id, tool_call_message_ordinal, call_index, event_index,
			COALESCE(tool_use_id, ''), COALESCE(agent_id, ''),
			COALESCE(subagent_session_id, ''), source, status,
			content, content_length, timestamp
		 FROM tool_result_events
		WHERE session_id = ANY($1)
		ORDER BY session_id, tool_call_message_ordinal ASC, call_index ASC, event_index ASC
	`, sessionIDs)
	if err != nil {
		return err
	}
	defer rows.Close()

	builders := make(map[string]*strings.Builder, len(sessionIDs))
	for rows.Next() {
		var sessionID string
		var messageOrdinal, callIndex, eventIndex, contentLength int
		var toolUseID, agentID, subagentSessionID string
		var source, status, content string
		var timestamp sql.NullTime
		if err := rows.Scan(
			&sessionID, &messageOrdinal, &callIndex, &eventIndex,
			&toolUseID, &agentID, &subagentSessionID,
			&source, &status, &content, &contentLength, &timestamp,
		); err != nil {
			return err
		}
		b := builders[sessionID]
		if b == nil {
			b = &strings.Builder{}
			builders[sessionID] = b
		}
		timestampText := ""
		if timestamp.Valid {
			timestampText = FormatISO8601(timestamp.Time)
		}
		toolUseID = db.SanitizeUTF8(toolUseID)
		agentID = db.SanitizeUTF8(agentID)
		subagentSessionID = db.SanitizeUTF8(subagentSessionID)
		source = db.SanitizeUTF8(source)
		status = db.SanitizeUTF8(status)
		content = db.SanitizeUTF8(content)
		contentSum := sha256.Sum256([]byte(content))
		fmt.Fprintf(
			b,
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
	if err := rows.Err(); err != nil {
		return err
	}
	for sessionID, b := range builders {
		out[sessionID] = b.String()
	}
	return nil
}

func localToolResultEventPGFingerprint(
	local *db.DB, sessionID string,
) (string, error) {
	return local.ToolResultEventFingerprintWithTimestampNormalizer(
		sessionID,
		pgPushTimestampFingerprintText,
	)
}

func pgToolResultEventFingerprint(
	ctx context.Context, tx *sql.Tx, sessionID string,
) (string, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT tool_call_message_ordinal, call_index, event_index,
			COALESCE(tool_use_id, ''), COALESCE(agent_id, ''),
			COALESCE(subagent_session_id, ''), source, status,
			content, content_length, timestamp
		 FROM tool_result_events
		 WHERE session_id = $1
		 ORDER BY tool_call_message_ordinal ASC, call_index ASC, event_index ASC`,
		sessionID,
	)
	if err != nil {
		return "", err
	}
	defer rows.Close()

	var b strings.Builder
	for rows.Next() {
		var messageOrdinal, callIndex, eventIndex, contentLength int
		var toolUseID, agentID, subagentSessionID string
		var source, status, content string
		var timestamp sql.NullTime
		if err := rows.Scan(
			&messageOrdinal, &callIndex, &eventIndex,
			&toolUseID, &agentID, &subagentSessionID,
			&source, &status, &content, &contentLength, &timestamp,
		); err != nil {
			return "", err
		}
		timestampText := ""
		if timestamp.Valid {
			timestampText = FormatISO8601(timestamp.Time)
		}
		toolUseID = db.SanitizeUTF8(toolUseID)
		agentID = db.SanitizeUTF8(agentID)
		subagentSessionID = db.SanitizeUTF8(subagentSessionID)
		source = db.SanitizeUTF8(source)
		status = db.SanitizeUTF8(status)
		content = db.SanitizeUTF8(content)
		contentSum := sha256.Sum256([]byte(content))
		fmt.Fprintf(&b,
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
		localFP.ToolResultFP == comparisons.ToolResultFingerprint[sessionID] &&
		localFP.TokenFP == comparisons.MessageTokenFingerprint[sessionID] &&
		localFP.UsageEventFP == comparisons.UsageEventFingerprint[sessionID]
}
