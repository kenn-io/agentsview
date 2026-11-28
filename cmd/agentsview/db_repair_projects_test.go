package main

import (
	"bytes"
	"database/sql"
	"encoding/json/v2"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/config"
	"go.kenn.io/agentsview/internal/db"
	"go.kenn.io/agentsview/internal/export"
)

func TestDBRepairProjectsPreviewAndApply(t *testing.T) {
	for _, tc := range []struct {
		name           string
		pendingUpgrade bool
		project        string
		wantCount      int
		wantOther      string
	}{
		{"upgraded_filtered", false, "pr123", 1, "pr456"},
		{"pending_filtered", true, "pr123", 1, "pr456"},
		{"upgraded_all", false, "", 2, "sample"},
		{"pending_all", true, "", 2, "sample"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testDataDir(t)
			ctx := t.Context()
			cfg, err := config.LoadReadOnly()
			require.NoError(t, err)
			database, err := db.Open(ctx, cfg.DBPath)
			require.NoError(t, err)
			for _, project := range []string{"pr123", "pr456"} {
				s := db.Session{ID: project, Agent: "codex", Machine: "local", Project: project, Cwd: "/workspace/" + project}
				seedLegacyRepositoryProject(t, database, s, "https://example.com/team/sample.git")
			}
			if tc.pendingUpgrade {
				clearRepositoryProjectRepairMarker(t, database)
			}
			require.NoError(t, database.Close())

			args := []string{"--format", "json"}
			if tc.project != "" {
				args = append(args, "--project", tc.project)
			}
			var preview db.RepositoryProjectRepair
			for _, apply := range []bool{false, true} {
				cmd := newDBRepairProjectsCommand()
				if apply {
					args = append(args, "--apply")
				}
				cmd.SetArgs(args)
				var output bytes.Buffer
				cmd.SetOut(&output)
				require.NoError(t, cmd.Execute())
				var report db.RepositoryProjectRepair
				require.NoError(t, json.Unmarshal(output.Bytes(), &report))
				assert.Equal(t, tc.wantCount, report.Sessions)
				if apply {
					assert.Equal(t, preview, report, "apply reports exactly the previewed scope")
				} else {
					preview = report
				}
				database, err := db.OpenReadOnly(ctx, cfg.DBPath)
				require.NoError(t, err)
				for id, repaired := range map[string]string{"pr123": "sample", "pr456": tc.wantOther} {
					got, err := database.GetSessionFull(ctx, id)
					require.NoError(t, err)
					require.NotNil(t, got)
					want := id
					if apply {
						want = repaired
					}
					assert.Equal(t, want, got.Project, id)
				}
				require.NoError(t, database.Close())
			}
			if tc.pendingUpgrade {
				// A filtered repair must leave the automatic upgrade pending.
				database, err := db.Open(ctx, cfg.DBPath)
				require.NoError(t, err)
				defer database.Close()
				got, err := database.GetSessionFull(ctx, "pr456")
				require.NoError(t, err)
				assert.Equal(t, "sample", got.Project)
			}
		})
	}

	t.Run("configured_only", func(t *testing.T) {
		ctx := t.Context()
		cfg := loadRepositoryProjectRepairConfig(t)
		database, err := db.Open(ctx, cfg.DBPath)
		require.NoError(t, err)
		t.Cleanup(func() { _ = database.Close() })
		seedLegacyRepositoryProject(t, database, db.Session{
			ID: "configured", Agent: "codex", Machine: cfg.InstallationID, Cwd: "/workspace/app.dev", Project: "app",
		}, "https://example.com/team/other.git")
		clearRepositoryProjectRepairMarker(t, database)
		require.NoError(t, database.Close())

		for _, args := range [][]string{{"--format", "json"}, {"--format", "json", "--apply"}} {
			cmd := newDBRepairProjectsCommand()
			cmd.SetContext(ctx)
			output, err := executeCommand(cmd, args...)
			require.NoError(t, err)
			var report db.RepositoryProjectRepair
			require.NoError(t, json.Unmarshal([]byte(output), &report))
			assert.Zero(t, report.Sessions, "%v", args)
			assert.Empty(t, report.Changes, "%v", args)
		}

		reader, err := db.OpenReadOnly(ctx, cfg.DBPath)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, reader.Close()) })
		assertRepositorySessionProject(t, reader, "configured", "app")
		assert.False(t, repositoryProjectRepairCompleted(t, reader), "explicit repair must leave the startup migration pending")
		rules, err := reader.ListAllWorktreeProjectMappings(ctx)
		require.NoError(t, err)
		assert.Empty(t, rules)
	})
}

func TestRepositoryProjectUpgradeUsesConfiguredWorktreeMappings(t *testing.T) {
	for _, tc := range []struct {
		name    string
		upgrade func(*testing.T, config.Config)
	}{
		{"ordinary_open", func(t *testing.T, cfg config.Config) {
			t.Helper()
			database, err := openDB(t.Context(), cfg)
			require.NoError(t, err)
			require.NoError(t, database.Close())
		}},
		{"startup_worker", func(t *testing.T, cfg config.Config) {
			t.Helper()
			database, lock, err := openWorkerWriteDB(t.Context(), cfg, true, nil)
			require.NoError(t, err)
			closeWriteDB(database, lock)
		}},
		{"adopt_machine", func(t *testing.T, _ config.Config) {
			t.Helper()
			cmd := newDBAdoptMachineCommand()
			cmd.SetContext(t.Context())
			_, err := executeCommand(cmd, "former-machine")
			require.NoError(t, err)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			cfg := loadRepositoryProjectRepairConfig(t)
			database, err := db.Open(ctx, cfg.DBPath)
			require.NoError(t, err)
			t.Cleanup(func() { _ = database.Close() })
			_, err = database.CreateWorktreeProjectMapping(ctx, db.WorktreeProjectMapping{
				Machine: cfg.InstallationID, PathPrefix: "/stored", Project: "app", Enabled: true,
			})
			require.NoError(t, err)
			for _, s := range []db.Session{
				{ID: "stored-rule", Agent: "codex", Machine: cfg.InstallationID, Cwd: "/stored/app.dev", Project: "app"},
				{ID: "config-rule", Agent: "codex", Machine: cfg.InstallationID, Cwd: "/workspace/app.dev", Project: "app"},
				{ID: "former-owner", Agent: "codex", Machine: "former-machine", Cwd: "/unmapped/app.dev", Project: "app"},
			} {
				seedLegacyRepositoryProject(t, database, s, "https://example.com/team/other.git")
			}
			clearRepositoryProjectRepairMarker(t, database)
			require.NoError(t, database.Close())

			tc.upgrade(t, cfg)

			// Reopen read-only so a second writable open cannot run the migration
			// on behalf of the command we are checking.
			reader, err := db.OpenReadOnly(ctx, cfg.DBPath)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, reader.Close()) })
			assertRepositorySessionProject(t, reader, "stored-rule", "app")
			assertRepositorySessionProject(t, reader, "config-rule", "app")
			assertRepositorySessionProject(t, reader, "former-owner", "other")
			assert.True(t, repositoryProjectRepairCompleted(t, reader))
			rules, err := reader.ListAllWorktreeProjectMappings(ctx)
			require.NoError(t, err)
			require.Len(t, rules, 1)
			assert.Equal(t, "/stored", rules[0].PathPrefix)
		})
	}
}

func loadRepositoryProjectRepairConfig(t *testing.T) config.Config {
	t.Helper()
	dataDir := testDataDir(t)
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "config.toml"), []byte(`
[[worktree_mappings]]
path_prefix = "/workspace"
layout = "repo_dot_suffix"
`), 0o600))
	cfg, err := config.LoadMinimalFor(config.ScopeLocal)
	require.NoError(t, err)
	require.NoError(t, config.ValidateInstallationID(cfg.InstallationID))
	return cfg
}

func seedLegacyRepositoryProject(t *testing.T, database *db.DB, s db.Session, remote string) {
	t.Helper()
	ctx := t.Context()
	obs := export.WithRecordedGitRemote(export.ProjectIdentityObservation{
		SessionID: s.ID, Project: s.Project, Machine: s.Machine, RootPath: s.Cwd,
		ObservedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}, remote)
	require.NoError(t, database.UpsertSessionWithProjectIdentity(ctx, s, obs, s.Project))
	// Reproduce the effective label stored by an older importer even though
	// identity-aware writes can already use the recorded repository.
	require.NoError(t, database.UpsertSession(ctx, s))
}

func clearRepositoryProjectRepairMarker(t *testing.T, database *db.DB) {
	t.Helper()
	ctx := t.Context()
	require.NoError(t, database.Update(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM stats WHERE key = 'repository_projects_repaired'`)
		return err
	}))
}

func repositoryProjectRepairCompleted(t *testing.T, database *db.DB) bool {
	t.Helper()
	var complete bool
	require.NoError(t, database.Reader().QueryRowContext(t.Context(), `SELECT EXISTS (
		SELECT 1 FROM stats WHERE key = 'repository_projects_repaired' AND value = 1
	)`).Scan(&complete))
	return complete
}

func assertRepositorySessionProject(t *testing.T, database *db.DB, id, project string) {
	t.Helper()
	got, err := database.GetSessionFull(t.Context(), id)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, project, got.Project, id)
}
