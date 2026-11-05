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
}

// CodexForkState is local archive provenance, independent of optional parser
// checkpoints. ParentKnown also distinguishes a resolved, turnless parent.
type CodexForkState struct {
	ParentID        string
	ParentKnown     bool
	PreserveHistory bool
	ParentTurns     map[string]struct{}
	Messages        []CodexForkMessage
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
