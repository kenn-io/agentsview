package rawarchive

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/dbtest"
	"go.kenn.io/agentsview/internal/testjsonl"
	"go.kenn.io/agentsview/internal/worktree"
)

func TestReparseUsesConfiguredWorktreeMappingsOnlyForLocalOwner(t *testing.T) {
	const owner = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const foreign = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	const id = "019eb791-cf7d-75c1-8439-9ed74c122e02"
	const cwd = "/code/app.dev"
	for _, tc := range []struct {
		name, device, sessionID, project string
	}{
		{"same_owner", owner, id, "configured"},
		{"foreign_owner", foreign, foreign + "~" + id, "app.dev"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			root := t.TempDir()
			path := filepath.Join(root, "app.dev", id+".jsonl")
			dbtest.WriteTestFile(t, path, []byte(testjsonl.NewSessionBuilder().
				AddClaudeUserWithSessionID("2026-01-01T00:00:00Z", "retained transcript", id, cwd).String()))
			capture := newImportCapture(t, tc.device, "source", RootSpec{Provider: "claude", Path: root})
			spec := loadTestCapture(t, &capture)

			data := t.TempDir()
			dbtest.WriteTestFile(t, filepath.Join(data, "telemetry-install-id"), []byte(owner))
			database := dbtest.OpenTestDB(t)
			require.NoError(t, database.EnableArchiveOnly(ctx))
			database.SetConfiguredWorktreeMappings(owner, []worktree.Rule{
				{PathPrefix: "/code", Layout: worktree.LayoutExplicit, Project: "configured"},
			})
			archive, err := Open(ctx, database, data, nil)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, archive.Close()) })
			report, err := archive.Import(ctx, spec)
			require.NoError(t, err)
			require.Empty(t, report.Gaps)
			require.Equal(t, 1, report.Sources)
			require.NoError(t, os.Remove(path))

			report, err = archive.Reparse(ctx, ReparseOptions{All: true, ScratchBytes: 1 << 20})
			require.NoError(t, err)
			require.Equal(t, 1, report.Parsed)
			session, err := database.GetSessionFull(ctx, tc.sessionID)
			require.NoError(t, err)
			require.NotNil(t, session)
			assert.Equal(t, tc.device, session.Machine)
			assert.Equal(t, cwd, session.Cwd)
			assert.Equal(t, tc.project, session.Project)
			rules, err := database.ListAllWorktreeProjectMappings(ctx)
			require.NoError(t, err)
			assert.Empty(t, rules)
			revision, err := database.WorktreeMappingPublicationRevision(ctx)
			require.NoError(t, err)
			assert.Zero(t, revision)
		})
	}
}
