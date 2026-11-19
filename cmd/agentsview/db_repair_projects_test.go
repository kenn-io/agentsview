package main

import (
	"bytes"
	"database/sql"
	"encoding/json/v2"
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
				obs := export.WithRecordedGitRemote(export.ProjectIdentityObservation{
					SessionID: s.ID, Project: s.Project, Machine: s.Machine, RootPath: s.Cwd, ObservedAt: time.Now().UTC(),
				}, "https://example.com/team/sample.git")
				require.NoError(t, database.UpsertSessionWithProjectIdentity(ctx, s, obs, s.Project))
				require.NoError(t, database.UpsertSession(ctx, s))
			}
			if tc.pendingUpgrade {
				require.NoError(t, database.Update(ctx, func(tx *sql.Tx) error {
					_, err := tx.ExecContext(ctx, `DELETE FROM stats WHERE key = 'repository_projects_repaired'`)
					return err
				}))
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
}
