package duckdb

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/db"
	pricingpkg "go.kenn.io/agentsview/internal/pricing"
)

func (s *Sync) syncModelPricing(ctx context.Context) error {
	prices, err := s.local.ListModelPricing(ctx)
	if err != nil {
		return err
	}
	if len(prices) == 0 {
		prices = duckFallbackPricingRows()
	}
	if len(prices) == 0 {
		return nil
	}

	existing, err := listDuckModelPricing(ctx, s.duck)
	if err != nil {
		return err
	}
	_, prices = db.FilterChangedModelPricing(existing, prices)
	if len(prices) == 0 {
		return nil
	}

	tx, err := s.duck.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning duckdb pricing sync: %w", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()

	for i := 0; i < len(prices); i += duckPricingUpsertBatch {
		end := min(i+duckPricingUpsertBatch, len(prices))
		batch := prices[i:end]
		query, args := duckPricingUpsertStatement(batch)
		if err := s.execMutation(ctx, tx, query, args...); err != nil {
			return fmt.Errorf(
				"syncing duckdb pricing batch starting at %d: %w",
				i, err,
			)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing duckdb pricing sync: %w", err)
	}
	return nil
}

const duckPricingUpsertBatch = 100

func duckPricingUpsertStatement(prices []db.ModelPricing) (string, []any) {
	var b strings.Builder
	b.WriteString(`INSERT INTO model_pricing (
		model_pattern, input_per_mtok, output_per_mtok,
		cache_creation_per_mtok, cache_read_per_mtok, updated_at
	) VALUES `)
	args := make([]any, 0, len(prices)*6)
	for i, p := range prices {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString("(?, ?, ?, ?, ?, ?)")
		args = append(args,
			p.ModelPattern,
			p.InputPerMTok,
			p.OutputPerMTok,
			p.CacheCreationPerMTok,
			p.CacheReadPerMTok,
			p.UpdatedAt,
		)
	}
	b.WriteString(`
	ON CONFLICT(model_pattern) DO UPDATE SET
		input_per_mtok = excluded.input_per_mtok,
		output_per_mtok = excluded.output_per_mtok,
		cache_creation_per_mtok = excluded.cache_creation_per_mtok,
		cache_read_per_mtok = excluded.cache_read_per_mtok,
		updated_at = excluded.updated_at`)
	return b.String(), args
}

func listDuckModelPricing(ctx context.Context, duck *sql.DB) ([]db.ModelPricing, error) {
	rows, err := duck.QueryContext(ctx,
		`SELECT model_pattern, input_per_mtok,
			output_per_mtok, cache_creation_per_mtok,
			cache_read_per_mtok, updated_at
		 FROM model_pricing`,
	)
	if err != nil {
		return nil, fmt.Errorf("listing duckdb pricing: %w", err)
	}
	defer rows.Close()

	var out []db.ModelPricing
	for rows.Next() {
		var p db.ModelPricing
		if err := rows.Scan(
			&p.ModelPattern,
			&p.InputPerMTok,
			&p.OutputPerMTok,
			&p.CacheCreationPerMTok,
			&p.CacheReadPerMTok,
			&p.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scanning duckdb pricing: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating duckdb pricing: %w", err)
	}
	return out, nil
}

func (s *Sync) syncCursorUsageEvents(ctx context.Context) error {
	// Cursor admin rows are global and unattributed, so project-filtered pushes
	// cannot sync them honestly.
	if s.isFiltered() {
		return nil
	}

	events, err := s.local.GetCursorUsageEvents(ctx)
	if err != nil {
		return fmt.Errorf("loading local cursor usage events: %w", err)
	}
	if len(events) == 0 {
		return nil
	}

	tx, err := s.duck.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning duckdb cursor usage sync: %w", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()

	if err := s.bulkInsertCursorUsageEvents(ctx, tx, events); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("committing duckdb cursor usage sync: %w", err)
	}
	return nil
}

func duckFallbackPricingRows() []db.ModelPricing {
	src := pricingpkg.FallbackPricing()
	out := make([]db.ModelPricing, len(src))
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for i, p := range src {
		out[i] = db.ModelPricing{
			ModelPattern:         p.ModelPattern,
			InputPerMTok:         p.InputPerMTok,
			OutputPerMTok:        p.OutputPerMTok,
			CacheCreationPerMTok: p.CacheCreationPerMTok,
			CacheReadPerMTok:     p.CacheReadPerMTok,
			UpdatedAt:            now,
		}
	}
	return out
}

func (s *Sync) replaceStarredSessions(
	ctx context.Context, tx *sql.Tx, sessions []db.Session,
) error {
	ids, err := s.local.ListStarredSessionIDs(ctx)
	if err != nil {
		return err
	}
	allowed := make(map[string]bool, len(sessions))
	for _, sess := range sessions {
		allowed[sess.ID] = true
	}
	if s.isFiltered() {
		for _, sess := range sessions {
			if err := s.execMutation(ctx, tx,
				`DELETE FROM starred_sessions WHERE session_id = ?`, sess.ID,
			); err != nil {
				return fmt.Errorf("clearing duckdb starred session %s: %w", sess.ID, err)
			}
		}
	} else {
		if err := s.execMutation(ctx, tx, `
			DELETE FROM starred_sessions
			WHERE session_id IN (
				SELECT id FROM sessions WHERE machine = ?
			)`, s.machine); err != nil {
			return fmt.Errorf("clearing duckdb starred_sessions: %w", err)
		}
	}
	for _, id := range ids {
		if !allowed[id] {
			continue
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO starred_sessions (session_id, created_at)
			 VALUES (?, current_timestamp)`,
			id,
		); err != nil {
			return fmt.Errorf("syncing starred session %s: %w", id, err)
		}
	}
	return nil
}

func (s *Sync) pushSession(
	ctx context.Context, tx *sql.Tx, sess db.Session,
) (int, error) {
	if err := s.upsertSession(ctx, tx, sess); err != nil {
		return 0, err
	}
	if err := s.replaceSessionDependents(ctx, tx, sess.ID); err != nil {
		return 0, err
	}
	if err := s.replaceUsageEvents(ctx, tx, sess.ID); err != nil {
		return 0, err
	}
	msgs, err := s.local.GetAllMessages(ctx, sess.ID)
	if err != nil {
		return 0, fmt.Errorf("reading local messages for %s: %w", sess.ID, err)
	}
	if err := insertMessages(ctx, tx, msgs); err != nil {
		return 0, err
	}
	toolCallKeys, err := s.upsertToolCalls(ctx, tx, msgs)
	if err != nil {
		return 0, err
	}
	eventKeys, err := s.upsertToolResultEvents(ctx, tx, msgs)
	if err != nil {
		return 0, err
	}
	if err := s.deleteStaleToolCalls(ctx, tx, sess.ID, toolCallKeys); err != nil {
		return 0, err
	}
	if err := s.deleteStaleToolResultEvents(ctx, tx, sess.ID, eventKeys); err != nil {
		return 0, err
	}
	if err := s.replaceSecretFindings(ctx, tx, sess.ID); err != nil {
		return 0, err
	}
	if err := s.replacePinnedMessages(ctx, tx, sess.ID); err != nil {
		return 0, err
	}
	return len(msgs), nil
}

func (s *Sync) replaceSessionDependents(
	ctx context.Context, tx *sql.Tx, sessionID string,
) error {
	for _, stmt := range []string{
		`DELETE FROM pinned_messages WHERE session_id = ?`,
		`DELETE FROM secret_findings WHERE session_id = ?`,
		`DELETE FROM usage_events WHERE session_id = ?`,
		`DELETE FROM messages WHERE session_id = ?`,
	} {
		if err := s.execMutation(ctx, tx, stmt, sessionID); err != nil {
			return fmt.Errorf("clearing duckdb session dependents: %w", err)
		}
	}
	return nil
}

func (s *Sync) deleteHardDeletedMirrorSessions(
	ctx context.Context, tx *sql.Tx, localSessions []db.Session,
	machine string, projects, excludeProjects []string,
) ([]string, error) {
	localIDs := make(map[string]bool, len(localSessions))
	for _, sess := range localSessions {
		localIDs[sess.ID] = true
	}
	rows, err := tx.QueryContext(ctx,
		`SELECT id, project FROM sessions WHERE machine = ?`,
		machine,
	)
	if err != nil {
		return nil, fmt.Errorf("listing duckdb sessions for deletion reconciliation: %w", err)
	}
	defer rows.Close()
	var stale []string
	for rows.Next() {
		var id, project string
		if err := rows.Scan(&id, &project); err != nil {
			return nil, fmt.Errorf("scanning duckdb session for deletion reconciliation: %w", err)
		}
		if !projectInSyncScope(project, projects, excludeProjects) {
			continue
		}
		if !localIDs[id] {
			stale = append(stale, id)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Strings(stale)
	for _, id := range stale {
		if err := s.deleteMirrorSession(ctx, tx, id); err != nil {
			return nil, err
		}
	}
	return stale, nil
}

func (s *Sync) deleteMirrorSession(
	ctx context.Context, tx *sql.Tx, sessionID string,
) error {
	for _, stmt := range []string{
		`DELETE FROM pinned_messages WHERE session_id = ?`,
		`DELETE FROM starred_sessions WHERE session_id = ?`,
		`DELETE FROM secret_findings WHERE session_id = ?`,
		`DELETE FROM tool_result_events WHERE session_id = ?`,
		`DELETE FROM tool_calls WHERE session_id = ?`,
		`DELETE FROM usage_events WHERE session_id = ?`,
		`DELETE FROM messages WHERE session_id = ?`,
		`DELETE FROM sessions WHERE id = ?`,
	} {
		if err := s.execMutation(ctx, tx, stmt, sessionID); err != nil {
			return fmt.Errorf("deleting hard-deleted duckdb session %s: %w", sessionID, err)
		}
	}
	return nil
}

func projectInSyncScope(project string, projects, excludeProjects []string) bool {
	if len(projects) > 0 {
		found := slices.Contains(projects, project)
		if !found {
			return false
		}
	}
	return !slices.Contains(excludeProjects, project)
}

func (s *Sync) execMutation(
	ctx context.Context, tx *sql.Tx, stmt string, args ...any,
) error {
	if s.connectionKind != duckDBQuackClientConnection {
		_, err := tx.ExecContext(ctx, stmt, args...)
		return err
	}
	// Quack attachments can accept plain inserts, but DELETE, UPDATE, and
	// ON CONFLICT are planned against proxy storage and currently fail with
	// GetStorageInfo errors. Run those mutations on the server-side base DB.
	sqlText, err := duckSQLWithArgs(stmt, args...)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "FROM "+quackAttachmentName+".query(?)", sqlText)
	return err
}

func duckSQLWithArgs(stmt string, args ...any) (string, error) {
	var b strings.Builder
	argIndex := 0
	for _, r := range stmt {
		if r != '?' {
			b.WriteRune(r)
			continue
		}
		if argIndex >= len(args) {
			return "", fmt.Errorf("duckdb remote statement missing argument")
		}
		lit, err := duckValueLiteral(args[argIndex])
		if err != nil {
			return "", err
		}
		b.WriteString(lit)
		argIndex++
	}
	if argIndex != len(args) {
		return "", fmt.Errorf("duckdb remote statement has unused argument")
	}
	return b.String(), nil
}

func duckValueLiteral(v any) (string, error) {
	switch value := v.(type) {
	case nil:
		return "NULL", nil
	case string:
		return duckLiteral(value), nil
	case int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64,
		float32, float64:
		return fmt.Sprint(value), nil
	case *int:
		if value == nil {
			return "NULL", nil
		}
		return fmt.Sprint(*value), nil
	case *int64:
		if value == nil {
			return "NULL", nil
		}
		return fmt.Sprint(*value), nil
	case *float64:
		if value == nil {
			return "NULL", nil
		}
		return fmt.Sprint(*value), nil
	case bool:
		if value {
			return "TRUE", nil
		}
		return "FALSE", nil
	case time.Time:
		return "TIMESTAMP " + duckLiteral(
			value.UTC().Format("2006-01-02 15:04:05.999999"),
		), nil
	default:
		return "", fmt.Errorf("unsupported duckdb remote argument type %T", v)
	}
}

func (s *Sync) upsertSession(
	ctx context.Context, tx *sql.Tx, sess db.Session,
) error {
	query := `
		INSERT INTO sessions (
			id, project, machine, agent,
			first_message, display_name, session_name, started_at, ended_at,
			message_count, user_message_count,
			file_path, file_size, file_mtime, file_inode, file_device,
			file_hash, local_modified_at, parent_session_id,
			relationship_type, total_output_tokens, peak_context_tokens,
			has_total_output_tokens, has_peak_context_tokens, is_automated,
			tool_failure_signal_count, tool_retry_count, edit_churn_count,
			consecutive_failure_max, outcome, outcome_confidence,
			ended_with_role, final_failure_streak, signals_pending_since,
			compaction_count, mid_task_compaction_count,
			context_pressure_max, health_score, health_grade,
			has_tool_calls, has_context_data,
			quality_signal_version, short_prompt_count, unstructured_start,
			missing_success_criteria_count, missing_verification_count,
			duplicate_prompt_count, no_code_context_count,
			runaway_tool_loop_count, data_version,
			cwd, git_branch, source_session_id, source_version, transcript_fidelity,
			parser_malformed_lines, is_truncated, deleted_at, created_at,
			termination_status, secret_leak_count, secrets_rules_version
		) VALUES (
			?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
			?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
			?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
			?, ?, ?, ?, ?, ?, ?, ?
		)`
	query += `
		ON CONFLICT(id) DO UPDATE SET
			project = excluded.project,
			machine = excluded.machine,
			agent = excluded.agent,
			first_message = excluded.first_message,
			display_name = excluded.display_name,
			session_name = excluded.session_name,
			started_at = excluded.started_at,
			ended_at = excluded.ended_at,
			message_count = excluded.message_count,
			user_message_count = excluded.user_message_count,
			file_path = excluded.file_path,
			file_size = excluded.file_size,
			file_mtime = excluded.file_mtime,
			file_inode = excluded.file_inode,
			file_device = excluded.file_device,
			file_hash = excluded.file_hash,
			local_modified_at = excluded.local_modified_at,
			parent_session_id = excluded.parent_session_id,
			relationship_type = excluded.relationship_type,
			total_output_tokens = excluded.total_output_tokens,
			peak_context_tokens = excluded.peak_context_tokens,
			has_total_output_tokens = excluded.has_total_output_tokens,
			has_peak_context_tokens = excluded.has_peak_context_tokens,
			is_automated = excluded.is_automated,
			tool_failure_signal_count = excluded.tool_failure_signal_count,
			tool_retry_count = excluded.tool_retry_count,
			edit_churn_count = excluded.edit_churn_count,
			consecutive_failure_max = excluded.consecutive_failure_max,
			outcome = excluded.outcome,
			outcome_confidence = excluded.outcome_confidence,
			ended_with_role = excluded.ended_with_role,
			final_failure_streak = excluded.final_failure_streak,
			signals_pending_since = excluded.signals_pending_since,
			compaction_count = excluded.compaction_count,
			mid_task_compaction_count = excluded.mid_task_compaction_count,
			context_pressure_max = excluded.context_pressure_max,
			health_score = excluded.health_score,
			health_grade = excluded.health_grade,
			has_tool_calls = excluded.has_tool_calls,
			has_context_data = excluded.has_context_data,
			quality_signal_version = excluded.quality_signal_version,
			short_prompt_count = excluded.short_prompt_count,
			unstructured_start = excluded.unstructured_start,
			missing_success_criteria_count = excluded.missing_success_criteria_count,
			missing_verification_count = excluded.missing_verification_count,
			duplicate_prompt_count = excluded.duplicate_prompt_count,
			no_code_context_count = excluded.no_code_context_count,
			runaway_tool_loop_count = excluded.runaway_tool_loop_count,
			data_version = excluded.data_version,
			cwd = excluded.cwd,
			git_branch = excluded.git_branch,
			source_session_id = excluded.source_session_id,
			source_version = excluded.source_version,
			transcript_fidelity = excluded.transcript_fidelity,
			parser_malformed_lines = excluded.parser_malformed_lines,
			is_truncated = excluded.is_truncated,
			deleted_at = excluded.deleted_at,
			created_at = excluded.created_at,
			termination_status = excluded.termination_status,
			secret_leak_count = excluded.secret_leak_count,
			secrets_rules_version = excluded.secrets_rules_version`

	if err := s.execMutation(ctx, tx, query, sessionInsertArgs(sess, s.machine)...); err != nil {
		return fmt.Errorf("writing duckdb session %s: %w", sess.ID, err)
	}
	return nil
}

func sessionInsertArgs(sess db.Session, machine string) []any {
	return []any{
		sess.ID, sess.Project, machine, sess.Agent,
		nilString(sess.FirstMessage), nilString(sess.DisplayName),
		nilString(sess.SessionName),
		nilTime(sess.StartedAt), nilTime(sess.EndedAt),
		sess.MessageCount, sess.UserMessageCount,
		nilString(sess.FilePath), sess.FileSize, sess.FileMtime,
		sess.FileInode, sess.FileDevice, nilString(sess.FileHash),
		nilTime(sess.LocalModifiedAt), nilString(sess.ParentSessionID),
		sess.RelationshipType, sess.TotalOutputTokens,
		sess.PeakContextTokens, sess.HasTotalOutputTokens,
		sess.HasPeakContextTokens, sess.IsAutomated,
		sess.ToolFailureSignalCount, sess.ToolRetryCount,
		sess.EditChurnCount, sess.ConsecutiveFailureMax,
		sess.Outcome, sess.OutcomeConfidence,
		sess.EndedWithRole, sess.FinalFailureStreak,
		nilString(sess.SignalsPendingSince),
		sess.CompactionCount, sess.MidTaskCompactionCount,
		sess.ContextPressureMax, sess.HealthScore,
		nilString(sess.HealthGrade), sess.HasToolCalls,
		sess.HasContextData,
		sess.QualitySignalVersion, sess.ShortPromptCount,
		sess.UnstructuredStart, sess.MissingSuccessCriteriaCount,
		sess.MissingVerificationCount, sess.DuplicatePromptCount,
		sess.NoCodeContextCount, sess.RunawayToolLoopCount,
		sess.DataVersion,
		sess.Cwd, sess.GitBranch, sess.SourceSessionID,
		sess.SourceVersion, sess.TranscriptFidelity, sess.ParserMalformedLines,
		sess.IsTruncated, nilTime(sess.DeletedAt),
		timeValue(sess.CreatedAt), nilString(sess.TerminationStatus),
		sess.SecretLeakCount, sess.SecretsRulesVersion,
	}
}

func insertMessages(ctx context.Context, tx *sql.Tx, msgs []db.Message) error {
	for _, m := range msgs {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO messages (
				id, session_id, ordinal, role, content, thinking_text,
				timestamp, has_thinking, has_tool_use, content_length,
				is_system, model, token_usage, context_tokens, output_tokens,
				has_context_tokens, has_output_tokens, claude_message_id,
				claude_request_id, source_type, source_subtype, source_uuid,
				source_parent_uuid, is_sidechain, is_compact_boundary
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			m.ID, m.SessionID, m.Ordinal, m.Role, m.Content,
			m.ThinkingText, timeValue(m.Timestamp),
			m.HasThinking, m.HasToolUse, m.ContentLength,
			m.IsSystem, m.Model, string(m.TokenUsage),
			m.ContextTokens, m.OutputTokens,
			m.HasContextTokens, m.HasOutputTokens,
			m.ClaudeMessageID, m.ClaudeRequestID,
			m.SourceType, m.SourceSubtype, m.SourceUUID,
			m.SourceParentUUID, m.IsSidechain, m.IsCompactBoundary,
		); err != nil {
			return fmt.Errorf("inserting duckdb message %s/%d: %w", m.SessionID, m.Ordinal, err)
		}
	}
	return nil
}

type duckToolCallKey struct {
	messageID int64
	callIndex int
}

type duckToolResultEventKey struct {
	ordinal    int
	callIndex  int
	eventIndex int
}

func (s *Sync) upsertToolCalls(
	ctx context.Context, tx *sql.Tx, msgs []db.Message,
) ([]duckToolCallKey, error) {
	keys := []duckToolCallKey{}
	for _, m := range msgs {
		for i, tc := range m.ToolCalls {
			key := duckToolCallKey{messageID: m.ID, callIndex: i}
			keys = append(keys, key)
			exists, err := toolCallExists(ctx, tx, m.SessionID, key)
			if err != nil {
				return nil, err
			}
			if exists {
				if err := s.execMutation(ctx, tx, `
					UPDATE tool_calls SET
						tool_name = ?, category = ?, tool_use_id = ?,
						input_json = ?, skill_name = ?,
						result_content_length = ?, result_content = ?,
						subagent_session_id = ?, file_path = ?
					WHERE session_id = ? AND message_id = ? AND call_index = ?`,
					tc.ToolName, tc.Category, tc.ToolUseID,
					nilEmpty(tc.InputJSON), nilEmpty(tc.SkillName),
					nilZero(tc.ResultContentLength), nilEmpty(tc.ResultContent),
					nilEmpty(tc.SubagentSessionID), nilEmpty(tc.FilePath),
					m.SessionID, m.ID, i,
				); err != nil {
					return nil, fmt.Errorf("updating duckdb tool_call %s/%d/%d: %w",
						m.SessionID, m.Ordinal, i, err)
				}
				continue
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO tool_calls (
					message_id, session_id, tool_name, category,
					call_index, tool_use_id, input_json, skill_name,
					result_content_length, result_content,
					subagent_session_id, file_path
				) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				m.ID, m.SessionID, tc.ToolName, tc.Category,
				i, tc.ToolUseID, nilEmpty(tc.InputJSON),
				nilEmpty(tc.SkillName), nilZero(tc.ResultContentLength),
				nilEmpty(tc.ResultContent), nilEmpty(tc.SubagentSessionID),
				nilEmpty(tc.FilePath),
			); err != nil {
				return nil, fmt.Errorf("inserting duckdb tool_call %s/%d/%d: %w",
					m.SessionID, m.Ordinal, i, err)
			}
		}
	}
	return keys, nil
}

func toolCallExists(
	ctx context.Context, tx *sql.Tx, sessionID string, key duckToolCallKey,
) (bool, error) {
	var exists bool
	err := tx.QueryRowContext(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM tool_calls
			WHERE session_id = ? AND message_id = ? AND call_index = ?
		)`, sessionID, key.messageID, key.callIndex).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("checking duckdb tool_call %s/%d/%d: %w",
			sessionID, key.messageID, key.callIndex, err)
	}
	return exists, nil
}

func (s *Sync) upsertToolResultEvents(
	ctx context.Context, tx *sql.Tx, msgs []db.Message,
) ([]duckToolResultEventKey, error) {
	keys := []duckToolResultEventKey{}
	for _, m := range msgs {
		for i, tc := range m.ToolCalls {
			for _, ev := range tc.ResultEvents {
				key := duckToolResultEventKey{
					ordinal:    m.Ordinal,
					callIndex:  i,
					eventIndex: ev.EventIndex,
				}
				keys = append(keys, key)
				exists, err := toolResultEventExists(ctx, tx, m.SessionID, key)
				if err != nil {
					return nil, err
				}
				if exists {
					if err := s.execMutation(ctx, tx, `
						UPDATE tool_result_events SET
							tool_use_id = ?, agent_id = ?, subagent_session_id = ?,
							source = ?, status = ?, content = ?,
							content_length = ?, timestamp = ?
						WHERE session_id = ?
							AND tool_call_message_ordinal = ?
							AND call_index = ?
							AND event_index = ?`,
						nilEmpty(ev.ToolUseID), nilEmpty(ev.AgentID),
						nilEmpty(ev.SubagentSessionID), ev.Source, ev.Status,
						ev.Content, ev.ContentLength, timeValue(ev.Timestamp),
						m.SessionID, m.Ordinal, i, ev.EventIndex,
					); err != nil {
						return nil, fmt.Errorf("updating duckdb tool_result_event %s/%d/%d: %w",
							m.SessionID, m.Ordinal, i, err)
					}
					continue
				}
				if _, err := tx.ExecContext(ctx, `
					INSERT INTO tool_result_events (
						session_id, tool_call_message_ordinal, call_index,
						tool_use_id, agent_id, subagent_session_id,
						source, status, content, content_length,
						timestamp, event_index
					) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
					m.SessionID, m.Ordinal, i,
					nilEmpty(ev.ToolUseID), nilEmpty(ev.AgentID),
					nilEmpty(ev.SubagentSessionID), ev.Source, ev.Status,
					ev.Content, ev.ContentLength, timeValue(ev.Timestamp),
					ev.EventIndex,
				); err != nil {
					return nil, fmt.Errorf("inserting duckdb tool_result_event %s/%d/%d: %w",
						m.SessionID, m.Ordinal, i, err)
				}
			}
		}
	}
	return keys, nil
}

func toolResultEventExists(
	ctx context.Context, tx *sql.Tx, sessionID string, key duckToolResultEventKey,
) (bool, error) {
	var exists bool
	err := tx.QueryRowContext(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM tool_result_events
			WHERE session_id = ?
				AND tool_call_message_ordinal = ?
				AND call_index = ?
				AND event_index = ?
		)`, sessionID, key.ordinal, key.callIndex, key.eventIndex).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("checking duckdb tool_result_event %s/%d/%d/%d: %w",
			sessionID, key.ordinal, key.callIndex, key.eventIndex, err)
	}
	return exists, nil
}

func (s *Sync) deleteStaleToolCalls(
	ctx context.Context, tx *sql.Tx, sessionID string, keep []duckToolCallKey,
) error {
	keepSet := make(map[duckToolCallKey]bool, len(keep))
	for _, key := range keep {
		keepSet[key] = true
	}
	rows, err := tx.QueryContext(ctx,
		`SELECT message_id, call_index FROM tool_calls WHERE session_id = ?`,
		sessionID,
	)
	if err != nil {
		return fmt.Errorf("listing duckdb tool_calls for stale delete %s: %w", sessionID, err)
	}
	defer rows.Close()
	for rows.Next() {
		var key duckToolCallKey
		if err := rows.Scan(&key.messageID, &key.callIndex); err != nil {
			return err
		}
		if keepSet[key] {
			continue
		}
		if err := s.execMutation(ctx, tx,
			`DELETE FROM tool_calls WHERE session_id = ? AND message_id = ? AND call_index = ?`,
			sessionID, key.messageID, key.callIndex,
		); err != nil {
			return fmt.Errorf("deleting stale duckdb tool_call %s: %w", sessionID, err)
		}
	}
	return rows.Err()
}

func (s *Sync) deleteStaleToolResultEvents(
	ctx context.Context, tx *sql.Tx, sessionID string, keep []duckToolResultEventKey,
) error {
	keepSet := make(map[duckToolResultEventKey]bool, len(keep))
	for _, key := range keep {
		keepSet[key] = true
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT tool_call_message_ordinal, call_index, event_index
		FROM tool_result_events
		WHERE session_id = ?`,
		sessionID,
	)
	if err != nil {
		return fmt.Errorf("listing duckdb tool_result_events for stale delete %s: %w", sessionID, err)
	}
	defer rows.Close()
	for rows.Next() {
		var key duckToolResultEventKey
		if err := rows.Scan(&key.ordinal, &key.callIndex, &key.eventIndex); err != nil {
			return err
		}
		if keepSet[key] {
			continue
		}
		if err := s.execMutation(ctx, tx, `
			DELETE FROM tool_result_events
			WHERE session_id = ?
				AND tool_call_message_ordinal = ?
				AND call_index = ?
				AND event_index = ?`,
			sessionID, key.ordinal, key.callIndex, key.eventIndex,
		); err != nil {
			return fmt.Errorf("deleting stale duckdb tool_result_event %s: %w", sessionID, err)
		}
	}
	return rows.Err()
}

func (s *Sync) replaceUsageEvents(
	ctx context.Context, tx *sql.Tx, sessionID string,
) error {
	events, err := s.local.GetUsageEvents(ctx, sessionID)
	if err != nil {
		return err
	}
	if err := s.execMutation(ctx, tx,
		`DELETE FROM usage_events WHERE session_id = ?`,
		sessionID,
	); err != nil {
		return fmt.Errorf("clearing duckdb usage_events for %s: %w", sessionID, err)
	}
	for _, ev := range events {
		if err := insertUsageEvent(ctx, tx, ev); err != nil {
			return fmt.Errorf("inserting duckdb usage_event %s: %w", sessionID, err)
		}
	}
	return nil
}

func insertUsageEvent(ctx context.Context, tx *sql.Tx, ev db.UsageEvent) error {
	ordinal, cost, occurredAt := usageEventNullableValues(ev)
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO usage_events (
			id, session_id, message_ordinal, source, model,
			input_tokens, output_tokens,
			cache_creation_input_tokens, cache_read_input_tokens,
			reasoning_tokens, cost_usd, cost_status, cost_source,
			occurred_at, dedup_key
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ev.ID, ev.SessionID, ordinal, ev.Source, ev.Model,
		ev.InputTokens, ev.OutputTokens,
		ev.CacheCreationInputTokens, ev.CacheReadInputTokens,
		ev.ReasoningTokens, cost, ev.CostStatus,
		ev.CostSource, occurredAt, ev.DedupKey,
	); err != nil {
		return err
	}
	return nil
}

func (s *Sync) bulkInsertCursorUsageEvents(
	ctx context.Context, tx *sql.Tx, events []db.CursorUsageEvent,
) error {
	if len(events) == 0 {
		return nil
	}
	const cursorBatch = 100
	for i := 0; i < len(events); i += cursorBatch {
		end := min(i+cursorBatch, len(events))
		batch := events[i:end]

		var b strings.Builder
		b.WriteString(`INSERT INTO cursor_usage_events (
			occurred_at, model, kind,
			input_tokens, output_tokens,
			cache_write_tokens, cache_read_tokens,
			charged_cents, cursor_token_fee,
			user_id, user_email, is_headless, dedup_key
		) VALUES `)
		args := make([]any, 0, len(batch)*13)
		for j, ev := range batch {
			if j > 0 {
				b.WriteByte(',')
			}
			b.WriteString("(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)")
			occurredAt, ok := parseTimestamp(ev.OccurredAt)
			if !ok {
				return fmt.Errorf("parsing cursor usage occurred_at %q", ev.OccurredAt)
			}
			args = append(args,
				occurredAt,
				db.SanitizeUTF8(ev.Model),
				db.SanitizeUTF8(ev.Kind),
				ev.InputTokens,
				ev.OutputTokens,
				ev.CacheWriteTokens,
				ev.CacheReadTokens,
				ev.ChargedCents,
				ev.CursorTokenFee,
				db.SanitizeUTF8(ev.UserID),
				db.SanitizeUTF8(ev.UserEmail),
				ev.IsHeadless,
				db.SanitizeUTF8(ev.DedupKey),
			)
		}
		b.WriteString(` ON CONFLICT DO NOTHING`)
		if err := s.execMutation(ctx, tx, b.String(), args...); err != nil {
			return fmt.Errorf("bulk inserting duckdb cursor_usage_events: %w", err)
		}
	}
	return nil
}

func usageEventNullableValues(ev db.UsageEvent) (any, any, any) {
	var ordinal any
	if ev.MessageOrdinal != nil {
		ordinal = *ev.MessageOrdinal
	}
	var cost any
	if ev.CostUSD != nil {
		cost = *ev.CostUSD
	}
	var occurredAt any
	if ev.OccurredAt != "" {
		occurredAt = timeValue(ev.OccurredAt)
	}
	return ordinal, cost, occurredAt
}

func (s *Sync) replaceSecretFindings(
	ctx context.Context, tx *sql.Tx, sessionID string,
) error {
	findings, err := s.local.SessionSecretFindings(ctx, sessionID)
	if err != nil {
		return err
	}
	for _, f := range findings {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO secret_findings (
				session_id, rule_name, confidence, location_kind,
				message_ordinal, call_index, event_index,
				match_start, match_end, match_index,
				redacted_match, rules_version, created_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, current_timestamp)`,
			f.SessionID, f.RuleName, f.Confidence, f.LocationKind,
			f.MessageOrdinal, f.CallIndex, f.EventIndex,
			f.MatchStart, f.MatchEnd, f.MatchIndex,
			f.RedactedMatch, f.RulesVersion,
		); err != nil {
			return fmt.Errorf("inserting duckdb secret_finding %s: %w", sessionID, err)
		}
	}
	return nil
}

func (s *Sync) replacePinnedMessages(
	ctx context.Context, tx *sql.Tx, sessionID string,
) error {
	pins, err := s.local.ListPinnedMessages(ctx, sessionID, "")
	if err != nil {
		return err
	}
	for _, p := range pins {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO pinned_messages (
				id, session_id, message_id, ordinal, note, created_at
			) VALUES (?, ?, ?, ?, ?, ?)`,
			p.ID, p.SessionID, p.MessageID, p.Ordinal,
			p.Note, timeValue(p.CreatedAt),
		); err != nil {
			return fmt.Errorf("inserting duckdb pinned_message %s/%d: %w",
				sessionID, p.MessageID, err)
		}
	}
	return nil
}

func (s *Sync) replaceAllPinnedMessages(
	ctx context.Context, tx *sql.Tx, sessions []db.Session,
) error {
	if err := s.execMutation(ctx, tx, `
		DELETE FROM pinned_messages
		WHERE session_id IN (
			SELECT id FROM sessions WHERE machine = ?
		)`, s.machine); err != nil {
		return fmt.Errorf("clearing duckdb pinned_messages: %w", err)
	}
	for _, sess := range sessions {
		if err := s.replacePinnedMessages(ctx, tx, sess.ID); err != nil {
			return err
		}
	}
	return nil
}

func (s *Sync) replaceScopedPinnedMessages(
	ctx context.Context, tx *sql.Tx, sessions []db.Session,
) error {
	for _, sess := range sessions {
		if err := s.execMutation(ctx, tx,
			`DELETE FROM pinned_messages WHERE session_id = ?`, sess.ID,
		); err != nil {
			return fmt.Errorf("clearing duckdb pinned_messages for %s: %w", sess.ID, err)
		}
		if err := s.replacePinnedMessages(ctx, tx, sess.ID); err != nil {
			return err
		}
	}
	return nil
}

func nilString(value *string) any {
	if value == nil || *value == "" {
		return nil
	}
	return *value
}

func nilEmpty(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func nilZero(value int) any {
	if value == 0 {
		return nil
	}
	return value
}

func nilTime(value *string) any {
	if value == nil || *value == "" {
		return nil
	}
	return timeValue(*value)
}

func timeValue(value string) any {
	if value == "" {
		return nil
	}
	if t, ok := parseTimestamp(value); ok {
		return t
	}
	return value
}

func parseTimestamp(value string) (time.Time, bool) {
	candidates := []string{
		time.RFC3339Nano,
		"2006-01-02T15:04:05.000Z",
		"2006-01-02 15:04:05",
	}
	value = strings.TrimSpace(value)
	for _, layout := range candidates {
		t, err := time.Parse(layout, value)
		if err == nil {
			return t.UTC(), true
		}
	}
	return time.Time{}, false
}
