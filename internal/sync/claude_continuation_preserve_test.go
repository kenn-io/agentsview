package sync

import (
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestClaudeContinuationWriteFailureContinuesBatch(t *testing.T) {
	database := openTestDB(t)
	engine := NewEngine(t.Context(), database, EngineConfig{Machine: "local"})
	t.Cleanup(engine.Close)
	require.NoError(t, database.Update(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `CREATE TRIGGER reject_claude_sources
			BEFORE INSERT ON claude_subagent_sources
			BEGIN SELECT RAISE(FAIL, 'reject source provenance'); END`)
		return err
	}))
	root := t.TempDir()
	batch := []pendingWrite{
		{
			sess: parser.ParsedSession{
				ID: "agent-reviewer", Agent: parser.AgentClaude, Machine: "local", Project: "project",
				ClaudeSubagentSources: []string{filepath.Join(root, "first.jsonl"), filepath.Join(root, "second.jsonl")},
			},
			msgs: []parser.ParsedMessage{{Ordinal: 0, Role: parser.RoleUser, Content: "rejected"}},
		},
		{
			sess: parser.ParsedSession{ID: "independent", Agent: parser.AgentClaude, Machine: "local", Project: "project"},
			msgs: []parser.ParsedMessage{{Ordinal: 0, Role: parser.RoleUser, Content: "saved"}},
		},
	}
	outcome := engine.writeBatchWithOutcome(batch, syncWriteDefault, true)
	require.Equal(t, 1, outcome.failedSessions)
	require.Equal(t, 1, outcome.writtenSessions)
	messages, err := database.GetAllMessages(t.Context(), "independent")
	require.NoError(t, err)
	require.Len(t, messages, 1)
	assert.Equal(t, "saved", messages[0].Content)
}

func TestClaudeContinuationMissingSourcePreservesArchive(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		rebuild, symlink, single bool
	}{
		{name: "refresh"},
		{name: "rebuild", rebuild: true},
		{name: "symlink", symlink: true},
		{name: "single-session import", single: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.symlink && runtime.GOOS == "windows" {
				t.Skip("symlink creation requires privileges on Windows")
			}
			root := t.TempDir()
			project := filepath.Join(root, "project")
			paths := []string{
				filepath.Join(project, "parent-a", "subagents", "agent-reviewer.jsonl"),
				filepath.Join(project, "parent-b", "subagents", "agent-reviewer.jsonl"),
				filepath.Join(project, "parent.jsonl"),
			}
			contents := []string{
				testjsonl.NewSessionBuilder().AddClaudeUser("2026-08-05T03:40:00Z", "A0").
					AddClaudeAssistant("2026-08-05T03:41:00Z", "A1").String(),
				testjsonl.NewSessionBuilder().AddClaudeUser("2026-08-05T03:42:00Z", "B0").
					AddClaudeAssistant("2026-08-05T03:43:00Z", "B1").String(),
				testjsonl.NewSessionBuilder().AddClaudeUser("2026-08-05T03:39:00Z", "parent").
					AddClaudeAssistant("2026-08-05T03:44:00Z", "done").String(),
			}
			for i, path := range paths {
				require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
				require.NoError(t, os.WriteFile(path, []byte(contents[i]), 0o600))
			}
			database := openTestDB(t)
			cfg := EngineConfig{Machine: "local", AgentDirs: map[parser.AgentType][]string{parser.AgentClaude: {root}}}
			engine := NewEngine(t.Context(), database, cfg)
			if tc.single {
				require.NoError(t, os.Remove(paths[1]))
				require.NoError(t, engine.SyncPathsContext(t.Context(), []string{paths[0], paths[2]}))
				require.NoError(t, os.WriteFile(paths[1], []byte(contents[1]), 0o600))
				require.NoError(t, os.WriteFile(paths[0], []byte(contents[0]+"{\"type\":\"ai-title\",\"aiTitle\":\"Review\"}\n"), 0o600))
				require.NoError(t, engine.SyncSingleSessionContext(t.Context(), "agent-reviewer"))
			} else {
				require.NoError(t, engine.SyncPathsContext(t.Context(), paths))
			}
			stored, err := database.GetSessionFull(t.Context(), "agent-reviewer")
			require.NoError(t, err)
			require.NotNil(t, stored)
			require.NotNil(t, stored.FilePath)
			require.Equal(t, 4, stored.MessageCount)
			missing := 0
			if *stored.FilePath == paths[0] {
				missing = 1
			}
			engine.Close()
			dbPath := database.Path()
			require.NoError(t, database.Close())
			database, err = db.Open(t.Context(), dbPath)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, database.Close()) })
			engine = NewEngine(t.Context(), database, cfg)
			t.Cleanup(engine.Close)
			require.NoError(t, os.Remove(paths[missing]))
			if tc.symlink {
				target := filepath.Join(t.TempDir(), "moved.jsonl")
				require.NoError(t, os.WriteFile(target, []byte(contents[missing]), 0o600))
				require.NoError(t, os.Symlink(target, paths[missing]))
			}
			// Two rebuilds prove preservation also copies source ownership.
			for range 2 {
				if tc.rebuild {
					stats, err := engine.ResyncAllWithOptions(t.Context(), nil, RebuildOptions{})
					require.NoError(t, err)
					require.Zero(t, stats.Failed)
				} else {
					stats := engine.SyncAllForceParse(t.Context(), nil)
					require.Zero(t, stats.Failed)
				}
				messages, err := database.GetAllMessages(t.Context(), "agent-reviewer")
				require.NoError(t, err)
				got := make([]string, len(messages))
				for i, message := range messages {
					got[i] = message.Content
				}
				assert.Equal(t, []string{"A0", "A1", "B0", "B1"}, got)
			}
			// Readable corrections remain authoritative once all contributors return.
			if tc.symlink {
				require.NoError(t, os.Remove(paths[missing]))
			}
			require.NoError(t, os.WriteFile(paths[missing], []byte(contents[missing]), 0o600))
			corrected := testjsonl.NewSessionBuilder().
				AddClaudeUser("2026-08-05T03:40:00Z", "corrected").String()
			require.NoError(t, os.WriteFile(paths[0], []byte(corrected), 0o600))
			require.Zero(t, engine.SyncAllForceParse(t.Context(), nil).Failed)
			messages, err := database.GetAllMessages(t.Context(), "agent-reviewer")
			require.NoError(t, err)
			require.Len(t, messages, 3)
			assert.Equal(t, "corrected", messages[0].Content)
		})
	}
}

func TestClaudeS3WritesDoNotRetainTemporaryContributors(t *testing.T) {
	database := openTestDB(t)
	engine := NewEngine(t.Context(), database, EngineConfig{Machine: "local"})
	t.Cleanup(engine.Close)
	write := pendingWrite{
		sess: parser.ParsedSession{
			ID: "s3:agent-reviewer", Agent: parser.AgentClaude, Machine: "local", Project: "project",
			File:                  parser.FileInfo{Path: "s3://example-bucket/project/parent/subagents/agent-reviewer.jsonl"},
			ClaudeSubagentSources: []string{filepath.Join(t.TempDir(), "removed-materialization.jsonl")},
		},
		msgs: []parser.ParsedMessage{{Ordinal: 0, Role: parser.RoleUser, Content: "original"}},
	}
	require.Equal(t, 1, engine.writeBatchWithOutcome([]pendingWrite{write}, syncWriteDefault, true).writtenSessions)
	paths, err := database.GetClaudeSubagentSources(t.Context(), write.sess.ID)
	require.NoError(t, err)
	assert.Empty(t, paths)
	write.msgs[0].Content = "updated"
	require.Equal(t, 1, engine.writeBatchWithOutcome([]pendingWrite{write}, syncWriteDefault, true).writtenSessions)
	messages, err := database.GetAllMessages(t.Context(), write.sess.ID)
	require.NoError(t, err)
	require.Len(t, messages, 1)
	assert.Equal(t, "updated", messages[0].Content)
}
