package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/parser"
	"go.kenn.io/agentsview/internal/testjsonl"
)

func TestWatchStartupUsesConfiguredWorktreeMappings(t *testing.T) {
	ctx := t.Context()
	dataDir, sourceDir := t.TempDir(), t.TempDir()
	t.Setenv("AGENTSVIEW_DATA_DIR", dataDir)
	const id = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	const freshID = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	const sessionID = "codex:" + id
	// Only the transcript is mounted. Neither the repository nor this checkout
	// exists on the importing host.
	cwd := filepath.Join(t.TempDir(), "code", "sample-app.abc1")
	source := filepath.Join(sourceDir, "rollout-"+id+".jsonl")
	seedRollout := func(t *testing.T, id string) {
		t.Helper()
		meta := testjsonl.CodexSessionMetaWithFieldsJSON(id, cwd, "codex_cli_rs", "2026-10-08T12:00:00Z",
			map[string]any{"git": map[string]any{"branch": "feat/abc1-retry-logic"}})
		dialogue := testjsonl.CodexMsgJSON("user", "add retry handling", "2026-10-08T12:00:01Z")
		path := filepath.Join(sourceDir, "rollout-"+id+".jsonl")
		require.NoError(t, os.WriteFile(path, []byte(meta+"\n"+dialogue+"\n"), 0o600))
	}
	seedRollout(t, id)

	var baselineProject string
	for _, tc := range []struct {
		name, mapping, project string
		full                   bool
	}{
		{"baseline", "", "", false},
		{"suffix", "layout = 'repo_dot_suffix'", "sample_app", false},
		{"change on resync", "project = 'canonical-app'", "canonical_app", true},
		{"remove on resync", "", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conf := ""
			if tc.mapping != "" {
				conf = "[[worktree_mappings]]\npath_prefix = '" + filepath.ToSlash(filepath.Dir(cwd)) + "'\n" + tc.mapping + "\n"
			}
			require.NoError(t, os.WriteFile(filepath.Join(dataDir, "config.toml"), []byte(conf), 0o600))
			cfg, err := config.LoadMinimal()
			require.NoError(t, err)
			cfg.AgentDirs = map[parser.AgentType][]string{parser.AgentCodex: {sourceDir}}
			for _, def := range parser.Registry {
				if def.Type != parser.AgentCodex {
					cfg.DisabledAgents = append(cfg.DisabledAgents, def.Type)
				}
			}
			database, err := openDB(ctx, cfg)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, database.Close()) })
			owner := localArchiveWriteBackend{appCfg: cfg, database: database}
			engine, err := owner.startWatchEngine(ctx, nil)
			require.NoError(t, err)
			t.Cleanup(engine.Close)

			if tc.name == "suffix" {
				seedRollout(t, freshID)
				// Append to an existing session to exercise the incremental
				// write path without a full reparse.
				f, err := os.OpenFile(source, os.O_WRONLY|os.O_APPEND, 0o600)
				require.NoError(t, err)
				_, err = f.WriteString(testjsonl.CodexMsgJSON("assistant", "added retry handling", "2026-10-08T12:00:02Z") + "\n")
				require.NoError(t, err)
				require.NoError(t, f.Close())
			}
			didResync, err := runPGWatchStartupSync(ctx, engine, tc.full)
			require.NoError(t, err)
			assert.Equal(t, tc.full, didResync)
			session, err := database.GetSessionFull(ctx, sessionID)
			require.NoError(t, err)
			require.NotNil(t, session)
			if tc.name == "baseline" {
				baselineProject = session.Project
				assert.NotEqual(t, "sample_app", baselineProject)
			} else if tc.project == "" {
				assert.Equal(t, baselineProject, session.Project)
			} else {
				assert.Equal(t, tc.project, session.Project)
			}
			if tc.name != "baseline" {
				fresh, err := database.GetSessionFull(ctx, "codex:"+freshID)
				require.NoError(t, err)
				require.NotNil(t, fresh)
				assert.Equal(t, session.Project, fresh.Project, "new and existing sessions converge")
				assert.Equal(t, 2, session.MessageCount)
			}
			if tc.name == "suffix" {
				assert.True(t, session.LastWriteIncremental)
			}
			rules, err := database.ListAllWorktreeProjectMappings(ctx)
			require.NoError(t, err)
			assert.Empty(t, rules)
			revision, err := database.WorktreeMappingPublicationRevision(ctx)
			require.NoError(t, err)
			assert.Zero(t, revision)
		})
	}
}
