package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/rawparity"
)

// LoadRawParitySnapshots loads complete physical content, including trashed rows,
// without served-view filters or row caps. Only field-matrix columns and row
// coordinates are read; curation, payload blobs and derived signals stay out.
// Missing IDs are omitted and repeated IDs are loaded once. Relationship IDs
// remain literal for the caller's provider-aware match map.
//
// The caller must validate its hosted runtime pool with CheckHostedTenant, begin
// one REPEATABLE READ READ ONLY transaction, and reuse it for all report batches
// and readiness reads. This function never commits or opens another connection.
func LoadRawParitySnapshots(ctx context.Context, tx *sql.Tx, ids []string) (map[string]rawparity.Snapshot, error) {
	if err := checkRawParityTransaction(ctx, tx); err != nil {
		return nil, err
	}
	out := make(map[string]rawparity.Snapshot, len(ids))
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		s, err := loadRawParitySnapshot(ctx, tx, id)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("loading physical parity rows: %w", err)
		}
		out[id] = s
	}
	return out, nil
}

func checkRawParityTransaction(ctx context.Context, tx *sql.Tx) error {
	if tx == nil {
		return errors.New("raw parity requires a REPEATABLE READ READ ONLY transaction")
	}
	var isolation string
	var readOnly bool
	if err := tx.QueryRowContext(ctx, `SELECT current_setting('transaction_isolation'), current_setting('transaction_read_only')='on'`).Scan(&isolation, &readOnly); err != nil {
		return err
	}
	if isolation != "repeatable read" || !readOnly {
		return errors.New("raw parity requires a REPEATABLE READ READ ONLY transaction")
	}
	return nil
}

func loadRawParitySnapshot(ctx context.Context, tx *sql.Tx, id string) (rawparity.Snapshot, error) {
	var s rawparity.Snapshot
	var start, end *time.Time
	err := tx.QueryRowContext(ctx, `SELECT id,agent,project,cwd,first_message,started_at,ended_at,message_count,user_message_count,parent_session_id,relationship_type
	 FROM sessions WHERE id=$1`, id).Scan(&s.Session.ID, &s.Session.Agent, &s.Session.Project, &s.Session.Cwd,
		&s.Session.FirstMessage, &start, &end, &s.Session.MessageCount, &s.Session.UserMessageCount, &s.Session.ParentSessionID, &s.Session.RelationshipType)
	if err != nil {
		return s, err
	}
	if start != nil {
		s.Session.StartedAt = new(FormatISO8601(*start))
	}
	if end != nil {
		s.Session.EndedAt = new(FormatISO8601(*end))
	}
	err = rawParityRows(ctx, tx, id, `SELECT `+pgMessageCols+` FROM messages WHERE session_id=$1 ORDER BY ordinal`, func(rows *sql.Rows) error {
		var m db.Message
		var timestamp *time.Time
		var usage string
		if err := rows.Scan(&m.SessionID, &m.Ordinal, &m.Role, &m.Content, &m.ThinkingText, &timestamp,
			&m.HasThinking, &m.HasToolUse, &m.ContentLength, &m.IsSystem, &m.Model, &m.ReasoningEffort, &usage,
			&m.ContextTokens, &m.OutputTokens, &m.ProviderID, &m.HasContextTokens, &m.HasOutputTokens,
			&m.ClaudeMessageID, &m.ClaudeRequestID, &m.SourceType, &m.SourceSubtype, &m.PromptSource,
			&m.SourceUUID, &m.SourceParentUUID, &m.IsSidechain, &m.IsCompactBoundary); err != nil {
			return err
		}
		if timestamp != nil {
			m.Timestamp = FormatISO8601(*timestamp)
		}
		// Public readers discard malformed usage JSON for safe response encoding.
		// Parity compares stored bytes, including distinct malformed values.
		if usage != "" {
			m.TokenUsage = []byte(usage)
		}
		s.Messages = append(s.Messages, m)
		return nil
	})
	if err != nil {
		return s, err
	}
	if err = loadRawParityTools(ctx, tx, id, s.Messages); err != nil {
		return s, err
	}
	err = loadRawParityFindingsAndUsage(ctx, tx, id, &s)
	return s, err
}

func loadRawParityTools(ctx context.Context, tx *sql.Tx, id string, messages []db.Message) error {
	byOrdinal := make(map[int]int, len(messages))
	for i, m := range messages {
		byOrdinal[m.Ordinal] = i
	}
	err := rawParityRows(ctx, tx, id, `SELECT message_ordinal,call_index,tool_name,category,COALESCE(tool_use_id,''),
	 COALESCE(input_json,''),COALESCE(skill_name,''),COALESCE(file_path,''),COALESCE(result_content_length,0),
	 COALESCE(result_content,''),COALESCE(subagent_session_id,'') FROM tool_calls WHERE session_id=$1 ORDER BY message_ordinal,call_index`, func(rows *sql.Rows) error {
		var ordinal int
		var c db.ToolCall
		if err := rows.Scan(&ordinal, &c.CallIndex, &c.ToolName, &c.Category, &c.ToolUseID, &c.InputJSON,
			&c.SkillName, &c.FilePath, &c.ResultContentLength, &c.ResultContent, &c.SubagentSessionID); err != nil {
			return err
		}
		i, ok := byOrdinal[ordinal]
		if !ok {
			return errors.New("parity tool call has no owning message")
		}
		messages[i].ToolCalls = append(messages[i].ToolCalls, c)
		return nil
	})
	if err != nil {
		return err
	}
	byCall := make(map[[2]int]*db.ToolCall)
	for _, m := range messages {
		for i := range m.ToolCalls {
			byCall[[2]int{m.Ordinal, m.ToolCalls[i].CallIndex}] = &m.ToolCalls[i]
		}
	}
	err = rawParityRows(ctx, tx, id, `SELECT tool_call_message_ordinal,call_index,event_index,COALESCE(tool_use_id,''),
	 COALESCE(agent_id,''),COALESCE(subagent_session_id,''),source,status,content,content_length,timestamp
	 FROM tool_result_events WHERE session_id=$1 ORDER BY tool_call_message_ordinal,call_index,event_index`, func(rows *sql.Rows) error {
		var ordinal, call int
		var e db.ToolResultEvent
		var timestamp *time.Time
		if err := rows.Scan(&ordinal, &call, &e.EventIndex, &e.ToolUseID, &e.AgentID, &e.SubagentSessionID,
			&e.Source, &e.Status, &e.Content, &e.ContentLength, &timestamp); err != nil {
			return err
		}
		c := byCall[[2]int{ordinal, call}]
		if c == nil {
			return errors.New("parity tool result has no owning call")
		}
		if timestamp != nil {
			e.Timestamp = FormatISO8601(*timestamp)
		}
		c.ResultEvents = append(c.ResultEvents, e)
		return nil
	})
	if err == nil {
		// Push and raw projection elide a summary equal to its single event.
		db.RestoreMessageResultContent(messages)
	}
	return err
}

func loadRawParityFindingsAndUsage(ctx context.Context, tx *sql.Tx, id string, s *rawparity.Snapshot) error {
	loads := []struct {
		query string
		scan  func(*sql.Rows) error
	}{
		{`SELECT message_ordinal,source,model,provider_id,input_tokens,output_tokens,cache_creation_input_tokens,
		 cache_read_input_tokens,reasoning_tokens,cost_microdollars,cost_status,cost_source,occurred_at,dedup_key
		 FROM usage_events WHERE session_id=$1 ORDER BY id`, func(rows *sql.Rows) error {
			var e db.UsageEvent
			var timestamp *time.Time
			if err := rows.Scan(&e.MessageOrdinal, &e.Source, &e.Model, &e.ProviderID, &e.InputTokens, &e.OutputTokens,
				&e.CacheCreationInputTokens, &e.CacheReadInputTokens, &e.ReasoningTokens, &e.Cost, &e.CostStatus, &e.CostSource,
				&timestamp, &e.DedupKey); err != nil {
				return err
			}
			if timestamp != nil {
				e.OccurredAt = FormatISO8601(*timestamp)
			}
			s.Usage = append(s.Usage, e)
			return nil
		}},
		{`SELECT rule_name,confidence,location_kind,message_ordinal,call_index,event_index,match_start,match_end,match_index,redacted_match
		 FROM secret_findings WHERE session_id=$1 ORDER BY id`, func(rows *sql.Rows) error {
			var f db.SecretFinding
			if err := rows.Scan(&f.RuleName, &f.Confidence, &f.LocationKind, &f.MessageOrdinal, &f.CallIndex, &f.EventIndex,
				&f.MatchStart, &f.MatchEnd, &f.MatchIndex, &f.RedactedMatch); err != nil {
				return err
			}
			s.Secrets = append(s.Secrets, f)
			return nil
		}},
		{`SELECT kind,detector,message_ordinal,call_index,tool_name,label,text,evidence,occurred_at,seq
		 FROM friction_findings WHERE session_id=$1 ORDER BY seq,id`, func(rows *sql.Rows) error {
			var f db.FrictionFinding
			if err := rows.Scan(&f.Kind, &f.Detector, &f.MessageOrdinal, &f.CallIndex, &f.ToolName, &f.Label, &f.Text,
				&f.Evidence, &f.OccurredAt, &f.Seq); err != nil {
				return err
			}
			if f.OccurredAt != nil {
				f.OccurredAt = new(f.OccurredAt.UTC())
			}
			s.Friction = append(s.Friction, f)
			return nil
		}},
	}
	for _, load := range loads {
		if err := rawParityRows(ctx, tx, id, load.query, load.scan); err != nil {
			return err
		}
	}
	return nil
}

func rawParityRows(ctx context.Context, tx *sql.Tx, id, query string, scan func(*sql.Rows) error) error {
	rows, err := tx.QueryContext(ctx, query, id)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := scan(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}
