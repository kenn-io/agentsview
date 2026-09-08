package main

import (
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"strings"
	"time"
)

// OpenCode dff8fbc149fb core/src/session/sql.ts. The v1 tables coexist with
// projections. See session-format-sources.md for the producer and updater.
const openCodeV2Schema = `
CREATE TABLE session_message (
 id TEXT PRIMARY KEY, session_id TEXT NOT NULL REFERENCES session(id) ON DELETE CASCADE,
 type TEXT NOT NULL, seq INTEGER NOT NULL, time_created INTEGER NOT NULL,
 time_updated INTEGER NOT NULL, data TEXT NOT NULL
);
CREATE UNIQUE INDEX session_message_session_seq_idx ON session_message(session_id, seq);
CREATE INDEX session_message_session_type_seq_idx ON session_message(session_id, type, seq);
CREATE INDEX session_message_session_time_created_id_idx ON session_message(session_id, time_created, id);
CREATE INDEX session_message_time_created_idx ON session_message(time_created);
`

// Simulate the persisted states produced by Prompted, Step.Started, Text.Ended,
// and Step.Ended. Updates replace data while preserving the initial event seq.
func (s *source) writeSQLiteV2Turns(tx *sql.Tx, n, contentBytes int) error {
	for j := range n {
		turn := s.Turns + j
		stamp := s.Start.Add(time.Duration(turn) * time.Minute).UnixMilli()
		text := fmt.Sprintf("Investigate query latency in module %d. ", turn) + strings.Repeat("sample code and context ", contentBytes/24)
		for roleIndex, role := range []string{"user", "assistant"} {
			id := fmt.Sprintf("msg_%s_%08d_%d", s.ID, turn, roleIndex)
			data := map[string]any{"time": map[string]int64{"created": stamp + int64(roleIndex)}}
			if role == "user" {
				data["text"], data["files"], data["agents"] = text, []any{}, []any{}
			} else {
				data["agent"] = "build"
				data["model"] = map[string]string{"id": "gpt-5.4", "providerID": "openai"}
				data["content"] = []any{}
			}
			encoded, err := json.Marshal(data)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(`INSERT INTO session_message (id, session_id, type, seq, time_created, time_updated, data) VALUES (?, ?, ?, ?, ?, ?, ?)`,
				id, s.ID, role, turn*10+roleIndex, stamp+int64(roleIndex), stamp+int64(roleIndex), string(encoded)); err != nil {
				return err
			}
			if role == "assistant" {
				data["content"] = []map[string]string{{"type": "text", "id": "txt_" + id, "text": "Implemented and checked the query. " + text}}
				data["time"] = map[string]int64{"created": stamp + 1, "completed": stamp + 2}
				data["finish"], data["cost"] = "stop", 0
				data["tokens"] = map[string]any{"input": 1000 + turn, "output": 200, "reasoning": 0, "cache": map[string]int{"read": 100, "write": 0}}
				encoded, err = json.Marshal(data)
				if err != nil {
					return err
				}
				if _, err := tx.Exec(`UPDATE session_message SET data = ?, time_updated = ? WHERE id = ?`, string(encoded), stamp+2, id); err != nil {
					return err
				}
			}
		}
		if _, err := tx.Exec(`UPDATE session SET time_updated = ? WHERE id = ?`, stamp+2, s.ID); err != nil {
			return err
		}
	}
	return nil
}
