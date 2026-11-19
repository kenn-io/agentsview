package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"

	"go.kenn.io/agentsview/internal/parser"
)

// CodexForkUsage retains uncounted token metadata until replay can be resolved.
type CodexForkUsage struct {
	TokenUsage       jsontext.Value
	ContextTokens    int
	OutputTokens     int
	HasContextTokens bool
	HasOutputTokens  bool
}

// CodexForkMessage pairs a stored ordinal with source replay evidence.
// An empty origin is retained history without current source bytes.
type CodexForkMessage struct {
	Digest       [32]byte
	PendingUsage *CodexForkUsage
	Origin       parser.CodexMessageOrigin
	Provisional  bool
	TurnContext  int
	// Observed summary contributors in each call's archived event order.
	// This local evidence does not replace nullable raw event metadata.
	ResultSummaryParticipants [][]CodexForkSummaryParticipant `json:",omitempty"`
}

// CodexForkSummaryParticipant binds observed participation to an archived event.
// An index alone can refer to a different event after an artifact replacement.
type CodexForkSummaryParticipant struct {
	EventIndex    int
	AgentID       string
	Source        string
	Status        string
	Timestamp     string
	ContentLength int
}

// CodexForkTurnContext shares source prefixes across retained snapshots.
// Parent is a one-based index of an earlier context; zero starts a source.
type CodexForkTurnContext struct {
	Parent int
	TurnID string
}

// CodexForkState is local archive provenance, independent of optional parser
// checkpoints. ParentKnown also distinguishes a resolved, turnless parent.
type CodexForkState struct {
	ParentID        string
	ParentKnown     bool
	PreserveHistory bool
	ParentTurns     map[string]struct{}
	Messages        []CodexForkMessage
	TurnContexts    []CodexForkTurnContext
}

func (db *DB) GetCodexForkState(ctx context.Context, sessionID string) (*CodexForkState, error) {
	// The rebuild worker reads the original archive without migrating it.
	var exists bool
	if err := db.getReader().QueryRow(ctx, `SELECT EXISTS (
 SELECT 1 FROM sqlite_master WHERE type='table' AND name='codex_fork_state')`).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, nil
	}
	var encoded string
	err := db.getReader().QueryRow(ctx, `SELECT state_json FROM codex_fork_state WHERE session_id = ?`, sessionID).Scan(&encoded)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var state CodexForkState
	if err := json.Unmarshal([]byte(encoded), &state); err != nil {
		return nil, err
	}
	return &state, nil
}

func (db *DB) HasCodexForkSource(ctx context.Context, agent, path string) (bool, error) {
	var found bool
	err := db.getReader().QueryRow(ctx, `SELECT EXISTS (
	 SELECT 1 FROM sessions s JOIN codex_fork_state f ON f.session_id = s.id
	 WHERE s.agent = ? AND s.file_path = ?)`, agent, path).Scan(&found)
	return found, err
}

// HasCodexForkToolHistory identifies retained calls whose result payloads are
// not bounded by the size of the current source file.
func (db *DB) HasCodexForkToolHistory(ctx context.Context, agent, path string) (bool, error) {
	// Rebuilds read the original archive before applying schema migrations.
	var exists bool
	if err := db.getReader().QueryRowContext(ctx, `SELECT EXISTS (
	 SELECT 1 FROM sqlite_master WHERE type='table' AND name='codex_fork_state')`).Scan(&exists); err != nil || !exists {
		return false, err
	}
	var found bool
	err := db.getReader().QueryRow(ctx, `SELECT EXISTS (
	 SELECT 1 FROM sessions s JOIN codex_fork_state f ON f.session_id = s.id
	 JOIN messages m ON m.session_id = s.id JOIN tool_calls t ON t.message_id = m.id
	 WHERE s.agent = ? AND s.file_path = ?)`, agent, path).Scan(&found)
	return found, err
}

// GetCodexForkMessageMetadata omits result bodies. Staged fork reconciliation
// reads archived results one message at a time and spools them back to scratch.
func (db *DB) GetCodexForkMessageMetadata(ctx context.Context, sessionID string) ([]Message, error) {
	rows, err := db.getReader().QueryContext(ctx, `SELECT `+selectMessageCols+`
 FROM messages WHERE session_id=? ORDER BY ordinal`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	messages, err := scanMessages(rows)
	if err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	ids := make([]int64, len(messages))
	positions := make(map[int64]int, len(messages))
	for i, m := range messages {
		ids[i], positions[m.ID] = m.ID, i
	}
	for start := 0; start < len(ids); start += attachToolCallBatchSize {
		if err := attachToolCallsBatch(ctx, db.getReader(), messages, positions,
			ids[start:min(start+attachToolCallBatchSize, len(ids))], false); err != nil {
			return nil, err
		}
	}
	return messages, nil
}

func writeCodexForkStateTx(ctx context.Context, tx *sql.Tx, id string, state *CodexForkState) error {
	if state == nil {
		return nil
	}
	encoded, err := json.Marshal(state)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO codex_fork_state(session_id, state_json) VALUES (?, ?)
	 ON CONFLICT(session_id) DO UPDATE SET state_json = excluded.state_json`, id, string(encoded))
	return err
}

// Artifact replacement changes the result observations even when every event's
// metadata matches. Preserve fork origins and pending usage, but require fresh
// evidence before counting imported results as summary contributors again.
func clearCodexForkSummaryParticipantsTx(ctx context.Context, tx *sql.Tx, id string) error {
	var encoded string
	err := tx.QueryRowContext(ctx, `SELECT state_json FROM codex_fork_state WHERE session_id = ?`, id).Scan(&encoded)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var state CodexForkState
	if err := json.Unmarshal([]byte(encoded), &state); err != nil {
		return err
	}
	changed := false
	for i := range state.Messages {
		changed = changed || len(state.Messages[i].ResultSummaryParticipants) > 0
		state.Messages[i].ResultSummaryParticipants = nil
	}
	if !changed {
		return nil
	}
	return writeCodexForkStateTx(ctx, tx, id, &state)
}

// CodexForkMessageKey identifies storage-normalized message content, excluding usage and results.
func CodexForkMessageKey(m Message) ([32]byte, error) {
	// Legacy archives can lack model, length and rendering metadata. Match
	// visible content and source timestamp, with occurrence queues for repeats.
	// Tool identities include inputs, while hydrated output belongs to history.
	type callKey struct{ Name, ID, Input string }
	key := struct {
		Role, Content, Thinking, Timestamp string
		Calls                              []callKey
	}{Role: m.Role, Content: m.Content, Thinking: m.ThinkingText, Timestamp: m.Timestamp}
	for _, call := range m.ToolCalls {
		key.Calls = append(key.Calls, callKey{call.ToolName, call.ToolUseID, call.InputJSON})
	}
	encoded, err := json.Marshal(key)
	return sha256.Sum256(encoded), err
}

func refreshCopiedCodexForkDigestsTx(ctx context.Context, tx *sql.Tx, tempIDsTable string) error {
	rows, err := tx.QueryContext(ctx, `SELECT f.session_id, f.state_json
 FROM codex_fork_state f JOIN `+tempIDsTable+` copied ON copied.id=f.session_id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	type savedState struct{ id, encoded string }
	var saved []savedState
	for rows.Next() {
		var s savedState
		if err := rows.Scan(&s.id, &s.encoded); err != nil {
			_ = rows.Close()
			return err
		}
		saved = append(saved, s)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, s := range saved {
		var state CodexForkState
		if err := json.Unmarshal([]byte(s.encoded), &state); err != nil {
			return err
		}
		messages, err := allMessagesWithQuerier(ctx, tx, s.id, selectMessageCols)
		if err != nil {
			return err
		}
		for _, m := range messages {
			if m.Ordinal >= len(state.Messages) {
				continue
			}
			state.Messages[m.Ordinal].Digest, err = CodexForkMessageKey(m)
			if err != nil {
				return err
			}
		}
		if err := writeCodexForkStateTx(ctx, tx, s.id, &state); err != nil {
			return err
		}
	}
	return nil
}
