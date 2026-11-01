package db

import (
	"context"
	"database/sql"

	"go.kenn.io/agentsview/internal/commitlinks"
)

// trailerTurnTx derives context from the same bounded revision as the stored
// identity. No later lookup reattaches prompts by ordinal.
func trailerTurnTx(ctx context.Context, tx *sql.Tx, s commitSessionInput, at string, paths []string) (*CommitTurn, error) {
	var bytes int64
	var count int
	err := tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT SUM(length(CAST(content AS BLOB))+length(CAST(COALESCE(dialogue_text,'') AS BLOB))) FROM messages WHERE session_id=?),0)+COALESCE((SELECT SUM(length(CAST(COALESCE(input_json,'') AS BLOB))) FROM tool_calls WHERE session_id=?),0),(SELECT COUNT(*) FROM messages WHERE session_id=?)+(SELECT COUNT(*) FROM tool_calls WHERE session_id=?)`, s.id, s.id, s.id, s.id).Scan(&bytes, &count)
	if err != nil {
		return nil, err
	}
	if bytes > maxCommitSessionBytes || count > 30000 {
		return nil, nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT ordinal,COALESCE(timestamp,''),role,content,COALESCE(dialogue_text,content),is_system,COALESCE(source_type,''),COALESCE(source_subtype,'') FROM messages WHERE session_id=? ORDER BY ordinal`, s.id)
	if err != nil {
		return nil, err
	}
	entries := []commitlinks.TurnEntry{}
	byOrdinal := map[int]int{}
	for rows.Next() {
		var m Message
		var text string
		if err := rows.Scan(&m.Ordinal, &m.Timestamp, &m.Role, &m.Content, &text, &m.IsSystem, &m.SourceType, &m.SourceSubtype); err != nil {
			rows.Close()
			return nil, err
		}
		m.DialogueText = &text
		e := commitlinks.TurnEntry{Ordinal: m.Ordinal, At: m.Timestamp}
		if realCommitPrompt(m) {
			e.Prompt = text
		}
		byOrdinal[m.Ordinal] = len(entries)
		entries = append(entries, e)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	rows, err = tx.QueryContext(ctx, `SELECT m.ordinal,COALESCE(t.input_json,'') FROM tool_calls t JOIN messages m ON m.id=t.message_id WHERE t.session_id=? ORDER BY m.ordinal,t.call_index`, s.id)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var ordinal int
		var input string
		if err := rows.Scan(&ordinal, &input); err != nil {
			rows.Close()
			return nil, err
		}
		if i, ok := byOrdinal[ordinal]; ok && len(input) <= commitlinks.MaxCommandBytes {
			entries[i].Calls = append(entries[i].Calls, input)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	turn := commitlinks.SelectTurn(entries, at, paths)
	if turn == nil {
		return nil, nil
	}
	return &CommitTurn{Ordinal: turn.Ordinal, At: turn.At, Prompt: turn.Prompt, Why: turn.Why}, nil
}

// StoreCommitTrailerLinks accepts bounded changed paths only for the writing-turn
// heuristic. Paths are never persisted in the wire card or used as identities.
func (d *DB) StoreCommitTrailerLinks(ctx context.Context, links []CommitLink, paths []string, complete ...bool) error {
	ok := true
	if len(complete) > 0 {
		ok = complete[0]
	}
	return d.storeCommitLinks(ctx, links, paths, ok, true)
}

// StoreCommitTrailerFacts publishes cheap facts before the scan cursor advances.
// Turn derivation is durable pending session work, independent of Git pagination.
func (d *DB) StoreCommitTrailerFacts(ctx context.Context, links []CommitLink, paths []string, complete bool) error {
	return d.storeCommitLinks(ctx, links, paths, complete, false)
}
