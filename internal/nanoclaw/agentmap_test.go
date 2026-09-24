package nanoclaw

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/nanoclaw/nanoclawtest"
)

func TestLoadAgentMap(t *testing.T) {
	t.Run("maps_persona_folder_and_channel", func(t *testing.T) {
		dbPath := nanoclawtest.WriteV2DB(t, t.TempDir())
		got, err := LoadAgentMap(t.Context(), dbPath)
		require.NoError(t, err)
		assert.Equal(t, AgentMap{
			"ag-1": {ID: "ag-1", Persona: "helper", Folder: "general", Channel: "general"},
			"ag-2": {ID: "ag-2", Persona: "reviewer", Folder: "reviewer", Channel: "events team"},
			"ag-3": {ID: "ag-3", Persona: "helper", Folder: "steering", Channel: "steering committee"},
		}, got)
	})

	t.Run("channels_join_in_priority_then_created_order", func(t *testing.T) {
		dbPath := nanoclawtest.WriteV2DB(t, t.TempDir())
		nanoclawtest.Exec(t, dbPath, `
			INSERT INTO messaging_groups VALUES
			    ('mg-9', 'chat', '9@g.example', 'chat', 'ops', 1, 'public', '2026-05-06', NULL),
			    ('mg-8', 'chat', '8@g.example', 'chat', 'late', 1, 'public', '2026-05-06', NULL),
			    ('mg-7', 'chat', '7@g.example', 'chat', NULL, 1, 'public', '2026-05-06', NULL);
			INSERT INTO messaging_group_agents VALUES
			    ('mga-9', 'mg-9', 'ag-1', 'shared', 5, '2026-05-07'),
			    ('mga-8', 'mg-8', 'ag-1', 'shared', 0, '2026-05-08'),
			    ('mga-7', 'mg-7', 'ag-1', 'shared', 0, '2026-05-09');`)
		got, err := LoadAgentMap(t.Context(), dbPath)
		require.NoError(t, err)
		// priority DESC, then created_at; a NULL group name is skipped
		// (nanoclaw.rs:143-145).
		assert.Equal(t, "ops, general, late", got["ag-1"].Channel)
	})

	t.Run("agent_without_groups_has_empty_channel", func(t *testing.T) {
		dbPath := nanoclawtest.WriteV2DB(t, t.TempDir())
		nanoclawtest.Exec(t, dbPath,
			`INSERT INTO agent_groups VALUES ('ag-9', 'quiet', 'quiet', NULL, '2026-05-06')`)
		got, err := LoadAgentMap(t.Context(), dbPath)
		require.NoError(t, err)
		assert.Equal(t, Agent{ID: "ag-9", Persona: "quiet", Folder: "quiet"}, got["ag-9"])
	})

	errorCases := []struct {
		name  string
		setup func(t *testing.T, dir string) string
	}{
		{"missing_file", func(t *testing.T, dir string) string {
			return filepath.Join(dir, "v2.db")
		}},
		{"torn_copy", func(t *testing.T, dir string) string {
			path := filepath.Join(dir, "v2.db")
			require.NoError(t, os.WriteFile(path, []byte("not a database"), 0o644))
			return path
		}},
		{"schema_drift", func(t *testing.T, dir string) string {
			path := filepath.Join(dir, "v2.db")
			nanoclawtest.Exec(t, path, `CREATE TABLE agent_groups (id TEXT, name TEXT, folder TEXT)`)
			return path
		}},
		{"null_persona", func(t *testing.T, dir string) string {
			path := nanoclawtest.WriteV2DB(t, dir)
			nanoclawtest.Exec(t, path, `
				CREATE TABLE ag2 AS SELECT * FROM agent_groups;
				DROP TABLE agent_groups;
				CREATE TABLE agent_groups (id TEXT PRIMARY KEY, name TEXT, folder TEXT NOT NULL, agent_provider TEXT, created_at TEXT NOT NULL);
				INSERT INTO agent_groups SELECT * FROM ag2;
				UPDATE agent_groups SET name = NULL WHERE id = 'ag-2';`)
			return path
		}},
	}
	for _, tt := range errorCases {
		t.Run("error/"+tt.name, func(t *testing.T) {
			_, err := LoadAgentMap(t.Context(), tt.setup(t, t.TempDir()))
			require.Error(t, err)
		})
	}
}

func TestFilter(t *testing.T) {
	helper := Agent{ID: "ag-1", Persona: "helper", Folder: "general"}
	steering := Agent{ID: "ag-3", Persona: "helper", Folder: "steering"}
	tests := []struct {
		name       string
		filter     Filter
		agent      Agent
		allowed    bool
		configured bool
	}{
		{"empty_filter_allows", Filter{}, helper, true, false},
		{"exclude_by_persona", Filter{Exclude: []string{"helper"}}, helper, false, true},
		{"exclude_by_folder", Filter{Exclude: []string{"steering"}}, steering, false, true},
		{"exclude_by_id", Filter{Exclude: []string{"ag-1"}}, helper, false, true},
		{"include_allows_match", Filter{Include: []string{"general"}}, helper, true, true},
		{"include_is_allowlist", Filter{Include: []string{"general"}}, steering, false, true},
		{"exclude_wins_over_include", Filter{Include: []string{"helper"}, Exclude: []string{"steering"}}, steering, false, true},
		{"include_other_allows_nothing_else", Filter{Include: []string{"reviewer"}}, helper, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.allowed, tt.filter.Allowed(tt.agent))
			assert.Equal(t, tt.configured, tt.filter.Configured())
		})
	}
}

func TestAgentIDFromPath(t *testing.T) {
	data := filepath.Join(string(filepath.Separator), "srv", "cell")
	join := func(parts ...string) string { return filepath.Join(append([]string{data}, parts...)...) }
	tests := []struct {
		name    string
		dataDir string
		path    string
		want    string
		ok      bool
	}{
		{"cell_transcript", data, join("v2-sessions", "ag-1", ".claude-shared", "projects", "-w", "s.jsonl"), "ag-1", true},
		{"nested_project_dirs", data, join("v2-sessions", "ag-1", ".claude-shared", "projects", "a", "b", "s.jsonl"), "ag-1", true},
		{"trailing_separator_on_data_dir", data + string(filepath.Separator), join("v2-sessions", "ag-2", ".claude-shared", "projects", "-w", "s.jsonl"), "ag-2", true},
		{"outside_v2_sessions", data, join("other", "ag-1", ".claude-shared", "projects", "-w", "s.jsonl"), "", false},
		{"sibling_directory_prefix", data, join("v2-sessions-old", "ag-1", ".claude-shared", "projects", "-w", "s.jsonl"), "", false},
		{"not_under_claude_shared_projects", data, join("v2-sessions", "ag-1", "notes", "s.jsonl"), "", false},
		{"agent_dir_itself", data, join("v2-sessions", "ag-1"), "", false},
		{"remote_rewritten_path", data, "host:" + join("v2-sessions", "ag-1", ".claude-shared", "projects", "-w", "s.jsonl"), "", false},
		{"relative_path", data, filepath.Join("v2-sessions", "ag-1", ".claude-shared", "projects", "-w", "s.jsonl"), "", false},
		{"parent_escape", data, join("v2-sessions", "..", "..", "etc", ".claude-shared", "projects", "x"), "", false},
		{"empty_data_dir", "", join("v2-sessions", "ag-1", ".claude-shared", "projects", "-w", "s.jsonl"), "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := AgentIDFromPath(tt.dataDir, tt.path)
			assert.Equal(t, tt.ok, ok)
			assert.Equal(t, tt.want, got)
		})
	}
}
