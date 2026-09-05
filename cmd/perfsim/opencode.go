package main

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	syncengine "go.kenn.io/agentsview/internal/sync"
)

// The four provider-consumed tables and their indexes follow OpenCode's pinned
// core/session/sql.ts and core/project/sql.ts; see session-format-sources.md.
// Do not add watermark indexes absent from the producer: that hides scan costs.
const openCodeSchema = `
CREATE TABLE project (
 id TEXT PRIMARY KEY, worktree TEXT NOT NULL, vcs TEXT, name TEXT,
 icon_url TEXT, icon_url_override TEXT, icon_color TEXT,
 time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL,
 time_initialized INTEGER, sandboxes TEXT NOT NULL, commands TEXT
);
CREATE TABLE session (
 id TEXT PRIMARY KEY, project_id TEXT NOT NULL REFERENCES project(id) ON DELETE CASCADE,
 workspace_id TEXT, parent_id TEXT, slug TEXT NOT NULL, directory TEXT NOT NULL,
 path TEXT, title TEXT NOT NULL, version TEXT NOT NULL, share_url TEXT,
 summary_additions INTEGER, summary_deletions INTEGER, summary_files INTEGER,
 summary_diffs TEXT, metadata TEXT, cost REAL NOT NULL DEFAULT 0,
 tokens_input INTEGER NOT NULL DEFAULT 0, tokens_output INTEGER NOT NULL DEFAULT 0,
 tokens_reasoning INTEGER NOT NULL DEFAULT 0, tokens_cache_read INTEGER NOT NULL DEFAULT 0,
 tokens_cache_write INTEGER NOT NULL DEFAULT 0, revert TEXT, permission TEXT,
 agent TEXT, model TEXT, time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL,
 time_compacting INTEGER, time_archived INTEGER
);
CREATE INDEX session_project_idx ON session(project_id);
CREATE INDEX session_workspace_idx ON session(workspace_id);
CREATE INDEX session_parent_idx ON session(parent_id);
CREATE TABLE message (
 id TEXT PRIMARY KEY, session_id TEXT NOT NULL REFERENCES session(id) ON DELETE CASCADE,
 time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL, data TEXT NOT NULL
);
CREATE INDEX message_session_time_created_id_idx ON message(session_id, time_created, id);
CREATE TABLE part (
 id TEXT PRIMARY KEY, message_id TEXT NOT NULL REFERENCES message(id) ON DELETE CASCADE,
 session_id TEXT NOT NULL, time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL,
 data TEXT NOT NULL
);
CREATE INDEX part_message_id_id_idx ON part(message_id, id);
CREATE INDEX part_session_idx ON part(session_id);
`

func openCodeCorpus(dir string, o options) ([]source, map[parser.AgentType][]string, error) {
	root := filepath.Join(dir, "opencode")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, nil, err
	}
	path := filepath.Join(root, "opencode.db")
	store, err := sql.Open("sqlite3", path)
	if err != nil {
		return nil, nil, err
	}
	complete := false
	defer func() {
		if !complete {
			store.Close()
		}
	}()
	store.SetMaxOpenConns(1)
	if _, err := store.Exec("PRAGMA journal_mode=WAL; PRAGMA foreign_keys=ON;" + openCodeSchema); err != nil {
		return nil, nil, err
	}
	tx, err := store.Begin()
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = tx.Rollback() }()
	start := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	for i := range 20 {
		if _, err := tx.Exec(`INSERT INTO project
 (id, worktree, time_created, time_updated, sandboxes) VALUES (?, ?, ?, ?, '[]')`,
			fmt.Sprintf("project-%02d", i), fmt.Sprintf("/workspace/project-%02d", i), start.UnixMilli(), start.UnixMilli()); err != nil {
			return nil, nil, err
		}
	}
	sources := make([]source, 0, o.Sessions)
	for i := range o.Sessions {
		s := source{Path: path, ID: fmt.Sprintf("ses_%012d", i+1), Agent: parser.AgentOpenCode, Store: store, Start: start.AddDate(0, 0, i%28)}
		if _, err := tx.Exec(`INSERT INTO session
 (id, project_id, slug, directory, title, version, time_created, time_updated)
 VALUES (?, ?, ?, ?, ?, 'simulation', ?, ?)`, s.ID, fmt.Sprintf("project-%02d", i%20), s.ID,
			fmt.Sprintf("/workspace/project-%02d", i%20), "Investigate query latency", s.Start.UnixMilli(), s.Start.UnixMilli()); err != nil {
			return nil, nil, err
		}
		turns := o.Turns
		if i < o.Active && o.ActiveTurns > 0 {
			turns = o.ActiveTurns
		}
		if err := s.writeSQLiteTurns(tx, turns, o.ContentBytes); err != nil {
			return nil, nil, err
		}
		s.Turns = turns
		sources = append(sources, s)
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, err
	}
	complete = true
	return sources, map[parser.AgentType][]string{parser.AgentOpenCode: {root}}, nil
}

func (s *source) writeSQLiteTurns(tx *sql.Tx, n, contentBytes int) error {
	for j := range n {
		turn := s.Turns + j
		for roleIndex, role := range []string{"user", "assistant"} {
			stamp := s.Start.Add(time.Duration(turn)*time.Minute).UnixMilli() + int64(roleIndex)
			id := fmt.Sprintf("msg_%s_%08d_%d", s.ID, turn, roleIndex)
			data := map[string]any{"role": role, "time": map[string]int64{"created": stamp}}
			if role == "assistant" {
				data["parentID"] = fmt.Sprintf("msg_%s_%08d_0", s.ID, turn)
				data["modelID"], data["providerID"] = "gpt-5.4", "openai"
				data["mode"], data["agent"], data["finish"], data["cost"] = "build", "build", "stop", 0
				data["path"] = map[string]string{"cwd": "/workspace/project-a", "root": "/workspace"}
				data["time"] = map[string]int64{"created": stamp, "completed": stamp + 1}
				data["tokens"] = map[string]any{"input": 1000 + turn, "output": 200, "reasoning": 0, "cache": map[string]int{"read": 100, "write": 0}}
			}
			encoded, err := json.Marshal(data)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(`INSERT INTO message (id, session_id, time_created, time_updated, data) VALUES (?, ?, ?, ?, ?)`, id, s.ID, stamp, stamp, string(encoded)); err != nil {
				return err
			}
			text := fmt.Sprintf("Investigate query latency in module %d. ", turn) + strings.Repeat("sample code and context ", contentBytes/24)
			encoded, err = json.Marshal(map[string]string{"type": "text", "text": text})
			if err != nil {
				return err
			}
			if _, err := tx.Exec(`INSERT INTO part (id, message_id, session_id, time_created, time_updated, data) VALUES (?, ?, ?, ?, ?, ?)`, "prt_"+id, id, s.ID, stamp, stamp, string(encoded)); err != nil {
				return err
			}
		}
	}
	_, err := tx.Exec("UPDATE session SET time_updated = ? WHERE id = ?", s.Start.Add(time.Duration(s.Turns+n-1)*time.Minute).UnixMilli()+2, s.ID)
	return err
}

const sqliteEditedText = "Streaming part finalized after session metadata update."

func (s *source) editSQLitePart() error {
	encoded, err := json.Marshal(map[string]string{"type": "text", "text": sqliteEditedText})
	if err != nil {
		return err
	}
	// Only the part watermark advances, exercising the active-session poll.
	_, err = s.Store.Exec("UPDATE part SET data = ?, time_updated = ? WHERE id = ?", string(encoded),
		s.Start.Add(time.Duration(s.Turns-1)*time.Minute).UnixMilli()+10, fmt.Sprintf("prt_msg_%s_%08d_1", s.ID, s.Turns-1))
	return err
}

func pollSQLiteSessions(sources []source) ([]int64, error) {
	watermarks := make([]int64, len(sources))
	for i, s := range sources {
		mtime, err := parser.OpenCodeSourceMtime(parser.OpenCodeSQLiteVirtualPath(s.Path, s.ID))
		if err != nil {
			return nil, err
		}
		if mtime == 0 {
			return nil, fmt.Errorf("active SQLite session missing a watermark")
		}
		watermarks[i] = mtime
	}
	return watermarks, nil
}

func measureSQLiteScans(ctx context.Context, r *report, sources []source, active int) error {
	for _, scan := range []struct {
		name string
		run  func(context.Context, string, func(parser.OpenCodeSessionMeta) error) error
	}{
		{"metadata_scan", parser.ForEachOpenCodeSessionWatermarkMeta},
		{"full_digest_scan", parser.ForEachOpenCodeSessionMeta},
	} {
		if err := r.measure("warm", scan.name, func() error {
			count := 0
			err := scan.run(ctx, sources[0].Path, func(parser.OpenCodeSessionMeta) error { count++; return nil })
			if err != nil {
				return err
			}
			if count != len(sources) {
				return fmt.Errorf("SQLite scan returned %d sessions, want %d", count, len(sources))
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return r.measure("warm", "session_poll", func() error {
		_, err := pollSQLiteSessions(sources[:active])
		return err
	})
}

func syncSQLiteChildEdits(ctx context.Context, r *report, engine *syncengine.Engine, database *db.DB, sources []source) error {
	before, err := pollSQLiteSessions(sources)
	if err != nil {
		return err
	}
	var virtualPaths []string
	for j := range sources {
		s := &sources[j]
		if err := s.editSQLitePart(); err != nil {
			return err
		}
		virtualPaths = append(virtualPaths, parser.OpenCodeSQLiteVirtualPath(s.Path, s.ID))
	}
	if err := r.measure("active", "session_poll", func() error {
		after, err := pollSQLiteSessions(sources)
		if err != nil {
			return err
		}
		for j := range after {
			if after[j] == before[j] {
				return fmt.Errorf("active-session poll missed a child-only edit for session %d", j)
			}
		}
		return nil
	}); err != nil {
		return err
	}
	if err := r.measure("active", "child_edit", func() error { return engine.SyncPathsContext(ctx, virtualPaths) }); err != nil {
		return err
	}
	for j := range sources {
		messages, err := database.GetAllMessages(ctx, "opencode:"+sources[j].ID)
		if err != nil {
			return err
		}
		if len(messages) == 0 || messages[len(messages)-1].Content != sqliteEditedText {
			return fmt.Errorf("child-only edit was not archived for active session %d", j)
		}
	}
	return nil
}
