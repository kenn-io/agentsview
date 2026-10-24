package db

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/agentsview/internal/export"
)

func setFrictionVersion(t *testing.T, d *DB, id string) {
	t.Helper()
	_, err := d.getWriter().Exec(t.Context(),
		`UPDATE sessions SET friction_rules_version = 'friction-v1' WHERE id = ?`, id)
	require.NoError(t, err)
}

func frictionVersion(t *testing.T, d *DB, id string) string {
	t.Helper()
	var version string
	require.NoError(t, d.getReader().QueryRow(t.Context(),
		`SELECT friction_rules_version FROM sessions WHERE id = ?`, id,
	).Scan(&version))
	return version
}

func fullSession(t *testing.T, d *DB, id string) *Session {
	t.Helper()
	s, err := d.GetSessionFull(t.Context(), id)
	require.NoError(t, err)
	require.NotNil(t, s)
	return s
}

func observationsByProject(t *testing.T, d *DB) map[string][]export.ProjectIdentityObservation {
	t.Helper()
	observations, err := d.ListProjectIdentityObservations(t.Context(), nil)
	require.NoError(t, err)
	byProject := make(map[string][]export.ProjectIdentityObservation)
	for _, obs := range observations {
		byProject[obs.Project] = append(byProject[obs.Project], obs)
	}
	return byProject
}

func nativeMetadata(id, cwd, project, parent string) NativeSessionMetadata {
	return NativeSessionMetadata{
		Cwd: cwd, Project: project, ParentSessionID: parent,
		Identity: export.ProjectIdentityObservation{
			SessionID: id, Project: project, Machine: defaultMachine, RootPath: cwd,
		},
		IdentitySnapshotProject: project,
	}
}

func TestFillNativeSessionMetadataFillsOnlyEmptyValues(t *testing.T) {
	d := testDB(t)
	ctx := t.Context()
	cwd := filepath.Join(t.TempDir(), "synthetic-repo")
	insertSession(t, d, "child", "before")
	setFrictionVersion(t, d, "child")

	require.NoError(t, d.FillNativeSessionMetadata(ctx, "child",
		nativeMetadata("child", cwd, "synthetic_repo", "parent")))
	s := fullSession(t, d, "child")
	assert.Equal(t, cwd, s.Cwd)
	assert.Equal(t, "synthetic_repo", s.Project)
	assert.Equal(t, new("parent"), s.ParentSessionID)
	assert.Equal(t, new("parent"), s.ParserParentSessionID)
	assert.Equal(t, "subagent", s.RelationshipType)
	assert.Empty(t, frictionVersion(t, d, "child"), "a new parent invalidates friction results")
	snapshots, err := d.ListPublishableSessionProjectIdentitySnapshots(ctx, []string{"child"}, nil, nil)
	require.NoError(t, err)
	require.Len(t, snapshots, 1, "the filled workspace's identity is publishable")
	assert.Equal(t, export.ProjectIdentityKeySourceRootPath, snapshots[0].KeySource)
	byProject := observationsByProject(t, d)
	require.Len(t, byProject["synthetic_repo"], 1)
	assert.Equal(t, cwd, byProject["synthetic_repo"][0].RootPath)

	// Stored values are never replaced.
	setFrictionVersion(t, d, "child")
	other := filepath.Join(t.TempDir(), "other")
	require.NoError(t, d.FillNativeSessionMetadata(ctx, "child",
		nativeMetadata("child", other, "other", "other-parent")))
	s = fullSession(t, d, "child")
	assert.Equal(t, cwd, s.Cwd)
	assert.Equal(t, "synthetic_repo", s.Project)
	assert.Equal(t, new("parent"), s.ParentSessionID)
	assert.Equal(t, "friction-v1", frictionVersion(t, d, "child"))
}

func TestFillNativeSessionMetadataIdentityFollowsStoredProject(t *testing.T) {
	for _, tc := range []struct {
		name     string
		assigned bool
		trashed  bool
	}{
		{name: "assigned project", assigned: true},
		{name: "trashed session", trashed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := testDB(t)
			ctx := t.Context()
			cwd := filepath.Join(t.TempDir(), "synthetic-repo")
			insertSession(t, d, "child", "before")
			stored := "synthetic_repo"
			if tc.assigned {
				_, err := d.AssignSessionProject(ctx, "child", "chosen")
				require.NoError(t, err)
				stored = "chosen"
			}
			if tc.trashed {
				require.NoError(t, d.SoftDeleteSession(ctx, "child"))
			}

			require.NoError(t, d.FillNativeSessionMetadata(ctx, "child",
				nativeMetadata("child", cwd, "synthetic_repo", "")))
			s := fullSession(t, d, "child")
			assert.Equal(t, cwd, s.Cwd)
			assert.Equal(t, stored, s.Project)
			snapshots, err := d.listSessionProjectIdentitySnapshots(ctx, []string{"child"})
			require.NoError(t, err)
			require.Contains(t, snapshots, "child")
			assert.Equal(t, "synthetic_repo", snapshots["child"].Project, "the snapshot keeps its parser-time label")

			byProject := observationsByProject(t, d)
			if tc.trashed {
				assert.Empty(t, byProject, "a trashed session publishes no aggregate evidence")
				return
			}
			require.Len(t, byProject, 1)
			require.Len(t, byProject[stored], 1, "evidence is published under the stored project")
			assert.Equal(t, cwd, byProject[stored][0].RootPath)
		})
	}
}

func TestFillNativeSessionMetadataParentAndLauncherLinks(t *testing.T) {
	t.Run("native parent overrides a launcher link", func(t *testing.T) {
		d := testDB(t)
		ctx := t.Context()
		for _, id := range []string{"child", "launcher", "native"} {
			insertSession(t, d, id, "p")
		}
		_, err := d.SetSessionExternalParent(ctx, "child", "launcher")
		require.NoError(t, err)
		require.Equal(t, new("launcher"), fullSession(t, d, "child").ParentSessionID)

		require.NoError(t, d.FillNativeSessionMetadata(ctx, "child", NativeSessionMetadata{ParentSessionID: "native"}))
		child := fullSession(t, d, "child")
		assert.Equal(t, new("native"), child.ParentSessionID)
		assert.Equal(t, new("native"), child.ParserParentSessionID)
		link, err := d.GetSessionExternalParent(ctx, "child")
		require.NoError(t, err)
		assert.False(t, link.Applied, "the launcher link stays recorded but no longer applies")
	})
	t.Run("native parent never closes a cycle with a launcher link", func(t *testing.T) {
		d := testDB(t)
		ctx := t.Context()
		insertSession(t, d, "launched", "p")
		insertSession(t, d, "launcher", "p")
		_, err := d.SetSessionExternalParent(ctx, "launched", "launcher")
		require.NoError(t, err)

		require.NoError(t, d.FillNativeSessionMetadata(ctx, "launcher", NativeSessionMetadata{ParentSessionID: "launched"}))
		assert.Equal(t, new("launched"), fullSession(t, d, "launcher").ParentSessionID)
		assert.Nil(t, fullSession(t, d, "launched").ParentSessionID)
	})
}

func TestCopySessionMetadataKeepsFreshIdentityOverEmptyPlaceholder(t *testing.T) {
	dir := t.TempDir()
	ctx := t.Context()
	oldPath := filepath.Join(dir, "old.db")
	oldDB, err := Open(ctx, oldPath)
	require.NoError(t, err)
	// The insert trigger leaves a keyless placeholder with no root.
	require.NoError(t, oldDB.UpsertSession(ctx, Session{
		ID: "session-a", Project: "app", Machine: "host", Agent: "antigravity",
	}))
	require.NoError(t, oldDB.Close())

	fresh := testDB(t)
	root := filepath.Join(dir, "workspace")
	require.NoError(t, fresh.UpsertSession(ctx, Session{
		ID: "session-a", Project: "app", Machine: "host", Agent: "antigravity", Cwd: root,
	}))
	require.NoError(t, fresh.UpsertProjectIdentityObservation(ctx, export.ProjectIdentityObservation{
		SessionID: "session-a", Project: "app", Machine: "host", RootPath: root,
	}))

	require.NoError(t, fresh.CopySessionMetadataFrom(oldPath))
	publishable, err := fresh.ListPublishableSessionProjectIdentitySnapshots(ctx, []string{"session-a"}, nil, nil)
	require.NoError(t, err)
	require.Len(t, publishable, 1, "an evidence-free placeholder must not replace the fresh identity")
	assert.Equal(t, root, publishable[0].RootPath)
}
