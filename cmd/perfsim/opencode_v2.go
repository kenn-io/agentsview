package main

import (
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"strings"
	"time"
)

// Provider-consumed tables captured from OpenCode beta 19381.
// See session-format-sources.md for provenance.
const openCodeV2Schema = `
CREATE TABLE project (
 id text PRIMARY KEY,
 worktree text NOT NULL,
 vcs text,
 name text,
 icon_url text,
 icon_url_override text,
 icon_color text,
 time_created integer NOT NULL,
 time_updated integer NOT NULL,
 time_initialized integer,
 sandboxes text NOT NULL,
 commands text
);
CREATE TABLE session_v2 (
 id text PRIMARY KEY,
 project_id text NOT NULL,
 workspace_id text,
 parent_id text,
 fork_session_id text,
 fork_boundary text,
 slug text NOT NULL,
 directory text NOT NULL,
 path text,
 title text,
 version text NOT NULL,
 share_url text,
 summary_additions integer,
 summary_deletions integer,
 summary_files integer,
 summary_diffs text,
 metadata text,
 cost real DEFAULT 0 NOT NULL,
 tokens_input integer DEFAULT 0 NOT NULL,
 tokens_output integer DEFAULT 0 NOT NULL,
 tokens_reasoning integer DEFAULT 0 NOT NULL,
 tokens_cache_read integer DEFAULT 0 NOT NULL,
 tokens_cache_write integer DEFAULT 0 NOT NULL,
 revert text,
 permission text,
 agent text,
 model text,
 time_created integer NOT NULL,
 time_updated integer NOT NULL,
 time_idle integer,
 time_viewed integer,
 idle_outcome text,
 time_compacting integer,
 time_archived integer,
 time_suspended integer,
 resume_attempts integer DEFAULT 0 NOT NULL,
 CONSTRAINT fk_session_v2_project_id_project_id_fk FOREIGN KEY (project_id) REFERENCES project(id) ON DELETE CASCADE
);
CREATE INDEX session_v2_project_idx ON session_v2 (project_id);
CREATE INDEX session_v2_workspace_idx ON session_v2 (workspace_id);
CREATE INDEX session_v2_parent_idx ON session_v2 (parent_id);
CREATE INDEX session_v2_time_suspended_idx ON session_v2 (time_suspended) WHERE "session_v2"."time_suspended" is not null;
CREATE TABLE session_message (
 id text PRIMARY KEY,
 session_id text NOT NULL,
 type text NOT NULL,
 seq integer NOT NULL,
 time_created integer NOT NULL,
 time_updated integer NOT NULL,
 data text NOT NULL,
 CONSTRAINT fk_session_message_session_id_session_v2_id_fk FOREIGN KEY (session_id) REFERENCES session_v2(id) ON DELETE CASCADE
);
CREATE UNIQUE INDEX session_message_session_seq_idx ON session_message (session_id,seq);
CREATE INDEX session_message_session_type_seq_idx ON session_message (session_id,type,seq);
CREATE INDEX session_message_session_time_created_id_idx ON session_message (session_id,time_created,id);
CREATE INDEX session_message_time_created_idx ON session_message (time_created);
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
		if _, err := tx.Exec(`UPDATE session_v2 SET time_updated = ? WHERE id = ?`, stamp+2, s.ID); err != nil {
			return err
		}
	}
	return nil
}
