package sync

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"strings"

	_ "github.com/mattn/go-sqlite3"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/secrets"
)

// codexStagingSink implements parser.CodexSessionSink for the streaming
// full-parse path: messages and tool-call metadata stay in memory (they are
// small relative to tool outputs), while every tool-result event row and
// the per-call agent summary state are written to a scratch SQLite file as
// they arrive. The in-memory model therefore never holds result-event
// content — events carry a unique placeholder — and peak memory is
// O(messages + batch), not O(file size). The scratch database is also the
// publish source: the staged write inserts tool_result_events straight
// from it and resolves result_content summaries per call with transient
// memory bounded by one call's distinct agents.
type codexStagingSink struct {
	*parser.CodexCollectingSink

	scratch *sql.DB
	path    string

	// blocked marks categories whose stored content is blanked; staged
	// rows keep the raw content plus a blank flag so the publish mirrors
	// the legacy write exactly.
	blocked map[string]bool

	// categoryByCall tracks each emitted call's category.
	categoryByCall map[string]string

	// findings collects definite findings from result-event content as
	// it streams by; findingPos records the coordinates to patch after
	// ordinal finalization.
	findings    []db.SecretFinding
	findingPos  []stagedFindingPos
	eventByCall map[string]int64
	eventSeq    int64
}

type stagedFindingPos struct {
	toolUseID  string
	eventIndex int
}

const codexStagingSchema = `
CREATE TABLE stage_events (
    seq INTEGER PRIMARY KEY,
    tool_use_id TEXT NOT NULL,
    agent_id TEXT NOT NULL DEFAULT '',
    subagent_session_id TEXT NOT NULL DEFAULT '',
    source TEXT NOT NULL,
    status TEXT NOT NULL,
    content TEXT NOT NULL,
    content_length INTEGER NOT NULL,
    timestamp TEXT NOT NULL DEFAULT '',
    blanked INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX idx_stage_events_call ON stage_events(tool_use_id, seq);
CREATE TABLE stage_agent_summary (
    tool_use_id TEXT NOT NULL,
    agent_id TEXT NOT NULL,
    content TEXT NOT NULL,
    content_length INTEGER NOT NULL,
    first_seq INTEGER NOT NULL,
    PRIMARY KEY (tool_use_id, agent_id)
);`

// newCodexStagingSink opens a scratch SQLite database for one streaming
// parse. The caller must Close it once the staged write has published.
func newCodexStagingSink(
	blocked map[string]bool,
) (*codexStagingSink, error) {
	f, err := os.CreateTemp("", "agentsview-codex-stage-*.sqlite")
	if err != nil {
		return nil, fmt.Errorf("creating codex staging file: %w", err)
	}
	path := f.Name()
	if err := f.Close(); err != nil {
		os.Remove(path)
		return nil, err
	}
	scratch, err := sql.Open("sqlite3", path)
	if err != nil {
		os.Remove(path)
		return nil, fmt.Errorf("opening codex staging db: %w", err)
	}
	for _, pragma := range []string{
		"PRAGMA journal_mode=OFF",
		"PRAGMA synchronous=OFF",
		"PRAGMA temp_store=MEMORY",
	} {
		if _, err := scratch.Exec(pragma); err != nil {
			scratch.Close()
			os.Remove(path)
			return nil, fmt.Errorf("configuring codex staging db: %w", err)
		}
	}
	if _, err := scratch.Exec(codexStagingSchema); err != nil {
		scratch.Close()
		os.Remove(path)
		return nil, fmt.Errorf("creating codex staging schema: %w", err)
	}
	return &codexStagingSink{
		CodexCollectingSink: parser.NewCodexCollectingSink(0),
		scratch:             scratch,
		path:                path,
		blocked:             blocked,
		categoryByCall:      make(map[string]string),
		eventByCall:         make(map[string]int64),
	}, nil
}

// Close releases the scratch database and removes its file.
func (s *codexStagingSink) Close() error {
	err := s.scratch.Close()
	if err == nil {
		err = os.Remove(s.path)
	}
	return err
}

// Path returns the staging file path for ATTACH-based publishing.
func (s *codexStagingSink) Path() string {
	return s.path
}

func (s *codexStagingSink) AppendMessage(m parser.ParsedMessage) {
	for _, tc := range m.ToolCalls {
		if tc.ToolUseID != "" {
			s.categoryByCall[tc.ToolUseID] = tc.Category
		}
	}
	s.CodexCollectingSink.AppendMessage(m)
}

// AppendToolResultEvent stages the full event row and the per-call summary
// state, then records a contentless placeholder in the in-memory model so
// downstream conversions stay shape-compatible without retaining content.
func (s *codexStagingSink) AppendToolResultEvent(
	callID string, ev parser.ParsedToolResultEvent,
) {
	if callID == "" {
		return
	}
	// The parser extracts event fields as gjson substrings of the source
	// line. Storing those small strings in the in-memory model (event
	// identity fields, map keys below) would pin the entire line's backing
	// buffer — for large tool outputs that keeps the whole transcript's
	// line bytes reachable across the parse. Clone the fields the model
	// keeps; content is replaced by a placeholder after staging.
	callID = strings.Clone(callID)
	ev.ToolUseID = strings.Clone(ev.ToolUseID)
	ev.AgentID = strings.Clone(ev.AgentID)
	ev.SubagentSessionID = strings.Clone(ev.SubagentSessionID)
	ev.Status = strings.Clone(ev.Status)
	ev.Source = strings.Clone(ev.Source)
	// The legacy deduplication compares raw parser content; the staged
	// rows keep the raw content plus a blank flag, so the equivalence
	// check matches even for blocked categories.
	var exists int
	err := s.scratch.QueryRow(
		`SELECT 1 FROM stage_events
		 WHERE tool_use_id = ? AND agent_id = ? AND status = ?
		   AND content = ? LIMIT 1`,
		callID, ev.AgentID, ev.Status, ev.Content,
	).Scan(&exists)
	if err == nil {
		return // equivalent event already staged
	}
	if err != sql.ErrNoRows {
		// A query failure must not drop transcript content: fall back to
		// keeping the content in memory for this event.
		s.CodexCollectingSink.AppendToolResultEvent(callID, ev)
		return
	}

	s.eventSeq++
	seq := s.eventSeq
	subagent := ev.SubagentSessionID
	if subagent == "" && strings.TrimSpace(ev.AgentID) != "" {
		agentID := strings.TrimSpace(ev.AgentID)
		if strings.HasPrefix(agentID, "codex:") {
			subagent = agentID
		} else {
			subagent = "codex:" + agentID
		}
	}
	blanked := 0
	if s.blocked[s.categoryByCall[callID]] {
		blanked = 1
	}
	contentLength := len(ev.Content)
	if _, err := s.scratch.Exec(
		`INSERT INTO stage_events (
		     seq, tool_use_id, agent_id, subagent_session_id,
		     source, status, content, content_length,
		     timestamp, blanked
		 ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		seq, callID, ev.AgentID, subagent, ev.Source, ev.Status,
		ev.Content, contentLength, ev.Timestamp, blanked,
	); err != nil {
		s.CodexCollectingSink.AppendToolResultEvent(callID, ev)
		return
	}

	// Per-call summary state: latest raw content per agent, keeping the
	// first-write order the legacy summary walks.
	if strings.TrimSpace(ev.Content) != "" {
		if _, err := s.scratch.Exec(
			`INSERT INTO stage_agent_summary (
			     tool_use_id, agent_id, content, content_length, first_seq
			 ) VALUES (?, ?, ?, ?, ?)
			 ON CONFLICT(tool_use_id, agent_id) DO UPDATE SET
			     content = excluded.content,
			     content_length = excluded.content_length`,
			callID, strings.TrimSpace(ev.AgentID), ev.Content,
			contentLength, seq,
		); err != nil {
			log.Printf("codex staging summary upsert %s: %v", callID, err)
			s.CodexCollectingSink.AppendToolResultEvent(callID, ev)
			return
		}
	}

	// Definite findings from the stored (blanked) content — the same
	// text the legacy scan reads back from the database.
	storedContent := ev.Content
	if blanked != 0 {
		storedContent = ""
	}
	eventIndex := int(s.eventByCall[callID])
	s.eventByCall[callID]++
	s.addEventFindings(callID, eventIndex, storedContent)

	// The in-memory model keeps a unique placeholder instead of the
	// content: downstream conversions stay shape-compatible and the
	// collecting dedup treats every staged event as distinct.
	ev.Content = fmt.Sprintf("staged:%d", seq)
	s.CodexCollectingSink.AppendToolResultEvent(callID, ev)
}

func (s *codexStagingSink) addEventFindings(
	toolUseID string, eventIndex int, content string,
) {
	if content == "" {
		return
	}
	matches := secrets.ScanDefinite(content)
	for _, match := range matches {
		s.findings = append(s.findings, db.SecretFinding{
			RuleName:      match.Rule,
			Confidence:    match.Confidence,
			LocationKind:  "tool_result_event",
			MatchStart:    match.Start,
			MatchEnd:      match.End,
			MatchIndex:    match.Index,
			RedactedMatch: match.Redacted,
			RulesVersion:  secrets.DefiniteRulesVersion(),
		})
		s.findingPos = append(s.findingPos, stagedFindingPos{
			toolUseID:  toolUseID,
			eventIndex: eventIndex,
		})
	}
}

// Findings returns the staged event findings with session, ordinal, and
// call coordinates stamped from the final message model.
func (s *codexStagingSink) Findings(
	sessionID string,
	positions map[string]db.StagedToolCallPosition,
) []db.SecretFinding {
	out := make([]db.SecretFinding, len(s.findings))
	for i, f := range s.findings {
		f.SessionID = sessionID
		pos, ok := positions[s.findingPos[i].toolUseID]
		if ok {
			f.MessageOrdinal = pos.Ordinal
			callIdx := pos.CallIndex
			evIdx := s.findingPos[i].eventIndex
			f.CallIndex = &callIdx
			f.EventIndex = &evIdx
		}
		out[i] = f
	}
	return out
}

// InsertEventsTx inserts the staged result events into tool_result_events
// within the caller's publish transaction, ordered by emission so
// event_index matches the legacy slice order.
func (s *codexStagingSink) InsertEventsTx(
	ctx context.Context, tx *sql.Tx, sessionID string,
	messageOrdinals map[string]db.StagedToolCallPosition,
) error {
	// The publish transaction runs on the main archive connection; attach
	// the scratch database for the copy. The transaction only ever
	// modifies main, so the cross-database crash-atomicity limit for
	// WAL-mode attached databases is respected.
	if _, err := tx.ExecContext(ctx,
		"ATTACH DATABASE ? AS codex_staging", s.path,
	); err != nil {
		return fmt.Errorf("attaching codex staging db: %w", err)
	}
	defer func() {
		_, _ = tx.ExecContext(ctx, "DETACH DATABASE codex_staging")
	}()
	for toolUseID, pos := range messageOrdinals {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO tool_result_events (
				session_id, tool_call_message_ordinal, call_index,
				tool_use_id, agent_id, subagent_session_id,
				source, status, content, content_length,
				timestamp, event_index
			)
			SELECT ?, ?, ?, tool_use_id,
			       CASE WHEN agent_id = '' THEN NULL ELSE agent_id END,
			       CASE WHEN subagent_session_id = ''
			            THEN NULL ELSE subagent_session_id END,
			       source, status,
			       CASE WHEN blanked = 1 THEN '' ELSE content END,
			       content_length,
			       CASE WHEN timestamp = '' THEN NULL ELSE timestamp END,
			       row_number() OVER (ORDER BY seq) - 1
			FROM codex_staging.stage_events
			WHERE tool_use_id = ?
			ORDER BY seq`,
			sessionID, pos.Ordinal, pos.CallIndex, pos.ToolUseID,
			toolUseID,
		); err != nil {
			return fmt.Errorf(
				"publishing staged events for %s/%s: %w",
				sessionID, toolUseID, err,
			)
		}
	}
	return nil
}

// ResolveSummary computes the stored result summary for one call from the
// staged per-agent state, mirroring db.SummarizeToolResultEvents: the
// latest raw content per agent in first-write order, followed by the
// trailing anonymous content. Memory is transient and bounded by the
// call's distinct agents.
func (s *codexStagingSink) ResolveSummary(
	ctx context.Context, toolUseID string,
) (summary string, contentLength int, err error) {
	rows, err := s.scratch.QueryContext(ctx, `
		SELECT agent_id, content, content_length
		FROM stage_agent_summary
		WHERE tool_use_id = ?
		ORDER BY first_seq`,
		toolUseID,
	)
	if err != nil {
		return "", 0, err
	}
	defer rows.Close()
	var parts []string
	var lastAnon string
	for rows.Next() {
		var agentID, content string
		var length int
		if err := rows.Scan(&agentID, &content, &length); err != nil {
			return "", 0, err
		}
		if agentID == "" {
			lastAnon = content
			continue
		}
		parts = append(parts, agentID+":\n"+content)
	}
	if err := rows.Err(); err != nil {
		return "", 0, err
	}
	switch {
	case len(parts) == 0:
		summary = lastAnon
	case len(parts) == 1:
		summary = parts[0][strings.IndexByte(parts[0], '\n')+1:]
		if lastAnon != "" {
			summary += "\n\n" + lastAnon
		}
	default:
		summary = strings.Join(parts, "\n\n")
		if lastAnon != "" {
			summary += "\n\n" + lastAnon
		}
	}
	return summary, len(summary), nil
}
