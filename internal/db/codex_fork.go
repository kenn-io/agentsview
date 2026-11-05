package db

import (
	"context"
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
	// RetainedUsage carries normalized historical events only for this write.
	RetainedUsage   []UsageEvent `json:"-"`
	ParentID        string
	ParentKnown     bool
	PreserveHistory bool
	ParentTurns     map[string]struct{}
	Messages        []CodexForkMessage
}

func (db *DB) GetCodexForkState(ctx context.Context, sessionID string) (*CodexForkState, error) {
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
